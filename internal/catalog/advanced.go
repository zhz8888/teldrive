// Package catalog owns the metadata of the file tree: folders and files, path
// resolution, listing and search, rename, move and copy conflict handling, trash
// and restore, and the per-file part records that point at Telegram messages.
//
// Reads and writes are scoped to one owner: their SQL carries the caller's user
// ID, so an ID belonging to another user is reported as ErrNotFound instead of
// exposing somebody else's row. The part-size backfills are the exception, as
// they take no user ID and rely on the caller having checked ownership.
// Single-entry mutations are one statement;
// multi-row operations such as BulkTrash and MoveWithPolicy/BulkMove run in a
// single transaction and invalidate the affected cache entries only after the
// commit succeeds. The zero value of Service is not usable: build one with
// NewService.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

// validCategories holds the category filters accepted by listAdvanced. Each key
// matches a bucket that ListFilesAdvanced derives from MIME type and file name,
// so an unknown value is rejected up front instead of silently matching nothing.
var validCategories = map[string]struct{}{
	"archive": {}, "audio": {}, "document": {}, "image": {}, "video": {}, "other": {},
}

// ResolveFolderPath resolves a slash-separated folder path below rootID. A nil
// rootID means the user's drive root; an empty path returns rootID unchanged.
func (s *Service) ResolveFolderPath(ctx context.Context, userID int64, rootID *uuid.UUID, rawPath string) (*uuid.UUID, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	path := strings.Trim(strings.TrimSpace(rawPath), "/")
	if path == "" {
		if rootID == nil {
			return nil, nil
		}
		copyID := *rootID
		return &copyID, nil
	}
	if strings.Contains(path, "\\") {
		return nil, ErrInvalidParent
	}
	current := rootID
	for component := range strings.SplitSeq(path, "/") {
		if component == "" || component == "." || component == ".." {
			return nil, ErrInvalidParent
		}
		id, err := s.queries.ResolveActiveChildFolder(ctx, sqlcgen.ResolveActiveChildFolderParams{
			UserID: userID, ParentID: dbtypes.OptionalUUID(current), Name: component,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidParent
		}
		if err != nil {
			return nil, fmt.Errorf("resolve folder path: %w", err)
		}
		resolved, ok := dbtypes.GoogleUUID(id)
		if !ok {
			return nil, ErrInvalidParent
		}
		current = &resolved
	}
	return current, nil
}

// EnsureFolderPath resolves a slash-separated folder path and creates missing folders.
func (s *Service) EnsureFolderPath(ctx context.Context, userID int64, rootID *uuid.UUID, rawPath string) (*uuid.UUID, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	path := strings.Trim(strings.TrimSpace(rawPath), "/")
	if path == "" {
		if rootID == nil {
			return nil, nil
		}
		copyID := *rootID
		return &copyID, nil
	}
	if strings.Contains(path, "\\") {
		return nil, ErrInvalidParent
	}
	components := strings.Split(path, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." || isBlankName(component) {
			return nil, ErrInvalidParent
		}
	}

	current := rootID
	for _, component := range components {
		id, err := s.queries.ResolveActiveChildFolder(ctx, sqlcgen.ResolveActiveChildFolderParams{
			UserID: userID, ParentID: dbtypes.OptionalUUID(current), Name: component,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			folder, createErr := s.CreateFolder(ctx, CreateFolderInput{UserID: userID, ParentID: current, Name: component})
			if errors.Is(createErr, ErrConflict) {
				id, err = s.queries.ResolveActiveChildFolder(ctx, sqlcgen.ResolveActiveChildFolderParams{
					UserID: userID, ParentID: dbtypes.OptionalUUID(current), Name: component,
				})
			} else if createErr != nil {
				return nil, createErr
			} else {
				id = folder.ID
				err = nil
			}
		}
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The name is taken by something other than an active folder, so
				// the retry after a create conflict cannot resolve it. That is a
				// name conflict, not an internal failure.
				return nil, fmt.Errorf("%w: %q exists and is not a folder", ErrConflict, component)
			}
			return nil, fmt.Errorf("ensure folder path: %w", err)
		}
		resolved, ok := dbtypes.GoogleUUID(id)
		if !ok {
			return nil, ErrInvalidParent
		}
		current = &resolved
	}
	return current, nil
}

// listAdvanced serves the filtered branch of List. It validates the filter
// vocabulary before touching the database (search type, sort key, order, updated
// window and categories) and reports every invalid filter as ErrInvalidFilter,
// never as ErrInvalidParent. A regex search is compiled first so a broken pattern
// is rejected rather than sent to PostgreSQL. When in.AfterID is set the keyset
// cursor is decoded according to in.Sort: "name" takes the name itself, updatedAt
// expects RFC3339Nano and size a decimal byte count, and "id" needs no cursor value
// because it compares IDs alone. in.AfterValue takes precedence over in.AfterName.
func (s *Service) listAdvanced(ctx context.Context, in ListInput) ([]*sqlcgen.File, error) {
	if in.SearchType != "text" && in.SearchType != "regex" {
		return nil, ErrInvalidFilter
	}
	if in.Sort != "name" && in.Sort != "updatedAt" && in.Sort != "size" && in.Sort != "id" {
		return nil, ErrInvalidFilter
	}
	if in.Order != "asc" && in.Order != "desc" {
		return nil, ErrInvalidFilter
	}
	if in.UpdatedAfter != nil && in.UpdatedBefore != nil && !in.UpdatedAfter.Before(*in.UpdatedBefore) {
		return nil, ErrInvalidFilter
	}
	for _, category := range in.Categories {
		if _, ok := validCategories[category]; !ok {
			return nil, ErrInvalidFilter
		}
	}

	var search *string
	if value := strings.TrimSpace(in.Search); value != "" {
		if in.SearchType == "regex" {
			if _, err := regexp.Compile(value); err != nil {
				return nil, ErrInvalidFilter
			}
			search = &value
		} else {
			search = &value
		}
	}

	var kind sqlcgen.NullFileKind
	if in.Kind != nil {
		kind = sqlcgen.NullFileKind{FileKind: *in.Kind, Valid: true}
	}
	categories := in.Categories
	if categories == nil {
		categories = []string{}
	}
	var afterName *string
	var afterUpdatedAt *time.Time
	var afterSize *int64
	if in.AfterID != nil && in.Sort != "id" {
		value := in.AfterValue
		if value == "" {
			value = in.AfterName
		}
		switch in.Sort {
		case "name":
			afterName = &value
		case "updatedAt":
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, ErrInvalidFilter
			}
			afterUpdatedAt = &parsed
		case "size":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, ErrInvalidFilter
			}
			afterSize = &parsed
		}
	}

	files, err := s.queries.ListFilesAdvanced(ctx, sqlcgen.ListFilesAdvancedParams{
		UserID: in.UserID, Scope: in.Scope, ScopeFolder: dbtypes.OptionalUUID(in.ScopeFolderID), ParentID: dbtypes.OptionalUUID(in.ParentID), Status: in.Status,
		Kind: kind, Search: dbtypes.OptionalText(search), SearchType: in.SearchType,
		Categories: categories, UpdatedAfter: dbtypes.OptionalTime(in.UpdatedAfter),
		UpdatedBefore: dbtypes.OptionalTime(in.UpdatedBefore), AfterID: dbtypes.OptionalUUID(in.AfterID),
		SortBy: in.Sort, AfterName: dbtypes.OptionalText(afterName), SortOrder: in.Order,
		AfterUpdatedAt: dbtypes.OptionalTime(afterUpdatedAt), AfterSize: dbtypes.OptionalInt8(afterSize),
		PageSize: in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("advanced file list: %w", err)
	}
	return files, nil
}

// ParentPaths resolves the display path of every listed file with one batched
// query, keyed by file ID. An empty ID list returns an empty map, and a row whose
// stored ID cannot be decoded is skipped rather than failing the whole page.
func (s *Service) ParentPaths(ctx context.Context, userID int64, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	paths := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return paths, nil
	}
	fileIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		fileIDs[i] = dbtypes.UUID(id)
	}
	rows, err := s.queries.ListFileParentPaths(ctx, sqlcgen.ListFileParentPathsParams{UserID: userID, FileIds: fileIDs})
	if err != nil {
		return nil, fmt.Errorf("list file parent paths: %w", err)
	}
	for _, row := range rows {
		id, ok := dbtypes.GoogleUUID(row.FileID)
		if !ok {
			continue
		}
		paths[id] = row.ParentPath
	}
	return paths, nil
}

// FileCursorValue renders the keyset cursor value of file for sortBy, using the
// same encoding listAdvanced parses back: RFC3339Nano in UTC for "updatedAt", a
// decimal byte count for "size", the UUID string for "id" and the plain name for
// any other value. It returns "" for a nil file or one whose ID is NULL and
// cannot be formatted, and "-1" for a size-sorted file without a recorded size,
// mirroring the COALESCE(size, -1) the SQL ordering uses.
func FileCursorValue(file *sqlcgen.File, sortBy string) string {
	if file == nil {
		return ""
	}
	switch sortBy {
	case "updatedAt":
		return file.UpdatedAt.Time.UTC().Format(time.RFC3339Nano)
	case "size":
		if file.Size.Valid {
			return strconv.FormatInt(file.Size.Int64, 10)
		}
		return "-1"
	case "id":
		if id, ok := dbtypes.GoogleUUID(file.ID); ok {
			return id.String()
		}
		return ""
	default:
		return file.Name
	}
}

// CategoryStatistic aggregates the active files of one drive by the category
// bucket derived from their MIME type and name.
type CategoryStatistic struct {
	// Category is the bucket name; it is one of the keys of validCategories.
	Category string
	// TotalFiles is the number of active files in the bucket.
	TotalFiles int64
	// TotalSize is the summed logical size of those files in bytes.
	TotalSize int64
}

// CategoryStatistics returns one CategoryStatistic per bucket that holds at
// least one of the caller's active files, ordered by category name. Folders and
// trashed entries are excluded; the result is an empty slice, never nil.
func (s *Service) CategoryStatistics(ctx context.Context, userID int64) ([]CategoryStatistic, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	rows, err := s.queries.ListFileCategoryStatistics(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("file category statistics: %w", err)
	}
	items := make([]CategoryStatistic, 0, len(rows))
	for _, row := range rows {
		items = append(items, CategoryStatistic{
			Category: row.Category, TotalFiles: row.TotalFiles, TotalSize: row.TotalSize,
		})
	}
	return items, nil
}

// DriveStatistic summarises one user's drive: live entries and their bytes,
// trashed entries, and the shares and upload sessions currently in flight.
type DriveStatistic struct {
	// TotalFiles is the number of active files.
	TotalFiles int64
	// TotalFolders is the number of active folders.
	TotalFolders int64
	// TotalBytes is the summed size of active files in bytes.
	TotalBytes int64
	// TrashedFiles counts trashed file entries; trashed folders are not counted,
	// matching StorageSummary.TrashedFiles.
	TrashedFiles int64
	// ActiveShares counts shares that are neither revoked nor expired.
	ActiveShares int64
	// OpenUploads counts upload sessions still in the open or completing state.
	OpenUploads int64
}

// DriveStatistics returns the DriveStatistic for userID in a single query. It
// returns ErrInvalidOwner for a non-positive user ID and wraps database failures.
func (s *Service) DriveStatistics(ctx context.Context, userID int64) (DriveStatistic, error) {
	if userID <= 0 {
		return DriveStatistic{}, ErrInvalidOwner
	}
	row, err := s.queries.GetDriveStatistics(ctx, userID)
	if err != nil {
		return DriveStatistic{}, fmt.Errorf("drive statistics: %w", err)
	}
	return DriveStatistic{
		TotalFiles: row.TotalFiles, TotalFolders: row.TotalFolders, TotalBytes: row.TotalBytes,
		TrashedFiles: row.TrashedFiles, ActiveShares: row.ActiveShares, OpenUploads: row.OpenUploads,
	}, nil
}
