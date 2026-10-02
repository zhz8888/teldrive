package botgateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// ErrUploadRunnerConfiguration reports that the runner cannot serve a call: a
// missing dependency or unsupported rotation backend was passed to
// NewUploadAwareRunner, or Run was invoked on an incompletely built runner or
// with invalid arguments. Callers must test it with errors.Is.
var ErrUploadRunnerConfiguration = errors.New("upload-aware Telegram runner is not configured")

// UploadAwareRunner executes uploads and optionally downloads through an enabled
// bot. Selection is independent per operation, and authenticated bot sessions
// are reused from encrypted Telethon StringSession values stored in the bots table.
type UploadAwareRunner struct {
	// queries lists the candidate bots of an operation and records upload
	// successes and failures on them.
	queries *sqlcgen.Queries
	// selector supplies the rotation counter that picks one candidate bot.
	selector botSelector
	// cipher opens bot tokens and seals bot sessions under their respective
	// purposes.
	cipher *secureblob.Cipher
	// factory builds the gotd client for the selected bot.
	factory *telegramstore.Factory
	// fallback runs the operation when no bot is eligible or the operation is
	// not bot-capable, using the user's own session.
	fallback telegramstore.Runner
	// downloadBots caps how many enabled bots are considered for downloads;
	// zero disables bots for downloads entirely and sends every download to the
	// fallback.
	downloadBots int
	// runBotFunc replaces the built-in runBot when set. It exists so tests can
	// exercise selection without a Telegram client.
	runBotFunc func(context.Context, int64, *sqlcgen.Bot, int, func(context.Context, *tg.Client) error) error
}

// NewUploadAwareRunner returns a runner that executes an operation through one
// of the user's enabled bots when a bot is available for it, and through
// fallback otherwise. downloadBots caps the candidates used for downloads and
// may be zero. The optional rotation argument names the rotation backend and
// defaults to RotationMemory when empty or omitted; any other value is rejected
// with ErrUploadRunnerConfiguration, as are a nil pool, cipher, factory or
// fallback and a negative downloadBots.
func NewUploadAwareRunner(pool *pgxpool.Pool, cipher *secureblob.Cipher, factory *telegramstore.Factory, fallback telegramstore.Runner, downloadBots int, rotation ...string) (*UploadAwareRunner, error) {
	if pool == nil || cipher == nil || factory == nil || fallback == nil || downloadBots < 0 {
		return nil, ErrUploadRunnerConfiguration
	}
	queries := sqlcgen.New(pool)
	mode := RotationMemory
	if len(rotation) > 0 && rotation[0] != "" {
		mode = rotation[0]
	}
	var selector botSelector
	switch mode {
	case RotationMemory:
		selector = &memoryBotSelector{}
	case RotationDatabase:
		selector = databaseBotSelector{queries: queries}
	default:
		return nil, fmt.Errorf("%w: unsupported bot rotation backend %q", ErrUploadRunnerConfiguration, mode)
	}
	return &UploadAwareRunner{
		queries: queries, selector: selector, cipher: cipher, factory: factory, fallback: fallback,
		downloadBots: downloadBots,
	}, nil
}

// Run executes fn on a single connection, through a selected bot when the
// operation has eligible bots and through the fallback runner otherwise. It
// implements telegramstore.Runner, so callers pass the operation they are
// performing and never choose a bot themselves.
func (r *UploadAwareRunner) Run(ctx context.Context, userID int64, operation telegramstore.Operation, fn func(context.Context, *tg.Client) error) error {
	return r.run(ctx, userID, operation, 1, fn)
}

// RunPooled is Run with the call spread over the requested number of parallel
// Telegram connections. A bot run builds a pooled API from the bot's own client
// and closes it before returning; a fallback run uses the fallback's
// PooledRunner capability when it has one and its single-connection Run
// otherwise.
func (r *UploadAwareRunner) RunPooled(ctx context.Context, userID int64, operation telegramstore.Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	return r.run(ctx, userID, operation, connections, fn)
}

// run picks the candidate list for operation, chooses one bot with the rotation
// selector and executes fn through it. Uploads draw from bots that are enabled
// and past any retry backoff, downloads from all enabled bots capped at
// downloadBots, and every other operation goes straight to the fallback.
// Success and failure of an upload are written back to the chosen bot row, and
// a failure is returned wrapped with the operation and bot ID. It returns
// ErrUploadRunnerConfiguration for an incompletely built runner or invalid
// arguments; a failure to record the outcome on the bot row is ignored so that
// it cannot mask the operation error.
func (r *UploadAwareRunner) run(ctx context.Context, userID int64, operation telegramstore.Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	if r == nil || r.queries == nil || r.selector == nil || r.cipher == nil || r.factory == nil || r.fallback == nil || userID <= 0 || connections < 1 || fn == nil {
		return ErrUploadRunnerConfiguration
	}
	var bots []*sqlcgen.Bot
	var err error
	switch operation {
	case telegramstore.OperationUpload:
		bots, err = r.queries.ListUploadEligibleBots(ctx, userID)
	case telegramstore.OperationDownload:
		if r.downloadBots == 0 {
			return r.runFallback(ctx, userID, operation, connections, fn)
		}
		bots, err = r.queries.ListEnabledBots(ctx, userID)
		if len(bots) > r.downloadBots {
			bots = bots[:r.downloadBots]
		}
	default:
		return r.runFallback(ctx, userID, operation, connections, fn)
	}
	if err != nil {
		return fmt.Errorf("list %s bots: %w", operation, err)
	}
	if len(bots) == 0 {
		return r.runFallback(ctx, userID, operation, connections, fn)
	}
	selection, err := r.selector.Next(ctx, userID, operation)
	if err != nil {
		return fmt.Errorf("select %s bot: %w", operation, err)
	}
	bot := bots[int(selection%uint64(len(bots)))]
	runBot := r.runBot
	if r.runBotFunc != nil {
		runBot = r.runBotFunc
	}
	if err := runBot(ctx, userID, bot, connections, fn); err != nil {
		if operation == telegramstore.OperationUpload {
			_, _ = r.queries.MarkBotUploadFailure(ctx, sqlcgen.MarkBotUploadFailureParams{
				UserID: userID, BotID: bot.BotID, LastError: err.Error(),
			})
		}
		return fmt.Errorf("%s with bot %d: %w", operation, bot.BotID, err)
	}
	if operation == telegramstore.OperationUpload {
		_, _ = r.queries.MarkBotUploadSuccess(ctx, sqlcgen.MarkBotUploadSuccessParams{UserID: userID, BotID: bot.BotID})
	}
	return nil
}

// runBot starts a gotd client for the selected bot, reusing the encrypted
// StringSession stored on its row and logging in with the stored token only
// when that session is no longer authorized. A refreshed session is written
// back to the bot row by the storage. When connections is greater than one the
// call is spread over that many connections and the pool is closed before
// returning. The client stops when fn returns and fn's context is cancelled
// with it, so the callback must not be retained.
func (r *UploadAwareRunner) runBot(ctx context.Context, userID int64, bot *sqlcgen.Bot, connections int, fn func(context.Context, *tg.Client) error) error {
	storage := &botSessionStorage{
		queries: r.queries, cipher: r.cipher, userID: userID, botID: bot.BotID,
		stored: append([]byte(nil), bot.Session...),
	}
	client, err := r.factory.New(storage)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	err = client.Run(ctx, func(runCtx context.Context) error {
		status, err := client.Auth().Status(runCtx)
		if err != nil {
			return fmt.Errorf("check authorization: %w", err)
		}
		if !status.Authorized {
			token, err := r.cipher.Open("bot-token", bot.TokenCiphertext)
			if err != nil {
				return fmt.Errorf("decrypt token: %w", err)
			}
			if _, err := client.Auth().Bot(runCtx, string(token)); err != nil {
				return fmt.Errorf("authenticate: %w", err)
			}
		}
		api := client.API()
		if connections > 1 {
			pooled, closePool, err := r.factory.PooledAPI(client, connections)
			if err != nil {
				return fmt.Errorf("create pooled API: %w", err)
			}
			defer closePool()
			api = pooled
		}
		return fn(telegramstore.WithClientID(runCtx, bot.BotID), api)
	})
	if err != nil {
		return err
	}
	return nil
}

// runFallback executes the operation on the fallback runner, using its
// PooledRunner form when it implements telegramstore.PooledRunner and
// connections is greater than one, and its single-connection Run otherwise.
// Requesting more connections than the fallback offers therefore degrades to
// one connection instead of failing.
func (r *UploadAwareRunner) runFallback(ctx context.Context, userID int64, operation telegramstore.Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	if pooled, ok := r.fallback.(telegramstore.PooledRunner); ok && connections > 1 {
		return pooled.RunPooled(ctx, userID, operation, connections, fn)
	}
	return r.fallback.Run(ctx, userID, operation, fn)
}

var _ telegramstore.Runner = (*UploadAwareRunner)(nil)
var _ telegramstore.PooledRunner = (*UploadAwareRunner)(nil)
