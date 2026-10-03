package dblock

import (
	"testing"

	"github.com/google/uuid"
)

// TestDestinationIsStablePerFolder pins the properties the callers rely on: one
// folder always maps to one key, different folders and different users do not
// share a key by construction, and the root is distinct from any folder.
func TestDestinationIsStablePerFolder(t *testing.T) {
	t.Parallel()
	folder := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	other := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	zero := uuid.Nil

	if Destination(7, &folder) != Destination(7, &folder) {
		t.Fatal("the same folder produced two different keys")
	}
	if Destination(7, &folder) == Destination(8, &folder) {
		t.Fatal("two users share one key for the same folder")
	}
	if Destination(7, &folder) == Destination(7, &other) {
		t.Fatal("two folders share one key")
	}
	if Destination(7, nil) == Destination(7, &folder) {
		t.Fatal("the root shares a key with a folder")
	}
	if Destination(7, nil) != Destination(7, &zero) {
		t.Fatal("a nil parent and an all-zero parent must encode alike")
	}
}
