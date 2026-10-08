//go:build integration

package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/catalog"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// TestViewStateRoundTripsPerUserAndFile covers the reader's memory of where it was:
// the state is keyed by the user who saved it, so two people reading the same file
// keep separate positions, and one user's state is invisible to the other.
func TestViewStateRoundTripsPerUserAndFile(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	seedUser(t, db.Pool, 2002)
	fileID := seedFile(t, db.Pool, 1001, nil, "manual.pdf", "application/pdf", 10, time.Now())

	service := catalog.NewService(db.Pool, nil)

	// Reading before anything is stored is a miss, not an error the caller has to
	// treat differently from a stored state.
	if _, err := service.GetViewState(ctx, 1001, fileID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("GetViewState() before any state error = %v, want ErrNotFound", err)
	}

	first, err := service.UpsertViewState(ctx, 1001, fileID, "pdf",
		[]byte(`{"page":12}`), []byte(`{"zoom":1.5}`), []byte(`[3,4]`))
	if err != nil {
		t.Fatalf("UpsertViewState() error = %v", err)
	}
	if first.ViewerKind != "pdf" {
		t.Fatalf("viewer kind = %q, want pdf", first.ViewerKind)
	}

	// The same user storing again replaces the state instead of adding a second
	// row, so the reader never reads a stale position back.
	updated, err := service.UpsertViewState(ctx, 1001, fileID, "pdf",
		[]byte(`{"page":40}`), []byte(`{"zoom":2}`), []byte(`[5]`))
	if err != nil {
		t.Fatalf("second UpsertViewState() error = %v", err)
	}
	if page := jsonPage(t, updated.Position); page != 40 {
		t.Fatalf("page = %d, want 40", page)
	}

	stored, err := service.GetViewState(ctx, 1001, fileID)
	if err != nil {
		t.Fatalf("GetViewState() error = %v", err)
	}
	if page := jsonPage(t, stored.Position); page != 40 {
		t.Fatalf("stored page = %d, want 40", page)
	}
	if zoom := jsonZoom(t, stored.Preferences); zoom != 2 {
		t.Fatalf("stored zoom = %v, want 2", zoom)
	}
	var bookmarks []int
	if err := json.Unmarshal(stored.Bookmarks, &bookmarks); err != nil {
		t.Fatal(err)
	}
	if len(bookmarks) != 1 || bookmarks[0] != 5 {
		t.Fatalf("bookmarks = %v, want [5]", bookmarks)
	}

	// Another user reading the same file starts from nothing.
	if _, err := service.GetViewState(ctx, 2002, fileID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("GetViewState() for another user error = %v, want ErrNotFound", err)
	}

	if err := service.DeleteViewState(ctx, 1001, fileID); err != nil {
		t.Fatalf("DeleteViewState() error = %v", err)
	}
	if _, err := service.GetViewState(ctx, 1001, fileID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("GetViewState() after delete error = %v, want ErrNotFound", err)
	}
	// Deleting again is harmless: the reader clears its state on unmount even when
	// the tab that owned it is already gone.
	if err := service.DeleteViewState(ctx, 1001, fileID); err != nil {
		t.Fatalf("second DeleteViewState() error = %v", err)
	}
}

// TestPartsReturnsOwnedCopiesOnEveryRead guards the contract the download path
// depends on: callers mutate the returned rows to backfill legacy part sizes, so
// a row handed back from the cache must not alias the one a later call returns.
// jsonPage reads the page out of a stored position, so the assertions do not depend
// on how PostgreSQL reformats the jsonb it was handed.
func jsonPage(t *testing.T, raw []byte) int {
	t.Helper()
	var decoded struct {
		Page int `json:"page"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode position %s: %v", raw, err)
	}
	return decoded.Page
}

func jsonZoom(t *testing.T, raw []byte) float64 {
	t.Helper()
	var decoded struct {
		Zoom float64 `json:"zoom"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode preferences %s: %v", raw, err)
	}
	return decoded.Zoom
}

func TestPartsReturnsOwnedCopiesOnEveryRead(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	fileID := seedFile(t, db.Pool, 1001, nil, "movie.mp4", "video/mp4", 300, time.Now())
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id, part_no, channel_id, message_id, plain_size, stored_size)
SELECT $1, value, 9001, value, 100, 120
FROM generate_series(1, 3) AS value`, fileID); err != nil {
		t.Fatal(err)
	}

	service := catalog.NewService(db.Pool, nil)

	first, err := service.Parts(ctx, 1001, fileID)
	if err != nil {
		t.Fatalf("Parts() error = %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("parts = %d, want 3", len(first))
	}
	// Parts come back in ascending part order so the transfer reassembles the file.
	for index, part := range first {
		if part.PartNo != int32(index+1) {
			t.Fatalf("part %d has part_no %d, want %d", index, part.PartNo, index+1)
		}
	}

	// Mutating the first read must not be visible to the next one.
	first[0].PlainSize = pgtype.Int8{Int64: -1, Valid: true}
	first[0].MessageID = -1
	second, err := service.Parts(ctx, 1001, fileID)
	if err != nil {
		t.Fatalf("second Parts() error = %v", err)
	}
	if second[0].PlainSize.Int64 == -1 || second[0].MessageID == -1 {
		t.Fatalf("second Parts() returned the mutated row: plain_size %d message_id %d",
			second[0].PlainSize.Int64, second[0].MessageID)
	}
}

// TestPartsHidesAnotherUsersFile checks the ownership boundary: parts are only
// reachable through the file's owner.
func TestPartsHidesAnotherUsersFile(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	seedUser(t, db.Pool, 2002)
	fileID := seedFile(t, db.Pool, 1001, nil, "private.bin", "application/octet-stream", 10, time.Now())
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id, part_no, channel_id, message_id, plain_size, stored_size)
VALUES ($1, 1, 9001, 1, 10, 12)`, fileID); err != nil {
		t.Fatal(err)
	}

	service := catalog.NewService(db.Pool, nil)
	parts, err := service.Parts(ctx, 2002, fileID)
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Parts() error = %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("Parts() for another user returned %d parts, want none", len(parts))
	}
}

// TestUpdatePartSizesBackfillsOnlyMissingSizes pins the legacy repair the download
// path performs. It fills in a part written before sizes were recorded, and it
// never touches a part that already knows its sizes, so a retry cannot corrupt a
// good row.
func TestUpdatePartSizesBackfillsOnlyMissingSizes(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	fileID := seedFile(t, db.Pool, 1001, nil, "sizes.bin", "application/octet-stream", 30, time.Now())
	// Part 1 is legacy and has no sizes; part 2 already knows them.
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id, part_no, channel_id, message_id, plain_size, stored_size)
VALUES ($1, 1, 9001, 1, NULL, NULL),
       ($1, 2, 9001, 2, 10, 12)`, fileID); err != nil {
		t.Fatal(err)
	}

	service := catalog.NewService(db.Pool, nil)
	if err := service.UpdatePartSizes(ctx, fileID, 1, 20, 24); err != nil {
		t.Fatalf("UpdatePartSizes() for the legacy part error = %v", err)
	}

	var plain, stored int64
	if err := db.Pool.QueryRow(ctx,
		"SELECT plain_size, stored_size FROM file_parts WHERE file_id = $1 AND part_no = 1", fileID).Scan(&plain, &stored); err != nil {
		t.Fatal(err)
	}
	if plain != 20 || stored != 24 {
		t.Fatalf("backfilled sizes = %d/%d, want 20/24", plain, stored)
	}

	// A part whose sizes are already recorded must be left exactly as it was.
	if err := db.Pool.QueryRow(ctx,
		"SELECT plain_size, stored_size FROM file_parts WHERE file_id = $1 AND part_no = 2", fileID).Scan(&plain, &stored); err != nil {
		t.Fatal(err)
	}
	if plain != 10 || stored != 12 {
		t.Fatalf("known sizes = %d/%d, want the stored 10/12", plain, stored)
	}
}
