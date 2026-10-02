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

type fakeListenerConnector struct {
	mu    sync.Mutex
	conns []listenerConn
	calls int
}

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

type fakeListenerConn struct {
	mu            sync.Mutex
	notifications []*pgconn.Notification
	err           error
	closed        bool
}

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

func (c *fakeListenerConn) Ping(context.Context) error { return nil }
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
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

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
