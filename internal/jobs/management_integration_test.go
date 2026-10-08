//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// newManagementRuntime builds a Runtime over the test database; the management
// tests only read the River tables, so the storage never moves a byte.
func newManagementRuntime(t *testing.T, db *testpostgres.Database) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(db.Pool, defaultsStorage{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return runtime
}

// TestPurgeRejectsEveryStateThatIsNotFinalized guards the destructive path: only a
// state a job can never leave again may be purged, and the check has to run
// before any SQL so a mistyped state cannot delete a job that still has work to do.
func TestPurgeRejectsEveryStateThatIsNotFinalized(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()

	for _, state := range []string{"available", "running", "retryable", "scheduled", "", "COMPLETED", "Completed"} {
		if _, err := runtime.Purge(ctx, state); !errors.Is(err, ErrInvalidJobState) {
			t.Fatalf("Purge(%q) error = %v, want ErrInvalidJobState", state, err)
		}
		if _, err := runtime.PurgeForUser(ctx, 1001, state); !errors.Is(err, ErrInvalidJobState) {
			t.Fatalf("PurgeForUser(%q) error = %v, want ErrInvalidJobState", state, err)
		}
	}
}

// TestPurgeForUserRejectsANonPositiveUserID keeps a malformed request from
// deleting every user's history at once. It needs a real pool: the pool guard runs
// before the user check, so a runtime without one would report the missing pool for
// every input and prove nothing about the guard.
func TestPurgeForUserRejectsANonPositiveUserID(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	insertUserJob(t, runtime, 2002, "completed")

	for _, userID := range []int64{0, -1} {
		if _, err := runtime.PurgeForUser(ctx, userID, "completed"); !errors.Is(err, ErrRuntimeNotConfigured) {
			t.Fatalf("PurgeForUser(%d) error = %v, want ErrRuntimeNotConfigured", userID, err)
		}
	}
	// Nothing may have been deleted by the rejected calls.
	var left int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE args->>'user_id' = '2002'").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("remaining jobs = %d, want the other user's history untouched", left)
	}
}

// TestPurgeDeletesOnlyTheRequestedFinalizedState checks both halves of the
// guarantee: a finalized job goes, and a job that still has to run stays.
func TestPurgeDeletesOnlyTheRequestedFinalizedState(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	for _, state := range []string{"available", "retryable", "discarded", "completed", "cancelled", "scheduled"} {
		insertManagementJob(t, runtime, state)
	}

	purged, err := runtime.Purge(ctx, "discarded")
	if err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged %d jobs, want 1", purged)
	}

	var remaining []string
	rows, err := db.Pool.Query(ctx, "SELECT state::text FROM river_job ORDER BY state::text")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, state)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	// Everything except the discarded job must survive, including the ones that
	// are still going to run.
	for _, state := range []string{"available", "retryable", "completed", "cancelled", "scheduled"} {
		if !containsState(remaining, state) {
			t.Fatalf("state %s was purged, remaining = %v", state, remaining)
		}
	}
	if containsState(remaining, "discarded") {
		t.Fatalf("discarded job survived, remaining = %v", remaining)
	}
}

// TestPurgeForUserOnlyTouchesThatUsersJobs is the privacy guarantee behind the
// per-user purge: clearing your own history must not touch anybody else's.
func TestPurgeForUserOnlyTouchesThatUsersJobs(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	insertUserJob(t, runtime, 1001, "completed")
	insertUserJob(t, runtime, 1001, "completed")
	insertUserJob(t, runtime, 2002, "completed")

	purged, err := runtime.PurgeForUser(ctx, 1001, "completed")
	if err != nil {
		t.Fatalf("PurgeForUser() error = %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged %d jobs, want 2", purged)
	}
	var remaining int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE args->>'user_id' = $1", "2002").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("other user's jobs = %d, want 1", remaining)
	}
}

// TestListQueuesCountsJobsAndReportsPausedState covers the administrator listing:
// queues come back sorted with River's paused flag and the jobs River can count.
func TestListQueuesCountsJobsAndReportsPausedState(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	registerRiverQueue(t, runtime, CleanupQueue)
	insertManagementJob(t, runtime, "available")
	insertManagementJob(t, runtime, "available")
	insertManagementJob(t, runtime, "retryable")

	queues, err := runtime.ListQueues(ctx)
	if err != nil {
		t.Fatalf("ListQueues() error = %v", err)
	}
	if len(queues) == 0 {
		t.Fatal("ListQueues() returned no queues, want the one holding the inserted job")
	}
	for index := 1; index < len(queues); index++ {
		if queues[index-1].Name > queues[index].Name {
			t.Fatalf("queues are not sorted by name: %v", queues)
		}
	}
	// River's per-queue counter reports available and running only, so the
	// retryable job is deliberately left out of the total.
	var available, running int64
	for _, queue := range queues {
		available += queue.Available
		running += queue.Running
	}
	if available != 2 || running != 0 {
		t.Fatalf("counted %d available and %d running, want 2 and 0", available, running)
	}
}

// TestListQueuesForUserCountsEveryStateAndMergesThePausedFlag covers the listing a
// user sees: only their own queues, all four states counted, and the global paused
// flag folded in so a paused queue explains why nothing is moving.
func TestListQueuesForUserCountsEveryStateAndMergesThePausedFlag(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	registerRiverQueue(t, runtime, CleanupQueue)
	insertUserJob(t, runtime, 1001, "available")
	insertUserJob(t, runtime, 1001, "retryable")
	insertUserJob(t, runtime, 1001, "scheduled")
	insertUserJob(t, runtime, 2002, "completed")

	queues, err := runtime.ListQueuesForUser(ctx, 1001)
	if err != nil {
		t.Fatalf("ListQueuesForUser() error = %v", err)
	}
	if len(queues) != 1 {
		t.Fatalf("queues = %v, want only the queue holding user 1001's jobs", queues)
	}
	got := queues[0]
	if got.Available != 1 || got.Retryable != 1 || got.Scheduled != 1 {
		t.Fatalf("counts = %+v, want one available, one retryable and one scheduled", got)
	}
	// The other user's job must be invisible here.
	if got.Available+got.Retryable+got.Scheduled+got.Running == 0 {
		t.Fatalf("counts = %+v, want the user's own jobs", got)
	}
}

// TestPauseAndResumeQueue flips River's queue flag and is idempotent, which is
// what lets an operator stop a runaway queue and let it drain again.
func TestPauseAndResumeQueue(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	registerRiverQueue(t, runtime, CleanupQueue)
	insertManagementJob(t, runtime, "available")

	if err := runtime.PauseQueue(ctx, CleanupQueue); err != nil {
		t.Fatalf("PauseQueue() error = %v", err)
	}
	if !queueIsPaused(t, runtime, ctx, CleanupQueue) {
		t.Fatal("queue is not paused after PauseQueue()")
	}
	// Pausing twice is harmless.
	if err := runtime.PauseQueue(ctx, CleanupQueue); err != nil {
		t.Fatalf("second PauseQueue() error = %v", err)
	}
	if err := runtime.ResumeQueue(ctx, CleanupQueue); err != nil {
		t.Fatalf("ResumeQueue() error = %v", err)
	}
	if queueIsPaused(t, runtime, ctx, CleanupQueue) {
		t.Fatal("queue is still paused after ResumeQueue()")
	}
}

// TestPauseAndResumePeriodicJob walks a schedule through its whole life: create
// it, suspend it, bring it back and finally delete it.
func TestPauseAndResumePeriodicJob(t *testing.T) {
	db := testpostgres.New(t)
	runtime := newManagementRuntime(t, db)
	ctx := context.Background()
	const id = "upload-sweep"
	created, err := runtime.CreatePeriodicJob(ctx, PeriodicJobInput{
		ID:       id,
		Kind:     UploadCleanupSweepKind,
		Args:     map[string]json.RawMessage{"user_id": json.RawMessage(`1001`)},
		Schedule: PeriodicSchedule{CronExpression: "@every 24h"},
	})
	if err != nil {
		t.Fatalf("CreatePeriodicJob() error = %v", err)
	}
	if created.Paused {
		t.Fatal("new periodic job is paused, want it active")
	}

	paused, err := runtime.PausePeriodicJob(ctx, id)
	if err != nil {
		t.Fatalf("PausePeriodicJob() error = %v", err)
	}
	if !paused.Paused {
		t.Fatal("PausePeriodicJob() returned an active job, want it paused")
	}
	// Pausing an already paused definition is harmless.
	if _, err := runtime.PausePeriodicJob(ctx, id); err != nil {
		t.Fatalf("second PausePeriodicJob() error = %v", err)
	}

	resumed, err := runtime.ResumePeriodicJob(ctx, id)
	if err != nil {
		t.Fatalf("ResumePeriodicJob() error = %v", err)
	}
	if resumed.Paused {
		t.Fatal("ResumePeriodicJob() returned a paused job, want it active")
	}

	if err := runtime.DeletePeriodicJob(ctx, id); err != nil {
		t.Fatalf("DeletePeriodicJob() error = %v", err)
	}
	if _, err := runtime.ListPeriodicJobs(ctx); err != nil {
		t.Fatal(err)
	}
	for _, job := range mustListPeriodicJobs(t, runtime, ctx) {
		if job.ID == id {
			t.Fatalf("periodic job %s survived DeletePeriodicJob()", id)
		}
	}
}

// insertManagementJob inserts one job and forces it into a finalized state. River
// only reaches those states by running a worker, which this test deliberately does
// not start, so the state is written directly.
func insertManagementJob(t *testing.T, runtime *Runtime, state string) {
	t.Helper()
	insertManagementJobForUser(t, runtime, state, 0)
}

// insertUserJob attributes the inserted job to userID, which is the argument the
// per-user listing and the per-user purge read.
func insertUserJob(t *testing.T, runtime *Runtime, userID int64, state string) {
	t.Helper()
	insertManagementJobForUser(t, runtime, state, userID)
}

// insertManagementJobForUser inserts one job and forces it into state, storing the
// user_id argument that the per-user listing and the per-user purge read ownership
// from when userID is positive; a zero userID leaves the job global.
func insertManagementJobForUser(t *testing.T, runtime *Runtime, state string, userID int64) {
	t.Helper()
	ctx := context.Background()
	inserted, err := runtime.client.Insert(ctx, UploadCleanupSweepArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if userID == 0 {
		if _, err := dbExecState(ctx, runtime, inserted.Job.ID, state); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := dbExecStateAndUser(ctx, runtime, inserted.Job.ID, state, userID); err != nil {
		t.Fatal(err)
	}
}

// registerRiverQueue writes the queue row River's maintainer would otherwise
// create only after the service has been running. The management API reads that
// table directly, so a listing test has to seed it.
func registerRiverQueue(t *testing.T, runtime *Runtime, name string) {
	t.Helper()
	table := pgx.Identifier{runtime.schema, "river_queue"}.Sanitize()
	if _, err := runtime.pool.Exec(context.Background(), fmt.Sprintf(
		"INSERT INTO %s (name, created_at, updated_at) VALUES ($1, now(), now())", table), name); err != nil {
		t.Fatal(err)
	}
}

// finalizedAt mirrors River's rule that a finalized job carries a completion
// timestamp and a live one carries none. Writing the state alone trips the table's
// check constraint.
func finalizedAt(state string) *time.Time {
	switch state {
	case "completed", "cancelled", "discarded":
		now := time.Now()
		return &now
	default:
		return nil
	}
}

// dbExecState forces a job into state and returns the number of rows touched, so a
// caller can tell a real update from a mistyped ID. It writes finalized_at in the
// same statement because the table's check constraint requires a completion
// timestamp for a finalized state.
func dbExecState(ctx context.Context, runtime *Runtime, id int64, state string) (int64, error) {
	tag, err := runtime.pool.Exec(ctx,
		"UPDATE river_job SET state = $2, finalized_at = $3 WHERE id = $1", id, state, finalizedAt(state))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// dbExecStateAndUser additionally tags the job with a user_id argument, which is
// the only field the per-user listing and the per-user purge treat as ownership.
func dbExecStateAndUser(ctx context.Context, runtime *Runtime, id int64, state string, userID int64) (int64, error) {
	tag, err := runtime.pool.Exec(ctx,
		"UPDATE river_job SET state = $2, finalized_at = $3, args = args || jsonb_build_object('user_id', $4::bigint) WHERE id = $1",
		id, state, finalizedAt(state), fmt.Sprintf("%d", userID))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// queueIsPaused reports the paused flag of one listed queue. It fails the test when
// the queue is not listed at all, so a wrong queue name cannot read as "not paused".
func queueIsPaused(t *testing.T, runtime *Runtime, ctx context.Context, name string) bool {
	t.Helper()
	queues, err := runtime.ListQueues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, queue := range queues {
		if queue.Name == name {
			return queue.Paused
		}
	}
	t.Fatalf("queue %s is not listed, got %v", name, queues)
	return false
}

// mustListPeriodicJobs returns the persisted periodic definitions and fails the
// test on error.
func mustListPeriodicJobs(t *testing.T, runtime *Runtime, ctx context.Context) []PeriodicJob {
	t.Helper()
	definitions, err := runtime.ListPeriodicJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return definitions
}

// containsState reports whether the states read back from river_job contain want, so
// the purge tests can assert that a state survived or was removed.
func containsState(states []string, want string) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}
