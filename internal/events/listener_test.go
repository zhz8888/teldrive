package events

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeListenerConnector hands out the listener connections a test queued, one per
// Connect, so reconnect behaviour can be scripted.
type fakeListenerConnector struct {
	// mu guards conns and calls against the listener goroutine.
	mu sync.Mutex
	// conns holds the connections still to hand out, first element first.
	conns []listenerConn
	// calls counts Connect invocations, which the reconnect test asserts grew.
	calls int
}

// Connect records the call and returns the next queued connection, or an error
// once the queue is drained so the listener keeps retrying.
func (c *fakeListenerConnector) Connect(context.Context, string) (listenerConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if len(c.conns) == 0 {
		return nil, errors.New("no fake listener connection")
	}
	conn := c.conns[0]
	c.conns = c.conns[1:]
	return conn, nil
}

// fakeListenerConn is one scripted listener connection: it delivers the queued
// notifications, then either returns the queued error once or blocks until the
// context is cancelled, which keeps the listener idle without a real socket.
type fakeListenerConn struct {
	// mu guards the fields below against the listener goroutine.
	mu sync.Mutex
	// notifications holds the notifications still to deliver, first element first.
	notifications []*pgconn.Notification
	// err is returned once after the queue is drained; a test sets it to end a
	// connection the way a dropped socket would.
	err error
	// closed records that Close ran, so a test can tell the connection was released.
	closed bool
}

// WaitForNotification delivers the next queued notification; with none left it
// returns err once when set, otherwise blocks until ctx is done and returns its
// error.
func (c *fakeListenerConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	c.mu.Lock()
	if len(c.notifications) > 0 {
		notification := c.notifications[0]
		c.notifications = c.notifications[1:]
		c.mu.Unlock()
		return notification, nil
	}
	if c.err != nil {
		err := c.err
		c.err = nil
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

// Ping always succeeds: the listener's liveness check is not what these tests
// exercise.
func (c *fakeListenerConn) Ping(context.Context) error { return nil }

// Close marks the connection closed so the test can observe the release.
func (c *fakeListenerConn) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func TestListenerReconnectsAndDelivers(t *testing.T) {
	t.Parallel()

	first := &fakeListenerConn{err: errors.New("connection lost")}
	second := &fakeListenerConn{notifications: []*pgconn.Notification{{
		Channel: notificationChannel,
		Payload: `{"user_id":42}`,
	}}}
	connector := &fakeListenerConnector{conns: []listenerConn{first, second}}
	hub := NewHub(1)
	wake, unsubscribe, err := hub.Subscribe(42)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()

	listener := newListenerWithConnector(connector, hub, slog.New(slog.NewTextHandler(io.Discard, nil)), listenerConfig{
		ConnectTimeout: time.Second,
		PingInterval:   10 * time.Millisecond,
		ReconnectMin:   time.Millisecond,
		ReconnectMax:   5 * time.Millisecond,
	})
	ctx := t.Context()
	if err := listener.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered after reconnect")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := listener.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	connector.mu.Lock()
	calls := connector.calls
	connector.mu.Unlock()
	if calls < 2 {
		t.Fatalf("Connect() calls = %d, want at least 2", calls)
	}
}

// syncBuffer collects the listener's log output, which the run goroutine keeps
// writing while the test reads it.
type syncBuffer struct {
	// mu guards buf against the listener goroutine, which keeps writing while the
	// test reads the captured output.
	mu sync.Mutex
	// buf accumulates the captured log output.
	buf bytes.Buffer
}

// Write appends p under the mutex and reports a complete write.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything captured so far, under the mutex.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// disconnectDelays waits until the listener has logged want disconnects and
// returns the backoff delay it applied to each of them, oldest first.
func disconnectDelays(t *testing.T, logs *syncBuffer, want int) []time.Duration {
	t.Helper()
	pattern := regexp.MustCompile(`event listener disconnected; reconnecting.* delay=(\S+)`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		matches := pattern.FindAllStringSubmatch(logs.String(), -1)
		if len(matches) >= want {
			delays := make([]time.Duration, 0, len(matches))
			for _, match := range matches {
				delay, err := time.ParseDuration(match[1])
				if err != nil {
					t.Fatalf("parse logged delay %q: %v", match[1], err)
				}
				delays = append(delays, delay)
			}
			return delays
		}
		if time.Now().After(deadline) {
			t.Fatalf("saw %d disconnect logs, want %d: %s", len(matches), want, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestListenerResetsBackoffAfterHealthyConnection(t *testing.T) {
	t.Parallel()

	// The first and third connections fail before proving themselves; only the
	// second delivers a notification, which marks it healthy.
	first := &fakeListenerConn{err: errors.New("connection lost")}
	healthy := &fakeListenerConn{
		notifications: []*pgconn.Notification{{Channel: notificationChannel, Payload: `{"user_id":42}`}},
		err:           errors.New("connection lost"),
	}
	third := &fakeListenerConn{err: errors.New("connection lost")}
	connector := &fakeListenerConnector{conns: []listenerConn{first, healthy, third}}

	logs := &syncBuffer{}
	listener := newListenerWithConnector(connector, NewHub(1), slog.New(slog.NewTextHandler(logs, nil)), listenerConfig{
		ConnectTimeout: time.Second,
		PingInterval:   time.Hour,
		ReconnectMin:   100 * time.Millisecond,
		ReconnectMax:   time.Minute,
	})
	if err := listener.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	delays := disconnectDelays(t, logs, 3)

	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := listener.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// The disconnect after the healthy connection starts from ReconnectMin again,
	// while the one after the connection that never worked keeps the doubled
	// backoff. A reset on every loop iteration would make the latter ReconnectMin
	// too and turn a flapping connection into a hot reconnect loop.
	if delays[1] >= 150*time.Millisecond {
		t.Fatalf("delay after healthy connection = %v, want about %v", delays[1], 100*time.Millisecond)
	}
	if delays[2] <= 150*time.Millisecond {
		t.Fatalf("delay after unhealthy connection = %v, want about %v", delays[2], 200*time.Millisecond)
	}
}
