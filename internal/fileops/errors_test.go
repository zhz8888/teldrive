package fileops

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zhz8888/teldrive/v2/internal/catalog"
)

// TestClassifyWriteErrorTurnsOnlyCollisionsIntoConflicts decides what a caller sees
// when a copy or a move races another writer. A unique-constraint violation means
// "somebody else claimed this name first", which the caller can retry or report as
// a conflict. Every other database error must keep its own identity: an outage
// reported as a conflict would tell the user their folder is taken when it is not.
func TestClassifyWriteErrorTurnsOnlyCollisionsIntoConflicts(t *testing.T) {
	t.Parallel()
	collision := &pgconn.PgError{Code: "23505", ConstraintName: "files_unique_name_in_parent"}
	if got := classifyWriteError("copy file", collision); !errors.Is(got, catalog.ErrConflict) {
		t.Fatalf("classifyWriteError(unique violation) = %v, want ErrConflict", got)
	}
	// The collision is recognised however far it was wrapped.
	if got := classifyWriteError("copy file", fmt.Errorf("insert row: %w", collision)); !errors.Is(got, catalog.ErrConflict) {
		t.Fatalf("classifyWriteError(wrapped violation) = %v, want ErrConflict", got)
	}

	other := &pgconn.PgError{Code: "23503", ConstraintName: "files_parent_id_fkey"}
	got := classifyWriteError("copy file", other)
	if errors.Is(got, catalog.ErrConflict) {
		t.Fatalf("classifyWriteError(foreign key violation) = %v, want the original cause kept", got)
	}
	if !errors.Is(got, other) {
		t.Fatalf("classifyWriteError() = %v, want it to wrap the original error", got)
	}
	// Any other failure keeps its own identity and names the action, so an
	// operator reading the log knows which write failed.
	cause := errors.New("connection reset")
	got = classifyWriteError("move folder", cause)
	if errors.Is(got, catalog.ErrConflict) {
		t.Fatalf("classifyWriteError(outage) = %v, want it not reported as a conflict", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("classifyWriteError() = %v, want it to wrap the original cause", got)
	}
}
