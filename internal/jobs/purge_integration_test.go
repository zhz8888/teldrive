//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/zhz8888/teldrive/v2/internal/jobs"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestPendingFilePurgeWorkerProcessesDeletionPendingRoots(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	rootID := uuid.New()
	childID := uuid.New()
	secondRootID := uuid.New()
	activeID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id, user_id, parent_id, name, kind, size, status, mod_time, deleted_at)
VALUES
    ($1, 1001, NULL, 'root', 'folder', NULL, 'deletion_pending', now(), now()),
    ($2, 1001, $1, 'child', 'file', 0, 'deletion_pending', now(), now()),
    ($3, 1001, NULL, 'second-root', 'file', 0, 'deletion_pending', now(), now()),
    ($4, 1001, NULL, 'active', 'file', 0, 'active', now(), NULL)
`, rootID, childID, secondRootID, activeID); err != nil {
		t.Fatal(err)
	}

	service := &recordingPurgeService{after: func(ctx context.Context, userID int64, fileID uuid.UUID) error {
		if _, err := db.Pool.Exec(ctx, "DELETE FROM files WHERE user_id = $1 AND parent_id = $2", userID, fileID); err != nil {
			return err
		}
		_, err := db.Pool.Exec(ctx, "DELETE FROM files WHERE user_id = $1 AND id = $2", userID, fileID)
		return err
	}}
	worker := jobs.NewPendingFilePurgeWorker(db.Pool, service)
	job := &river.Job[jobs.PurgeSweepArgs]{Args: jobs.PurgeSweepArgs{}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	calls := service.callsSnapshot()
	if len(calls) != 2 || calls[0].userID != 1001 || calls[1].userID != 1001 {
		t.Fatalf("purge calls = %#v", calls)
	}
	calledIDs := map[uuid.UUID]bool{calls[0].fileID: true, calls[1].fileID: true}
	if !calledIDs[rootID] || !calledIDs[secondRootID] {
		t.Fatalf("purge calls = %#v", calls)
	}
	if batches := service.batchesSnapshot(); len(batches) != 1 || batches[0] != 2 {
		t.Fatalf("purge batches = %v, want [2]", batches)
	}

	if got := (jobs.PurgeSweepArgs{}).Kind(); got != jobs.PurgeSweepKind {
		t.Fatalf("Kind() = %q", got)
	}
	opts := (jobs.PurgeSweepArgs{}).InsertOpts()
	if opts.Queue != jobs.PurgeQueue || opts.MaxAttempts != 3 || opts.Priority != 1 {
		t.Fatalf("InsertOpts() = %#v", opts)
	}
	if got := worker.Timeout(job); got != 2*time.Hour {
		t.Fatalf("Timeout() = %s", got)
	}

	if err := (*jobs.PendingFilePurgeWorker)(nil).Work(ctx, job); !errors.Is(err, jobs.ErrPurgeNotConfigured) {
		t.Fatalf("nil worker error = %v", err)
	}
	if err := jobs.NewPendingFilePurgeWorker(db.Pool, nil).Work(ctx, job); !errors.Is(err, jobs.ErrPurgeNotConfigured) {
		t.Fatalf("nil service error = %v", err)
	}
}

func TestPendingFilePurgeWorkerDrainsMultiplePages(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (user_id, name, kind, size, status, mod_time, deleted_at)
SELECT 1001, 'pending-' || value, 'file', 0, 'deletion_pending', now(), now()
FROM generate_series(1, 1001) AS value
`); err != nil {
		t.Fatal(err)
	}

	service := &recordingPurgeService{after: func(ctx context.Context, userID int64, fileID uuid.UUID) error {
		_, err := db.Pool.Exec(ctx, "DELETE FROM files WHERE user_id = $1 AND id = $2", userID, fileID)
		return err
	}}
	worker := jobs.NewPendingFilePurgeWorker(db.Pool, service)
	if err := worker.Work(ctx, &river.Job[jobs.PurgeSweepArgs]{Args: jobs.PurgeSweepArgs{}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if calls := service.callsSnapshot(); len(calls) != 1001 {
		t.Fatalf("purge calls = %d, want 1001", len(calls))
	}
	if batches := service.batchesSnapshot(); len(batches) != 2 || batches[0] != 1000 || batches[1] != 1 {
		t.Fatalf("purge batches = %v, want [1000 1]", batches)
	}
	var remaining int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM files WHERE status = 'deletion_pending'").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("deletion-pending files = %d, want 0", remaining)
	}
}

func TestPendingFilePurgeWorkerReturnsServiceFailure(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	fileID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id, user_id, name, kind, size, status, mod_time, deleted_at)
VALUES ($1, 1001, 'pending', 'file', 0, 'deletion_pending', now(), now())
`, fileID); err != nil {
		t.Fatal(err)
	}
	serviceErr := errors.New("purge failed")
	worker := jobs.NewPendingFilePurgeWorker(db.Pool, &recordingPurgeService{err: serviceErr})
	if err := worker.Work(ctx, &river.Job[jobs.PurgeSweepArgs]{Args: jobs.PurgeSweepArgs{}}); !errors.Is(err, serviceErr) {
		t.Fatalf("Work() error = %v", err)
	}
}

// purgeCall is one Purge invocation, kept so a test can tell which root each call
// resolved to.
type purgeCall struct {
	// userID is the owner the purge was attributed to.
	userID int64
	// fileID is the root the worker asked to purge.
	fileID uuid.UUID
}

// recordingPurgeService is a PurgeService that logs the calls it receives and the
// size of every batch, can be made to fail, and can run a hook per call so a test
// can delete the row the worker is about to purge.
type recordingPurgeService struct {
	// mu guards every field below, because the worker may call from a goroutine.
	mu sync.Mutex
	// calls records each single-file purge in order.
	calls []purgeCall
	// batches records the file count of every PurgeMany call, which is how the
	// page size the worker chose becomes visible.
	batches []int
	// err, when set, is returned by Purge without running after.
	err error
	// after runs once a call has been recorded, so the test can delete the file and
	// make the sweep fetch another page.
	after func(context.Context, int64, uuid.UUID) error
}

// PurgeMany records the batch size and then purges the files one by one, stopping
// at the first failure.
func (s *recordingPurgeService) PurgeMany(ctx context.Context, userID int64, fileIDs []uuid.UUID) error {
	s.mu.Lock()
	s.batches = append(s.batches, len(fileIDs))
	s.mu.Unlock()
	for _, fileID := range fileIDs {
		if err := s.Purge(ctx, userID, fileID); err != nil {
			return err
		}
	}
	return nil
}

// Purge records the call, then runs after unless the service is configured to
// fail; the failure wins over the hook.
func (s *recordingPurgeService) Purge(ctx context.Context, userID int64, fileID uuid.UUID) error {
	s.mu.Lock()
	s.calls = append(s.calls, purgeCall{userID: userID, fileID: fileID})
	err, after := s.err, s.after
	s.mu.Unlock()
	if err != nil || after == nil {
		return err
	}
	return after(ctx, userID, fileID)
}

// callsSnapshot returns a copy of the recorded calls.
func (s *recordingPurgeService) callsSnapshot() []purgeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]purgeCall(nil), s.calls...)
}

// batchesSnapshot returns a copy of the recorded batch sizes.
func (s *recordingPurgeService) batchesSnapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.batches...)
}
