package channels

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUniqueViolationOnlyMatchesTheConstraintCode decides whether a failed
// channel insert should be read as "somebody else already registered this Telegram
// channel" and retried as an update. Treating any database error that way would
// swallow a real outage and leave the channel unregistered, so the check stays
// narrow even though it unwraps.
func TestIsUniqueViolationOnlyMatchesTheConstraintCode(t *testing.T) {
	t.Parallel()
	duplicate := &pgconn.PgError{Code: "23505", ConstraintName: "channels_channel_id_key"}
	if !isUniqueViolation(duplicate) {
		t.Fatal("isUniqueViolation(unique violation) = false, want true")
	}
	// The same error, wrapped by the layers above, still counts: the caller has
	// to recognise its own collision however far it was wrapped.
	if !isUniqueViolation(fmt.Errorf("register channel: %w", duplicate)) {
		t.Fatal("isUniqueViolation(wrapped unique violation) = false, want true")
	}
	other := &pgconn.PgError{Code: "23503", ConstraintName: "channels_user_id_fkey"}
	if isUniqueViolation(other) {
		t.Fatal("isUniqueViolation(foreign key violation) = true, want false")
	}
	if isUniqueViolation(nil) {
		t.Fatal("isUniqueViolation(nil) = true, want false")
	}
	if isUniqueViolation(errors.New("connection refused")) {
		t.Fatal("isUniqueViolation(plain error) = true, want false")
	}
}
