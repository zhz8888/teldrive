package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// ErrClientUnavailable reports that no running Telegram client could be handed
// to the operation, either because the provider returned nil or because a
// pooled download session lost its client. Callers should test it with
// errors.Is.
var ErrClientUnavailable = errors.New("Telegram client is unavailable")

// clientIDContextKey keys the context value that carries the authenticated
// Telegram account ID of the current call.
type clientIDContextKey struct{}

// WithClientID stores the authenticated Telegram account ID in ctx. The ID is a
// Telegram user or bot ID, not a TelDrive user ID, and is used to scope caches
// per account. Non-positive IDs carry no account information, so ctx is
// returned unchanged.
func WithClientID(ctx context.Context, clientID int64) context.Context {
	if clientID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, clientIDContextKey{}, clientID)
}

// ClientID returns the Telegram account ID stored by WithClientID. The boolean
// is false when ctx carries no positive ID, so the zero ID is never valid.
func ClientID(ctx context.Context) (int64, bool) {
	clientID, ok := ctx.Value(clientIDContextKey{}).(int64)
	return clientID, ok && clientID > 0
}

// ClientProvider owns user/bot session lookup, persistent session storage, and
// gotd middleware construction. Returning a new or safely reusable client is an
// infrastructure concern; storage business logic only needs the running API.
type ClientProvider interface {
	// Client returns an unstarted gotd client for the user's session. The
	// runner owns starting it with telegram.Client.Run and the teardown that
	// follows, so an implementation decides whether to build a fresh client per
	// call or hand out a safely reusable one; it must not let two concurrent
	// runs drive the same client, because Client.Run is not reentrant. The
	// returned client may be nil, which the runner reports as
	// ErrClientUnavailable. Implementations must be safe for concurrent use and
	// should report a missing or unreadable session as an error rather than as
	// a nil client.
	Client(ctx context.Context, userID int64, operation Operation) (*telegram.Client, error)
}

// ClientProviderFunc adapts a plain function to the ClientProvider interface.
type ClientProviderFunc func(context.Context, int64, Operation) (*telegram.Client, error)

// Client calls f, which must honour the ClientProvider contract.
func (f ClientProviderFunc) Client(ctx context.Context, userID int64, operation Operation) (*telegram.Client, error) {
	return f(ctx, userID, operation)
}

// ClientRunner is the production Runner for gotd. It guarantees that every raw
// tg.Client callback executes inside telegram.Client.Run, so authentication,
// updates, reconnects, flood waits, and connection cleanup share one lifetime.
type ClientRunner struct {
	// Provider resolves the session for every run and must be set; a nil
	// provider makes Run and RunPooled fail with ErrInvalidRequest.
	Provider ClientProvider
	// Factory supplies the extra connections of a pooled run. It is only
	// required when RunPooled is called with connections greater than one,
	// where a nil Factory yields ErrTelegramConfiguration.
	Factory *Factory
}

// PooledRunner is the optional capability of spreading one operation over
// several Telegram connections. runWithConnections uses it when a storage call
// asks for more than one connection and silently falls back to the single
// connection Run otherwise.
type PooledRunner interface {
	// RunPooled runs fn inside telegram.Client.Run with the requested number of
	// parallel connections. It returns ErrInvalidRequest when connections is
	// below one and otherwise follows the same validation, authorization, and
	// error-wrapping rules as Run.
	RunPooled(ctx context.Context, userID int64, operation Operation, connections int, fn func(context.Context, *tg.Client) error) error
}

// Run executes fn inside telegram.Client.Run for the user's session, so
// authentication, reconnects, flood waits, and connection teardown share one
// lifetime. It returns ErrInvalidRequest for a non-positive user ID or a nil
// callback, ErrClientUnavailable when the provider yields no client, and refuses
// to call fn when the session is unauthorized or when a management operation
// would run on a bot account. fn runs at most once, and its context is
// cancelled as soon as the client stops, so it must not be retained.
func (r ClientRunner) Run(ctx context.Context, userID int64, operation Operation, fn func(context.Context, *tg.Client) error) error {
	return r.run(ctx, userID, operation, 1, fn)
}

// RunPooled is Run with the session spread over the requested number of
// parallel connections. More than one connection requires a Factory; values
// below one return ErrInvalidRequest and a nil Factory returns
// ErrTelegramConfiguration.
func (r ClientRunner) RunPooled(ctx context.Context, userID int64, operation Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	return r.run(ctx, userID, operation, connections, fn)
}

// run validates the request, starts the user's client, verifies that the session
// is authorized and permitted for operation, and then calls fn with the client
// API, pooled when connections is greater than one. Errors coming out of the
// client are wrapped with the operation name so callers can tell which step of
// a multi-step request failed.
func (r ClientRunner) run(ctx context.Context, userID int64, operation Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	if r.Provider == nil || userID <= 0 || connections < 1 || fn == nil {
		return ErrInvalidRequest
	}
	client, err := r.Provider.Client(ctx, userID, operation)
	if err != nil {
		return fmt.Errorf("get Telegram client for %s: %w", operation, err)
	}
	if client == nil {
		return ErrClientUnavailable
	}
	if err := client.Run(ctx, func(runCtx context.Context) error {
		status, err := client.Auth().Status(runCtx)
		if err != nil {
			return fmt.Errorf("get Telegram authorization status: %w", err)
		}
		if !status.Authorized || status.User == nil {
			return fmt.Errorf("Telegram session is not authorized")
		}
		if operation == OperationManage && status.User.Bot {
			return fmt.Errorf("Telegram manage operation requires a user session, got bot account %d", status.User.ID)
		}
		slog.DebugContext(runCtx, "Telegram client authenticated",
			"user_id", userID,
			"operation", operation,
			"telegram_id", status.User.ID,
			"telegram_username", status.User.Username,
			"is_bot", status.User.Bot,
		)

		api := client.API()
		if connections > 1 {
			if r.Factory == nil {
				return ErrTelegramConfiguration
			}
			pooled, closePool, err := r.Factory.PooledAPI(client, connections)
			if err != nil {
				return err
			}
			defer closePool()
			api = pooled
		}
		return fn(WithClientID(runCtx, status.User.ID), api)
	}); err != nil {
		return fmt.Errorf("run Telegram client for %s: %w", operation, err)
	}
	return nil
}

// runWithConnections dispatches to the runner's pooled entry point when it
// implements PooledRunner and more than one connection is requested, and to the
// single connection Run otherwise. Runners without the pooled capability
// silently degrade to one connection, so the callback must not depend on the
// requested parallelism.
func runWithConnections(ctx context.Context, runner Runner, userID int64, operation Operation, connections int, fn func(context.Context, *tg.Client) error) error {
	if pooled, ok := runner.(PooledRunner); ok && connections > 1 {
		return pooled.RunPooled(ctx, userID, operation, connections, fn)
	}
	return runner.Run(ctx, userID, operation, fn)
}

var _ Runner = ClientRunner{}
var _ PooledRunner = ClientRunner{}
