//go:build integration

package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/catalog"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
	"github.com/zhz8888/teldrive/v2/internal/testutil/querytrace"
)

func TestCatalogPreservesExactNames(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	svc := catalog.NewService(db.Pool, nil)

	decomposed := "  Cafe\u0301  " + strings.Repeat("x", 300)
	created, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: decomposed})
	if err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if created.Name != decomposed {
		t.Fatalf("created name = %q, want exact %q", created.Name, decomposed)
	}
	if _, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "  Café  " + strings.Repeat("x", 300)}); err != nil {
		t.Fatalf("create NFC-distinct name: %v", err)
	}
}

func TestBulkMoveUsesSetBasedQueries(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	destinationID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id,user_id,name,kind,encryption,status,mod_time)
VALUES ($1,1001,'destination','folder',false,'active',now())`, destinationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id,user_id,name,kind,size,encryption,status,mod_time)
SELECT gen_random_uuid(), 1001, 'file-' || value, 'file', 1, false, 'active', now()
FROM generate_series(1, 500) AS value`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Pool.Query(ctx, "SELECT id FROM files WHERE kind = 'file' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var fileIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		fileIDs = append(fileIDs, id)
	}
	rows.Close()

	tracer := &querytrace.Counter{}
	config, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc := catalog.NewService(pool, nil)
	moved, err := svc.BulkMove(ctx, 1001, fileIDs, &destinationID, "fail")
	if err != nil {
		t.Fatalf("BulkMove() error = %v", err)
	}
	if len(moved) != len(fileIDs) {
		t.Fatalf("moved files = %d, want %d", len(moved), len(fileIDs))
	}
	for _, name := range []string{"LockActiveFiles", "ListFileAncestorIDs", "LockActiveDestinationEntries", "MoveFilesWithNames"} {
		if got := tracer.Count(name); got != 1 {
			t.Fatalf("%s queries = %d, want 1", name, got)
		}
	}
	if got := tracer.Count("MoveFileWithName"); got != 0 {
		t.Fatalf("MoveFileWithName queries = %d, want 0", got)
	}
}

// TestBulkMoveSwapDoesNotDeadlock pins that two moves can swap two folders without
// taking the same two rows in opposite orders. The move locks the destination folder
// and the moving rows in one ORDER BY id statement, so whichever move gets the
// destination advisory lock first also takes both rows first and the other one waits;
// taking the destination folder row in a statement of its own, before the moved rows,
// left PostgreSQL free to find one transaction holding A and asking for B while the
// other held B and asked for A, and a PostgreSQL deadlock is a 40P01 the caller cannot
// recover from. Whichever move loses still reports a cycle, because by then the folder
// it wants to move into sits under the folder it is moving.
func TestBulkMoveSwapDoesNotDeadlock(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	firstID, secondID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id,user_id,name,kind,mime_type,encryption,status,mod_time)
VALUES
    ($1,1001,'first','folder','inode/directory',false,'active',now()),
    ($2,1001,'second','folder','inode/directory',false,'active',now())`, firstID, secondID); err != nil {
		t.Fatal(err)
	}
	svc := catalog.NewService(db.Pool, nil)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, move := range []struct{ from, into uuid.UUID }{{firstID, secondID}, {secondID, firstID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Move(ctx, 1001, move.from, &move.into, nil)
			results <- err
		}()
	}
	close(start)
	moved := make(chan struct{})
	go func() {
		wg.Wait()
		close(moved)
	}()
	select {
	case <-moved:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent folder swap did not finish, which means the moves deadlocked")
	}
	close(results)
	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, catalog.ErrCycle), errors.Is(err, catalog.ErrInvalidParent):
		default:
			t.Fatalf("concurrent folder swap error = %v", err)
		}
	}
	if succeeded == 0 {
		t.Fatal("neither move of the concurrent swap succeeded")
	}
}

func TestUpdatePartSizesManyUsesOneQuery(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	fileID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id,user_id,name,kind,size,encryption,status,mod_time)
VALUES ($1,1001,'legacy.bin','file',1000,false,'active',now())`, fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO channels (channel_id,user_id,name,selected) VALUES (9001,1001,'storage',true)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id,part_no,channel_id,message_id)
SELECT $1, value, 9001, value
FROM generate_series(1,1000) AS value`, fileID); err != nil {
		t.Fatal(err)
	}
	sizes := make(map[int32][2]int64, 1000)
	for partNo := int32(1); partNo <= 1000; partNo++ {
		sizes[partNo] = [2]int64{1, 1}
	}
	tracer := &querytrace.Counter{}
	config, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := catalog.NewService(pool, nil).UpdatePartSizesMany(ctx, fileID, sizes); err != nil {
		t.Fatalf("UpdatePartSizesMany() error = %v", err)
	}
	if got := tracer.Count("UpdateFilePartSizesMany"); got != 1 {
		t.Fatalf("UpdateFilePartSizesMany queries = %d, want 1", got)
	}
	if got := tracer.Count("UpdateFilePartSizes"); got != 0 {
		t.Fatalf("UpdateFilePartSizes queries = %d, want 0", got)
	}
}

func TestCatalogLifecycleAgainstRealPostgres(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	seedUser(t, db.Pool, 2002)

	svc := catalog.NewService(db.Pool, nil)
	docs, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "Docs"})
	if err != nil {
		t.Fatalf("create root folder: %v", err)
	}
	docsID := mustUUID(t, docs.ID)

	lowerDocs, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "docs"})
	if err != nil {
		t.Fatalf("create case-distinct root folder: %v", err)
	}
	if lowerDocs.Name != "docs" || docs.Name != "Docs" {
		t.Fatalf("case-sensitive names = %q, %q", docs.Name, lowerDocs.Name)
	}
	if _, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "Docs"}); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("exact duplicate error = %v, want ErrConflict", err)
	}
	if _, err := svc.Get(ctx, 2002, docsID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("cross-owner read error = %v, want ErrNotFound", err)
	}

	reports, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, ParentID: &docsID, Name: "Reports"})
	if err != nil {
		t.Fatalf("create reports folder: %v", err)
	}
	reportsID := mustUUID(t, reports.ID)
	archive, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, ParentID: &reportsID, Name: "Archive"})
	if err != nil {
		t.Fatalf("create archive folder: %v", err)
	}
	archiveID := mustUUID(t, archive.ID)

	if _, err := svc.Move(ctx, 1001, docsID, &archiveID, nil); !errors.Is(err, catalog.ErrCycle) {
		t.Fatalf("cycle move error = %v, want ErrCycle", err)
	}

	moved, err := svc.Move(ctx, 1001, reportsID, nil, &reports.Generation)
	if err != nil {
		t.Fatalf("move reports to root: %v", err)
	}
	wrongGeneration := moved.Generation - 1
	if _, err := svc.Rename(ctx, 1001, reportsID, &wrongGeneration, "Quarterly"); !errors.Is(err, catalog.ErrPrecondition) {
		t.Fatalf("stale rename error = %v, want ErrPrecondition", err)
	}
	renamed, err := svc.Rename(ctx, 1001, reportsID, &moved.Generation, "Quarterly")
	if err != nil {
		t.Fatalf("rename reports: %v", err)
	}
	if renamed.Name != "Quarterly" || renamed.Generation != moved.Generation+1 {
		t.Fatalf("renamed file = %#v", renamed)
	}

	items, err := svc.List(ctx, catalog.ListInput{UserID: 1001, Limit: 200})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("root listing = %#v", items)
	}
	listedNames := map[string]bool{}
	for _, item := range items {
		listedNames[item.Name] = true
	}
	for _, name := range []string{"Docs", "docs", "Quarterly"} {
		if !listedNames[name] {
			t.Fatalf("root listing missing %q: %#v", name, items)
		}
	}

	trashed, err := svc.Trash(ctx, 1001, docsID)
	if err != nil {
		t.Fatalf("trash docs: %v", err)
	}
	if trashed.Status != sqlcgen.FileStatusTrashed || !trashed.DeletedAt.Valid {
		t.Fatalf("trashed file = %#v", trashed)
	}
	if _, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "DOCS"}); err != nil {
		t.Fatalf("create case-distinct name after trash: %v", err)
	}
	if restored, err := svc.Restore(ctx, 1001, docsID); err != nil || restored.Name != "Docs" {
		t.Fatalf("restore case-distinct folder = %#v, %v", restored, err)
	}
}

func TestTrashRootListingIncludesNestedDeletedItems(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	svc := catalog.NewService(db.Pool, nil)

	activeFolder, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "Active"})
	if err != nil {
		t.Fatal(err)
	}
	activeFolderID := mustUUID(t, activeFolder.ID)
	deletedFileID := seedFile(t, db.Pool, 1001, &activeFolderID, "deleted-by-rclone.txt", "text/plain", 10, time.Now())
	if _, err := svc.Trash(ctx, 1001, deletedFileID); err != nil {
		t.Fatalf("trash nested file: %v", err)
	}

	trashedFolder, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "DeletedFolder"})
	if err != nil {
		t.Fatal(err)
	}
	trashedFolderID := mustUUID(t, trashedFolder.ID)
	trashedChildID := seedFile(t, db.Pool, 1001, &trashedFolderID, "child.txt", "text/plain", 20, time.Now())
	if _, err := svc.BulkTrash(ctx, 1001, []uuid.UUID{trashedFolderID}); err != nil {
		t.Fatalf("trash folder subtree: %v", err)
	}

	items, err := svc.List(ctx, catalog.ListInput{UserID: 1001, Status: sqlcgen.FileStatusTrashed, Limit: 100})
	if err != nil {
		t.Fatalf("list trash root: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, item := range items {
		got[mustUUID(t, item.ID)] = true
	}
	if !got[deletedFileID] {
		t.Fatalf("trash root missing nested deleted file %s: %#v", deletedFileID, items)
	}
	if !got[trashedFolderID] {
		t.Fatalf("trash root missing trashed folder %s: %#v", trashedFolderID, items)
	}
	if got[trashedChildID] {
		t.Fatalf("trash root unexpectedly includes child of trashed folder %s: %#v", trashedChildID, items)
	}
}

func TestAdvancedListingBulkOperationsAndStatistics(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	svc := catalog.NewService(db.Pool, nil)

	docs, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "Docs"})
	if err != nil {
		t.Fatal(err)
	}
	docsID := mustUUID(t, docs.ID)
	reports, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, ParentID: &docsID, Name: "Reports"})
	if err != nil {
		t.Fatal(err)
	}
	reportsID := mustUUID(t, reports.ID)
	archive, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, Name: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	archiveID := mustUUID(t, archive.ID)

	pdfID := seedFile(t, db.Pool, 1001, &reportsID, "Annual Report.pdf", "application/pdf", 1200, time.Now().Add(-2*time.Hour))
	imageID := seedFile(t, db.Pool, 1001, &reportsID, "Cover.JPG", "image/jpeg", 300, time.Now().Add(-time.Hour))
	_ = imageID
	seedFile(t, db.Pool, 1001, &archiveID, "Annual Report.pdf", "application/pdf", 800, time.Now())

	resolved, err := svc.ResolveFolderPath(ctx, 1001, nil, "/Docs/Reports/")
	if err != nil || resolved == nil || *resolved != reportsID {
		t.Fatalf("ResolveFolderPath() = %v, %v", resolved, err)
	}
	listed, err := svc.List(ctx, catalog.ListInput{
		UserID: 1001, Path: "Docs/Reports", Search: `(?i)report\.pdf$`, SearchType: "regex",
		Categories: []string{"document"}, Sort: "size", Order: "desc", Limit: 10,
	})
	if err != nil || len(listed) != 1 || mustUUID(t, listed[0].ID) != pdfID {
		t.Fatalf("advanced List() = %#v, %v", listed, err)
	}
	categoryStats, err := svc.CategoryStatistics(ctx, 1001)
	if err != nil {
		t.Fatalf("CategoryStatistics() error = %v", err)
	}
	var documentFiles, imageFiles int64
	for _, item := range categoryStats {
		switch item.Category {
		case "document":
			documentFiles = item.TotalFiles
		case "image":
			imageFiles = item.TotalFiles
		}
	}
	if documentFiles != 2 || imageFiles != 1 {
		t.Fatalf("category stats = %#v", categoryStats)
	}
	driveStats, err := svc.DriveStatistics(ctx, 1001)
	if err != nil || driveStats.TotalFiles != 3 || driveStats.TotalFolders != 3 || driveStats.TotalBytes != 2300 {
		t.Fatalf("DriveStatistics() = %#v, %v", driveStats, err)
	}

	moved, err := svc.BulkMove(ctx, 1001, []uuid.UUID{pdfID}, &archiveID, "rename")
	if err != nil || len(moved) != 1 || moved[0].Name != "Annual Report (1).pdf" {
		t.Fatalf("BulkMove(rename) = %#v, %v", moved, err)
	}
	trashed, err := svc.BulkTrash(ctx, 1001, []uuid.UUID{docsID})
	if err != nil || len(trashed) != 3 {
		t.Fatalf("BulkTrash() = %#v, %v", trashed, err)
	}
	for _, id := range []uuid.UUID{docsID, reportsID, imageID} {
		file, err := svc.Get(ctx, 1001, id)
		if err != nil || file.Status != sqlcgen.FileStatusTrashed {
			t.Fatalf("trashed subtree file %s = %#v, %v", id, file, err)
		}
	}
	restored, err := svc.Restore(ctx, 1001, docsID)
	if err != nil || mustUUID(t, restored.ID) != docsID {
		t.Fatalf("Restore(folder) = %#v, %v", restored, err)
	}
	for _, id := range []uuid.UUID{docsID, reportsID, imageID} {
		file, err := svc.Get(ctx, 1001, id)
		if err != nil || file.Status != sqlcgen.FileStatusActive || file.DeletedAt.Valid {
			t.Fatalf("restored subtree file %s = %#v, %v", id, file, err)
		}
	}
}

func TestCatalogRejectsInvalidParent(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedUser(t, db.Pool, 1001)
	svc := catalog.NewService(db.Pool, nil)
	missing := uuid.New()
	if _, err := svc.CreateFolder(ctx, catalog.CreateFolderInput{UserID: 1001, ParentID: &missing, Name: "child"}); !errors.Is(err, catalog.ErrInvalidParent) {
		t.Fatalf("invalid parent error = %v", err)
	}
}

// seedUser inserts the bare users row the file fixtures reference, so a test can
// create files for a user that never logged in.
func seedUser(t testing.TB, db *pgxpool.Pool, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), "INSERT INTO users (user_id) VALUES ($1)", userID); err != nil {
		t.Fatalf("seed user %d: %v", userID, err)
	}
}

// mustUUID converts a nullable UUID column into a uuid.UUID, failing the test on a
// NULL or malformed value instead of returning a zero ID.
func mustUUID(t testing.TB, value pgtype.UUID) uuid.UUID {
	t.Helper()
	id, ok := dbtypes.GoogleUUID(value)
	if !ok {
		t.Fatal("expected UUID value")
	}
	return id
}

// seedFile inserts one active, unencrypted file row and returns its generated ID;
// created_at, mod_time and updated_at all carry updatedAt so ordering assertions
// have a timestamp the test controls.
func seedFile(t testing.TB, db *pgxpool.Pool, userID int64, parentID *uuid.UUID, name, mime string, size int64, updatedAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(context.Background(), `
INSERT INTO files (
  id, user_id, parent_id, name, kind, mime_type, size,
  encryption, status, mod_time, created_at, updated_at
) VALUES ($1,$2,$3,$4,'file',$5,$6,false,'active',$7,$7,$7)`,
		id, userID, parentID, name, mime, size, updatedAt.UTC()); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	return id
}
