package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/cache"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

var (
	// ErrNotFound reports that the requested entry does not exist for this user,
	// or is not in a state the operation accepts (for example already active when
	// the caller asked to restore it). The bulk helpers also use it for an empty,
	// oversized or nil-containing ID batch. Callers must test it with errors.Is.
	ErrNotFound = errors.New("file not found")
	// ErrConflict reports a name already taken at the destination, a restore
	// whose parent entry is still trashed, or a rename that ran out of free
	// names.
	ErrConflict = errors.New("file name conflict")
	// ErrUnsupportedConflictPolicy reports a move conflict policy that is not one
	// of "fail", "replace" or "rename". It is a request-validation failure and is
	// therefore kept apart from ErrConflict, which stays reserved for a real name
	// clash.
	ErrUnsupportedConflictPolicy = errors.New("move conflict policy is not supported")
	// ErrInvalidName reports an update that carries a nil file ID, would change
	// neither the name nor the modification time of an entry, or carries a name
	// that is empty or only whitespace.
	ErrInvalidName = errors.New("invalid file name")
	// ErrInvalidOwner reports a missing or non-positive user ID; nothing was read
	// or written when it is returned.
	ErrInvalidOwner = errors.New("invalid owner")
	// ErrInvalidParent reports a parent or path that cannot be used: a folder that
	// is missing, foreign or not a folder, or a path with a backslash, an empty
	// component, "." or "..".
	ErrInvalidParent = errors.New("invalid parent folder")
	// ErrInvalidFilter reports list filter arguments that cannot be used: an
	// unknown search type, sort key, order or category, an inverted updated
	// window, a cursor value that does not parse for the active sort key, an
	// uncompilable regex, or a parent and path supplied together. It is a
	// validation failure, so the HTTP layer maps it to 422, while ErrInvalidParent
	// stays reserved for a parent or path that cannot be resolved.
	ErrInvalidFilter = errors.New("invalid list filter")
	// ErrNotAFile reports that the entry is not an active file, because it is a
	// folder or has been trashed.
	ErrNotAFile = errors.New("catalog entry is not an active file")
	// ErrCycle reports a move that would place a folder inside itself or inside
	// one of its own descendants.
	ErrCycle = errors.New("folder move would create a cycle")
	// ErrPrecondition reports that the generation supplied by the caller no
	// longer matches the stored row, which the HTTP layer maps to 412.
	ErrPrecondition = errors.New("generation precondition failed")
)

// Service is the catalog domain service: one instance is created at application
// start and shared by every request. It is safe for concurrent use; each method
// takes the caller's user ID and scopes its SQL to that owner, so no per-request
// state lives in the struct.
type Service struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
	// now supplies the current time and is a field so tests can pin it; only
	// CreateFolder consults it, to fill in a zero ModTime.
	now func() time.Time
	// cache holds read-through copies of files and parts. A nil cache disables
	// caching entirely and sends every read to PostgreSQL.
	cache cache.Cacher
	// cacheStripes are the striped locks that serialise cache access per file,
	// indexed by the last byte of the file UUID. Readers hold a stripe for
	// reading while they refill it, writers take it exclusively to invalidate,
	// so a fill can never republish an entry that a concurrent write removed.
	cacheStripes [64]sync.RWMutex
}

// NewService returns a catalog Service that runs its queries on pool and caches
// files and parts in c. Passing a nil cache is supported and disables caching;
// the cache is used as given, without copying or ownership transfer.
func NewService(pool *pgxpool.Pool, c cache.Cacher) *Service {
	return &Service{pool: pool, queries: sqlcgen.New(pool), now: time.Now, cache: c}
}

// cacheKey namespaces one cache entry for this service. The parts are rendered
// by cache.Key, which sorts map entries and dereferences pointers, so the same
// logical arguments always produce the same key.
func (s *Service) cacheKey(parts ...any) string {
	return cache.Key(parts...)
}

// cacheStripe picks the lock that guards cache entries for fileID. All callers
// that read or invalidate a given file must go through this method so they agree
// on the stripe.
func (s *Service) cacheStripe(fileID uuid.UUID) *sync.RWMutex {
	return &s.cacheStripes[int(fileID[15])%len(s.cacheStripes)]
}

// CreateFolderInput describes a folder to create.
type CreateFolderInput struct {
	// UserID is the owner of the new folder and must be positive.
	UserID int64
	// ParentID is the containing folder, or nil for the user's drive root.
	ParentID *uuid.UUID
	// Name is the folder name, passed to the database as given.
	Name string
	// ModTime is the modification time to record; the zero time means now.
	ModTime time.Time
}

// CreateFolder creates an active folder for in.UserID. A non-nil in.ParentID must
// name an active folder of the same user, otherwise ErrInvalidParent is returned;
// a zero in.ModTime is replaced by the current UTC time. A blank name is rejected
// as ErrInvalidName, which the database would otherwise reject as a constraint
// violation, and a name already present in the parent folder surfaces as
// ErrConflict.
func (s *Service) CreateFolder(ctx context.Context, in CreateFolderInput) (*sqlcgen.File, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidOwner
	}
	if isBlankName(in.Name) {
		return nil, ErrInvalidName
	}
	if in.ParentID != nil {
		if _, err := s.queries.GetActiveFolderForUser(ctx, sqlcgen.GetActiveFolderForUserParams{
			FolderID: dbtypes.UUID(*in.ParentID),
			UserID:   in.UserID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidParent
			}
			return nil, fmt.Errorf("get parent folder: %w", err)
		}
	}
	modTime := in.ModTime
	if modTime.IsZero() {
		modTime = s.now().UTC()
	}
	file, err := s.queries.CreateFolder(ctx, sqlcgen.CreateFolderParams{
		ID:       dbtypes.UUID(uuid.New()),
		UserID:   in.UserID,
		ParentID: dbtypes.OptionalUUID(in.ParentID),
		Name:     in.Name,
		ModTime:  dbtypes.Time(modTime.UTC()),
	})
	if err != nil {
		return nil, classifyWriteError("create folder", err)
	}
	return file, nil
}

// Get loads the entry with fileID owned by userID, whether it is a file or a
// folder and whatever its status. A row that belongs to another user is reported
// as ErrNotFound. With a cache configured the row is read through it and stored
// without a TTL, so freshness relies on the write paths calling InvalidateFiles.
func (s *Service) Get(ctx context.Context, userID int64, fileID uuid.UUID) (*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	if s.cache != nil {
		stripe := s.cacheStripe(fileID)
		stripe.RLock()
		defer stripe.RUnlock()
	}
	if s.cache != nil {
		key := s.cacheKey("catalog", "file", userID, fileID.String())
		return cache.Fetch(ctx, s.cache, key, 0, func() (*sqlcgen.File, error) {
			f, err := s.queries.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{
				FileID: dbtypes.UUID(fileID),
				UserID: userID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			if err != nil {
				return nil, fmt.Errorf("get file: %w", err)
			}
			return f, nil
		})
	}
	file, err := s.queries.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{
		FileID: dbtypes.UUID(fileID),
		UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	return file, nil
}

// GetViewState returns the reader state userID saved for fileID, or ErrNotFound
// when the caller never stored one. It performs no access check of its own: the
// caller must have established that userID may read the file.
func (s *Service) GetViewState(ctx context.Context, userID int64, fileID uuid.UUID) (*sqlcgen.FileViewState, error) {
	state, err := s.queries.GetFileViewState(ctx, sqlcgen.GetFileViewStateParams{
		UserID: userID, FileID: dbtypes.UUID(fileID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file view state: %w", err)
	}
	return state, nil
}

// UpsertViewState stores the reader state of one active file for userID, keyed by
// user and file. kind is the viewer kind, and position, preferences and bookmarks
// are opaque JSON documents supplied by the caller. Because the write selects its
// row from the active files of the owner, a missing, trashed or foreign file is
// reported as ErrNotFound.
func (s *Service) UpsertViewState(ctx context.Context, userID int64, fileID uuid.UUID, kind string, position, preferences, bookmarks []byte) (*sqlcgen.FileViewState, error) {
	state, err := s.queries.UpsertFileViewState(ctx, sqlcgen.UpsertFileViewStateParams{
		UserID: userID, FileID: dbtypes.UUID(fileID), ViewerKind: kind,
		Position: position, Preferences: preferences, Bookmarks: bookmarks,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("upsert file view state: %w", err)
	}
	return state, nil
}

// DeleteViewState removes the reader state userID saved for fileID. Deleting
// state that was never stored succeeds, so callers can treat the operation as
// idempotent; only database failures are reported.
func (s *Service) DeleteViewState(ctx context.Context, userID int64, fileID uuid.UUID) error {
	if _, err := s.queries.DeleteFileViewState(ctx, sqlcgen.DeleteFileViewStateParams{
		UserID: userID, FileID: dbtypes.UUID(fileID),
	}); err != nil {
		return fmt.Errorf("delete file view state: %w", err)
	}
	return nil
}

// Parts returns finalized Telegram parts for an active file owned by userID,
// ordered by ascending part number. Every returned row is a copy the caller owns,
// on a cache hit and a cache miss alike, so callers may mutate the rows in place
// (the download path backfills legacy part sizes that way) without writing
// through to the cache.
func (s *Service) Parts(ctx context.Context, userID int64, fileID uuid.UUID) ([]*sqlcgen.FilePart, error) {
	file, err := s.Get(ctx, userID, fileID)
	if err != nil {
		return nil, err
	}
	if file.Kind != sqlcgen.FileKindFile || file.Status != sqlcgen.FileStatusActive {
		return nil, ErrNotAFile
	}
	if s.cache != nil {
		stripe := s.cacheStripe(fileID)
		stripe.RLock()
		defer stripe.RUnlock()
		key := s.cacheKey("catalog", "parts", fileID.String())
		parts, err := cache.Fetch(ctx, s.cache, key, 0, func() ([]*sqlcgen.FilePart, error) {
			parts, err := s.queries.ListFileParts(ctx, dbtypes.UUID(fileID))
			if err != nil {
				return nil, fmt.Errorf("list file parts: %w", err)
			}
			return parts, nil
		})
		if err != nil {
			return nil, err
		}
		return cloneFileParts(parts), nil
	}
	parts, err := s.queries.ListFileParts(ctx, dbtypes.UUID(fileID))
	if err != nil {
		return nil, fmt.Errorf("list file parts: %w", err)
	}
	return cloneFileParts(parts), nil
}

// cloneFileParts returns a new slice holding a value copy of every part, so the
// caller never shares a *sqlcgen.FilePart with the cache or with another caller.
// A nil slice and nil entries are preserved as they are.
func cloneFileParts(parts []*sqlcgen.FilePart) []*sqlcgen.FilePart {
	if parts == nil {
		return nil
	}
	cloned := make([]*sqlcgen.FilePart, len(parts))
	for index, part := range parts {
		if part == nil {
			continue
		}
		copied := *part
		cloned[index] = &copied
	}
	return cloned
}

// UpdatePartSizes backfills the sizes of one part of an uploaded file: plainSize
// is the plaintext size and storedSize the size as stored on Telegram, both in
// bytes, and partNo is the 1-based part number. A row is matched while either
// plain_size or stored_size is still NULL, and both columns are then written
// together, so a part that already has one of the two sizes set gets it overwritten
// as well. The number of affected rows is not checked, so a missing part is not an
// error. The cached part list of fileID is dropped afterwards. There is no user ID:
// the caller must have verified ownership of the file.
func (s *Service) UpdatePartSizes(ctx context.Context, fileID uuid.UUID, partNo int32, plainSize, storedSize int64) error {
	_, err := s.queries.UpdateFilePartSizes(ctx, sqlcgen.UpdateFilePartSizesParams{
		FileID: dbtypes.UUID(fileID), PartNo: partNo,
		PlainSize: dbtypes.Int8(plainSize), StoredSize: dbtypes.Int8(storedSize),
	})
	if err != nil {
		return fmt.Errorf("update file part sizes: %w", err)
	}
	if s.cache != nil {
		stripe := s.cacheStripe(fileID)
		stripe.Lock()
		_ = s.cache.Delete(ctx, s.cacheKey("catalog", "parts", fileID.String()))
		stripe.Unlock()
	}
	return nil
}

// UpdatePartSizesMany is the batched form of UpdatePartSizes: sizes maps each
// 1-based part number to its {plaintext, stored} byte pair and is sent as one
// JSON record set. An empty map is a no-op that touches neither the database nor
// the cache. A part is matched, and both of its columns are written, while either
// of the two stored sizes is still NULL, so a part that already has one of them set
// is overwritten as well, and the cached part list of fileID is dropped after the
// update.
func (s *Service) UpdatePartSizesMany(ctx context.Context, fileID uuid.UUID, sizes map[int32][2]int64) error {
	if len(sizes) == 0 {
		return nil
	}
	type partSizeRecord struct {
		PartNo     int32 `json:"part_no"`
		PlainSize  int64 `json:"plain_size"`
		StoredSize int64 `json:"stored_size"`
	}
	records := make([]partSizeRecord, 0, len(sizes))
	for partNo, partSizes := range sizes {
		records = append(records, partSizeRecord{PartNo: partNo, PlainSize: partSizes[0], StoredSize: partSizes[1]})
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("encode file part sizes: %w", err)
	}
	_, err = s.queries.UpdateFilePartSizesMany(ctx, sqlcgen.UpdateFilePartSizesManyParams{
		FileID: dbtypes.UUID(fileID), Parts: encoded,
	})
	if err != nil {
		return fmt.Errorf("update file part sizes: %w", err)
	}
	if s.cache != nil {
		stripe := s.cacheStripe(fileID)
		stripe.Lock()
		_ = s.cache.Delete(ctx, s.cacheKey("catalog", "parts", fileID.String()))
		stripe.Unlock()
	}
	return nil
}

// ListInput describes one page of a directory listing or search. ParentID and
// Path are mutually exclusive: a non-empty Path is resolved from the drive root
// and replaces ParentID, and supplying both is rejected. Every zero field is
// replaced by the default documented on it before the query runs.
type ListInput struct {
	// UserID is the owner whose entries are listed and must be positive.
	UserID int64
	// ParentID restricts the listing to one folder; nil means the drive root.
	// With Status set to trashed it means "every trashed entry whose parent is
	// not itself trashed", which is how the trash view lists its roots.
	ParentID *uuid.UUID
	// Path is a slash-separated folder path resolved with ResolveFolderPath;
	// empty means ParentID is used unchanged.
	Path string
	// Status selects active or trashed entries; empty defaults to active.
	Status sqlcgen.FileStatus
	// Kind optionally restricts the result to files or to folders.
	Kind *sqlcgen.FileKind
	// Search filters by name. Text search combines trigram similarity with a
	// case-insensitive substring match; a regex search is case-insensitive.
	Search string
	// SearchType is "text" (the default) or "regex"; any other value is rejected.
	SearchType string
	// Categories keeps only entries whose derived category is listed. Valid
	// values are the keys of validCategories.
	Categories []string
	// UpdatedAfter keeps entries updated at or after this instant.
	UpdatedAfter *time.Time
	// UpdatedBefore keeps entries updated strictly before this instant.
	UpdatedBefore *time.Time
	// Sort is the keyset sort key: "name" (the default), "updatedAt", "size" or
	// "id".
	Sort string
	// Order is "asc" (the default) or "desc".
	Order string
	// AfterName is the cursor name, used as the cursor text when AfterValue is
	// empty. Only Sort "name" interprets it as a name; the updatedAt and size sort
	// keys parse it as their own value, while Sort "id" ignores cursor text
	// altogether and the SQL tie-breaker is always AfterID.
	AfterName string
	// AfterValue is the cursor's sort-specific value; it takes precedence over
	// AfterName and is formatted by FileCursorValue.
	AfterValue string
	// AfterID is the ID of the last entry of the previous page. It is required
	// for a cursor to take effect and is the only cursor input when sorting by id.
	AfterID *uuid.UUID
	// Limit is the page size: values <= 0 become 100 and values above 500 are
	// clamped to 500.
	Limit int32
}

// List returns one page of entries owned by in.UserID. It either runs the simple
// name-ordered scan or, as soon as any advanced option is set (categories, an
// updated window, a non-default search type, sort or order, or a cursor value),
// delegates to listAdvanced, which validates the vocabulary and reports bad
// combinations as ErrInvalidFilter; a parent and a path supplied together are
// rejected the same way. A parent the caller cannot use surfaces as
// ErrInvalidParent. The listing covers the direct children of the resolved parent
// and is never recursive. in is taken by value, so the defaults it applies do not
// leak back to the caller.
func (s *Service) List(ctx context.Context, in ListInput) ([]*sqlcgen.File, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidOwner
	}
	if in.ParentID != nil && strings.TrimSpace(in.Path) != "" {
		return nil, ErrInvalidFilter
	}
	if strings.TrimSpace(in.Path) != "" {
		resolved, err := s.ResolveFolderPath(ctx, in.UserID, nil, in.Path)
		if err != nil {
			return nil, err
		}
		in.ParentID = resolved
	}
	if in.Status == "" {
		in.Status = sqlcgen.FileStatusActive
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 500 {
		in.Limit = 500
	}
	if in.SearchType == "" {
		in.SearchType = "text"
	}
	if in.Sort == "" {
		in.Sort = "name"
	}
	if in.Order == "" {
		in.Order = "asc"
	}
	if len(in.Categories) > 0 || in.UpdatedAfter != nil || in.UpdatedBefore != nil || in.SearchType != "text" || in.Sort != "name" || in.Order != "asc" || in.AfterValue != "" {
		return s.listAdvanced(ctx, in)
	}
	var kind sqlcgen.NullFileKind
	if in.Kind != nil {
		kind = sqlcgen.NullFileKind{FileKind: *in.Kind, Valid: true}
	}
	var search pgtype.Text
	if strings.TrimSpace(in.Search) != "" {
		search = dbtypes.Text(in.Search)
	}
	var afterName pgtype.Text
	var afterID pgtype.UUID
	if in.AfterName != "" && in.AfterID != nil {
		afterName = dbtypes.Text(in.AfterName)
		afterID = dbtypes.UUID(*in.AfterID)
	}
	items, err := s.queries.ListFiles(ctx, sqlcgen.ListFilesParams{
		UserID:    in.UserID,
		ParentID:  dbtypes.OptionalUUID(in.ParentID),
		Status:    in.Status,
		Kind:      kind,
		Search:    search,
		AfterName: afterName,
		AfterID:   afterID,
		PageSize:  in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	return items, nil
}

// Rename changes the name of an active entry owned by userID. The name is written
// as given, including any surrounding whitespace, but a name that is empty or only
// whitespace is rejected with ErrInvalidName before the write, matching Update and
// the files_name_not_blank constraint. When expectedGeneration is non-nil it
// must equal the stored generation, and a mismatch is reported as ErrPrecondition
// instead of ErrNotFound. A name already used in the same folder surfaces as
// ErrConflict, and the cached row is dropped after the write.
func (s *Service) Rename(ctx context.Context, userID int64, fileID uuid.UUID, expectedGeneration *int64, rawName string) (*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	if isBlankName(rawName) {
		return nil, ErrInvalidName
	}
	file, err := s.queries.UpdateFileMetadata(ctx, sqlcgen.UpdateFileMetadataParams{
		Name:               dbtypes.Text(rawName),
		FileID:             dbtypes.UUID(fileID),
		UserID:             userID,
		ExpectedGeneration: dbtypes.OptionalInt8(expectedGeneration),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if expectedGeneration != nil {
			return nil, ErrPrecondition
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, classifyWriteError("rename file", err)
	}
	s.invalidateFile(ctx, userID, fileID)
	return file, nil
}

// Move relocates one entry under parentID, or to the drive root when parentID is
// nil, using the strictest conflict policy: a name already taken at the
// destination fails the move with ErrConflict. It is MoveWithPolicy with policy
// "fail", so it shares the locking, cycle detection and cache invalidation of the
// bulk path.
func (s *Service) Move(ctx context.Context, userID int64, fileID uuid.UUID, parentID *uuid.UUID, expectedGeneration *int64) (*sqlcgen.File, error) {
	return s.MoveWithPolicy(ctx, userID, fileID, parentID, expectedGeneration, "fail")
}

// Trash moves one entry and its entire subtree to the trash in a single
// transaction and returns the root row. It is the single-entry form of BulkTrash:
// an entry that is not an active row of userID yields ErrNotFound, and the root
// of the returned subtree is matched by ID.
func (s *Service) Trash(ctx context.Context, userID int64, fileID uuid.UUID) (*sqlcgen.File, error) {
	items, err := s.BulkTrash(ctx, userID, []uuid.UUID{fileID})
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if id, ok := fileUUID(item); ok && id == fileID {
			return item, nil
		}
	}
	return nil, ErrNotFound
}

// Restore reactivates a trashed entry together with its trashed descendants in
// one statement, and returns the root row. The entry must be restorable on its
// own, meaning a trashed row whose parent is either absent or active: a trashed
// entry whose parent is still trashed returns ErrConflict, telling the caller to
// restore the parent first. An entry that does not exist, or exists but is not
// trashed, returns ErrNotFound. Every restored row is evicted from the cache
// after the statement has been applied.
func (s *Service) Restore(ctx context.Context, userID int64, fileID uuid.UUID) (*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	files, err := s.queries.RestoreFileSubtree(ctx, sqlcgen.RestoreFileSubtreeParams{FileID: dbtypes.UUID(fileID), UserID: userID})
	if err != nil {
		return nil, classifyWriteError("restore file subtree", err)
	}
	if len(files) == 0 {
		file, err := s.queries.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{
			FileID: dbtypes.UUID(fileID), UserID: userID,
		})
		if err == nil && file.Status == sqlcgen.FileStatusTrashed {
			return nil, ErrConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("load restore root: %w", err)
		}
		return nil, ErrNotFound
	}
	var root *sqlcgen.File
	ids := make([]uuid.UUID, 0, len(files))
	for _, file := range files {
		id, ok := fileUUID(file)
		if !ok {
			return nil, ErrNotFound
		}
		ids = append(ids, id)
		if id == fileID {
			root = file
		}
	}
	if root == nil {
		return nil, ErrNotFound
	}
	s.InvalidateFiles(ctx, userID, ids...)
	return root, nil
}

// InvalidateFiles drops the cached row and cached part list of every listed file.
// It is a no-op without a cache or for a non-positive user ID, and skips uuid.Nil.
// It is meant to run after the corresponding write has committed, because the
// stripe lock only guarantees that an in-flight read-through fill cannot
// republish a value deleted here.
func (s *Service) InvalidateFiles(ctx context.Context, userID int64, fileIDs ...uuid.UUID) {
	if s.cache == nil || userID <= 0 {
		return
	}
	for _, fileID := range fileIDs {
		if fileID == uuid.Nil {
			continue
		}
		stripe := s.cacheStripe(fileID)
		stripe.Lock()
		_ = s.cache.Delete(ctx,
			s.cacheKey("catalog", "file", userID, fileID.String()),
			s.cacheKey("catalog", "parts", fileID.String()),
		)
		stripe.Unlock()
	}
}

// invalidateFile expires the cache entry of a single file after a one-row write;
// it is the one-file shorthand for InvalidateFiles.
func (s *Service) invalidateFile(ctx context.Context, userID int64, fileID uuid.UUID) {
	s.InvalidateFiles(ctx, userID, fileID)
}

// isBlankName reports whether a name is empty or made up of whitespace only, the
// case the files_name_not_blank check constraint rejects. Rename and Update share
// it so both report ErrInvalidName instead of a raw constraint violation; it is a
// superset of the constraint, which trims spaces only.
func isBlankName(name string) bool {
	return strings.TrimSpace(name) == ""
}

// classifyWriteError turns a PostgreSQL unique-violation (SQLSTATE 23505) into
// ErrConflict and wraps anything else as "<action>: <cause>". It returns the
// sentinel unwrapped so callers can keep using errors.Is.
func classifyWriteError(action string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return fmt.Errorf("%s: %w", action, err)
}
