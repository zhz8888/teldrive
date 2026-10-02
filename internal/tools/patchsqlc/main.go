// Package main rewrites the sqlc-generated database wrapper so that every
// generated Queries value goes through wrapDBTX, which is what applies the
// configured PostgreSQL schema at runtime. It is a build-time tool that must run
// immediately after `sqlc generate`; `just generate-db` chains the two steps and
// a bare sqlc run regenerates db.go without the patch, silently breaking schema
// rewriting. The generated file is never edited by hand.
package main

import (
	"bytes"
	"fmt"
	"os"
)

// generatedDBPath is the sqlc output patched by this tool, relative to the
// repository root, so the tool must run from there as the justfile does.
const generatedDBPath = "internal/db/sqlcgen/db.go"

// replacement is one literal rewrite applied to the generated file. Both sides
// must occur exactly once in a healthy generated file, which makes an
// unexpected sqlc output fail loudly instead of producing a half-patched file.
type replacement struct {
	from []byte
	to   []byte
}

// replacements rewrites the generated constructors and transaction helpers so
// they return schema-aware wrappers instead of the raw pgx connection.
var replacements = []replacement{
	{from: []byte("return &Queries{db: db}"), to: []byte("return &Queries{db: wrapDBTX(db)}")},
	{from: []byte("\t\tdb: tx,"), to: []byte("\t\tdb: wrapDBTX(tx),")},
}

// main patches generatedDBPath and exits with status 1 after printing the error
// to stderr; it produces no output on success.
func main() {
	if err := patchFile(generatedDBPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// patchFile applies every entry in replacements to the file at path and rewrites
// it only when something changed, so repeated runs are no-ops. It is
// idempotent: a replacement is skipped when its patched form is already present.
// It returns an error if the file cannot be read or written, or if a
// replacement matches neither its source nor its target exactly once, which
// signals unexpected sqlc output rather than a partially applied patch.
func patchFile(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	patched := append([]byte(nil), contents...)
	for _, replacement := range replacements {
		sourceCount := bytes.Count(patched, replacement.from)
		targetCount := bytes.Count(patched, replacement.to)
		switch {
		case sourceCount == 1:
			patched = bytes.Replace(patched, replacement.from, replacement.to, 1)
		case sourceCount == 0 && targetCount == 1:
			// Already patched; keep the tool safe to run repeatedly.
		default:
			return fmt.Errorf(
				"patch %s: expected exactly one generated source or patched target, got source=%d target=%d",
				path,
				sourceCount,
				targetCount,
			)
		}
	}

	if bytes.Equal(contents, patched) {
		return nil
	}
	if err := os.WriteFile(path, patched, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
