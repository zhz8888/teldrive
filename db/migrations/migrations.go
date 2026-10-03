// Package migrations embeds the versioned TelDrive SQL migrations and applies
// them through goose.
//
// The migration files are written against a placeholder schema so the same
// scripts can target any configured PostgreSQL schema. Up rewrites the
// placeholder before handing the files to goose, which keeps the SQL readable
// while still producing fully qualified object names at run time.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// schemaTemplateMarker is the placeholder that every embedded migration uses in
// place of a concrete schema name. Up replaces each occurrence with the
// sanitised, quoted schema followed by a dot. The marker must be preserved in
// the SQL files: the runtime rewrite matches it literally.
const schemaTemplateMarker = "/* TEMPLATE: schema */"

// schemaNamePattern validates a configured schema name before it is interpolated
// into SQL. It accepts lower-case unquoted identifiers only: every rendered
// migration qualifies the schema with a quoted identifier, while goose's version
// table is qualified without quotes, and PostgreSQL folds an unquoted mixed-case
// name to lower case. A name that needs quoting would therefore put the version
// table and the migrated objects in different schemas, so such a name is rejected
// at startup instead.
var schemaNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Files contains every versioned TelDrive SQL migration.
//
//go:embed *.sql
var Files embed.FS

// Up renders the configured schema into every migration and applies all pending
// versions with explicit object qualification.
func Up(ctx context.Context, db *sql.DB, schema string) error {
	rendered, err := renderedFiles(schema)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		rendered,
		// The version table is qualified without quotes, which is safe because
		// schemaNamePattern accepts only the lower-case form PostgreSQL keeps
		// unchanged; quoting it here would make goose's own existence check look
		// for a different name and try to create the table twice.
		goose.WithTableName(schema+".migrations"),
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// renderedFiles returns the embedded migrations with every schema placeholder
// replaced by a reference to schema, ready to hand to goose.
//
// The schema name is validated before use and any leading/trailing space is
// trimmed first. Each file is rewritten to use "<schema>." qualification, and
// the version table is placed in the same schema by the caller. A missing or
// unreadable embedded file is reported rather than skipped, so a broken build
// cannot silently apply a partial migration set.
func renderedFiles(schema string) (fs.FS, error) {
	schema = strings.TrimSpace(schema)
	if !schemaNamePattern.MatchString(schema) {
		return nil, fmt.Errorf("invalid database schema %q", schema)
	}
	prefix := pgx.Identifier{schema}.Sanitize() + "."
	result := fstest.MapFS{}
	entries, err := fs.ReadDir(Files, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := Files.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		result[entry.Name()] = &fstest.MapFile{
			Data: []byte(strings.ReplaceAll(string(data), schemaTemplateMarker, prefix)),
		}
	}
	return result, nil
}
