package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/cache"
)

// blockingCache is the process-local cache of an App under test: it records how often
// it is closed and holds the first close open until the test releases it, so a second
// Shutdown can be observed while the release is still running.
type blockingCache struct {
	started chan struct{}
	release chan struct{}
	closes  atomic.Int32
}

func (c *blockingCache) Close() {
	c.closes.Add(1)
	close(c.started)
	<-c.release
}

func (*blockingCache) Get(context.Context, string, any) error { return cache.ErrNotFound }

func (*blockingCache) Set(context.Context, string, any, time.Duration) error { return nil }

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
