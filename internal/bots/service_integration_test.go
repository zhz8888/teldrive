//go:build integration

package bots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
	"github.com/zhz8888/teldrive/v2/internal/testutil/querytrace"
)

func TestInsertPendingUsesOneBulkQuery(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 500)
	for index := range tokens {
		tokens[index] = fmt.Sprintf("%d:secret-token", 10_000+index)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*len(tokens))))
	if err != nil {
		t.Fatal(err)
	}
	tracer := &querytrace.Counter{}
	config, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	service, err := NewService(pool, cipher, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := service.InsertPending(ctx, 1001, tokens)
	if err != nil {
		t.Fatalf("InsertPending() error = %v", err)
	}
	if len(rows) != len(tokens) {
		t.Fatalf("inserted bots = %d, want %d", len(rows), len(tokens))
	}
	if got := tracer.Count("InsertPendingBots"); got != 1 {
		t.Fatalf("InsertPendingBots queries = %d, want 1", got)
	}
	if got := tracer.Count("InsertPendingBot") + tracer.Count("GetBot"); got != 0 {
		t.Fatalf("per-bot queries = %d, want 0", got)
	}
}

func TestBotCRUDEncryptsTokenAgainstRealPostgres(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	verifier := &fakeVerifier{identity: Identity{ID: 777, Username: "storage_bot"}}
	service, err := NewService(db.Pool, cipher, verifier)
	if err != nil {
		t.Fatal(err)
	}
	token := "777:super-secret-token"
	created, err := service.Create(ctx, 1001, token)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Create stores and verifies the credential, but a bot may only become an
	// upload candidate once the provisioning job has promoted it into every
	// channel, so the row it returns is still disabled and unnamed.
	if created.BotID != 777 || created.Enabled || created.Username.Valid {
		t.Fatalf("created bot = %#v, want a stored, disabled row without a username", created)
	}
	if bytes.Contains(created.TokenCiphertext, []byte(token)) {
		t.Fatal("bot token ciphertext contains plaintext")
	}
	plain, err := cipher.Open("bot-token", created.TokenCiphertext)
	if err != nil || string(plain) != token {
		t.Fatalf("decrypt bot token = %q, %v", plain, err)
	}
	verifier.identity = Identity{ID: 778, Username: "second_bot"}
	if _, err := service.Create(ctx, 1001, "778:second-secret"); err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	defaultRows, err := service.List(ctx, ListInput{UserID: 1001})
	if err != nil || len(defaultRows) != 2 {
		t.Fatalf("List(default) = %#v, %v", defaultRows, err)
	}
	rows, err := service.List(ctx, ListInput{UserID: 1001, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("List() = %#v, %v", rows, err)
	}
	page, err := service.List(ctx, ListInput{UserID: 1001, Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("List(page) = %#v, %v", page, err)
	}
	pageTime, pageID := page[0].CreatedAt.Time, page[0].BotID
	nextPage, err := service.List(ctx, ListInput{UserID: 1001, AfterCreatedAt: &pageTime, AfterBotID: &pageID, Limit: 500})
	if err != nil || len(nextPage) != 1 {
		t.Fatalf("List(next) = %#v, %v", nextPage, err)
	}
	if err := service.Delete(ctx, 1001, 778); err != nil {
		t.Fatalf("Delete(second) error = %v", err)
	}
	if err := service.Delete(ctx, 1001, 777); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := service.Delete(ctx, 1001, 777); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete() error = %v", err)
	}
	if verifier.calls != 2 {
		t.Fatalf("verifier calls = %d", verifier.calls)
	}
}

func TestMarkProvisionFailureReportsMissingBot(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	verifier := &fakeVerifier{identity: Identity{ID: 777, Username: "storage_bot"}}
	service, err := NewService(db.Pool, cipher, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, 1001, "777:super-secret-token"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := service.MarkProvisionFailure(ctx, 1001, 777, errors.New("invite failed")); err != nil {
		t.Fatalf("MarkProvisionFailure(existing) error = %v", err)
	}
	row, err := service.queries.GetBot(ctx, sqlcgen.GetBotParams{UserID: 1001, BotID: 777})
	if err != nil {
		t.Fatal(err)
	}
	if row.Enabled || row.ConsecutiveFailures != 1 || !row.LastError.Valid || row.LastError.String != "invite failed" {
		t.Fatalf("bot after failure = %#v", row)
	}
	if err := service.MarkProvisionFailure(ctx, 1001, 778, errors.New("invite failed")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkProvisionFailure(missing) error = %v", err)
	}
}

// TestVerifyPendingKeepsTheBotDisabledUntilActivation pins the two-step
// verification contract against a real database: verifying a credential must
// leave the row disabled, because the upload allocator only picks enabled bots
// and the provisioning job still has to promote the bot into every channel of
// the user before it may be enabled.
func TestVerifyPendingKeepsTheBotDisabledUntilActivation(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{2}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*4)))
	if err != nil {
		t.Fatal(err)
	}
	verifier := &fakeVerifier{identity: Identity{ID: 777, Username: " storage_bot "}}
	service, err := NewService(db.Pool, cipher, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InsertPending(ctx, 1001, []string{"777:super-secret-token"}); err != nil {
		t.Fatalf("InsertPending() error = %v", err)
	}
	identity, err := service.VerifyPending(ctx, 1001, 777)
	if err != nil {
		t.Fatalf("VerifyPending() error = %v", err)
	}
	// The username is the name the bot is promoted under, so the padding Telegram
	// may report around it is trimmed before the caller sees it.
	if identity.ID != 777 || identity.Username != "storage_bot" {
		t.Fatalf("VerifyPending() identity = %#v, want the reported bot with a trimmed username", identity)
	}
	assertBotRow(t, service, 777, false, "")
	// A credential that authenticates as somebody else must not even change the
	// pending row.
	verifier.identity = Identity{ID: 999, Username: "other_bot"}
	if _, err := service.VerifyPending(ctx, 1001, 777); !errors.Is(err, ErrNotBot) {
		t.Fatalf("VerifyPending(mismatched identity) error = %v, want ErrNotBot", err)
	}
	assertBotRow(t, service, 777, false, "")
	// Activation is what enables the bot and clears the failure history an earlier
	// provisioning run recorded; the job only reaches it after all promotions.
	verifier.identity = Identity{ID: 777, Username: "storage_bot"}
	if err := service.MarkProvisionFailure(ctx, 1001, 777, errors.New("invite failed")); err != nil {
		t.Fatalf("MarkProvisionFailure() error = %v", err)
	}
	activated, err := service.ActivateVerified(ctx, 1001, 777, identity.Username)
	if err != nil {
		t.Fatalf("ActivateVerified() error = %v", err)
	}
	if !activated.Enabled || !activated.Username.Valid || activated.Username.String != "storage_bot" {
		t.Fatalf("activated bot = %#v, want it enabled under the reported username", activated)
	}
	if activated.ConsecutiveFailures != 0 || activated.LastError.Valid {
		t.Fatalf("activated bot = %#v, want the failure history cleared", activated)
	}
	assertBotRow(t, service, 777, true, "storage_bot")
	// A bot that was deleted while its provisioning ran is reported as missing by
	// both halves of the flow, so the job stops retrying a row that is gone.
	if _, err := service.VerifyPending(ctx, 1001, 778); !errors.Is(err, ErrNotFound) {
		t.Fatalf("VerifyPending(missing bot) error = %v, want ErrNotFound", err)
	}
	if _, err := service.ActivateVerified(ctx, 1001, 778, "storage_bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActivateVerified(missing bot) error = %v, want ErrNotFound", err)
	}
}

// assertBotRow checks the enabled flag and stored username of one bot row, which
// is how a test proves that verification left it untouched or that activation
// enabled it.
func assertBotRow(t *testing.T, service *Service, botID int64, enabled bool, username string) {
	t.Helper()
	row, err := service.queries.GetBot(context.Background(), sqlcgen.GetBotParams{UserID: 1001, BotID: botID})
	if err != nil {
		t.Fatalf("GetBot(%d) error = %v", botID, err)
	}
	if row.Enabled != enabled {
		t.Fatalf("bot %d enabled = %v, want %v", botID, row.Enabled, enabled)
	}
	if username == "" {
		if row.Username.Valid {
			t.Fatalf("bot %d username = %q, want it unset", botID, row.Username.String)
		}
		return
	}
	if !row.Username.Valid || row.Username.String != username {
		t.Fatalf("bot %d username = %#v, want %q", botID, row.Username, username)
	}
}

// fakeVerifier stands in for the Telegram token check the service performs when a
// bot is created: it returns a fixed identity and counts calls.
type fakeVerifier struct {
	// identity is the bot resolved for an accepted token.
	identity Identity
	// err, when set, is returned instead of identity so a rejected token can be
	// exercised.
	err error
	// calls counts Verify invocations; one Create is expected to verify once.
	calls int
}

// Verify records the call and returns the configured identity and error.
func (f *fakeVerifier) Verify(context.Context, string) (Identity, error) {
	f.calls++
	return f.identity, f.err
}
