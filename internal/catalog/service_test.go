package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

func TestServiceRejectsInvalidInputsBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil)
	ctx := context.Background()
	id := uuid.New()

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "create owner", call: func() error { _, err := svc.CreateFolder(ctx, CreateFolderInput{Name: "x"}); return err }, want: ErrInvalidOwner},
		{name: "get owner", call: func() error { _, err := svc.Get(ctx, 0, id); return err }, want: ErrInvalidOwner},
		{name: "list owner", call: func() error { _, err := svc.List(ctx, ListInput{}); return err }, want: ErrInvalidOwner},
		{name: "rename owner", call: func() error { _, err := svc.Rename(ctx, 0, id, nil, "x"); return err }, want: ErrInvalidOwner},
		{name: "move owner", call: func() error { _, err := svc.Move(ctx, 0, id, nil, nil); return err }, want: ErrInvalidOwner},
		{name: "trash owner", call: func() error { _, err := svc.Trash(ctx, 0, id); return err }, want: ErrInvalidOwner},
		{name: "restore owner", call: func() error { _, err := svc.Restore(ctx, 0, id); return err }, want: ErrInvalidOwner},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestServiceRejectsBlankAndUnknownInputsBeforeDatabaseAccess covers the
// validation that runs before the first query, which a nil pool would otherwise
// panic on: a blank rename/update name, an unknown move policy and invalid list
// filters.
func TestServiceRejectsBlankAndUnknownInputsBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil)
	ctx := context.Background()
	id := uuid.New()

	blank := " \t "
	if _, err := svc.Rename(ctx, 1, id, nil, blank); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Rename(blank) error = %v, want ErrInvalidName", err)
	}
	if _, err := svc.Update(ctx, UpdateInput{UserID: 1, FileID: id, Name: &blank}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Update(blank) error = %v, want ErrInvalidName", err)
	}
	if _, err := svc.CreateFolder(ctx, CreateFolderInput{UserID: 1, Name: blank}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("CreateFolder(blank) error = %v, want ErrInvalidName", err)
	}
	if _, err := svc.MoveWithPolicy(ctx, 1, id, nil, nil, "bogus"); !errors.Is(err, ErrUnsupportedConflictPolicy) {
		t.Fatalf("MoveWithPolicy(unknown policy) error = %v, want ErrUnsupportedConflictPolicy", err)
	}

	filters := []ListInput{
		{UserID: 1, SearchType: "bogus"},
		{UserID: 1, SearchType: "regex", Search: "[", Sort: "name", Order: "asc"},
		{UserID: 1, Sort: "bogus"},
		{UserID: 1, Order: "bogus"},
		{UserID: 1, Categories: []string{"bogus"}},
		{UserID: 1, Sort: "size", AfterID: &id, AfterValue: "bogus"},
		{UserID: 1, ParentID: &id, Path: "/Docs"},
	}
	for _, input := range filters {
		if _, err := svc.List(ctx, input); !errors.Is(err, ErrInvalidFilter) {
			t.Fatalf("List(%#v) error = %v, want ErrInvalidFilter", input, err)
		}
	}
}

// TestCloneFilePartsReturnsOwnedCopies pins the ownership contract of Parts: the
// caller's slice and rows are copies, so mutating them cannot reach the cache.
func TestCloneFilePartsReturnsOwnedCopies(t *testing.T) {
	t.Parallel()

	if got := cloneFileParts(nil); got != nil {
		t.Fatalf("cloneFileParts(nil) = %#v, want nil", got)
	}
	original := &sqlcgen.FilePart{PartNo: 1, StoredSize: dbtypes.Int8(10)}
	cloned := cloneFileParts([]*sqlcgen.FilePart{original, nil})
	if len(cloned) != 2 || cloned[0] == original || cloned[0].PartNo != 1 || cloned[1] != nil {
		t.Fatalf("cloneFileParts() = %#v", cloned)
	}
	cloned[0].StoredSize = dbtypes.Int8(99)
	if original.StoredSize.Int64 != 10 {
		t.Fatalf("mutating the copy changed the original: %v", original.StoredSize)
	}
}
