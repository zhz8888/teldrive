//go:build integration

package authn

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"

	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// newAdminService returns a service whose token randomness is fixed, so a test that
// only exercises the administrator listings does not have to feed it entropy.
func newAdminService(t *testing.T, db *testpostgres.Database) *Service {
	t.Helper()
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{5}, 32), bytes.NewReader(bytes.Repeat([]byte{9}, 24*20)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db.Pool, cipher, &fakeTelegramLogin{}, Config{
		SigningKey: "0123456789abcdef0123456789abcdef", Issuer: "test",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour, LoginFlowTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.random = bytes.NewReader(bytes.Repeat([]byte{7}, 32*20))
	return service
}

func seedAccounts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, account := range []struct {
		id       int64
		name     string
		username string
		disabled bool
	}{
		{id: 1001, name: "Ada Lovelace", username: "ada"},
		{id: 1002, name: "Grace Hopper", username: "grace"},
		{id: 1003, name: "Alan Turing", username: "alan"},
		{id: 1004, name: "Retired Account", username: "retired", disabled: true},
	} {
		var disabledAt *time.Time
		if account.disabled {
			now := time.Now()
			disabledAt = &now
		}
		if _, err := pool.Exec(ctx,
			"INSERT INTO users (user_id, display_name, username, disabled_at) VALUES ($1, $2, $3, $4)",
			account.id, account.name, account.username, disabledAt); err != nil {
			t.Fatal(err)
		}
	}
}

// TestListUsersPaginatesEveryAccount checks the administrator listing returns the
// whole directory and that the search term narrows it without failing on a name
// that merely resembles it.
func TestListUsersPaginatesEveryAccount(t *testing.T) {
	db := testpostgres.New(t)
	seedAccounts(t, db.Pool)
	service := newAdminService(t, db)
	ctx := context.Background()

	all, err := service.ListUsers(ctx, "")
	if err != nil {
		t.Fatalf("ListUsers() error = %v", err)
	}
	// Disabled accounts stay in the administrator directory; they are only hidden
	// from share recipients.
	if len(all) != 4 {
		t.Fatalf("ListUsers() returned %d accounts, want 4", len(all))
	}

	matched, err := service.ListUsers(ctx, "  ada  ")
	if err != nil {
		t.Fatalf("ListUsers() with a search error = %v", err)
	}
	if len(matched) != 1 || matched[0].UserID != 1001 {
		t.Fatalf("ListUsers(search=ada) = %d rows, want just Ada", len(matched))
	}

	none, err := service.ListUsers(ctx, "nobody-by-that-name")
	if err != nil {
		t.Fatalf("ListUsers() with an unmatched search error = %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListUsers() with an unmatched search returned %d rows, want 0", len(none))
	}
}

// TestSearchUsersExcludesTheCallerAndDisabledAccounts is the guard behind sharing
// with somebody: you cannot share with yourself, and a disabled account cannot be
// picked as a recipient.
func TestSearchUsersExcludesTheCallerAndDisabledAccounts(t *testing.T) {
	db := testpostgres.New(t)
	seedAccounts(t, db.Pool)
	service := newAdminService(t, db)
	ctx := context.Background()

	// A search that matches everything still has to drop the caller and every
	// disabled account.
	found, err := service.SearchUsers(ctx, 1001, "a")
	if err != nil {
		t.Fatalf("SearchUsers() error = %v", err)
	}
	for _, user := range found {
		if user.UserID == 1001 {
			t.Fatal("SearchUsers() returned the caller, want the caller excluded")
		}
		if user.DisabledAt.Valid {
			t.Fatalf("SearchUsers() returned the disabled account %d", user.UserID)
		}
	}
	if len(found) != 2 {
		t.Fatalf("SearchUsers() returned %d rows, want grace and alan", len(found))
	}

	for _, testCase := range []struct {
		name    string
		actorID int64
		search  string
	}{
		{name: "blank search", actorID: 1001, search: "   "},
		{name: "zero actor", actorID: 0, search: "grace"},
		{name: "negative actor", actorID: -1, search: "grace"},
	} {
		if _, err := service.SearchUsers(ctx, testCase.actorID, testCase.search); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("SearchUsers(%s) error = %v, want ErrInvalidInput", testCase.name, err)
		}
	}
}

// TestRefreshTokenTTLIsReadOnlyConfig pins the accessor the HTTP layer uses to
// advertise how long a refresh token stays usable.
func TestRefreshTokenTTLIsReadOnlyConfig(t *testing.T) {
	t.Parallel()
	var missing *Service
	if got := missing.RefreshTokenTTL(); got != 0 {
		t.Fatalf("RefreshTokenTTL() on a nil service = %s, want 0", got)
	}
	service := &Service{config: Config{RefreshTokenTTL: 90 * time.Minute}}
	if got := service.RefreshTokenTTL(); got != 90*time.Minute {
		t.Fatalf("RefreshTokenTTL() = %s, want 1h30m", got)
	}
}
