//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

// provisionBotID is the bot ID the stub verifier reports, the ID the tests put in
// the job arguments, and the ID they look the bots row up by.
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
	// Every promotion reads the bot row, so a worker that activated the bot before
	// inviting it into the channels is caught by the observation and not only by
	// the state the run ends in.
	enabledDuringInvite := &enabledObserver{}
	inviter.observe = func(int64) {
		var enabled bool
		if err := db.Pool.QueryRow(ctx, "SELECT enabled FROM bots WHERE user_id = 1001 AND bot_id = $1", provisionBotID).Scan(&enabled); err != nil {
			t.Errorf("read bot row during promotion: %v", err)
			return
		}
		enabledDuringInvite.record(enabled)
	}
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
	if enabledDuringInvite.any() {
		t.Fatal("the bot was enabled while it was still being promoted, want activation only after every channel succeeded")
	}
	assertBotProvisioned(t, db, provisionBotID, "storage_bot")
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

// TestBotProvisionWorkerReportsADeletedBotAsNotFound covers the permanent half of
// the verification contract: a row that is gone, for example because the user
// deleted the bot while its job was queued, must be reported as ErrNotFound
// rather than as the transient Telegram failure the job would retry three times.
func TestBotProvisionWorkerReportsADeletedBotAsNotFound(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedProvisionUser(t, db.Pool)
	service, verifier := newProvisionBotsService(t, db, true)
	inviter := &countingInviter{}
	worker := jobs.NewBotProvisionWorker(db.Pool, service, inviter)

	err := worker.Work(ctx, provisionJob(6, 1001, provisionBotID))
	if !errors.Is(err, bots.ErrNotFound) {
		t.Fatalf("Work() error = %v, want it to wrap bots.ErrNotFound", err)
	}
	if verifier.calls != 0 || inviter.total() != 0 {
		t.Fatalf("verifier calls = %d and promotions = %d, want none for a deleted bot", verifier.calls, inviter.total())
	}
}

// seedProvisionUser inserts the single account every provisioning test acts as.
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

// seedPendingBot stores the pending bot row the worker is expected to promote and
// mark; the service seals the token with its cipher before the row is written.
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

// assertBotProvisioned checks the row a successful provisioning run has to leave
// behind: enabled under the username Telegram reported, which is what makes the
// bot an upload candidate.
func assertBotProvisioned(t *testing.T, db *testpostgres.Database, botID int64, username string) {
	t.Helper()
	var (
		enabled  bool
		reported *string
	)
	err := db.Pool.QueryRow(context.Background(),
		"SELECT enabled, username FROM bots WHERE user_id = 1001 AND bot_id = $1", botID).Scan(&enabled, &reported)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || reported == nil || *reported != username {
		t.Fatalf("bot row = enabled %v, username %v, want enabled under %q", enabled, reported, username)
	}
}

// enabledObserver records whether the bot row was already enabled each time a
// promotion ran. Promotions overlap, so the recorded flags are guarded.
type enabledObserver struct {
	// mu guards seen, which InviteBot appends to from several goroutines.
	mu sync.Mutex
	// seen holds one entry per observed promotion.
	seen []bool
}

// record appends one observation of the bot row's enabled flag.
func (o *enabledObserver) record(enabled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, enabled)
}

// any reports whether the bot was enabled during even one promotion.
func (o *enabledObserver) any() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Contains(o.seen, true)
}

// stubVerifier answers every token with the same identity, or fails outright
// when err is set.
type stubVerifier struct {
	// identity is the bot identity returned when err is nil.
	identity bots.Identity
	// err, when set, is returned instead of the identity, standing in for Telegram
	// refusing the credential.
	err error
	// calls counts how often the worker asked Telegram to verify a token.
	calls int
}

// Verify counts the call and returns the canned identity or error.
func (s *stubVerifier) Verify(context.Context, string) (bots.Identity, error) {
	s.calls++
	if s.err != nil {
		return bots.Identity{}, s.err
	}
	return s.identity, nil
}

// countingInviter records every promotion and can fail exactly one channel.
type countingInviter struct {
	// failChannel is the channel whose promotion fails; zero fails none of them.
	failChannel int64
	// observe, when set, runs once per promotion before it is recorded, so a test
	// can inspect the bot row at the moment the promotion happens.
	observe func(channelID int64)

	// mu guards the counters below, which the worker writes from several
	// goroutines.
	mu sync.Mutex
	// attempts is the number of promotions seen.
	attempts int
	// usernames holds the username of every promotion in call order.
	usernames []string
}

// InviteBot records the attempt and fails it when the channel is failChannel.
func (c *countingInviter) InviteBot(_ context.Context, _ int64, channelID int64, username string) error {
	if c.observe != nil {
		c.observe(channelID)
	}
	c.mu.Lock()
	c.attempts++
	c.usernames = append(c.usernames, username)
	c.mu.Unlock()
	if channelID == c.failChannel {
		return errors.New("telegram refused the promotion")
	}
	return nil
}

// total returns how many promotions were attempted.
func (c *countingInviter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// wrongUsername returns how many promotions used a username other than the one
// the stub verifier reports, so a worker that promotes under the wrong account
// name is caught.
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
