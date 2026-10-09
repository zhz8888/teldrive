package catalog

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

func TestBulkCatalogHelpers(t *testing.T) {
	t.Parallel()

	if base, extension := splitCatalogName("archive.tar.gz"); base != "archive.tar" || extension != ".gz" {
		t.Fatalf("splitCatalogName() = %q, %q", base, extension)
	}
	for _, name := range []string{"README", ".hidden", "trailing."} {
		if base, extension := splitCatalogName(name); base != name || extension != "" {
			t.Fatalf("splitCatalogName(%q) = %q, %q", name, base, extension)
		}
	}

	ids := []uuid.UUID{uuid.New(), uuid.New()}
	converted := pgUUIDs(ids)
	if len(converted) != len(ids) {
		t.Fatalf("pgUUIDs() length = %d", len(converted))
	}
	for index, id := range ids {
		got, ok := dbtypes.GoogleUUID(converted[index])
		if !ok || got != id {
			t.Fatalf("pgUUIDs()[%d] = %v, %t", index, got, ok)
		}
	}

	if id, ok := fileUUID(nil); ok || id != uuid.Nil {
		t.Fatalf("fileUUID(nil) = %v, %t", id, ok)
	}
	if id, ok := fileUUID(&sqlcgen.File{}); ok || id != uuid.Nil {
		t.Fatalf("fileUUID(invalid) = %v, %t", id, ok)
	}
	validID := uuid.New()
	validFile := &sqlcgen.File{ID: dbtypes.UUID(validID)}
	if id, ok := fileUUID(validFile); !ok || id != validID {
		t.Fatalf("fileUUID(valid) = %v, %t", id, ok)
	}

	first := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	stable := StableIDs([]*sqlcgen.File{
		{ID: dbtypes.UUID(first)}, nil, {ID: pgtype.UUID{}}, {ID: dbtypes.UUID(second)},
	})
	if len(stable) != 2 || stable[0] != second || stable[1] != first {
		t.Fatalf("StableIDs() = %v", stable)
	}
}

// TestRowIDsLocksOneDeterministicOrder pins the row-lock list bulkMove hands to its
// single FOR UPDATE statement. Every move has to present the same ids in the same
// order, whatever order the caller listed them in, because a statement locks its rows
// in the order it reads them and moves that acquire rows in different orders deadlock.
func TestRowIDsLocksOneDeterministicOrder(t *testing.T) {
	t.Parallel()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	third := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

	for _, tc := range []struct {
		name     string
		parentID *uuid.UUID
		ids      []uuid.UUID
		want     []uuid.UUID
	}{
		{name: "root destination", ids: []uuid.UUID{third, first}, want: []uuid.UUID{first, third}},
		{name: "folder destination", parentID: &second, ids: []uuid.UUID{third}, want: []uuid.UUID{second, third}},
		{name: "folder destination after its child", parentID: &third, ids: []uuid.UUID{first}, want: []uuid.UUID{first, third}},
		{name: "destination is also moved", parentID: &second, ids: []uuid.UUID{second, first}, want: []uuid.UUID{first, second}},
	} {
		got := rowIDs(tc.parentID, tc.ids)
		if len(got) != len(tc.want) {
			t.Fatalf("rowIDs(%s) = %v, want %v", tc.name, got, tc.want)
		}
		for index := range tc.want {
			if got[index] != tc.want[index] {
				t.Fatalf("rowIDs(%s) = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

func TestFileCursorValueVariants(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	updatedAt := time.Date(2026, 7, 2, 12, 34, 56, 789, time.UTC)
	file := &sqlcgen.File{
		ID:        dbtypes.UUID(id),
		Name:      "Name",
		Size:      pgtype.Int8{Int64: 42, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: updatedAt, Valid: true},
	}
	if got := FileCursorValue(file, "id"); got != id.String() {
		t.Fatalf("id cursor = %q", got)
	}
	if got := FileCursorValue(file, "size"); got != "42" {
		t.Fatalf("size cursor = %q", got)
	}
	if got := FileCursorValue(file, "updatedAt"); got != updatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("updated cursor = %q", got)
	}
	if got := FileCursorValue(file, "name"); got != "Name" {
		t.Fatalf("name cursor = %q", got)
	}
	if got := FileCursorValue(&sqlcgen.File{}, "size"); got != "-1" {
		t.Fatalf("invalid size cursor = %q", got)
	}
	if got := FileCursorValue(&sqlcgen.File{}, "id"); got != "" {
		t.Fatalf("invalid ID cursor = %q", got)
	}
	if got := FileCursorValue(nil, "name"); got != "" {
		t.Fatalf("nil cursor = %q", got)
	}
}
