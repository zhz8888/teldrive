package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dblock"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

// maxBulkFiles caps how many entries a single bulk operation may address. It
// keeps the generated UUID arrays and the per-entry name matching bounded, at the
// cost of rejecting larger batches with ErrNotFound.
const maxBulkFiles = 500

// normalizeBulkIDs validates a batch of entry IDs and removes duplicates while
// keeping the caller's order, which the move path relies on to return its results.
// An empty batch, one larger than maxBulkFiles, or one containing uuid.Nil is
// rejected with ErrNotFound; duplicates are silently dropped.
func normalizeBulkIDs(ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 || len(ids) > maxBulkFiles {
		return nil, ErrNotFound
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	result := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			return nil, ErrNotFound
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

// BulkTrash moves every requested root and all of its descendants to trash in
// one transaction. It returns every affected entry, including descendants.
func (s *Service) BulkTrash(ctx context.Context, userID int64, rawIDs []uuid.UUID) ([]*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	ids, err := normalizeBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin bulk trash: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	fileIDs := pgUUIDs(ids)

	roots, err := queries.LockActiveFiles(ctx, sqlcgen.LockActiveFilesParams{UserID: userID, FileIds: fileIDs})
	if err != nil {
		return nil, fmt.Errorf("lock bulk trash roots: %w", err)
	}
	if len(roots) != len(ids) {
		return nil, ErrNotFound
	}
	items, err := queries.TrashFileSubtrees(ctx, sqlcgen.TrashFileSubtreesParams{UserID: userID, FileIds: fileIDs})
	if err != nil {
		return nil, fmt.Errorf("bulk trash files: %w", err)
	}
	if err := queries.RevokeSharesForFileSubtrees(ctx, sqlcgen.RevokeSharesForFileSubtreesParams{
		UserID: userID, FileIds: fileIDs,
	}); err != nil {
		return nil, fmt.Errorf("revoke bulk trashed shares: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit bulk trash: %w", err)
	}
	s.InvalidateFiles(ctx, userID, StableIDs(items)...)
	return items, nil
}

// MoveWithPolicy moves one entry using the same conflict rules as bulk move.
func (s *Service) MoveWithPolicy(ctx context.Context, userID int64, fileID uuid.UUID, parentID *uuid.UUID, expectedGeneration *int64, policy string) (*sqlcgen.File, error) {
	items, err := s.bulkMove(ctx, userID, []uuid.UUID{fileID}, parentID, expectedGeneration, policy)
	if err != nil {
		return nil, err
	}
	if len(items) != 1 {
		return nil, ErrNotFound
	}
	return items[0], nil
}

// BulkMove moves several entries to parentID in one transaction. It applies the
// same conflict vocabulary as MoveWithPolicy: "fail" (also the meaning of an
// empty policy) aborts on the first name clash with ErrConflict, "replace" marks
// the clashing entry's subtree for deletion, and "rename" picks the next free
// "(n)" name. An unknown policy is rejected as ErrUnsupportedConflictPolicy,
// while a real name clash stays ErrConflict. It performs no generation
// precondition check, and the returned rows follow the order of ids.
func (s *Service) BulkMove(ctx context.Context, userID int64, ids []uuid.UUID, parentID *uuid.UUID, policy string) ([]*sqlcgen.File, error) {
	return s.bulkMove(ctx, userID, ids, parentID, nil, policy)
}

// bulkMove is the shared implementation of MoveWithPolicy and BulkMove. The whole
// move runs in one transaction: it takes the destination namespace advisory lock,
// locks the destination folder and the moving rows together in a deterministic
// order, rejects cycles, resolves name conflicts against the entries already
// there, marks replaced subtrees for deletion and revokes their shares, and
// finally updates the rows. Any error rolls the transaction back untouched; the
// cache is invalidated only after the commit, for both the replaced and the moved
// entries. A nil parentID means the drive root, and expectedGeneration is checked
// for every moved row, so only the single-entry path passes a non-nil value.
func (s *Service) bulkMove(ctx context.Context, userID int64, rawIDs []uuid.UUID, parentID *uuid.UUID, expectedGeneration *int64, policy string) ([]*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	ids, err := normalizeBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	if policy == "" {
		policy = "fail"
	}
	if policy != "fail" && policy != "replace" && policy != "rename" {
		return nil, ErrUnsupportedConflictPolicy
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin bulk move: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)

	// The destination folder is locked as one more row in the single lock statement
	// below instead of a statement of its own, because that statement takes its row
	// locks in a deterministic order. Two moves that swap two folders, one moving A
	// into B while the other moves B into A, would otherwise take the row locks in
	// opposite orders and deadlock; a deadlock is reported by PostgreSQL as 40P01,
	// which no caller of this path recovers from. The advisory lock is taken first:
	// it is the only lock in this transaction that does not come from that sorted
	// statement, so every transaction here acquires its locks in the same order.
	if err := queries.AcquireAdvisoryTransactionLock(ctx, dblock.Destination(userID, parentID)); err != nil {
		return nil, fmt.Errorf("lock bulk move namespace: %w", err)
	}

	lockIDs := rowIDs(parentID, ids)
	lockedRows, err := queries.LockActiveFiles(ctx, sqlcgen.LockActiveFilesParams{
		UserID: userID, FileIds: pgUUIDs(lockIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("lock bulk move files: %w", err)
	}
	locked := make(map[uuid.UUID]*sqlcgen.File, len(lockIDs))
	for _, file := range lockedRows {
		id, ok := fileUUID(file)
		if !ok {
			return nil, ErrNotFound
		}
		locked[id] = file
	}
	// A folder that is missing, foreign, trashed or not a folder is absent from the
	// result, exactly as the destination-folder query on its own reported it.
	if parentID != nil && locked[*parentID] == nil {
		return nil, ErrInvalidParent
	}
	if len(locked) != len(lockIDs) {
		return nil, ErrNotFound
	}

	requested := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		requested[id] = struct{}{}
		if parentID != nil && id == *parentID {
			return nil, ErrCycle
		}
	}
	if parentID != nil {
		ancestorIDs, err := queries.ListFileAncestorIDs(ctx, sqlcgen.ListFileAncestorIDsParams{
			FileID: dbtypes.UUID(*parentID), UserID: userID,
		})
		if err != nil {
			return nil, fmt.Errorf("list bulk move destination ancestors: %w", err)
		}
		for _, ancestorID := range ancestorIDs {
			id, ok := dbtypes.GoogleUUID(ancestorID)
			if !ok {
				continue
			}
			// The walk starts at the destination itself, which the lock statement above
			// put into locked as a folder; seeing it there says nothing about a cycle.
			if id == *parentID {
				continue
			}
			if file := locked[id]; file != nil && file.Kind == sqlcgen.FileKindFolder {
				return nil, ErrCycle
			}
		}
	}

	// The names of the folder are read without a lock, and only the entries a moved
	// name actually collides with are locked below: locking every child held the
	// whole destination for the length of the transaction.
	siblings, err := queries.ListActiveDestinationEntries(ctx, sqlcgen.ListActiveDestinationEntriesParams{
		UserID: userID, ParentID: dbtypes.OptionalUUID(parentID),
	})
	if err != nil {
		return nil, fmt.Errorf("list bulk move destination entries: %w", err)
	}
	movedNames := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		movedNames[locked[id].Name] = struct{}{}
	}
	usedNames := make(map[string]struct{}, len(siblings)+len(ids))
	colliding := make([]string, 0, len(ids))
	for _, entry := range siblings {
		usedNames[entry.Name] = struct{}{}
		if _, collides := movedNames[entry.Name]; collides {
			colliding = append(colliding, entry.Name)
		}
	}
	destination, err := queries.LockActiveDestinationEntries(ctx, sqlcgen.LockActiveDestinationEntriesParams{
		UserID: userID, ParentID: dbtypes.OptionalUUID(parentID), Names: colliding,
	})
	if err != nil {
		return nil, fmt.Errorf("lock bulk move destination entries: %w", err)
	}
	conflicts := make(map[string]uuid.UUID, len(destination))
	for _, entry := range destination {
		entryID, ok := dbtypes.GoogleUUID(entry.ID)
		if !ok {
			return nil, ErrConflict
		}
		conflicts[entry.Name] = entryID
	}

	names := make([]string, len(ids))
	replacedSet := make(map[uuid.UUID]struct{})
	for index, id := range ids {
		file := locked[id]
		name := file.Name
		if occupant, occupied := conflicts[name]; occupied && occupant == id {
			delete(usedNames, name)
			delete(conflicts, name)
		}
		_, nameUsed := usedNames[name]
		if nameUsed {
			switch policy {
			case "fail":
				return nil, ErrConflict
			case "replace":
				conflictID, exists := conflicts[name]
				if !exists {
					return nil, ErrConflict
				}
				if _, moving := requested[conflictID]; moving {
					return nil, ErrConflict
				}
				replacedSet[conflictID] = struct{}{}
				delete(usedNames, name)
				delete(conflicts, name)
			case "rename":
				var renameErr error
				name, renameErr = nextAvailableNameFromSet(file.Name, usedNames)
				if renameErr != nil {
					return nil, renameErr
				}
			}
		}
		usedNames[name] = struct{}{}
		conflicts[name] = id
		names[index] = name
	}

	invalidated := make([]uuid.UUID, 0)
	if len(replacedSet) > 0 {
		replacedRoots := make([]uuid.UUID, 0, len(replacedSet))
		for id := range replacedSet {
			replacedRoots = append(replacedRoots, id)
		}
		replacedRows, err := queries.LoadFileSubtrees(ctx, sqlcgen.LoadFileSubtreesParams{
			RootIds: pgUUIDs(replacedRoots), UserID: userID,
		})
		if err != nil {
			return nil, fmt.Errorf("load replaced subtrees: %w", err)
		}
		for _, row := range replacedRows {
			if id, ok := dbtypes.GoogleUUID(row.ID); ok {
				invalidated = append(invalidated, id)
			}
		}
		if err := queries.MarkFileSubtreesDeletionPending(ctx, sqlcgen.MarkFileSubtreesDeletionPendingParams{
			FileIds: pgUUIDs(replacedRoots), UserID: userID,
		}); err != nil {
			return nil, fmt.Errorf("mark replaced subtrees for deletion: %w", err)
		}
		if err := queries.RevokeSharesForFileSubtrees(ctx, sqlcgen.RevokeSharesForFileSubtreesParams{
			UserID: userID, FileIds: pgUUIDs(replacedRoots),
		}); err != nil {
			return nil, fmt.Errorf("revoke replaced subtree shares: %w", err)
		}
	}

	updatedRows, err := queries.MoveFilesWithNames(ctx, sqlcgen.MoveFilesWithNamesParams{
		ParentID: dbtypes.OptionalUUID(parentID), UserID: userID,
		ExpectedGeneration: dbtypes.OptionalInt8(expectedGeneration), FileIds: pgUUIDs(ids),
		Names: names,
	})
	if err != nil {
		return nil, classifyWriteError("move files", err)
	}
	if len(updatedRows) != len(ids) {
		if expectedGeneration != nil {
			return nil, ErrPrecondition
		}
		return nil, ErrNotFound
	}
	updatedByID := make(map[uuid.UUID]*sqlcgen.File, len(updatedRows))
	for _, file := range updatedRows {
		if id, ok := fileUUID(file); ok {
			updatedByID[id] = file
		}
	}
	result := make([]*sqlcgen.File, 0, len(ids))
	for _, id := range ids {
		file := updatedByID[id]
		if file == nil {
			return nil, ErrNotFound
		}
		result = append(result, file)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, classifyWriteError("commit bulk move", err)
	}
	for _, file := range result {
		if id, ok := fileUUID(file); ok {
			invalidated = append(invalidated, id)
		}
	}
	s.InvalidateFiles(ctx, userID, invalidated...)
	return result, nil
}

// nextAvailableNameFromSet returns a variant of original with " (n)" inserted
// before its extension, for the first n between 1 and 10000 that is not in used.
// It gives up with ErrConflict once that range is exhausted and does not add the
// name it returns to used, so the caller has to reserve it.
func nextAvailableNameFromSet(original string, used map[string]struct{}) (string, error) {
	base, extension := splitCatalogName(original)
	for sequence := 1; sequence <= 10000; sequence++ {
		candidate := base + fmt.Sprintf(" (%d)", sequence) + extension
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
	return "", ErrConflict
}

// splitCatalogName splits name at its last dot into base and extension, keeping
// the dot in the extension. A dot at the first position or at the very end does
// not start an extension, so dotfiles and names ending in a dot are returned
// whole with an empty extension.
func splitCatalogName(name string) (string, string) {
	index := strings.LastIndex(name, ".")
	if index <= 0 || index == len(name)-1 {
		return name, ""
	}
	return name[:index], name[index:]
}

// rowIDs returns the ids that one bulk move row-locks in a single statement: the
// destination folder, when there is one, followed by the moved entries in ascending
// UUID order. The sort is what keeps concurrent moves from deadlocking, because
// PostgreSQL locks the rows of a FOR UPDATE statement in the order the statement
// reads them, so every move in this package asks for its row locks in the same
// order and no two of them can hold what the other one waits for. The destination
// folder is part of the same sorted list for the same reason: locking it in a
// statement of its own, before the moved entries, is exactly how a move of A into B
// and a move of B into A used to take the same two rows in opposite orders.
//
// A nil parent means the drive root, which is not a row and contributes no id, and
// a parent that is also one of the moved ids appears once: moving a folder into
// itself is rejected as a cycle after the lock, so the duplicate must not be
// counted twice here.
func rowIDs(parentID *uuid.UUID, ids []uuid.UUID) []uuid.UUID {
	result := make([]uuid.UUID, 0, len(ids)+1)
	if parentID != nil {
		result = append(result, *parentID)
	}
	result = append(result, ids...)
	slices.SortFunc(result, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(result)
}

// pgUUIDs converts a batch of Google UUIDs into the pgtype form the array
// parameters of the generated bulk queries expect. It always returns a fresh
// slice, so callers may keep or modify the result.
func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	result := make([]pgtype.UUID, len(ids))
	for index, id := range ids {
		result[index] = dbtypes.UUID(id)
	}
	return result
}

// fileUUID converts the ID of a generated row into a Google UUID. It reports false
// for a nil row and for a NULL uuid column, which lets callers distinguish "row
// without an ID" from the valid uuid.Nil value.
func fileUUID(file *sqlcgen.File) (uuid.UUID, bool) {
	if file == nil || !file.ID.Valid {
		return uuid.Nil, false
	}
	return uuid.UUID(file.ID.Bytes), true
}

// StableIDs is useful to clients and tests that need deterministic ordering of
// an affected subtree returned by BulkTrash.
func StableIDs(files []*sqlcgen.File) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(files))
	for _, file := range files {
		if id, ok := fileUUID(file); ok {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}
