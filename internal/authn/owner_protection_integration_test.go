//go:build integration

package authn

import (
	"bytes"
	"context"
	"errors"
	"testing"

	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

// seedUsersWithRoles stores one account per role so the admin guards can be driven
// against every branch.
func seedUsersWithRoles(t *testing.T, db *testpostgres.Database) {
	t.Helper()
	ctx := context.Background()
	for _, account := range []struct {
		id   int64
		role string
	}{
		{id: 1001, role: "owner"},
		{id: 1002, role: "admin"},
		{id: 1003, role: "user"},
	} {
		if _, err := db.Pool.Exec(ctx,
			"INSERT INTO users (user_id, display_name, username, role) VALUES ($1, $2, $3, $4)",
			account.id, "Account", account.role, account.role); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSetUserDisabledProtectsTheOwnerAndReportsUnknownAccounts covers the account
// suspension switch. The owner cannot be suspended, because doing so would leave
// nobody able to undo it, and an unknown id is reported rather than silently
// creating a user.
func TestSetUserDisabledProtectsTheOwnerAndReportsUnknownAccounts(t *testing.T) {
	db := testpostgres.New(t)
	seedUsersWithRoles(t, db)
	service := newAdminService(t, db)
	ctx := context.Background()

	if _, err := service.SetUserDisabled(ctx, 1001, true); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("SetUserDisabled(owner) error = %v, want ErrOwnerProtected", err)
	}
	if _, err := service.SetUserDisabled(ctx, 999_999, true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SetUserDisabled(unknown) error = %v, want ErrUserNotFound", err)
	}
	for _, userID := range []int64{0, -1} {
		if _, err := service.SetUserDisabled(ctx, userID, true); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("SetUserDisabled(%d) error = %v, want ErrInvalidInput", userID, err)
		}
	}

	suspended, err := service.SetUserDisabled(ctx, 1003, true)
	if err != nil {
		t.Fatalf("SetUserDisabled(user) error = %v", err)
	}
	if !suspended.DisabledAt.Valid {
		t.Fatal("SetUserDisabled() left the account enabled")
	}

	restored, err := service.SetUserDisabled(ctx, 1003, false)
	if err != nil {
		t.Fatalf("SetUserDisabled() to re-enable error = %v", err)
	}
	if restored.DisabledAt.Valid {
		t.Fatal("SetUserDisabled(false) left the account disabled")
	}
}

// TestRevokeUserAccessProtectsTheOwnerAndEndsEveryCredential is the "sign this
// account out everywhere" action. It has to drop both the sessions and the API
// keys, or a suspended account keeps access through a key it issued earlier.
func TestRevokeUserAccessProtectsTheOwnerAndEndsEveryCredential(t *testing.T) {
	db := testpostgres.New(t)
	seedUsersWithRoles(t, db)
	service := newAdminService(t, db)
	ctx := context.Background()

	if err := service.RevokeUserAccess(ctx, 1001); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("RevokeUserAccess(owner) error = %v, want ErrOwnerProtected", err)
	}
	if err := service.RevokeUserAccess(ctx, 999_999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("RevokeUserAccess(unknown) error = %v, want ErrUserNotFound", err)
	}
	for _, userID := range []int64{0, -1} {
		if err := service.RevokeUserAccess(ctx, userID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("RevokeUserAccess(%d) error = %v, want ErrInvalidInput", userID, err)
		}
	}

	if _, err := service.CreateAPIKey(ctx, 1003, "revoke-me", nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if err := service.RevokeUserAccess(ctx, 1003); err != nil {
		t.Fatalf("RevokeUserAccess() error = %v", err)
	}

	var activeKeys int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM api_keys WHERE user_id = 1003 AND revoked_at IS NULL").Scan(&activeKeys); err != nil {
		t.Fatal(err)
	}
	if activeKeys != 0 {
		t.Fatalf("api keys still active after RevokeUserAccess() = %d, want 0", activeKeys)
	}
	// The row stays so the key's history is auditable; only its state changed.
	var retained int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM api_keys WHERE user_id = 1003").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("api key rows = %d, want the revoked row kept for audit", retained)
	}
}

// TestListSessionsRejectsAMissingUser covers the only part of the listing this
// test can prove: a request without a user must be refused before it reaches the
// database. The page-size clamping is exercised in
// TestListSessionsClampsThePageSizeAgainstRealRows pins the two limits the listing
// applies. It needs more rows than the clamp allows: with a handful of sessions a
// limit of 5000 and a limit of 200 return the same page, which would make the clamp
// invisible to this test.
func TestListSessionsClampsThePageSizeAgainstRealRows(t *testing.T) {
	db := testpostgres.New(t)
	seedUsersWithRoles(t, db)
	service := newAdminService(t, db)
	ctx := context.Background()
	const rows = 205
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO sessions (user_id, telegram_session, refresh_token_hash, expires_at, created_at)
SELECT 1003, $1, decode(md5(g::text), 'hex'), now() + interval '1 day', now() - make_interval(secs => g)
FROM generate_series(1, $2) AS g`,
		bytes.Repeat([]byte{1}, 16), rows); err != nil {
		t.Fatal(err)
	}

	// A limit of 0 falls back to the default rather than returning nothing.
	defaulted, err := service.ListSessions(ctx, ListSessionsInput{UserID: 1003})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(defaulted) != 100 {
		t.Fatalf("default page = %d sessions, want 100", len(defaulted))
	}

	// An explicit limit is honoured.
	limited, err := service.ListSessions(ctx, ListSessionsInput{UserID: 1003, Limit: 7})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(limited) != 7 {
		t.Fatalf("limited page = %d sessions, want 7", len(limited))
	}

	// A limit past the documented maximum is clamped to it rather than rejected,
	// so a client asking for everything still gets a page instead of an error.
	clamped, err := service.ListSessions(ctx, ListSessionsInput{UserID: 1003, Limit: 5000})
	if err != nil {
		t.Fatalf("ListSessions() with an oversized limit error = %v", err)
	}
	if len(clamped) != 200 {
		t.Fatalf("oversized-limit page = %d sessions, want the clamp at 200", len(clamped))
	}
	if len(clamped) == rows {
		t.Fatalf("oversized limit returned every row, want the clamp to apply")
	}
}

// TestUpdateUserRoleRejectsUnknownRolesAndKeepsTheOwner guards role assignment: a
// role outside the known set must never reach the column, because every permission
// check in the product reads it.
func TestUpdateUserRoleRejectsUnknownRolesAndKeepsTheOwner(t *testing.T) {
	db := testpostgres.New(t)
	seedUsersWithRoles(t, db)
	service := newAdminService(t, db)
	ctx := context.Background()

	if _, err := service.UpdateUserRole(ctx, 1003, "wizard"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("UpdateUserRole(unknown role) error = %v, want ErrInvalidInput", err)
	}
	if _, err := service.UpdateUserRole(ctx, 999_999, "admin"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UpdateUserRole(unknown user) error = %v, want ErrUserNotFound", err)
	}

	// The owner cannot be demoted: a deployment with no admin could never be
	// repaired, and nothing here grants the owner role back.
	if _, err := service.UpdateUserRole(ctx, 1001, "user"); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("UpdateUserRole(owner) error = %v, want ErrOwnerProtected", err)
	}

	promoted, err := service.UpdateUserRole(ctx, 1003, "admin")
	if err != nil {
		t.Fatalf("UpdateUserRole() error = %v", err)
	}
	if string(promoted.Role) != "admin" {
		t.Fatalf("role = %q, want admin", promoted.Role)
	}
	// The change has to take effect on the next authentication, because roles are
	// resolved from the database rather than trusted from the token.
	roles, err := service.rolesForUser(ctx, service.queries, 1003)
	if err != nil {
		t.Fatalf("rolesForUser() error = %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %v, want user and admin", roles)
	}
}
