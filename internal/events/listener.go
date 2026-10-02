package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notificationChannel is the PostgreSQL NOTIFY channel that carries the user id
// of a newly inserted event. It must match the channel used by the
// notify_user_event trigger; renaming it on one side only silently disables
// every wake-up and degrades the stream to polling.
const notificationChannel = "teldrive_events"

// listenerConfig holds the timing knobs of the dedicated LISTEN connection. It
// mirrors the subset of Config the listener needs and is copied into the
// listener at construction, so later changes to Config have no effect.
type listenerConfig struct {
	// ConnectTimeout bounds a single connect attempt, including the LISTEN
	// statement.
	ConnectTimeout time.Duration
	// PingInterval is how long one blocking read may idle before the listener
	// pings the server, which doubles as its liveness probe interval.
	PingInterval time.Duration
	// ReconnectMin is the delay before the first reconnect attempt.
	ReconnectMin time.Duration
	// ReconnectMax caps the exponential backoff between reconnect attempts.
	ReconnectMax time.Duration
}

// listenerConn is the subset of a dedicated PostgreSQL connection the listener
// uses. It exists as an interface so tests can substitute a fake connection.
type listenerConn interface {
	// WaitForNotification blocks until the channel receives a notification, ctx
	// ends or the connection fails. The listener treats a context deadline as an
	// idle tick and pings instead of reconnecting.
	WaitForNotification(context.Context) (*pgconn.Notification, error)
	// Ping verifies that a connection idle for PingInterval is still usable; a
	// failure makes the listener drop it and reconnect.
	Ping(context.Context) error
	// Close releases the underlying connection. The listener calls it before
	// every reconnect, so implementations must tolerate being called on a
	// connection that already failed.
	Close()
}

// listenerConnector opens one dedicated LISTEN connection. It is an interface so
// tests can inject a fake connector instead of a real PostgreSQL server.
type listenerConnector interface {
	// Connect acquires a connection, issues LISTEN on channel and hands it to
	// the caller. Ownership transfers to the caller, which must Close it.
	Connect(context.Context, string) (listenerConn, error)
}

// pgxConnector is the production listenerConnector, backed by the shared pool.
type pgxConnector struct {
	// pool is the application pool; Connect hijacks one connection out of it for
	// the whole lifetime of the listener.
	pool *pgxpool.Pool
}

// Connect acquires a pooled connection, issues a quoted LISTEN on channel and
// hijacks it out of the pool so the listener owns it exclusively. A failed
// LISTEN returns the connection to the pool; a successful hijack takes the
// connection out of the pool permanently, so the listener's Close destroys the
// server connection instead of recycling it.
func (c pgxConnector) Connect(ctx context.Context, channel string) (listenerConn, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		conn.Release()
		return nil, err
	}
	return &dedicatedListenerConn{conn: conn.Hijack()}, nil
}

// dedicatedListenerConn wraps the hijacked pgx connection the listener owns.
type dedicatedListenerConn struct {
	// conn is the hijacked connection; Close clears it so a second Close is a
	// harmless no-op.
	conn *pgx.Conn
}

// WaitForNotification delegates to the pgx connection and blocks until a
// notification arrives, ctx ends or the connection breaks.
func (c *dedicatedListenerConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	return c.conn.WaitForNotification(ctx)
}

// Ping delegates to the pgx connection to check that it is still alive.
func (c *dedicatedListenerConn) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

// Close shuts the hijacked connection down under a fixed five second grace
// period, so a hung server cannot stall listener shutdown, and clears the field
// to make repeated calls safe.
func (c *dedicatedListenerConn) Close() {
	if c == nil || c.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.conn.Close(ctx)
	c.conn = nil
}

// notificationPayload is the JSON body of a NOTIFY emitted by the user-event
// trigger. Only the user id is carried, never the event itself.
type notificationPayload struct {
	// UserID is the owner of the inserted event; the listener drops
	// notifications whose id is not positive as malformed.
	UserID int64 `json:"user_id"`
}

// Listener owns one dedicated PostgreSQL connection and reconnects after
// transport failures. Notifications only wake local subscribers; PostgreSQL
// rows remain the source of truth.
type Listener struct {
	// connector opens the dedicated connection; production uses pgxConnector and
	// tests substitute a fake.
	connector listenerConnector
	// hub receives the wake-up for the user named in each notification.
	hub *Hub
	// logger records reconnects and malformed notifications; newListenerWithConnector
	// replaces a nil logger with slog.Default.
	logger *slog.Logger
	// config holds the connect, ping and backoff timings.
	config listenerConfig

	// mu guards cancel, done and running.
	mu sync.Mutex
	// cancel stops the run goroutine; it is nil while the listener is stopped.
	cancel context.CancelFunc
	// done is closed by the run goroutine as it exits, which is what Close waits
	// for. It is nil while the listener is stopped.
	done chan struct{}
	// running reports whether the run goroutine is active.
	running bool
}

// newListener returns a listener that opens its dedicated connection from pool.
// The hub must be non-nil; a nil logger falls back to slog.Default. The listener
// stays idle until Start is called.
func newListener(pool *pgxpool.Pool, hub *Hub, logger *slog.Logger, cfg listenerConfig) *Listener {
	return newListenerWithConnector(pgxConnector{pool: pool}, hub, logger, cfg)
}

// newListenerWithConnector is newListener with an injected connector, which lets
// tests drive the reconnect logic without a PostgreSQL server. A nil logger
// falls back to slog.Default.
func newListenerWithConnector(connector listenerConnector, hub *Hub, logger *slog.Logger, cfg listenerConfig) *Listener {
	if logger == nil {
		logger = slog.Default()
	}
	return &Listener{connector: connector, hub: hub, logger: logger, config: cfg}
}

// Start connects the listener and launches its notification loop under ctx. It
// is idempotent while running and returns nil in that case. The initial
// connection is established synchronously, so a failure is reported to the
// caller instead of being retried in the background; once started, the loop
// reconnects on its own until ctx is cancelled or Close is called.
func (l *Listener) Start(ctx context.Context) error {
	if l == nil || l.connector == nil || l.hub == nil {
		return errors.New("event listener is not configured")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return nil
	}

	serviceCtx, cancel := context.WithCancel(ctx)
	conn, err := l.connect(serviceCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("connect event listener: %w", err)
	}

	l.cancel = cancel
	l.done = make(chan struct{})
	l.running = true
	go l.run(serviceCtx, conn, l.done)
	return nil
}

// Close cancels the listener's context and waits for the run goroutine to
// release its connection. It returns ctx.Err() when the caller's deadline
// expires first, and nil for a nil listener or one that is not running. It
// leaves the hub untouched, so subscribers keep their wake channels.
func (l *Listener) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	cancel, done := l.cancel, l.done
	l.mu.Unlock()

	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the listener loop. It waits for notifications until the connection
// fails, then closes that connection, sleeps for a jittered exponential backoff
// and reconnects until it succeeds; it only returns once ctx is cancelled. On
// return it closes conn, marks the listener stopped and closes done, which is
// what Close waits for.
func (l *Listener) run(ctx context.Context, conn listenerConn, done chan struct{}) {
	defer func() {
		if conn != nil {
			conn.Close()
		}
		l.mu.Lock()
		l.running = false
		l.cancel = nil
		close(done)
		l.mu.Unlock()
	}()

	attempt := 0
	for {
		err := l.wait(ctx, conn)
		conn.Close()
		conn = nil
		if ctx.Err() != nil {
			return
		}

		delay := jitterDelay(reconnectDelay(l.config.ReconnectMin, l.config.ReconnectMax, attempt))
		attempt++
		l.logger.ErrorContext(ctx, "event listener disconnected; reconnecting", "error", err, "delay", delay)
		if err := sleepContext(ctx, delay); err != nil {
			return
		}

		for {
			var connectErr error
			conn, connectErr = l.connect(ctx)
			if connectErr == nil {
				break
			}
			delay = jitterDelay(reconnectDelay(l.config.ReconnectMin, l.config.ReconnectMax, attempt))
			attempt++
			l.logger.ErrorContext(ctx, "event listener reconnect failed", "error", connectErr, "delay", delay)
			if err := sleepContext(ctx, delay); err != nil {
				return
			}
		}
	}
}

// connect opens one dedicated connection on notificationChannel, bounding the
// attempt by config.ConnectTimeout. The timeout is derived from ctx, so
// cancelling the listener also aborts a connect in flight.
func (l *Listener) connect(ctx context.Context) (listenerConn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, l.config.ConnectTimeout)
	defer cancel()
	return l.connector.Connect(connectCtx, notificationChannel)
}

// wait pumps notifications from conn until the connection fails or ctx is
// cancelled. Every blocking read is bounded by config.PingInterval so that an
// idle connection is probed with a ping instead of being trusted forever; each
// notification is handed to deliver and the loop continues. It returns ctx.Err()
// on shutdown and a wrapped error describing the failure otherwise.
func (l *Listener) wait(ctx context.Context, conn listenerConn) error {
	for {
		waitCtx, cancel := context.WithTimeout(ctx, l.config.PingInterval)
		notification, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err == nil {
			l.deliver(notification)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			pingCtx, pingCancel := context.WithTimeout(ctx, l.config.ConnectTimeout)
			pingErr := conn.Ping(pingCtx)
			pingCancel()
			if pingErr == nil {
				continue
			}
			return fmt.Errorf("ping event listener: %w", pingErr)
		}
		return fmt.Errorf("wait for event notification: %w", err)
	}
}

// deliver wakes the local subscribers of the user named in a single
// notification. A nil notification or one from another channel is dropped without
// being logged; a body that does not parse or a non-positive user id is logged at
// warn level and dropped, so a corrupt sender cannot wake unrelated subscribers.
func (l *Listener) deliver(notification *pgconn.Notification) {
	if notification == nil || notification.Channel != notificationChannel {
		return
	}
	var payload notificationPayload
	if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil || payload.UserID <= 0 {
		l.logger.Warn("ignored malformed event notification", "payload", notification.Payload, "error", err)
		return
	}
	l.hub.Notify(payload.UserID)
}

// reconnectDelay returns the exponential backoff to apply before reconnect
// attempt: minimum doubled once per attempt, capped at maximum. A non-positive
// minimum is replaced by 100ms, attempt is clamped to the range 0..30, and an
// overflow of the multiplication is also capped at maximum.
func reconnectDelay(minimum, maximum time.Duration, attempt int) time.Duration {
	if minimum <= 0 {
		minimum = 100 * time.Millisecond
	}
	if maximum < minimum {
		maximum = minimum
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 30 {
		attempt = 30
	}
	multiplier := math.Pow(2, float64(attempt))
	delay := time.Duration(float64(minimum) * multiplier)
	if delay < minimum || delay > maximum {
		return maximum
	}
	return delay
}

// jitterDelay spreads base by a uniform random amount of at most 20 percent in
// either direction, so listeners that reconnect at the same moment do not retry
// in lockstep. A base too small for a spread is returned unchanged.
func jitterDelay(base time.Duration) time.Duration {
	spread := base / 5
	if spread <= 0 {
		return base
	}
	return base - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

// sleepContext waits for duration and returns nil, or returns ctx.Err() as soon
// as ctx is done. It lets a reconnect backoff abort immediately on shutdown
// instead of sleeping through the whole delay.
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
