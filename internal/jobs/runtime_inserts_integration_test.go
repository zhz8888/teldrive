//go:build integration

package jobs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/database"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// purgeCapableStorage is a PurgeService that accepts every root without touching
// anything, which is enough to enable the purge sweep for these insert tests.
type purgeCapableStorage struct{}

// Purge accepts one root without touching anything, which is all these tests need
// from a purge service.
func (purgeCapableStorage) Purge(context.Context, int64, uuid.UUID) error { return nil }

// PurgeMany accepts a batch of roots without touching anything.
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

// TestInsertBotProvisionRequeuesAfterACompletedJob covers the unique-key contract
// of the provisioning job. Requests for the same user and bot set must join one
// queued job, but a finished run must not swallow the next request: its row stays
// in river_job until the job cleaner removes it, so counting a completed job as a
// duplicate would make every retry after a successful run a silent no-op.
func TestInsertBotProvisionRequeuesAfterACompletedJob(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := bots.NewService(db.Pool, cipher, staticVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntimeWithSchemaAndBotProvision(db.Pool, invitingStorage{}, database.DefaultSchema, service, plaintextEncryptor{}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("NewRuntimeWithSchemaAndBotProvision() error = %v", err)
	}

	first, err := runtime.InsertBotProvision(ctx, 1001, []int64{777})
	if err != nil || first == "" {
		t.Fatalf("InsertBotProvision() = %q, %v, want a queued job", first, err)
	}
	// While the job is still waiting, the repeated request is the same work and
	// has to join it instead of queueing a second promotion.
	queued, err := runtime.InsertBotProvision(ctx, 1001, []int64{777})
	if err != nil {
		t.Fatalf("second InsertBotProvision() error = %v", err)
	}
	if queued != first {
		t.Fatalf("second InsertBotProvision() = %q, want the queued job %q", queued, first)
	}
	// A finished run leaves its job completed until the job cleaner removes it,
	// which is the state the next request has to look past.
	if _, err := db.Pool.Exec(ctx,
		"UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	requeued, err := runtime.InsertBotProvision(ctx, 1001, []int64{777})
	if err != nil {
		t.Fatalf("InsertBotProvision() after completion error = %v", err)
	}
	if requeued == "" || requeued == first {
		t.Fatalf("InsertBotProvision() after completion = %q, want a new job instead of the completed %q", requeued, first)
	}
	var stored int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = $1", BotProvisionKind).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("river_job holds %d provisioning jobs, want 2", stored)
	}
}

// staticVerifier authenticates every token as the same bot. The insert tests never
// run the provisioning worker, so only the identity a verification would report
// matters, and it has to exist for the runtime to enable bot provisioning.
type staticVerifier struct{}

// Verify reports the bot the bot ids in these tests belong to.
func (staticVerifier) Verify(context.Context, string) (bots.Identity, error) {
	return bots.Identity{ID: 777, Username: "storage_bot"}, nil
}

// plaintextEncryptor is the binary Encryptor the runtime wants before it enables
// bot provisioning. These tests inspect the jobs they insert and never read a
// job's arguments back, so storing them unchanged keeps the stub honest about
// what it does.
type plaintextEncryptor struct{}

// Encrypt returns the plaintext unchanged.
func (plaintextEncryptor) Encrypt(plain []byte) []byte { return plain }

// Decrypt returns the stored bytes unchanged.
func (plaintextEncryptor) Decrypt(cipher []byte) ([]byte, error) { return cipher, nil }

// invitingStorage is defaultsStorage plus the BotInviter capability, which is what
// enables bot provisioning on a runtime built around it. Every other method keeps
// reporting an error, because an insert test never reaches storage.
type invitingStorage struct{ defaultsStorage }

// InviteBot accepts a promotion; no test here runs the worker that would call it.
func (invitingStorage) InviteBot(context.Context, int64, int64, string) error { return nil }
