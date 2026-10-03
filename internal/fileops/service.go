// Package fileops implements the file operations that span the catalog and the
// Telegram storage boundary: recursive copy, the trash-to-purge transition, and
// permanent deletion of whole subtrees.
//
// Copy pays the irreversible cost first. Every Telegram part is republished into a
// destination channel before a single catalog row is written, and each published
// message is remembered so that a later failure can delete it again; only then are
// the new rows inserted inside one transaction. A failure therefore never leaves
// catalog rows pointing at messages that were not copied, and the worst case is a
// best-effort compensating delete that the orphan cleanup sweep finishes.
//
// Purge runs the other way round and is deliberately restartable. Rows are marked
// deletion_pending before any Telegram message is touched, the messages are then
// removed channel by channel, and the catalog rows and their parts are deleted last
// inside one transaction, children before parents. A crash anywhere in the middle
// leaves files in deletion_pending, which the periodic purge sweep picks up, rather
// than leaking messages that nothing references any more.
//
// Both operations serialize on PostgreSQL advisory locks derived from the user ID
// and the affected file or folder, so the guarantees hold across server instances
// and not merely inside one process.
package fileops

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dblock"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

var (
	// ErrInvalidInput reports arguments the operation cannot act on: a non-positive
	// user ID, a nil file ID, an empty purge list, a conflict policy outside the
	// supported set, or a dependency missing from NewService. Callers test it with
	// errors.Is.
	ErrInvalidInput = errors.New("invalid file operation input")
	// ErrNotFound reports that a requested file, or a row the operation depends on,
	// does not exist for this user. A foreign file is reported exactly like a missing
	// one. It is also returned when an identifier cannot be decoded and when an
	// insert or delete affected a different number of rows than expected, which means
	// the tree changed underneath the operation.
	ErrNotFound = errors.New("file not found")
	// ErrNotTrashed reports that permanent deletion was requested for a root that is
	// neither trashed nor already deletion_pending. Purge and QueuePurge return it so
	// a caller cannot bypass the trash step and destroy a live file.
	ErrNotTrashed = errors.New("file must be trashed before permanent deletion")
)

// Service implements recursive copy and permanent deletion over the catalog and the
// Telegram storage boundary. All its state lives in the injected dependencies, so it
// is safe for concurrent use; the operations that must not interleave are serialized
// with PostgreSQL advisory locks rather than in-process mutexes, which keeps the
// guarantees valid across several server instances. A Service owns none of its
// dependencies and needs no closing.
type Service struct {
	// pool opens the transaction and the dedicated advisory-lock connection that the
	// copy and purge paths need. It is never closed here.
	pool *pgxpool.Pool
	// queries is the non-transactional query set. Work that must be atomic calls
	// queries.WithTx instead of using this value.
	queries *sqlcgen.Queries
	// catalog is the source of truth for file metadata and exposes the cache that
	// both operations invalidate once they have changed rows.
	catalog *catalog.Service
	// channels resolves the destination Telegram channels that receive copied parts.
	channels *channels.Service
	// storage republishes and deletes the Telegram messages behind file parts.
	storage telegramstore.Storage
}

// CopyInput describes one recursive copy request. UserID and FileID are mandatory;
// a nil ParentID means the user's root folder, a nil Name keeps the source name, and
// an empty ConflictPolicy behaves like NameConflictPolicyFail.
type CopyInput struct {
	// UserID is the owner of both the source and the copy and must be positive.
	UserID int64
	// FileID is the source entry. A folder copies its entire subtree.
	FileID uuid.UUID
	// ParentID is the destination folder, or nil for the user's root folder. It must
	// name an active folder of the same user, otherwise catalog.ErrInvalidParent is
	// returned.
	ParentID *uuid.UUID
	// Name overrides the copied root's name; nil keeps the source name. Descendants
	// always keep theirs. A name that is empty or only whitespace is rejected with
	// ErrInvalidInput before any work starts.
	Name *string
	// ConflictPolicy decides what happens when an active entry with the root's name
	// already exists in the destination: fail, rename the copy, or replace the
	// existing entry and its subtree.
	ConflictPolicy sqlcgen.NameConflictPolicy
}

// treeNode is one row of a loaded subtree together with its distance from the root
// that was requested. Depth 0 is that root, and purge uses larger depths to delete
// catalog rows children-first.
type treeNode struct {
	// File is the full catalog row as returned by the recursive query.
	File sqlcgen.File
	// Depth is the distance from the requested root, starting at 0.
	Depth int32
}

// copiedPart pairs a source part row with the Telegram message that now holds its
// copied bytes, so the catalog insert can record the new location.
type copiedPart struct {
	// Part is the source row; its sizes, checksum, salt and block hashes are carried
	// over unchanged because the bytes were duplicated rather than re-encoded.
	Part sqlcgen.FilePart
	// Stored identifies the freshly published Telegram document.
	Stored telegramstore.StoredPart
}

// copiedFileRecord is one new catalog row in the JSON array handed to
// InsertCopiedFiles. The JSON keys are part of the contract with that query, which
// reads the array with jsonb_to_recordset, so the tags must not be renamed. Status
// and generation are deliberately absent: the query always writes 'active' and
// generation 1 for a copy.
type copiedFileRecord struct {
	// ID is the freshly generated identifier of the copy, which also keys the part
	// records and links descendants to their copied parents.
	ID uuid.UUID `json:"id"`
	// UserID is the owner, always the caller, so a copy never crosses accounts.
	UserID int64 `json:"user_id"`
	// ParentID is the new parent folder, or nil for the user's root folder.
	ParentID *uuid.UUID `json:"parent_id"`
	// Name is the entry name, overridden for the root when requested and possibly
	// suffixed by the rename conflict policy.
	Name string `json:"name"`
	// Kind is the file kind copied verbatim, so folder structure is reproduced.
	Kind string `json:"kind"`
	// MIMEType is the source media type, nil for folders and entries without one.
	MIMEType *string `json:"mime_type"`
	// Size is the logical size in bytes, nil when the recorded source size is NULL.
	Size *int64 `json:"size"`
	// HashAlgorithm names the algorithm behind HashValue, nil when the source was
	// stored without hashing.
	HashAlgorithm *string `json:"hash_algorithm"`
	// HashValue is the source content digest, valid for the copy because the bytes
	// are identical.
	HashValue *string `json:"hash_value"`
	// Encryption reports whether the copied parts stay encrypted under the same key
	// version, which is safe because the ciphertext is duplicated as-is.
	Encryption bool `json:"encryption"`
	// EncryptionKeyVersion is the key generation that decrypts the parts, nil for
	// unencrypted entries.
	EncryptionKeyVersion *int32 `json:"encryption_key_version"`
	// ModTime is the source modification time, preserved so the copy sorts like the
	// original.
	ModTime time.Time `json:"mod_time"`
}

// copiedFilePartRecord is one new file_parts row in the JSON array handed to
// InsertCopiedFileParts. As with copiedFileRecord the JSON keys are the contract with
// the query. BlockHashes is a byte slice because encoding/json renders it as standard
// base64, which is exactly what the query decodes.
type copiedFilePartRecord struct {
	// FileID is the newly generated identifier of the copied file that owns the part.
	FileID uuid.UUID `json:"file_id"`
	// PartNo is the one-based part number carried over from the source row, which
	// identifies the byte range the part covers and matches the part_no > 0
	// constraint on the table.
	PartNo int32 `json:"part_no"`
	// ChannelID is the Telegram channel holding the copied message.
	ChannelID int64 `json:"channel_id"`
	// MessageID is the copied message inside ChannelID.
	MessageID int64 `json:"message_id"`
	// PlainSize is the number of plaintext bytes the part contributes, nil when
	// unknown.
	PlainSize *int64 `json:"plain_size"`
	// StoredSize is the number of bytes stored in Telegram, which exceeds PlainSize
	// for encrypted parts.
	StoredSize *int64 `json:"stored_size"`
	// Checksum is the recorded content digest for the part, nil when the source had
	// none.
	Checksum *string `json:"checksum"`
	// Salt is the per-part encryption salt, nil for unencrypted parts.
	Salt *string `json:"salt"`
	// BlockHashes is the opaque per-block hash chain carried over from the source
	// part; the query base64-decodes the JSON string back into bytea.
	BlockHashes []byte `json:"block_hashes"`
}

// NewService returns a file operations service over the given dependencies. All four
// are mandatory and a nil one is rejected with ErrInvalidInput rather than deferred
// to a later panic, because they are wired together at startup and a missing one is a
// programming error. The Service keeps references without taking ownership: the
// caller keeps the pool, services and storage open for the lifetime of the Service.
func NewService(pool *pgxpool.Pool, catalogService *catalog.Service, channelService *channels.Service, storage telegramstore.Storage) (*Service, error) {
	if pool == nil || catalogService == nil || channelService == nil || storage == nil {
		return nil, ErrInvalidInput
	}
	return &Service{pool: pool, queries: sqlcgen.New(pool), catalog: catalogService, channels: channelService, storage: storage}, nil
}

// Copy duplicates the subtree rooted at in.FileID into in.ParentID and returns the
// new root row, whose ID is freshly generated and whose status is active.
//
// The work is split so that a failure never leaves the catalog pointing at data that
// does not exist. A blank explicit name is rejected with ErrInvalidInput before any
// work starts. The source tree is then loaded and validated: the root must exist and
// be active, and every node of the subtree must be active, so a trashed folder is
// rejected with catalog.ErrNotAFile just like a trashed file instead of being
// resurrected as an active copy; each node also gets a new UUID up front so the copies
// form a self-contained tree. Destination channel capacity is
// reserved for all parts at once, and each part is then republished with
// storage.CopyPart, checking that the copied size matches the recorded stored size
// (telegramstore.ErrSizeMismatch otherwise). Every message the storage names is
// tracked as soon as it is reported, including one returned alongside a failing
// CopyPart call, because the storage contract reports a size mismatch only after the
// document was published; tracking is what makes compensation possible, so any later
// error deletes those messages again on a background context, cancelling the request
// does not cancel the cleanup, and the deletes are best effort with errors discarded.
// The one case the scheme cannot cover is a failed copy whose storage response names
// no message at all, which the Storage contract permits; that orphan is left to the
// periodic cleanup sweep.
//
// Only then does one transaction run: it takes the destination advisory lock, looks
// for an active entry with the root's name, applies in.ConflictPolicy, inserts the
// file rows and then the part rows, and checks the affected row counts. The fail
// policy returns catalog.ErrConflict, rename picks the next free " (n)" suffix, and
// replace marks the existing subtree deletion_pending and revokes its shares before
// the copy is inserted. The transaction commits as a unit, and after the commit the
// catalog cache is invalidated for the replaced entries.
//
// Errors surface unwrapped where the caller needs to classify them — catalog.ErrConflict,
// catalog.ErrInvalidParent, catalog.ErrNotAFile, ErrNotFound, ErrInvalidInput,
// telegramstore.ErrSizeMismatch and the channels errors — and are wrapped with context
// otherwise. Every error return except the final lookup of the inserted root happens
// before the commit, so a failed call leaves no new catalog row behind. A commit that
// fails is the one case where the outcome is unknown: the published messages are then
// kept rather than deleted, because the rows may have been written.
func (s *Service) Copy(ctx context.Context, in CopyInput) (*sqlcgen.File, error) {
	if in.UserID <= 0 || in.FileID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	// A blank explicit name would fail the files_name_not_blank check constraint
	// after the whole subtree was republished, so it is rejected before any work.
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return nil, ErrInvalidInput
	}
	if in.ConflictPolicy == "" {
		in.ConflictPolicy = sqlcgen.NameConflictPolicyFail
	}
	switch in.ConflictPolicy {
	case sqlcgen.NameConflictPolicyFail, sqlcgen.NameConflictPolicyRename, sqlcgen.NameConflictPolicyReplace:
	default:
		return nil, ErrInvalidInput
	}
	if in.ParentID != nil {
		parent, err := s.catalog.Get(ctx, in.UserID, *in.ParentID)
		if err != nil {
			return nil, err
		}
		if parent.Kind != sqlcgen.FileKindFolder || parent.Status != sqlcgen.FileStatusActive {
			return nil, catalog.ErrInvalidParent
		}
	}
	nodes, err := s.loadTree(ctx, in.UserID, in.FileID)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 || nodes[0].File.Status != sqlcgen.FileStatusActive {
		return nil, ErrNotFound
	}

	idMap := make(map[uuid.UUID]uuid.UUID, len(nodes))
	for _, node := range nodes {
		oldID, ok := dbtypes.GoogleUUID(node.File.ID)
		if !ok {
			return nil, ErrNotFound
		}
		idMap[oldID] = uuid.New()
	}
	rootOldID, _ := dbtypes.GoogleUUID(nodes[0].File.ID)
	rootNewID := idMap[rootOldID]
	rootName := nodes[0].File.Name
	if in.Name != nil {
		rootName = *in.Name
	}

	sourceFileIDs := make([]uuid.UUID, 0)
	for _, node := range nodes {
		// Every loaded node must be active, folders included: the insert below
		// hardcodes the active status, so copying a trashed descendant would revive
		// it as an active entry.
		if node.File.Status != sqlcgen.FileStatusActive {
			return nil, catalog.ErrNotAFile
		}
		if node.File.Kind != sqlcgen.FileKindFile {
			continue
		}
		oldID, _ := dbtypes.GoogleUUID(node.File.ID)
		sourceFileIDs = append(sourceFileIDs, oldID)
	}
	parts, err := s.queries.ListFilePartsByFileIDs(ctx, pgUUIDs(sourceFileIDs))
	if err != nil {
		return nil, fmt.Errorf("list copied file parts: %w", err)
	}
	destinationChannels, err := s.channels.ResolveMany(ctx, in.UserID, len(parts))
	if err != nil {
		return nil, err
	}
	copied, published, err := s.copyParts(ctx, in.UserID, parts, destinationChannels)
	compensate := func() { s.deleteCopiedParts(in.UserID, published) }
	if err != nil {
		compensate()
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		compensate()
		return nil, err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.AcquireAdvisoryTransactionLock(ctx, dblock.Destination(in.UserID, in.ParentID)); err != nil {
		compensate()
		return nil, fmt.Errorf("lock copy destination: %w", err)
	}
	if in.ParentID != nil {
		// The parent was read before the Telegram copy started, so it may have
		// been trashed, purged or replaced since. Reading it again under the
		// destination lock keeps the copy from landing in a folder that is no
		// longer active, which would leave rows that no listing shows and that
		// the purge of that folder cannot delete.
		if _, err := queries.LockActiveFolder(ctx, sqlcgen.LockActiveFolderParams{
			UserID: in.UserID, FolderID: dbtypes.UUID(*in.ParentID),
		}); err != nil {
			compensate()
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, catalog.ErrInvalidParent
			}
			return nil, fmt.Errorf("lock copy destination folder: %w", err)
		}
	}
	conflict, conflictErr := queries.LockUploadDestinationConflict(ctx, sqlcgen.LockUploadDestinationConflictParams{
		UserID: in.UserID, ParentID: dbtypes.OptionalUUID(in.ParentID), Name: rootName,
	})
	if conflictErr != nil && !errors.Is(conflictErr, pgx.ErrNoRows) {
		compensate()
		return nil, fmt.Errorf("check copy destination conflict: %w", conflictErr)
	}
	var replacedIDs []uuid.UUID
	if conflictErr == nil {
		conflictID, ok := dbtypes.GoogleUUID(conflict.ID)
		if !ok {
			compensate()
			return nil, catalog.ErrConflict
		}
		switch in.ConflictPolicy {
		case sqlcgen.NameConflictPolicyFail:
			compensate()
			return nil, catalog.ErrConflict
		case sqlcgen.NameConflictPolicyRename:
			rootName, err = nextAvailableCopyName(ctx, queries, in.UserID, in.ParentID, rootName)
			if err != nil {
				compensate()
				return nil, err
			}
		case sqlcgen.NameConflictPolicyReplace:
			subtreeIDs, err := queries.ListFileSubtreeIDs(ctx, sqlcgen.ListFileSubtreeIDsParams{
				FileID: dbtypes.UUID(conflictID), UserID: in.UserID,
			})
			if err != nil {
				compensate()
				return nil, fmt.Errorf("list replaced copy destination subtree: %w", err)
			}
			for _, subtreeID := range subtreeIDs {
				if value, ok := dbtypes.GoogleUUID(subtreeID); ok {
					replacedIDs = append(replacedIDs, value)
				}
			}
			if err := queries.MarkFileSubtreeDeletionPending(ctx, sqlcgen.MarkFileSubtreeDeletionPendingParams{
				UserID: in.UserID, FileID: dbtypes.UUID(conflictID),
			}); err != nil {
				compensate()
				return nil, fmt.Errorf("mark replaced copy destination for deletion: %w", err)
			}
			if err := queries.RevokeSharesForFileSubtree(ctx, sqlcgen.RevokeSharesForFileSubtreeParams{
				UserID: in.UserID, FileID: dbtypes.UUID(conflictID),
			}); err != nil {
				compensate()
				return nil, fmt.Errorf("revoke replaced copy destination shares: %w", err)
			}
		}
	}
	fileRecords := make([]copiedFileRecord, 0, len(nodes))
	partRecords := make([]copiedFilePartRecord, 0, len(parts))
	for _, node := range nodes {
		oldID, _ := dbtypes.GoogleUUID(node.File.ID)
		newID := idMap[oldID]
		var parentID *uuid.UUID
		if oldID == rootOldID {
			parentID = in.ParentID
		} else if oldParent, ok := dbtypes.GoogleUUID(node.File.ParentID); ok {
			mapped := idMap[oldParent]
			parentID = &mapped
		}
		name := node.File.Name
		if oldID == rootOldID {
			name = rootName
		}
		fileRecords = append(fileRecords, copiedFileRecord{
			ID: newID, UserID: in.UserID, ParentID: parentID, Name: name,
			Kind:     string(node.File.Kind),
			MIMEType: optionalText(node.File.MimeType), Size: optionalInt64(node.File.Size),
			HashAlgorithm: optionalText(node.File.HashAlgorithm), HashValue: optionalText(node.File.HashValue),
			Encryption: node.File.Encryption, EncryptionKeyVersion: optionalInt32(node.File.EncryptionKeyVersion),
			ModTime: node.File.ModTime.Time,
		})
		for _, part := range copied[oldID] {
			partRecords = append(partRecords, copiedFilePartRecord{
				FileID: newID, PartNo: part.Part.PartNo, ChannelID: part.Stored.ChannelID,
				MessageID: part.Stored.MessageID, PlainSize: optionalInt64(part.Part.PlainSize),
				StoredSize: optionalInt64(part.Part.StoredSize), Checksum: optionalText(part.Part.Checksum),
				Salt: optionalText(part.Part.Salt), BlockHashes: part.Part.BlockHashes,
			})
		}
	}
	encodedFiles, err := json.Marshal(fileRecords)
	if err != nil {
		compensate()
		return nil, fmt.Errorf("encode copied files: %w", err)
	}
	insertedFiles, err := queries.InsertCopiedFiles(ctx, encodedFiles)
	if err != nil {
		compensate()
		return nil, classifyWriteError("insert copied catalog rows", err)
	}
	if len(insertedFiles) != len(fileRecords) {
		compensate()
		return nil, ErrNotFound
	}
	if len(partRecords) > 0 {
		encodedParts, err := json.Marshal(partRecords)
		if err != nil {
			compensate()
			return nil, fmt.Errorf("encode copied file parts: %w", err)
		}
		insertedParts, err := queries.InsertCopiedFileParts(ctx, encodedParts)
		if err != nil {
			compensate()
			return nil, fmt.Errorf("insert copied file parts: %w", err)
		}
		if insertedParts != int64(len(partRecords)) {
			compensate()
			return nil, ErrNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		// A failed commit does not mean the transaction was rolled back: the
		// server may have committed before the connection broke, so deleting the
		// messages published for this copy could leave brand-new catalog rows
		// pointing at messages that no longer exist, which nothing can repair.
		// Keeping them is the safer failure: at worst the rows were not written
		// and the messages are orphans the cleanup sweep removes.
		return nil, fmt.Errorf("commit file copy: %w", err)
	}
	s.catalog.InvalidateFiles(ctx, in.UserID, replacedIDs...)
	for _, file := range insertedFiles {
		if id, ok := dbtypes.GoogleUUID(file.ID); ok && id == rootNewID {
			return file, nil
		}
	}
	return nil, ErrNotFound
}

// copyParts republishes every source part into the destination channel reserved for
// it and returns the new parts grouped by their source file together with every
// Telegram message that a later failure must delete again. The grouped map is keyed
// by the Google-format source file ID, which is how the caller links a part to the
// copied catalog row that will own it.
//
// The returned published list is what makes compensation complete: a message is
// recorded the moment the storage names it, before any error can escape, because the
// storage contract reports a size mismatch only after the document was published and
// an implementation that still identifies that document returns it alongside the
// error. Dropping such a part is what would turn the compensating delete into a
// no-op. A response that names no message carries nothing to delete and is skipped;
// its orphan is left to the periodic cleanup sweep, as the Copy documentation
// describes.
//
// Errors surface unwrapped where the caller classifies them: ErrNotFound for a source
// file ID that cannot be decoded and telegramstore.ErrSizeMismatch for a copied size
// that contradicts the recorded stored size or a stored size the catalog never
// recorded. A storage failure is wrapped with the part number so the log says which
// part failed.
func (s *Service) copyParts(ctx context.Context, userID int64, parts []*sqlcgen.FilePart, destinationChannels []int64) (map[uuid.UUID][]copiedPart, []telegramstore.StoredPart, error) {
	copied := make(map[uuid.UUID][]copiedPart, len(parts))
	published := make([]telegramstore.StoredPart, 0, len(parts))
	for index, part := range parts {
		oldID, ok := dbtypes.GoogleUUID(part.FileID)
		if !ok {
			return nil, published, ErrNotFound
		}
		stored, err := s.storage.CopyPart(ctx, userID, part.ChannelID, part.MessageID, destinationChannels[index])
		published = trackPublishedPart(published, stored)
		if err != nil {
			return nil, published, fmt.Errorf("copy Telegram part %d: %w", part.PartNo, err)
		}
		if !part.StoredSize.Valid || stored.Size != part.StoredSize.Int64 {
			return nil, published, telegramstore.ErrSizeMismatch
		}
		copied[oldID] = append(copied[oldID], copiedPart{Part: *part, Stored: stored})
	}
	return copied, published, nil
}

// trackPublishedPart appends a Telegram message the storage reported to the list that
// a later failure deletes again, and returns the list unchanged when the response
// names no message. It is applied to every CopyPart result, a failing one included,
// because the storage contract reports a size mismatch only after the document was
// published and a part without a channel or message ID has no message to delete.
func trackPublishedPart(published []telegramstore.StoredPart, part telegramstore.StoredPart) []telegramstore.StoredPart {
	if part.ChannelID == 0 || part.MessageID <= 0 {
		return published
	}
	return append(published, part)
}

// deleteCopiedParts removes the Telegram messages one Copy call published, grouped by
// channel so each channel costs a single DeleteMessages call. It runs on a background
// context, so cancelling the request does not cancel the cleanup, and the deletes are
// deliberately best effort: an error is discarded because the caller has to see the
// original failure, and a message the delete misses is finished by the periodic
// cleanup sweep. Entries that name no message are skipped, since there is nothing to
// delete for them.
func (s *Service) deleteCopiedParts(userID int64, published []telegramstore.StoredPart) {
	grouped := make(map[int64][]int64)
	for _, part := range published {
		if part.ChannelID == 0 || part.MessageID <= 0 {
			continue
		}
		grouped[part.ChannelID] = append(grouped[part.ChannelID], part.MessageID)
	}
	for channelID, messageIDs := range grouped {
		_ = s.storage.DeleteMessages(context.Background(), userID, channelID, messageIDs)
	}
}

// optionalText converts a possibly NULL text column into a pointer, reporting nil for
// SQL NULL. It bridges the pgtype representation used by the generated queries and
// the pointer fields of the JSON records sent back to the database.
func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// optionalInt64 converts a possibly NULL bigint column into a pointer, reporting nil
// for SQL NULL so the JSON record preserves the difference between "unknown" and zero.
func optionalInt64(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

// optionalInt32 converts a possibly NULL integer column into a pointer, reporting nil
// for SQL NULL so the encryption key version of an unencrypted copy stays absent
// rather than becoming a misleading zero.
func optionalInt32(value pgtype.Int4) *int32 {
	if !value.Valid {
		return nil
	}
	return &value.Int32
}

// nextAvailableCopyName returns a variant of original that no active entry of the
// destination folder already uses, built as "stem (n)extension" with n starting at 1.
// Only active names are consulted, so a trashed entry never forces a suffix. It gives
// up after 10000 attempts and returns catalog.ErrConflict, which bounds the work
// instead of looping forever in a folder where every candidate is taken.
func nextAvailableCopyName(ctx context.Context, queries *sqlcgen.Queries, userID int64, parentID *uuid.UUID, original string) (string, error) {
	names, err := queries.ListActiveNames(ctx, sqlcgen.ListActiveNamesParams{
		UserID: userID, ParentID: dbtypes.OptionalUUID(parentID),
	})
	if err != nil {
		return "", fmt.Errorf("list copy destination names: %w", err)
	}
	used := make(map[string]struct{}, len(names))
	for _, name := range names {
		used[name] = struct{}{}
	}
	base, extension := splitCopyName(original)
	for sequence := 1; sequence <= 10000; sequence++ {
		candidate := base + fmt.Sprintf(" (%d)", sequence) + extension
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
	return "", catalog.ErrConflict
}

// splitCopyName splits a name into the stem and the extension that nextAvailableCopyName
// keeps in place when it appends a sequence number. The extension is the suffix from
// the last dot and is empty when that dot is absent, leading (".bashrc", which is a
// whole name) or trailing ("name."), so such names are numbered as a unit.
func splitCopyName(name string) (string, string) {
	index := strings.LastIndex(name, ".")
	if index <= 0 || index == len(name)-1 {
		return name, ""
	}
	return name[:index], name[index:]
}

// classifyWriteError turns a PostgreSQL unique-violation (SQLSTATE 23505) into
// catalog.ErrConflict, so a name that a concurrent upload or copy claimed first is
// reported as a conflict rather than an internal failure. Everything else is wrapped
// with the action that failed.
func classifyWriteError(action string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return catalog.ErrConflict
	}
	return fmt.Errorf("%s: %w", action, err)
}

// releaseAdvisoryLocks drops the given session-level advisory locks on the
// connection behind queries, using a context that survives request cancellation.
// Only the named locks are touched, never pg_advisory_unlock_all, because the
// pooled connection may carry locks taken by other packages. Unlocking a lock
// the session does not hold reports false and changes nothing.
func releaseAdvisoryLocks(queries *sqlcgen.Queries, lockIDs []int64) {
	if len(lockIDs) == 0 {
		return
	}
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, lockID := range lockIDs {
		_, _ = queries.ReleaseAdvisoryLock(unlockCtx, lockID)
	}
}

// CleanTrash moves every trashed entry of userID to deletion_pending in one statement
// and reports how many rows changed. This is the explicit empty-trash action; it never
// touches Telegram, so it is cheap and interruptible, and the purge sweep performs the
// actual deletion afterwards. The returned count is 0 for a user whose trash is already
// empty, and the status change cannot be undone through the API.
func (s *Service) CleanTrash(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, ErrInvalidInput
	}
	count, err := s.queries.MarkAllTrashedDeletionPending(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("mark trash deletion pending: %w", err)
	}
	fileIDs := make([]uuid.UUID, 0, len(count))
	for _, id := range count {
		if value, ok := dbtypes.GoogleUUID(id); ok {
			fileIDs = append(fileIDs, value)
		}
	}
	// The rows changed status, so every cached copy of them would otherwise keep
	// reporting "trashed" until the process restarted: this cache has no TTL.
	s.catalog.InvalidateFiles(ctx, userID, fileIDs...)
	return int64(len(count)), nil
}

// QueuePurge marks the subtree rooted at fileID as deletion_pending so the purge sweep
// finishes the deletion, and returns nil once the rows are marked. Only a trashed root
// is accepted; when the marking statement matches nothing it distinguishes a file that
// does not exist (ErrNotFound) from one that is not trashed (ErrNotTrashed), so the
// caller can answer 404 or 409 accurately. No Telegram message is deleted here, and the
// cache entries for the marked subtree are evicted so later reads see the new status.
func (s *Service) QueuePurge(ctx context.Context, userID int64, fileID uuid.UUID) error {
	if userID <= 0 || fileID == uuid.Nil {
		return ErrInvalidInput
	}
	files, err := s.queries.QueueFileSubtreePurge(ctx, sqlcgen.QueueFileSubtreePurgeParams{
		UserID: userID, FileID: dbtypes.UUID(fileID),
	})
	if err != nil {
		return fmt.Errorf("queue file subtree purge: %w", err)
	}
	if len(files) == 0 {
		if _, err := s.queries.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{
			FileID: dbtypes.UUID(fileID), UserID: userID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load purge root: %w", err)
		}
		return ErrNotTrashed
	}
	ids := make([]uuid.UUID, 0, len(files))
	for _, file := range files {
		id, ok := dbtypes.GoogleUUID(file.ID)
		if !ok {
			return ErrNotFound
		}
		ids = append(ids, id)
	}
	s.catalog.InvalidateFiles(ctx, userID, ids...)
	return nil
}

// Purge permanently deletes the subtree rooted at fileID by delegating to PurgeMany,
// so it shares that method's locking, restartability and error semantics. A zero file
// ID is rejected by that method with ErrInvalidInput.
func (s *Service) Purge(ctx context.Context, userID int64, fileID uuid.UUID) error {
	return s.PurgeMany(ctx, userID, []uuid.UUID{fileID})
}

// PurgeMany permanently deletes several subtrees in one call. Zero roots are rejected
// with ErrInvalidInput and duplicates are collapsed, then each root is claimed with a
// non-blocking PostgreSQL advisory lock held on a dedicated connection for the whole
// call: a root that another purge already holds is skipped and reported as success, so
// concurrent sweeps split the work instead of colliding and a nil error does not
// guarantee that every root was removed. Only the locks this call took are released
// again, because the pooled connection may carry session-level locks another package
// holds. A root that no longer exists yields
// ErrNotFound and one that is neither trashed nor deletion_pending yields ErrNotTrashed;
// both abort before anything is marked or deleted.
//
// The deletion is ordered so that it can be resumed. All rows of the locked subtrees are
// marked deletion_pending first, then the Telegram messages are removed channel by
// channel, and finally the parts and catalog rows are deleted in one transaction,
// deepest rows first so no child outlives its parent. A failure after the first step
// leaves the files in deletion_pending for the periodic sweep to retry, a failure before
// it leaves the database untouched, and a failure in the middle leaves some channels
// already cleared, which the retry accepts because a channel that no longer resolves
// counts as already deleted. The catalog cache is invalidated after the marking and
// again after the commit.
func (s *Service) PurgeMany(ctx context.Context, userID int64, rootIDs []uuid.UUID) error {
	if userID <= 0 || len(rootIDs) == 0 {
		return ErrInvalidInput
	}
	uniqueRoots := make([]uuid.UUID, 0, len(rootIDs))
	seenRoots := make(map[uuid.UUID]struct{}, len(rootIDs))
	for _, rootID := range rootIDs {
		if rootID == uuid.Nil {
			return ErrInvalidInput
		}
		if _, ok := seenRoots[rootID]; ok {
			continue
		}
		seenRoots[rootID] = struct{}{}
		uniqueRoots = append(uniqueRoots, rootID)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire purge lock connection: %w", err)
	}
	defer conn.Release()
	lockQueries := sqlcgen.New(conn)
	lockedRoots := make([]uuid.UUID, 0, len(uniqueRoots))
	heldLockIDs := make([]int64, 0, len(uniqueRoots))
	defer func() {
		releaseAdvisoryLocks(lockQueries, heldLockIDs)
	}()
	rootByLockID := make(map[int64]uuid.UUID, len(uniqueRoots))
	lockIDs := make([]int64, 0, len(uniqueRoots))
	for _, rootID := range uniqueRoots {
		lockID := purgeAdvisoryLockID(userID, rootID)
		rootByLockID[lockID] = rootID
		lockIDs = append(lockIDs, lockID)
	}
	locks, err := lockQueries.TryAdvisoryLocks(ctx, lockIDs)
	if err != nil {
		// The statement can take some of these locks and then abort before their
		// rows reach this side, so the whole requested set is released: unlocking
		// a lock this session does not hold reports false and changes nothing.
		releaseAdvisoryLocks(lockQueries, lockIDs)
		return fmt.Errorf("acquire purge advisory locks: %w", err)
	}
	for _, lock := range locks {
		if lock.Locked {
			heldLockIDs = append(heldLockIDs, lock.LockID)
			lockedRoots = append(lockedRoots, rootByLockID[lock.LockID])
		}
	}
	if len(lockedRoots) == 0 {
		return nil
	}

	nodesByID := make(map[uuid.UUID]treeNode)
	rows, err := s.queries.LoadFileSubtrees(ctx, sqlcgen.LoadFileSubtreesParams{
		RootIds: pgUUIDs(lockedRoots), UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("load file subtrees: %w", err)
	}
	foundRoots := make(map[uuid.UUID]sqlcgen.FileStatus, len(lockedRoots))
	rootSet := make(map[uuid.UUID]struct{}, len(lockedRoots))
	for _, rootID := range lockedRoots {
		rootSet[rootID] = struct{}{}
	}
	for _, row := range rows {
		id, ok := dbtypes.GoogleUUID(row.ID)
		if !ok {
			return ErrNotFound
		}
		node := treeNode{File: sqlcgen.File{
			ID: row.ID, UserID: row.UserID, ParentID: row.ParentID, Name: row.Name,
			Kind: row.Kind, MimeType: row.MimeType,
			Size: row.Size, HashAlgorithm: row.HashAlgorithm, HashValue: row.HashValue,
			Encryption: row.Encryption, EncryptionKeyVersion: row.EncryptionKeyVersion,
			Status: row.Status, ModTime: row.ModTime, Generation: row.Generation,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, DeletedAt: row.DeletedAt,
		}, Depth: row.Depth}
		if _, isRoot := rootSet[id]; isRoot {
			foundRoots[id] = row.Status
		}
		if existing, exists := nodesByID[id]; !exists || node.Depth > existing.Depth {
			nodesByID[id] = node
		}
	}
	for _, rootID := range lockedRoots {
		status, ok := foundRoots[rootID]
		if !ok {
			return ErrNotFound
		}
		if status != sqlcgen.FileStatusTrashed && status != sqlcgen.FileStatusDeletionPending {
			return ErrNotTrashed
		}
	}
	nodes := make([]treeNode, 0, len(nodesByID))
	ids := make([]uuid.UUID, 0, len(nodesByID))
	for id, node := range nodesByID {
		nodes = append(nodes, node)
		ids = append(ids, id)
	}
	fileIDs := pgUUIDs(ids)
	if err := s.queries.MarkFileIDsDeletionPending(ctx, sqlcgen.MarkFileIDsDeletionPendingParams{
		UserID: userID, FileIds: fileIDs,
	}); err != nil {
		return fmt.Errorf("mark subtree deletion pending: %w", err)
	}
	s.catalog.InvalidateFiles(ctx, userID, ids...)

	refs, err := s.queries.ListFilePartMessageRefs(ctx, fileIDs)
	if err != nil {
		return fmt.Errorf("list purge parts: %w", err)
	}
	grouped := make(map[int64][]int64)
	for _, ref := range refs {
		grouped[ref.ChannelID] = append(grouped[ref.ChannelID], ref.MessageID)
	}
	channelIDs := slices.Sorted(maps.Keys(grouped))
	for _, channelID := range channelIDs {
		if err := s.storage.DeleteMessages(ctx, userID, channelID, grouped[channelID]); err != nil {
			return fmt.Errorf("delete Telegram purge messages for channel %d: %w", channelID, err)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.DeleteFilePartsByFileIDs(ctx, fileIDs); err != nil {
		return fmt.Errorf("delete purge parts: %w", err)
	}
	if err := queries.ClearUploadSessionParentsByFileIDs(ctx, sqlcgen.ClearUploadSessionParentsByFileIDsParams{
		UserID: userID, FileIds: fileIDs,
	}); err != nil {
		return fmt.Errorf("clear purge upload session parents: %w", err)
	}
	byDepth := make(map[int32][]uuid.UUID)
	depths := make([]int32, 0)
	for _, node := range nodes {
		id, _ := dbtypes.GoogleUUID(node.File.ID)
		if _, ok := byDepth[node.Depth]; !ok {
			depths = append(depths, node.Depth)
		}
		byDepth[node.Depth] = append(byDepth[node.Depth], id)
	}
	slices.SortFunc(depths, func(a, b int32) int { return cmp.Compare(b, a) })
	for _, depth := range depths {
		depthIDs := byDepth[depth]
		count, err := queries.DeleteFileCatalogRowsByIDs(ctx, sqlcgen.DeleteFileCatalogRowsByIDsParams{
			FileIds: pgUUIDs(depthIDs), UserID: userID,
		})
		if err != nil {
			return fmt.Errorf("delete purge catalog rows at depth %d: %w", depth, err)
		}
		if count != int64(len(depthIDs)) {
			return ErrNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit file purge: %w", err)
	}
	s.catalog.InvalidateFiles(ctx, userID, ids...)
	return nil
}

// loadTree reads the whole subtree under rootID for userID with one recursive query,
// ordered by depth, and returns ErrNotFound when the root does not exist or belongs to
// another user, so a foreign file is indistinguishable from a missing one. Unlike the
// multi-root variant PurgeMany uses, it neither filters nor deduplicates rows; Copy
// validates the returned statuses itself.
func (s *Service) loadTree(ctx context.Context, userID int64, rootID uuid.UUID) ([]treeNode, error) {
	rows, err := s.queries.LoadFileSubtree(ctx, sqlcgen.LoadFileSubtreeParams{
		RootID: dbtypes.UUID(rootID), UserID: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("load file subtree: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	out := make([]treeNode, 0, len(rows))
	for _, row := range rows {
		out = append(out, treeNode{File: sqlcgen.File{
			ID: row.ID, UserID: row.UserID, ParentID: row.ParentID, Name: row.Name,
			Kind: row.Kind, MimeType: row.MimeType,
			Size: row.Size, HashAlgorithm: row.HashAlgorithm, HashValue: row.HashValue,
			Encryption: row.Encryption, EncryptionKeyVersion: row.EncryptionKeyVersion,
			Status: row.Status, ModTime: row.ModTime, Generation: row.Generation,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, DeletedAt: row.DeletedAt,
		}, Depth: row.Depth})
	}
	return out, nil
}

// pgUUIDs converts Google-format UUIDs into the pgtype.UUID slice that generated
// queries bind as uuid[]. It always allocates a fresh slice, so the caller keeps
// ownership of the input, and it passes uuid.Nil through unchanged rather than
// rejecting it, leaving validation to the caller.
func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	result := make([]pgtype.UUID, len(ids))
	for index, id := range ids {
		result[index] = dbtypes.UUID(id)
	}
	return result
}

// purgeAdvisoryLockID derives the session-level advisory lock key that guards the
// permanent deletion of one file. The key is the leading 64 bits of a SHA-256 over a
// purge-specific namespace, the big-endian user ID and the file UUID, which makes it
// stable across processes and disjoint from the copy destination locks. PurgeMany takes
// these keys non-blocking to decide which roots the current call owns.
func purgeAdvisoryLockID(userID int64, fileID uuid.UUID) int64 {
	var user [8]byte
	binary.BigEndian.PutUint64(user[:], uint64(userID))
	input := append([]byte("teldrive/purge/"), user[:]...)
	input = append(input, fileID[:]...)
	digest := sha256.Sum256(input)
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
