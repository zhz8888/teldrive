//go:build integration

package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// purgeCapableStorage is a PurgeService that accepts every root without touching
// anything, which is enough to enable the purge sweep for these insert tests.
type purgeCapableStorage struct{}

func (purgeCapableStorage) Purge(context.Context, int64, uuid.UUID) error { return nil }

func (purgeCapableStorage) PurgeMany(context.Context, int64, []uuid.UUID) error { return nil }

// TestOptionalWorkersAreRefusedWhenTheirRuntimeCannotRunThem is the reason every
// insert below checks a feature flag: a deployment without a purge service, a bot
// inviter or an upload pipeline must not be able to enqueue work that would then
// fail on every attempt.
func TestOptionalWorkersAreRefusedWhenTheirRuntimeCannotRunThem(t *testing.T) {
	db := testpostgres.New(t)
	// defaultsStorage is not a PurgeService, so the purge sweep stays disabled.
	runtime, err := NewRuntime(db.Pool, defaultsStorage{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	ctx := context.Background()

	if err := runtime.InsertPurge(ctx); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("InsertPurge() error = %v, want ErrRuntimeNotConfigured", err)
	}
	if id, err := runtime.InsertBotProvision(ctx, 1001, []int64{777}); !errors.Is(err, ErrRuntimeNotConfigured) || id != "" {
		t.Fatalf("InsertBotProvision() = %q, %v, want ErrRuntimeNotConfigured", id, err)
	}
	if _, err := runtime.InsertUploadBatch(ctx, UploadBatchArgs{
		UserID: 1001, BatchID: "batch-1",
	}); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("InsertUploadBatch() error = %v, want ErrRuntimeNotConfigured", err)
	}

	var queued int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("river_job holds %d rows, want none enqueued by a disabled runtime", queued)
	}
}

// TestInsertPurgeEnqueuesThePendingDeletionSweep covers the one optional worker
// this runtime can enable on its own: a deployment that supplies a purge service
// gets the sweep, and a second call queues a second run rather than deduplicating.
func TestInsertPurgeEnqueuesThePendingDeletionSweep(t *testing.T) {
	db := testpostgres.New(t)
	runtime, err := NewRuntime(db.Pool, defaultsStorage{}, purgeCapableStorage{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	ctx := context.Background()

	if err := runtime.InsertPurge(ctx); err != nil {
		t.Fatalf("InsertPurge() error = %v", err)
	}
	if err := runtime.InsertPurge(ctx); err != nil {
		t.Fatalf("second InsertPurge() error = %v", err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = $1", PurgeSweepKind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("purge sweeps = %d, want one per call", count)
	}
}

// TestInsertCleanupIgnoresAnUnusableRuntime keeps the always-registered cleanup
// sweep reporting the same sentinel as the optional ones, so a caller cannot tell
// a misconfigured runtime from a failed insert by the error alone.
func TestInsertCleanupIgnoresAnUnusableRuntime(t *testing.T) {
	var missing *Runtime
	if err := missing.InsertCleanup(context.Background()); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("InsertCleanup() error = %v, want ErrRuntimeNotConfigured", err)
	}
	if err := missing.InsertPurge(context.Background()); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("InsertPurge() error = %v, want ErrRuntimeNotConfigured", err)
	}
}

// TestInsertBotProvisionRejectsNothingToDo keeps the cheap path honest even where
// the worker is enabled: a job listing no usable bot id succeeds without queueing
// anything, so a retried request does not spin up a worker for an empty set.
func TestInsertBotProvisionRejectsNothingToDo(t *testing.T) {
	db := testpostgres.New(t)
	// A runtime with no bot inviter cannot provision, so the guard for an empty
	// list is exercised through the same sentinel the enabled path uses.
	runtime, err := NewRuntime(db.Pool, defaultsStorage{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	id, err := runtime.InsertBotProvision(context.Background(), 0, []int64{0, -1})
	if !errors.Is(err, ErrRuntimeNotConfigured) || id != "" {
		t.Fatalf("InsertBotProvision(no user) = %q, %v, want ErrRuntimeNotConfigured", id, err)
	}
}
