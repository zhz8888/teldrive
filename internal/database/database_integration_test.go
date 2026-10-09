//go:build integration

package database_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/zhz8888/teldrive/v2/db/migrations"
	"github.com/zhz8888/teldrive/v2/internal/database"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestMigrateAndOpenAgainstPostgres18(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()

	if err := database.Migrate(ctx, database.Config{URL: db.URL, Schema: "teldrive"}); err != nil {
		t.Fatalf("second migration run should be idempotent: %v", err)
	}

	var version string
	if err := db.Pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	if !strings.HasPrefix(version, "18.") {
		t.Fatalf("expected PostgreSQL 18, got %q", version)
	}

	var appTableCount int
	if err := db.Pool.QueryRow(ctx, `
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = 'teldrive'
  AND table_type = 'BASE TABLE'
  AND table_name IN (
    'users', 'sessions', 'api_keys', 'bots', 'channels',
    'files', 'file_parts', 'upload_sessions', 'upload_parts',
    'file_shares',
    'user_events', 'user_event_stream_state', 'event_stream_tickets'
  )`).Scan(&appTableCount); err != nil {
		t.Fatalf("count migrated application tables: %v", err)
	}
	if appTableCount != 13 {
		t.Fatalf("migrated application table count = %d, want 13", appTableCount)
	}

	var publicTableCount int
	if err := db.Pool.QueryRow(ctx, `
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = 'public'
  AND table_type = 'BASE TABLE'`).Scan(&publicTableCount); err != nil {
		t.Fatalf("count public base tables: %v", err)
	}
	if publicTableCount != 0 {
		t.Fatalf("public base table count = %d, want 0", publicTableCount)
	}

	var migrationVersion int64
	if err := db.Pool.QueryRow(ctx, "SELECT max(version_id) FROM teldrive.migrations WHERE is_applied").Scan(&migrationVersion); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	// The expectation is read from the embedded migration files instead of being
	// written down here: the assertion then fails when a committed migration did
	// not run, which is the defect worth catching, rather than on every migration
	// that is added.
	if want := latestMigrationVersion(t); migrationVersion != want {
		t.Fatalf("migration version = %d, want %d", migrationVersion, want)
	}

	var normalizedNameColumns int
	if err := db.Pool.QueryRow(ctx, `
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = 'teldrive'
  AND table_name IN ('files', 'upload_sessions')
  AND column_name = 'normalized_name'`).Scan(&normalizedNameColumns); err != nil {
		t.Fatalf("count normalized name columns: %v", err)
	}
	if normalizedNameColumns != 0 {
		t.Fatalf("normalized name columns = %d, want 0", normalizedNameColumns)
	}
}

func TestMigrateCustomSchema(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	const schema = "teldrive_custom_test"

	if err := database.Migrate(ctx, database.Config{URL: db.URL, Schema: schema}); err != nil {
		t.Fatalf("migrate custom schema: %v", err)
	}

	for _, table := range []string{"users", "migrations", "river_job", "river_migration"} {
		var exists bool
		if err := db.Pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM information_schema.tables
    WHERE table_schema = $1 AND table_name = $2 AND table_type = 'BASE TABLE'
)`, schema, table).Scan(&exists); err != nil {
			t.Fatalf("inspect %s.%s: %v", schema, table, err)
		}
		if !exists {
			t.Fatalf("expected table %s.%s", schema, table)
		}
	}
}

// latestMigrationVersion returns the highest version in the embedded migration set,
// read from the file names the migrator derives its version ids from. Comparing the
// applied version against it fails when a committed migration did not run, and it
// keeps the assertion correct as migrations are added.
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	var latest int64
	for _, name := range names {
		prefix, _, found := strings.Cut(name, "_")
		if !found {
			continue
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		latest = max(latest, version)
	}
	if latest == 0 {
		t.Fatal("no embedded migration carries a version prefix")
	}
	return latest
}
