package telegramstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// transientTelegramErrors lists the Telegram RPC error types that are worth
// retrying, for example a failed RPC call or a busy storage worker. Matching is
// exact, by error type, unlike the substring list below.
var transientTelegramErrors = []string{
	"RPC_CALL_FAIL",
	"RPC_MCGET_FAIL",
	"WORKER_BUSY_TOO_LONG_RETRY",
	"STORAGE_CHOOSE_VOLUME_FAILED",
}

// transientTelegramMessages lists lowercased fragments that mark an error as
// transient. These cover transport and worker failures that arrive without an
// RPC error type, such as a dead connection or a closed engine, so matching is
// a case insensitive substring search over the error text.
var transientTelegramMessages = []string{
	"timeout",
	"timedout",
	"no workers running",
	"memory limit exit",
	"connection dead",
	"engine was closed",
	"broken pipe",
	"connection reset by peer",
}

// retryMiddleware retries one invocation that failed with a transient Telegram
// error. Its max field is the number of extra attempts after the first, so an
// invocation runs at most max+1 times. Retries happen immediately, because
// flood waits are handled by the separate flood-wait middleware that wraps this
// one.
type retryMiddleware struct{ max int }

// Handle wraps next so that a call failing with a transient Telegram error is
// retried while attempts remain, reusing the same input and output, so the
// decoder must tolerate being written again. It returns the last Telegram error
// once the attempts are exhausted or the error is permanent, and ctx.Err()
// instead of the Telegram error when the context ends between attempts. The
// middleware adds no delay of its own.
func (m retryMiddleware) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for attempt := 0; ; attempt++ {
			err := next.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}
			if attempt >= m.max || !isTransientTelegramError(err) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
}

// isTransientTelegramError reports whether err is worth retrying: first by exact
// Telegram RPC error type, then by case insensitive match of the error text
// against transientTelegramMessages. It must not be called with a nil error,
// because the text based check dereferences it.
func isTransientTelegramError(err error) bool {
	if tgerr.Is(err, transientTelegramErrors...) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range transientTelegramMessages {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// newRetryMiddleware builds the middleware that retries transient Telegram
// errors for a gotd client. max is the number of extra attempts per invocation;
// zero disables retrying and a negative count is rejected, so that a bad
// configuration fails at construction time instead of silently disabling the
// retry policy.
func newRetryMiddleware(max int) (telegram.Middleware, error) {
	if max < 0 {
		return nil, fmt.Errorf("Telegram retry count cannot be negative")
	}
	return retryMiddleware{max: max}, nil
}
