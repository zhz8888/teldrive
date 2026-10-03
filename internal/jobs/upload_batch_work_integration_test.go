//go:build integration

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

// seedBatchUser stores the account the batch jobs are attributed to.
func seedBatchUser(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "INSERT INTO users (user_id) VALUES ($1)", userID); err != nil {
		t.Fatal(err)
	}
}

// seedLocalTree writes files into a temporary directory and returns its path plus
// the number of files created.
func seedLocalTree(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("payload for "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func batchJob(ctx context.Context, runtime *Runtime, args UploadBatchArgs) *river.Job[UploadBatchArgs] {
	return &river.Job[UploadBatchArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   args,
	}
}

// TestUploadBatchWorkRefusesAMalformedPayload keeps every rejected shape free of
// side effects: nothing may be enqueued when the batch itself is unusable, because
// a half-enqueued batch would import files under an unusable configuration.
func TestUploadBatchWorkRefusesAMalformedPayload(t *testing.T) {
	db := testpostgres.New(t)
	seedBatchUser(t, db.Pool, 1001)
	runtime := newManagementRuntime(t, db)
	root := seedLocalTree(t, "a.txt")
	worker := NewUploadBatchWorker(nil, catalog.NewService(db.Pool, nil), []string{root})
	ctx := rivertest.WorkContext(context.Background(), runtime.client.Client)

	for _, testCase := range []struct {
		name string
		args UploadBatchArgs
	}{
		{name: "no user", args: UploadBatchArgs{BatchID: uuid.NewString(), Sources: []UploadSource{{Type: "local", Path: root}}}},
		{name: "negative user", args: UploadBatchArgs{BatchID: uuid.NewString(), UserID: -1, Sources: []UploadSource{{Type: "local", Path: root}}}},
		{name: "no sources", args: UploadBatchArgs{BatchID: uuid.NewString(), UserID: 1001}},
		{name: "invalid batch id", args: UploadBatchArgs{BatchID: "not-a-uuid", UserID: 1001, Sources: []UploadSource{{Type: "local", Path: root}}}},
		{name: "empty batch id", args: UploadBatchArgs{UserID: 1001, Sources: []UploadSource{{Type: "local", Path: root}}}},
		{name: "part concurrency above 16", args: UploadBatchArgs{
			BatchID: uuid.NewString(), UserID: 1001, PartConcurrency: 17,
			Sources: []UploadSource{{Type: "local", Path: root}},
		}},
	} {
		if err := worker.Work(ctx, batchJob(ctx, runtime, testCase.args)); !errors.Is(err, errInvalidUploadSource) {
			t.Fatalf("Work(%s) error = %v, want errInvalidUploadSource", testCase.name, err)
		}
	}

	var queued int
	if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM river_job").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("river_job holds %d rows, want nothing enqueued by a rejected batch", queued)
	}
}

// TestUploadBatchWorkStopsWhenASourceCannotBeInspected keeps a batch all or
// nothing: one unreachable source must not leave the files from the sources before
// it half-imported.
func TestUploadBatchWorkStopsWhenASourceCannotBeInspected(t *testing.T) {
	db := testpostgres.New(t)
	seedBatchUser(t, db.Pool, 1001)
	runtime := newManagementRuntime(t, db)
	root := t.TempDir()
	worker := NewUploadBatchWorker(nil, catalog.NewService(db.Pool, nil), []string{root})
	ctx := rivertest.WorkContext(context.Background(), runtime.client.Client)

	err := worker.Work(ctx, batchJob(ctx, runtime, UploadBatchArgs{
		BatchID: uuid.NewString(), UserID: 1001,
		Sources: []UploadSource{
			{Type: "local", Path: seedLocalTree(t, "first.txt")},
			{Type: "local", Path: filepath.Join(root, "does-not-exist")},
		},
	}))
	if err == nil {
		t.Fatal("Work() error = nil, want the unreadable source reported")
	}

	var queued int
	if err := db.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM river_job WHERE kind = $1", (UploadSourceArgs{}).Kind()).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("per-file jobs = %d, want none after the batch stopped", queued)
	}
}
