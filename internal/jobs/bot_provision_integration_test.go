//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

const provisionBotID = 777

// provisionJob builds the job River would hand the worker. The row is embedded
// by pointer and the worker logs its ID before doing anything else, so a job
// without one panics exactly where a production job never would.
func provisionJob(jobID, userID int64, botIDs ...int64) *river.Job[jobs.BotProvisionArgs] {
	return &river.Job[jobs.BotProvisionArgs]{
		JobRow: &rivertype.JobRow{ID: jobID},
		Args:   jobs.BotProvisionArgs{UserID: userID, BotIDs: botIDs},
	}
}

// TestBotProvisionWorkerPromotesEveryChannelPage drives the whole job against a
// real database: the worker has to walk the channel cursor past the first page,
// so a user owning more channels than one page is provisioned completely rather
// than only in its most recent page.
func TestBotProvisionWorkerPromotesEveryChannelPage(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedProvisionUser(t, db.Pool)
	const channelCount = 240
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO channels (channel_id, user_id, name, selected)
SELECT g, 1001, 'channel-' || g, false
FROM generate_series(9000::bigint, 9000 + $1::bigint - 1) AS g`, int64(channelCount)); err != nil {
		t.Fatal(err)
	}
	service, verifier := newProvisionBotsService(t, db, true)
	seedPendingBot(t, service)
	inviter := &countingInviter{}
	worker := jobs.NewBotProvisionWorker(db.Pool, service, inviter)

	err := worker.Work(ctx, provisionJob(1, 1001, provisionBotID))
	if err != nil {
		t.Fatalf("Work() error = %v, want nil", err)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", verifier.calls)
	}
	if inviter.total() != channelCount {
		t.Fatalf("promoted %d channels, want %d", inviter.total(), channelCount)
	}
	if wrong := inviter.wrongUsername(); wrong != 0 {
		t.Fatalf("promoted %d times with an unexpected username, want 0", wrong)
	}
}

// TestBotProvisionWorkerMarksTheBotWhenOneChannelFails covers the retry contract:
// every channel is still attempted, the job reports the aggregated failure, and
// the bot row records it so the next attempt knows what went wrong.
func TestBotProvisionWorkerMarksTheBotWhenOneChannelFails(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedProvisionUser(t, db.Pool)
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO channels (channel_id, user_id, name, selected)
VALUES (9001, 1001, 'first', false), (9002, 1001, 'second', false)`); err != nil {
		t.Fatal(err)
	}
	service, _ := newProvisionBotsService(t, db, true)
	seedPendingBot(t, service)
	inviter := &countingInviter{failChannel: 9002}
	worker := jobs.NewBotProvisionWorker(db.Pool, service, inviter)

	err := worker.Work(ctx, provisionJob(2, 1001, provisionBotID))
	if err == nil {
		t.Fatal("Work() error = nil, want the failed promotion to surface")
	}
	if !strings.Contains(err.Error(), "9002") {
		t.Fatalf("Work() error = %v, want it to name the failed channel", err)
	}
	// The failure must not cancel the remaining channel.
	if inviter.total() != 2 {
		t.Fatalf("attempted %d channels, want 2", inviter.total())
	}
	assertBotMarkedFailed(t, db, provisionBotID)
}

// TestBotProvisionWorkerMarksTheBotWhenVerificationFails covers the other exit:
// Telegram reports that the stored credential is not a bot, so the job stops
// before it promotes anything and records the failure on the bot row.
func TestBotProvisionWorkerMarksTheBotWhenVerificationFails(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedProvisionUser(t, db.Pool)
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO channels (channel_id, user_id, name, selected)
VALUES (9001, 1001, 'only', false)`); err != nil {
		t.Fatal(err)
	}
	service, _ := newProvisionBotsService(t, db, false)
	seedPendingBot(t, service)
	inviter := &countingInviter{}
	worker := jobs.NewBotProvisionWorker(db.Pool, service, inviter)

	err := worker.Work(ctx, provisionJob(3, 1001, provisionBotID))
	if !errors.Is(err, bots.ErrNotBot) {
		t.Fatalf("Work() error = %v, want it to wrap bots.ErrNotBot", err)
	}
	if inviter.total() != 0 {
		t.Fatalf("promoted %d channels, want 0 after a failed verification", inviter.total())
	}
	assertBotMarkedFailed(t, db, provisionBotID)
}

// TestBotProvisionWorkerSucceedsWithoutWorkToDo keeps the cheap path honest: a job
// whose bot list holds nothing positive must not reach Telegram at all.
func TestBotProvisionWorkerSucceedsWithoutWorkToDo(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedProvisionUser(t, db.Pool)
	service, verifier := newProvisionBotsService(t, db, true)
	inviter := &countingInviter{}
	worker := jobs.NewBotProvisionWorker(db.Pool, service, inviter)

	if err := worker.Work(ctx, provisionJob(4, 1001, 0, -1, 0)); err != nil {
		t.Fatalf("Work() error = %v, want nil", err)
	}
	if verifier.calls != 0 || inviter.total() != 0 {
		t.Fatalf("verifier calls = %d and promotions = %d, want none", verifier.calls, inviter.total())
	}
}

// TestBotProvisionWorkerRejectsANonPositiveUserID pins the guard that keeps a
// malformed job from provisioning somebody else's bots.
func TestBotProvisionWorkerRejectsANonPositiveUserID(t *testing.T) {
	db := testpostgres.New(t)
	service, _ := newProvisionBotsService(t, db, true)
	worker := jobs.NewBotProvisionWorker(db.Pool, service, &countingInviter{})

	err := worker.Work(context.Background(), provisionJob(5, 0, provisionBotID))
	if !errors.Is(err, jobs.ErrBotProvisionNotConfigured) {
		t.Fatalf("Work() error = %v, want ErrBotProvisionNotConfigured", err)
	}
}

func seedProvisionUser(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
}

// newProvisionBotsService returns a bot service wired to a stub verifier, plus
// the verifier so a test can count how often Telegram would have been asked.
// When acceptsBot is false the verifier reports that the credential is a regular
// user account, which is what the server answers for a user token.
func newProvisionBotsService(t *testing.T, db *testpostgres.Database, acceptsBot bool) (*bots.Service, *stubVerifier) {
	t.Helper()
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	verifier := &stubVerifier{identity: bots.Identity{ID: provisionBotID, Username: "storage_bot"}}
	if !acceptsBot {
		verifier.err = bots.ErrNotBot
	}
	service, err := bots.NewService(db.Pool, cipher, verifier)
	if err != nil {
		t.Fatal(err)
	}
	return service, verifier
}

func seedPendingBot(t *testing.T, service *bots.Service) {
	t.Helper()
	if _, err := service.InsertPending(context.Background(), 1001, []string{"777:super-secret-token"}); err != nil {
		t.Fatal(err)
	}
}

// assertBotMarkedFailed checks the row the failure path is required to write, so
// the test fails if the job stops reporting without recording why.
func assertBotMarkedFailed(t *testing.T, db *testpostgres.Database, botID int64) {
	t.Helper()
	var (
		enabled   bool
		failures  int32
		lastError *string
	)
	err := db.Pool.QueryRow(context.Background(),
		"SELECT enabled, consecutive_failures, last_error FROM bots WHERE user_id = 1001 AND bot_id = $1",
		botID).Scan(&enabled, &failures, &lastError)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("bot is still enabled, want the provisioning failure to disable it")
	}
	if failures < 1 {
		t.Fatalf("consecutive_failures = %d, want at least 1", failures)
	}
	if lastError == nil || *lastError == "" {
		t.Fatal("last_error is empty, want the failure recorded on the row")
	}
}

// stubVerifier answers every token with the same identity, or fails outright
// when err is set.
type stubVerifier struct {
	identity bots.Identity
	err      error
	calls    int
}

func (s *stubVerifier) Verify(context.Context, string) (bots.Identity, error) {
	s.calls++
	if s.err != nil {
		return bots.Identity{}, s.err
	}
	return s.identity, nil
}

// countingInviter records every promotion and can fail exactly one channel.
type countingInviter struct {
	failChannel int64

	mu        sync.Mutex
	attempts  int
	usernames []string
}

func (c *countingInviter) InviteBot(_ context.Context, _ int64, channelID int64, username string) error {
	c.mu.Lock()
	c.attempts++
	c.usernames = append(c.usernames, username)
	c.mu.Unlock()
	if channelID == c.failChannel {
		return errors.New("telegram refused the promotion")
	}
	return nil
}

func (c *countingInviter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

func (c *countingInviter) wrongUsername() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	wrong := 0
	for _, username := range c.usernames {
		if username != "storage_bot" {
			wrong++
		}
	}
	return wrong
}
