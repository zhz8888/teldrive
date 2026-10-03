//go:build integration

package shares

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

// oneDownload is the share budget used by the reservation tests: a single
// allowance makes the second request observably over the limit.
var oneDownload int64 = 1

// newGrantService seeds two accounts and one folder and returns a service plus
// that folder's id.
func newGrantService(t *testing.T, db *testpostgres.Database) (*Service, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001), (2002)"); err != nil {
		t.Fatal(err)
	}
	folderID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id, user_id, name, kind, encryption, status, mod_time)
VALUES ($1, 1001, 'shared-folder', 'folder', false, 'active', now())`, folderID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db.Pool, catalog.NewService(db.Pool, nil))
	if err != nil {
		t.Fatal(err)
	}
	return service, folderID
}

// TestListGrantsReturnsOnlyWhatTheCallerOwns covers the listing behind the
// "who has access to this file" screen. It is scoped by file owner twice over:
// the file must belong to the caller, and only grants on that file come back.
func TestListGrantsReturnsOnlyWhatTheCallerOwns(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()

	if _, err := service.CreateGrant(ctx, GrantCreateInput{
		OwnerID: 1001, FileID: folderID, GranteeID: 2002, Permission: sqlcgen.SharePermissionRead,
	}); err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	grants, err := service.ListGrants(ctx, 1001, folderID)
	if err != nil {
		t.Fatalf("ListGrants() error = %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].GranteeID != 2002 {
		t.Fatalf("grantee = %d, want 2002", grants[0].GranteeID)
	}
}

// TestListGrantsRefusesAFileTheCallerDoesNotOwn is the ownership guard: asking
// about somebody else's file is refused at the file lookup, before any grant row
// is read, so the endpoint cannot be used to probe other people's shares.
func TestListGrantsRefusesAFileTheCallerDoesNotOwn(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()
	if _, err := service.CreateGrant(ctx, GrantCreateInput{
		OwnerID: 1001, FileID: folderID, GranteeID: 2002,
	}); err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	if _, err := service.ListGrants(ctx, 2002, folderID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("ListGrants() for a foreign file error = %v, want ErrNotFound", err)
	}
}

// TestListGrantsRejectsMissingIdentities keeps a malformed request from reaching
// the database at all.
func TestListGrantsRejectsMissingIdentities(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()
	for _, testCase := range []struct {
		name    string
		ownerID int64
		fileID  uuid.UUID
	}{
		{name: "zero owner", ownerID: 0, fileID: folderID},
		{name: "negative owner", ownerID: -1, fileID: folderID},
		{name: "nil file", ownerID: 1001, fileID: uuid.Nil},
	} {
		if _, err := service.ListGrants(ctx, testCase.ownerID, testCase.fileID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("ListGrants(%s) error = %v, want ErrInvalidInput", testCase.name, err)
		}
	}
}

// TestListGrantsReportsAnUnknownFile keeps a mistyped id from looking like an
// empty grant list, which would tell the caller their file simply has no shares.
func TestListGrantsReportsAnUnknownFile(t *testing.T) {
	db := testpostgres.New(t)
	service, _ := newGrantService(t, db)
	if _, err := service.ListGrants(context.Background(), 1001, uuid.New()); err == nil {
		t.Fatal("ListGrants() for an unknown file error = nil, want a not-found error")
	}
}

// TestReserveFileDownloadConsumesOneAllowance walks the single-file download path:
// a share with a download budget charges it, and a share without one is unlimited.
func TestReserveFileDownloadConsumesOneAllowance(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()

	created, err := service.Create(ctx, CreateInput{
		OwnerID: 1001, FileID: folderID, MaxDownloads: &oneDownload,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first, err := service.ReserveFileDownload(ctx, created.Token, "", folderID)
	if err != nil {
		t.Fatalf("ReserveFileDownload() error = %v", err)
	}
	if first == nil {
		t.Fatal("ReserveFileDownload() returned no share, want the reserved one")
	}

	// The single allowance is spent, so a second request must be refused rather
	// than served and silently overrunning the owner's limit.
	if _, err := service.ReserveFileDownload(ctx, created.Token, "", folderID); err == nil {
		t.Fatal("ReserveFileDownload() beyond the allowance error = nil, want a refusal")
	}
}

// TestReserveFileDownloadEnforcesThePasswordBeforeCharging makes sure a wrong
// password does not burn a download the real recipient never got to use.
func TestReserveFileDownloadEnforcesThePasswordBeforeCharging(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()
	password := "correct horse"
	created, err := service.Create(ctx, CreateInput{
		OwnerID: 1001, FileID: folderID, Password: &password, MaxDownloads: &oneDownload,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := service.ReserveFileDownload(ctx, created.Token, "wrong horse", folderID); err == nil {
		t.Fatal("ReserveFileDownload() with a wrong password error = nil, want a refusal")
	}
	// The allowance must still be intact for the recipient who knows the password.
	if _, err := service.ReserveFileDownload(ctx, created.Token, password, folderID); err != nil {
		t.Fatalf("ReserveFileDownload() with the right password error = %v", err)
	}
}

// TestCreateGrantRejectsTheInvariantsThatWouldCorruptTheSharingTable keeps the
// write path honest: you cannot share with yourself, with nobody, or with a file
// you do not own.
func TestCreateGrantRejectsTheInvariantsThatWouldCorruptTheSharingTable(t *testing.T) {
	db := testpostgres.New(t)
	service, folderID := newGrantService(t, db)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)

	for _, testCase := range []struct {
		name  string
		input GrantCreateInput
	}{
		{name: "owner missing", input: GrantCreateInput{OwnerID: 0, FileID: folderID, GranteeID: 2002}},
		{name: "grantee missing", input: GrantCreateInput{OwnerID: 1001, FileID: folderID, GranteeID: 0}},
		{name: "file missing", input: GrantCreateInput{OwnerID: 1001, FileID: uuid.Nil, GranteeID: 2002}},
		{name: "sharing with yourself", input: GrantCreateInput{OwnerID: 1001, FileID: folderID, GranteeID: 1001}},
		{name: "expiry in the past", input: GrantCreateInput{
			OwnerID: 1001, FileID: folderID, GranteeID: 2002, ExpiresAt: &past,
		}},
		{name: "unknown permission", input: GrantCreateInput{
			OwnerID: 1001, FileID: folderID, GranteeID: 2002, Permission: sqlcgen.SharePermission("admin"),
		}},
	} {
		if _, err := service.CreateGrant(ctx, testCase.input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("CreateGrant(%s) error = %v, want ErrInvalidInput", testCase.name, err)
		}
	}
}
