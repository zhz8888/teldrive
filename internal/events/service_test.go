package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
)

// errUnexpectedQuery reports a test that reached the database through a code
// path it was supposed to reject before querying.
var errUnexpectedQuery = errors.New("unexpected database query")

// stubDBTX fails every statement, so a missing input validation surfaces as an
// error instead of a nil interface call.
type stubDBTX struct{}

// Exec fails the statement, so a validation gap surfaces as an error rather than
// an unexpected write.
func (stubDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnexpectedQuery
}

// Query fails the statement, so a validation gap surfaces as an error rather than
// unexpectedly reading rows.
func (stubDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errUnexpectedQuery
}

// QueryRow returns a row whose Scan always fails, so a validation gap surfaces on
// the first row read.
func (stubDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return stubRow{}
}

// stubRow is the QueryRow half of stubDBTX.
type stubRow struct{}

// Scan always reports the unexpected query.
func (stubRow) Scan(...any) error { return errUnexpectedQuery }

// newTestService builds a Service whose listener uses connector and whose
// database always fails, so lifecycle tests need no PostgreSQL pool. The ticket
// cleanup goroutine is only started by Start and never reaches its first tick
// during these tests.
func newTestService(connector listenerConnector) *Service {
	hub := NewHub(5)
	done := make(chan struct{})
	close(done)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Service{
		queries: sqlcgen.New(stubDBTX{}),
		hub:     hub,
		listener: newListenerWithConnector(connector, hub, logger, listenerConfig{
			ConnectTimeout: time.Second,
			PingInterval:   time.Millisecond,
			ReconnectMin:   time.Millisecond,
			ReconnectMax:   5 * time.Millisecond,
		}),
		logger: logger,
		config: Config{CleanupInterval: time.Hour, ConnectTimeout: time.Second},
		done:   done,
	}
}

// blockingConnector holds every Connect call until release is closed, which lets
// a test pin a Start inside its connect and observe the service from outside.
type blockingConnector struct {
	// entered is closed by the first Connect, so a test can wait until Start is
	// parked inside it.
	entered chan struct{}
	// release is closed by the test to let the parked Connect return conn.
	release chan struct{}
	// conn is the connection every Connect hands back once released.
	conn listenerConn

	// once guards entered so only the first Connect closes it.
	once sync.Once
	// mu guards calls against the listener goroutine.
	mu sync.Mutex
	// calls counts Connect invocations, read through callCount.
	calls int
}

// Connect counts the call, signals entered on the first invocation and then waits
// for release or ctx, so the test controls exactly when the listener gets a
// connection.
func (c *blockingConnector) Connect(ctx context.Context, _ string) (listenerConn, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
		return c.conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callCount returns the number of Connect invocations seen so far.
func (c *blockingConnector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// waitListenerConnClosed waits until the fake connection was closed, which is
// how these tests observe that a refused Start did not leak the listener.
func waitListenerConnClosed(t *testing.T, conn *fakeListenerConn) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		closed := conn.closed
		conn.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("listener connection was not closed")
}

func TestServiceStartConnectDoesNotBlockDoneOrClose(t *testing.T) {
	t.Parallel()

	conn := &fakeListenerConn{}
	connector := &blockingConnector{entered: make(chan struct{}), release: make(chan struct{}), conn: conn}
	service := newTestService(connector)

	startErr := make(chan error, 1)
	go func() { startErr <- service.Start(t.Context()) }()

	select {
	case <-connector.entered:
	case <-time.After(time.Second):
		t.Fatal("Start() did not reach the connector")
	}

	select {
	case <-service.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Done() blocked while Start() was connecting")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer closeCancel()
	if err := service.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v, want nil while Start() is still connecting", err)
	}

	close(connector.release)
	if err := <-startErr; !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("Start() error = %v, want %v", err, ErrServiceClosed)
	}
	waitListenerConnClosed(t, conn)
	if err := service.Start(t.Context()); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("Start() after Close error = %v, want %v", err, ErrServiceClosed)
	}
}

func TestServiceConcurrentStartConnectsOnce(t *testing.T) {
	t.Parallel()

	conn := &fakeListenerConn{}
	connector := &blockingConnector{entered: make(chan struct{}), release: make(chan struct{}), conn: conn}
	service := newTestService(connector)

	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- service.Start(t.Context())
		}()
	}

	select {
	case <-connector.entered:
	case <-time.After(time.Second):
		t.Fatal("Start() did not reach the connector")
	}
	close(connector.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	}
	if calls := connector.callCount(); calls != 1 {
		t.Fatalf("Connect() calls = %d, want 1", calls)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := service.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestCursorExpiredRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	service := newTestService(&blockingConnector{entered: make(chan struct{}), release: make(chan struct{})})
	for _, userID := range []int64{0, -1} {
		expired, err := service.CursorExpired(t.Context(), userID, 10)
		if err == nil {
			t.Fatalf("CursorExpired(userID=%d) error = nil, want an invalid request error", userID)
		}
		if expired {
			t.Fatalf("CursorExpired(userID=%d) = true, want false", userID)
		}
	}
	if expired, err := service.CursorExpired(t.Context(), 1, 0); err != nil || expired {
		t.Fatalf("CursorExpired(afterID=0) = %t, %v, want false, nil", expired, err)
	}
}
