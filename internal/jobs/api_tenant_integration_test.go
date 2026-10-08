//go:build integration

package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// TestPerUserJobAccessIsScopedToTheOwner is the tenant-isolation guarantee behind
// every user-facing job endpoint. A job that exists but belongs to somebody else
// has to be indistinguishable from one that does not exist, or the endpoint leaks
// the existence of foreign jobs.
func TestPerUserJobAccessIsScopedToTheOwner(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	id := insertUserJobID(t, runtime, 1001, "available")

	for _, testCase := range []struct {
		name   string
		call   func() error
		absent bool
	}{
		{name: "GetForUser", call: func() error { _, err := runtime.GetForUser(ctx, id, 2002); return err }},
		{name: "GetForUserUnowned", call: func() error { _, err := runtime.GetForUser(ctx, id, 0); return err }},
		{name: "CancelForUser", call: func() error { _, err := runtime.CancelForUser(ctx, id, 2002); return err }},
		{name: "RetryForUser", call: func() error { _, err := runtime.RetryForUser(ctx, id, 2002); return err }},
		{name: "DeleteForUser", call: func() error { return runtime.DeleteForUser(ctx, id, 2002) }},
	} {
		if err := testCase.call(); !errors.Is(err, river.ErrNotFound) {
			t.Fatalf("%s for a foreign user error = %v, want river.ErrNotFound", testCase.name, err)
		}
	}

	// None of the rejected calls may have changed the job.
	stored, err := runtime.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "available" {
		t.Fatalf("job state = %q after rejected calls, want available", stored.State)
	}
}

// TestPerUserJobAccessSucceedsForTheOwner is the other half: the owner really can
// read, cancel and retry their own job, so the guard is not simply refusing
// everything.
func TestPerUserJobAccessSucceedsForTheOwner(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	id := insertUserJobID(t, runtime, 1001, "available")

	job, err := runtime.GetForUser(ctx, id, 1001)
	if err != nil {
		t.Fatalf("GetForUser() error = %v", err)
	}
	if ownerID, ok := job.UserID(); !ok || ownerID != 1001 {
		t.Fatalf("UserID() = %d, %t, want 1001, true", ownerID, ok)
	}

	if _, err := runtime.CancelForUser(ctx, id, 1001); err != nil {
		t.Fatalf("CancelForUser() error = %v", err)
	}
	// A cancelled job is finalized, so River will not retry it; the owner-facing
	// retry path is checked separately on a discarded job below.
	var cancelled int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE id = $1 AND state = 'cancelled'", id).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatal("CancelForUser() did not leave the job cancelled")
	}

	discarded := insertUserJobID(t, runtime, 1001, "discarded")
	retried, err := runtime.RetryForUser(ctx, discarded, 1001)
	if err != nil {
		t.Fatalf("RetryForUser() error = %v", err)
	}
	if retried.State != string(string(rivertype.JobStateAvailable)) {
		t.Fatalf("RetryForUser() state = %q, want available", retried.State)
	}

	removable := insertUserJobID(t, runtime, 1001, "completed")
	if err := runtime.DeleteForUser(ctx, removable, 1001); err != nil {
		t.Fatalf("DeleteForUser() error = %v", err)
	}
	var left int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE id = $1", removable).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatal("DeleteForUser() left the job row behind")
	}
}

// insertUserJobID inserts one job owned by userID and returns its id.
func insertUserJobID(t *testing.T, runtime *Runtime, userID int64, state string) int64 {
	t.Helper()
	insertUserJob(t, runtime, userID, state)
	var id int64
	if err := runtime.pool.QueryRow(context.Background(),
		"SELECT id FROM river_job WHERE args->>'user_id' = $1 ORDER BY id DESC LIMIT 1",
		fmt.Sprintf("%d", userID)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDeleteRejectsAnUnknownJob keeps the administrator delete honest about a
// mistyped id.
func TestDeleteRejectsAnUnknownJob(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	if err := runtime.Delete(context.Background(), 999_999); !errors.Is(err, river.ErrNotFound) {
		t.Fatalf("Delete() error = %v, want river.ErrNotFound", err)
	}
}
