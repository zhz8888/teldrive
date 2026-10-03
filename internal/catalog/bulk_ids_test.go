package catalog_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
)

// TestBulkTrashRefusesEmptyAndNilIdentities keeps a malformed selection from
// being read as "trash everything". Every bulk entry point normalises its id list
// first, and an empty list or a nil id is reported as not found rather than
// silently operating on a different set of files.
func TestBulkTrashRefusesEmptyAndNilIdentities(t *testing.T) {
	t.Parallel()
	// The service is never given a pool: the selection is rejected before any
	// query runs, which is exactly what is being asserted.
	service := catalog.NewService(nil, nil)

	for _, ids := range [][]uuid.UUID{
		nil,
		{},
		{uuid.Nil},
		{uuid.New(), uuid.Nil},
	} {
		if _, err := service.BulkTrash(t.Context(), 1001, ids); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("BulkTrash(%v) error = %v, want ErrNotFound", ids, err)
		}
	}
}

// TestBulkTrashRefusesAMissingUser keeps an unscoped request from reaching the
// database, so a malformed caller cannot act on somebody's files.
func TestBulkTrashRefusesAMissingUser(t *testing.T) {
	t.Parallel()
	service := catalog.NewService(nil, nil)
	for _, userID := range []int64{0, -1} {
		if _, err := service.BulkTrash(t.Context(), userID, []uuid.UUID{uuid.New()}); err == nil {
			t.Fatalf("BulkTrash(user %d) error = nil, want a rejection", userID)
		}
	}
}
