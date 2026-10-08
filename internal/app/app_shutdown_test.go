package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/cache"
)

// blockingCache is the process-local cache of an App under test: it records how often
// it is closed and holds the first close open until the test releases it, so a second
// Shutdown can be observed while the release is still running.
type blockingCache struct {
	// started is closed by Close so the test knows the release has begun.
	started chan struct{}
	// release is closed by the test to let the blocked Close return.
	release chan struct{}
	// closes counts Close invocations, which lets the tests assert the cache is
	// released exactly once across repeated Shutdown calls.
	closes atomic.Int32
}

// Close counts the call, signals started and then blocks until the test closes
// release, keeping the caller inside the release for as long as the test needs.
func (c *blockingCache) Close() {
	c.closes.Add(1)
	close(c.started)
	<-c.release
}

// Get always reports a miss: the shutdown tests exercise the release path, never
// a cache read.
func (*blockingCache) Get(context.Context, string, any) error { return cache.ErrNotFound }

// Set discards the value, so storing it can never fail a shutdown test.
func (*blockingCache) Set(context.Context, string, any, time.Duration) error { return nil }

// Delete discards the keys, so invalidating them can never fail a shutdown test.
func (*blockingCache) Delete(context.Context, ...string) error { return nil }

// TestShutdownWaitsForAnInFlightRelease pins the reentrancy contract: a second
// Shutdown does not return while the first release is still running, both callers get
// the same result, and the resources are released exactly once.
func TestShutdownWaitsForAnInFlightRelease(t *testing.T) {
	t.Parallel()

	globalCache := &blockingCache{started: make(chan struct{}), release: make(chan struct{})}
	application := &App{globalCache: globalCache}

	first := make(chan error, 1)
	go func() { first <- application.Shutdown(context.Background()) }()
	<-globalCache.started

	second := make(chan error, 1)
	go func() { second <- application.Shutdown(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("second Shutdown() returned %v while the first release was still running", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(globalCache.release)
	if err := <-first; err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if closes := globalCache.closes.Load(); closes != 1 {
		t.Fatalf("cache closed %d times, want 1", closes)
	}
}

// TestShutdownRepeatsTheRecordedResult pins that a Shutdown after the release has
// finished returns the recorded result immediately instead of closing anything again.
func TestShutdownRepeatsTheRecordedResult(t *testing.T) {
	t.Parallel()

	globalCache := &blockingCache{started: make(chan struct{}), release: make(chan struct{})}
	close(globalCache.release)
	application := &App{globalCache: globalCache}

	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown() error = %v", err)
	}
	if closes := globalCache.closes.Load(); closes != 1 {
		t.Fatalf("cache closed %d times, want 1", closes)
	}
}
