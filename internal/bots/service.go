// Package bots manages the Telegram upload bots a TelDrive user registers. It
// validates bot tokens, stores them encrypted, verifies them against Telegram,
// and owns the per-user listing and deletion of bot rows. A bot inserted here
// stays disabled until the provisioning job verifies its identity and promotes
// it into the user's channels.
package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
)

var (
	// ErrInvalidInput reports a non-positive user or bot ID, a blank or
	// malformed bot token, or a missing dependency. Callers must test it with
	// errors.Is.
	ErrInvalidInput = errors.New("invalid bot input")
	// ErrNotFound reports that no bot row matches the requested user and bot
	// ID, as happens when deleting a bot that was never registered or belongs
	// to somebody else. Callers must test it with errors.Is.
	ErrNotFound = errors.New("bot not found")
	// ErrNotBot reports a credential that does not belong to the bot it was
	// registered for: Telegram answered for a different account, the account is
	// a regular user rather than a bot, or it carries no username. Callers must
	// test it with errors.Is.
	ErrNotBot = errors.New("Telegram credential does not belong to a bot")
)

// Identity is the Telegram account a bot token authenticates as. It is produced
// by Verifier and consumed by Service.VerifyPending, which rejects an identity
// whose ID does not match the token prefix or whose Username is blank.
type Identity struct {
	// ID is the numeric Telegram ID of the authenticated bot account.
	ID int64
	// Username is the bot's public username without a leading "@".
	Username string
}

// Verifier authenticates a raw bot token against Telegram and reports the
// account it belongs to. Implementations must be safe for concurrent use and
// must not persist anything derived from the credential.
type Verifier interface {
	// Verify logs in with token and returns the identity Telegram reports for
	// it. A credential that authenticates as a regular user must be rejected
	// with ErrNotBot; any other failure is returned as it is and leaves the
	// caller's bot row untouched.
	Verify(context.Context, string) (Identity, error)
}

// Provisioner gives a verified bot access to the channels the user already
// owns, so bots registered after those channels existed become usable too.
type Provisioner interface {
	// ProvisionBot grants the bot described by identity membership in every
	// channel userID currently owns. The caller records a failure and leaves
	// the bot disabled when this returns an error, and may call it again for an
	// already provisioned bot when a job is retried, so implementations must
	// tolerate a repeated grant.
	ProvisionBot(context.Context, int64, Identity) error
}

// ListInput selects one cursor page of a user's bots, newest first. UserID is
// mandatory; the After fields are the exclusive cursor taken from the previous
// page, and Limit is clamped by List.
type ListInput struct {
	// UserID is the TelDrive user whose bots are listed and must be positive.
	UserID int64
	// AfterCreatedAt is the created_at of the last row of the previous page;
	// nil starts at the newest bot. It is only meaningful together with
	// AfterBotID.
	AfterCreatedAt *time.Time
	// AfterBotID is the bot_id of the last row of the previous page and breaks
	// ties between rows sharing created_at; nil starts at the newest bot.
	AfterBotID *int64
	// Limit is the requested page size. Zero or negative values default to 100
	// and values above 200 are clamped to 200.
	Limit int32
}

// Service owns the bot rows of every user. It keeps no per-request state and is
// safe for concurrent use.
type Service struct {
	// pool is the connection pool that queries and the pending-insert
	// transaction run on.
	pool *pgxpool.Pool
	// queries is the sqlc handle bound to pool for statements outside a
	// transaction.
	queries *sqlcgen.Queries
	// cipher seals and opens bot tokens under the "bot-token" purpose, which
	// ties every stored token to this column.
	cipher *secureblob.Cipher
	// verifier authenticates tokens against Telegram during activation.
	verifier Verifier
}

// pendingBotRecord is one element of the JSON array passed to InsertPendingBots.
// Its field names and types mirror the jsonb_to_recordset signature of that
// query, and TokenCiphertext is base64 encoded by encoding/json to match the
// query's decode(..., 'base64').
type pendingBotRecord struct {
	// BotID is the numeric bot ID parsed from the token prefix.
	BotID int64 `json:"bot_id"`
	// TokenCiphertext is the sealed token, so the plaintext never reaches the
	// database.
	TokenCiphertext []byte `json:"token_ciphertext"`
}

// NewService returns a bot service that seals tokens with cipher and verifies
// them with verifier. All three dependencies are required and a nil one is
// reported as ErrInvalidInput. The pool is not pinged, so connectivity problems
// surface on first use.
func NewService(pool *pgxpool.Pool, cipher *secureblob.Cipher, verifier Verifier) (*Service, error) {
	if pool == nil || cipher == nil || verifier == nil {
		return nil, ErrInvalidInput
	}
	return &Service{pool: pool, queries: sqlcgen.New(pool), cipher: cipher, verifier: verifier}, nil
}

// TokenBotID extracts the numeric bot ID from a Telegram bot token of the form
// "<bot_id>:<secret>", returning ErrInvalidInput when the token is blank, has no
// colon, or carries a non-numeric or non-positive prefix. It never contacts
// Telegram, so the prefix alone does not prove the token is genuine.
func TokenBotID(token string) (int64, error) {
	token = strings.TrimSpace(token)
	prefix, _, ok := strings.Cut(token, ":")
	if !ok || prefix == "" {
		return 0, ErrInvalidInput
	}
	botID, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || botID <= 0 {
		return 0, ErrInvalidInput
	}
	return botID, nil
}

// Create registers a single bot token for userID and verifies it immediately.
// It stores the token as a pending, disabled bot row and then returns the
// activated row. A token that is already registered has its insert skipped and
// is re-verified instead, which lets a caller reactivate a bot that a previous
// provisioning run disabled. It returns ErrInvalidInput for a non-positive user
// ID or a blank or malformed token.
func (s *Service) Create(ctx context.Context, userID int64, token string) (*sqlcgen.Bot, error) {
	token = strings.TrimSpace(token)
	if userID <= 0 || token == "" {
		return nil, ErrInvalidInput
	}
	rows, err := s.InsertPending(ctx, userID, []string{token})
	if err != nil {
		return nil, err
	}
	botID, err := TokenBotID(token)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return s.VerifyPending(ctx, userID, botID)
	}
	return s.VerifyPending(ctx, userID, rows[0].BotID)
}

// InsertPending stores the given tokens as disabled bot rows for userID in one
// transaction and returns one row per accepted token, in input order. Blank or
// malformed tokens fail the whole call with ErrInvalidInput, and repeated IDs
// within the input are collapsed to their first occurrence.
//
// A token that is already registered replaces the stored one and puts the row back
// into the pending state, clearing its failure history. That is what makes a bot
// disabled by a failed provisioning recoverable: the row is identified by its bot
// id, and the provisioning job the caller queues next verifies the new token and
// enables the row again.
func (s *Service) InsertPending(ctx context.Context, userID int64, tokens []string) ([]*sqlcgen.Bot, error) {
	if userID <= 0 || len(tokens) == 0 {
		return nil, ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pending bot insert: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	records := make([]pendingBotRecord, 0, len(tokens))
	order := make([]int64, 0, len(tokens))
	seen := make(map[int64]struct{}, len(tokens))
	for _, raw := range tokens {
		token := strings.TrimSpace(raw)
		botID, parseErr := TokenBotID(token)
		if parseErr != nil {
			return nil, parseErr
		}
		if _, exists := seen[botID]; exists {
			continue
		}
		seen[botID] = struct{}{}
		ciphertext, sealErr := s.cipher.Seal("bot-token", []byte(token))
		if sealErr != nil {
			return nil, sealErr
		}
		records = append(records, pendingBotRecord{BotID: botID, TokenCiphertext: ciphertext})
		order = append(order, botID)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("encode pending bots: %w", err)
	}
	inserted, err := queries.InsertPendingBots(ctx, sqlcgen.InsertPendingBotsParams{UserID: userID, Bots: encoded})
	if err != nil {
		return nil, fmt.Errorf("insert pending bots: %w", err)
	}
	byID := make(map[int64]*sqlcgen.Bot, len(inserted))
	for _, row := range inserted {
		byID[row.BotID] = row
	}
	rows := make([]*sqlcgen.Bot, 0, len(inserted))
	for _, botID := range order {
		if row := byID[botID]; row != nil {
			rows = append(rows, row)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pending bots: %w", err)
	}
	return rows, nil
}

// VerifyPending decrypts the stored token of an existing bot row, authenticates
// it against Telegram, and activates the row when the reported identity matches
// the row's bot ID and carries a username. Activation also clears the recorded
// failures, so a bot disabled by an earlier provisioning run becomes eligible
// again. It returns ErrInvalidInput for non-positive IDs, ErrNotBot for an
// identity mismatch, and the verifier's error unchanged; a failed verification
// leaves the row disabled and is not recorded here.
func (s *Service) VerifyPending(ctx context.Context, userID, botID int64) (*sqlcgen.Bot, error) {
	if userID <= 0 || botID <= 0 {
		return nil, ErrInvalidInput
	}
	row, err := s.queries.GetBot(ctx, sqlcgen.GetBotParams{UserID: userID, BotID: botID})
	if err != nil {
		return nil, fmt.Errorf("load pending bot: %w", err)
	}
	token, err := s.cipher.Open("bot-token", row.TokenCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot token: %w", err)
	}
	identity, err := s.verifier.Verify(ctx, string(token))
	if err != nil {
		return nil, err
	}
	if identity.ID != botID || strings.TrimSpace(identity.Username) == "" {
		return nil, ErrNotBot
	}
	activated, err := s.queries.ActivateBot(ctx, sqlcgen.ActivateBotParams{
		Username: dbtypes.OptionalText(nonEmpty(identity.Username)), UserID: userID, BotID: botID,
	})
	if err != nil {
		return nil, fmt.Errorf("activate bot: %w", err)
	}
	return activated, nil
}

// MarkProvisionFailure disables the bot and records cause as its last error,
// incrementing the consecutive failure counter. A nil cause is stored as the
// generic text "bot provisioning failed". It returns ErrNotFound when the user
// and bot ID match no row, which covers a bot that was deleted while its
// provisioning ran.
func (s *Service) MarkProvisionFailure(ctx context.Context, userID, botID int64, cause error) error {
	message := "bot provisioning failed"
	if cause != nil {
		message = cause.Error()
	}
	count, err := s.queries.MarkBotProvisionFailure(ctx, sqlcgen.MarkBotProvisionFailureParams{
		LastError: dbtypes.OptionalText(&message), UserID: userID, BotID: botID,
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// HasExistingChannels reports whether the user owns at least one registered
// storage channel. It reads at most one row, returns ErrInvalidInput for a
// non-positive user ID, and wraps query failures.
func (s *Service) HasExistingChannels(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, ErrInvalidInput
	}
	rows, err := s.queries.ListChannels(ctx, sqlcgen.ListChannelsParams{UserID: userID, PageSize: 1})
	if err != nil {
		return false, fmt.Errorf("list channels for bot provisioning: %w", err)
	}
	return len(rows) > 0, nil
}

// List returns one cursor page of the user's bots, newest first, ordered by
// (created_at, bot_id) descending. Limit defaults to 100 and is capped at 200;
// a page shorter than the effective limit means the user's bots are exhausted.
// It returns ErrInvalidInput for a non-positive user ID and a wrapped query
// error otherwise.
func (s *Service) List(ctx context.Context, in ListInput) ([]*sqlcgen.Bot, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidInput
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	var afterID pgtype.Int8
	if in.AfterBotID != nil {
		afterID = dbtypes.Int8(*in.AfterBotID)
	}
	rows, err := s.queries.ListBots(ctx, sqlcgen.ListBotsParams{
		UserID: in.UserID, AfterCreatedAt: dbtypes.OptionalTime(in.AfterCreatedAt),
		AfterBotID: afterID, PageSize: in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	return rows, nil
}

// Delete removes the user's bot row, including the encrypted session and token
// stored on it. No Telegram call is made, so the bot keeps any channel
// membership it was granted. It returns ErrInvalidInput for a non-positive user
// or bot ID and ErrNotFound when nothing was deleted, which covers bots of
// other users.
func (s *Service) Delete(ctx context.Context, userID, botID int64) error {
	if userID <= 0 || botID <= 0 {
		return ErrInvalidInput
	}
	count, err := s.queries.DeleteBot(ctx, sqlcgen.DeleteBotParams{UserID: userID, BotID: botID})
	if err != nil {
		return fmt.Errorf("delete bot: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// nonEmpty trims value and returns a pointer to the trimmed string, or nil when
// nothing is left, so it can be passed to the optional text columns.
func nonEmpty(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
