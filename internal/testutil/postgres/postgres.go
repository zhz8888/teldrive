//go:build integration

// Package postgres provisions throwaway PostgreSQL databases for integration
// tests. It is compiled only under the integration build tag and expects the
// harness started by scripts/test-postgres.sh (Podman plus the pinned PostgreSQL
// image), which is what exports TEST_DATABASE_URL.
package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/database"
)

// testDatabaseEnv names the environment variable holding the connection URL of
// the harness PostgreSQL instance; it must point at a database whose credentials
// may create and drop other databases.
const testDatabaseEnv = "TEST_DATABASE_URL"

// Database is a test database owned by the caller. It is created by New and
// released by the cleanup that New registers with t, so tests must not close
// Pool or connect to URL after the test has finished.
type Database struct {
	// Pool is connected to the dedicated database and is closed by the cleanup
	// registered with the testing.TB passed to New.
	Pool *pgxpool.Pool
	// URL is the connection string of the dedicated database. It is only valid
	// until the test's cleanup runs, which drops the database.
	URL string
}

// New creates a dedicated PostgreSQL database, applies all embedded migrations,
// and returns a verified pool. The database is force-dropped during cleanup.
func New(t testing.TB) *Database {
	t.Helper()

	baseURL := os.Getenv(testDatabaseEnv)
	if baseURL == "" {
		t.Fatalf("%s is required; run tests through scripts/test-postgres.sh", testDatabaseEnv)
	}

	adminURL, err := withDatabase(baseURL, "postgres")
	if err != nil {
		t.Fatalf("build admin database URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL admin database: %v", err)
	}

	databaseName := "teldrive_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}

	targetURL, err := withDatabase(baseURL, databaseName)
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("build test database URL: %v", err)
	}
	if err := database.Migrate(ctx, database.Config{URL: targetURL}); err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(context.Background())
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := database.Open(ctx, database.Config{
		URL:             targetURL,
		ApplicationName: "teldrive-integration-test",
		MaxConnections:  8,
		ConnectTimeout:  10 * time.Second,
	})
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(context.Background())
		t.Fatalf("open test database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", databaseName, err)
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Errorf("close admin database connection: %v", err)
		}
	})

	return &Database{Pool: pool, URL: targetURL}
}

// withDatabase returns rawURL with its database component replaced by
// databaseName, preserving any credentials and query parameters. It returns an
// error when rawURL cannot be parsed or uses a scheme other than postgres or
// postgresql, and it never modifies its inputs.
func withDatabase(rawURL, databaseName string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("unsupported database URL scheme %q", u.Scheme)
	}
	u.Path = "/" + databaseName
	return u.String(), nil
}
