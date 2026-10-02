//go:build integration

package botgateway

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

func TestUploadAwareRunnerFallsBackWithoutEnabledBots(t *testing.T) {
	db := testpostgres.New(t)
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24)))
	if err != nil {
		t.Fatal(err)
	}
	factory, err := telegramstore.NewFactory(telegramstore.FactoryConfig{AppID: 12345, AppHash: "test-hash"})
	if err != nil {
		t.Fatal(err)
	}
	fallback := &recordingRunner{}
	runner, err := NewUploadAwareRunner(db.Pool, cipher, factory, fallback, 2)
	if err != nil {
		t.Fatal(err)
	}
	callback := func(context.Context, *tg.Client) error { return nil }
	if err := runner.Run(context.Background(), 1001, telegramstore.OperationUpload, callback); err != nil {
		t.Fatalf("upload fallback error = %v", err)
	}
	if err := runner.Run(context.Background(), 1001, telegramstore.OperationDownload, callback); err != nil {
		t.Fatalf("download fallback error = %v", err)
	}
	if fallback.calls != 2 || fallback.operations[0] != telegramstore.OperationUpload || fallback.operations[1] != telegramstore.OperationDownload {
		t.Fatalf("fallback calls = %d, operations=%v", fallback.calls, fallback.operations)
	}
}

func TestUploadAwareRunnerLimitsAndRotatesDownloadBots(t *testing.T) {
	db := testpostgres.New(t)
	if _, err := db.Pool.Exec(context.Background(), "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	for _, botID := range []int64{101, 102, 103} {
		if _, err := db.Pool.Exec(context.Background(), `
INSERT INTO bots (bot_id, user_id, token_ciphertext, enabled) VALUES ($1, 1001, $2, true)`, botID, []byte("ciphertext")); err != nil {
			t.Fatal(err)
		}
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24)))
	if err != nil {
		t.Fatal(err)
	}
	factory, err := telegramstore.NewFactory(telegramstore.FactoryConfig{AppID: 12345, AppHash: "test-hash"})
	if err != nil {
		t.Fatal(err)
	}
	fallback := &recordingRunner{}
	runner, err := NewUploadAwareRunner(db.Pool, cipher, factory, fallback, 2)
	if err != nil {
		t.Fatal(err)
	}
	var selected []int64
	runner.runBotFunc = func(_ context.Context, _ int64, bot *sqlcgen.Bot, _ int, _ func(context.Context, *tg.Client) error) error {
		selected = append(selected, bot.BotID)
		return nil
	}
	callback := func(context.Context, *tg.Client) error { return nil }
	for range 4 {
		if err := runner.Run(context.Background(), 1001, telegramstore.OperationDownload, callback); err != nil {
			t.Fatal(err)
		}
	}
	if want := []int64{101, 102, 101, 102}; !slices.Equal(selected, want) {
		t.Fatalf("selected download bots = %v, want %v", selected, want)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallback.calls)
	}

	runner.downloadBots = 0
	if err := runner.Run(context.Background(), 1001, telegramstore.OperationDownload, callback); err != nil {
		t.Fatal(err)
	}
	if fallback.calls != 1 || fallback.operations[0] != telegramstore.OperationDownload {
		t.Fatalf("disabled download bot fallback = %d, %v", fallback.calls, fallback.operations)
	}
}

func TestUploadAwareRunnerRecordsFailureAfterCancelledUpload(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO bots (bot_id, user_id, token_ciphertext, enabled) VALUES (101, 1001, $1, true)`, []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24)))
	if err != nil {
		t.Fatal(err)
	}
	factory, err := telegramstore.NewFactory(telegramstore.FactoryConfig{AppID: 12345, AppHash: "test-hash"})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewUploadAwareRunner(db.Pool, cipher, factory, &recordingRunner{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner.runBotFunc = func(context.Context, int64, *sqlcgen.Bot, int, func(context.Context, *tg.Client) error) error {
		cancel()
		return errors.New("upload failed")
	}
	if err := runner.Run(runCtx, 1001, telegramstore.OperationUpload, func(context.Context, *tg.Client) error { return nil }); err == nil {
		t.Fatal("Run() error = nil, want upload failure")
	}
	var failures int32
	var retryAfter pgtype.Timestamptz
	if err := db.Pool.QueryRow(ctx, "SELECT consecutive_failures, retry_after FROM bots WHERE user_id = 1001 AND bot_id = 101").Scan(&failures, &retryAfter); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || !retryAfter.Valid {
		t.Fatalf("recorded failures = %d, retry_after = %v, want 1 and a backoff", failures, retryAfter)
	}
}

func TestUploadAwareRunnerValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewUploadAwareRunner(nil, nil, nil, nil, 0); !errors.Is(err, ErrUploadRunnerConfiguration) {
		t.Fatalf("NewUploadAwareRunner() error = %v", err)
	}
	var runner *UploadAwareRunner
	if err := runner.Run(context.Background(), 1, telegramstore.OperationUpload, func(context.Context, *tg.Client) error { return nil }); !errors.Is(err, ErrUploadRunnerConfiguration) {
		t.Fatalf("Run() error = %v", err)
	}
}

type recordingRunner struct {
	calls      int
	operations []telegramstore.Operation
}

func (r *recordingRunner) Run(_ context.Context, _ int64, operation telegramstore.Operation, _ func(context.Context, *tg.Client) error) error {
	r.calls++
	r.operations = append(r.operations, operation)
	return nil
}
