package throttle

import (
	"testing"
	"time"
)

// fixedClock drives the limiter without sleeping: tests move it by hand.
type fixedClock struct {
	// now is the instant the limiter reads through its injected clock function.
	now time.Time
}

// advance moves the clock forward by d without sleeping, so a test can cross a
// backoff window instantly.
func (c *fixedClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newTestLimiter returns a Limiter whose clock starts at a fixed instant and the
// fixedClock that drives it, so backoff behaviour is tested without real waiting.
func newTestLimiter(failures int, base, max time.Duration, capacity int) (*Limiter, *fixedClock) {
	clock := &fixedClock{now: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
	limiter := New(failures, base, max, capacity)
	limiter.now = func() time.Time { return clock.now }
	return limiter, clock
}

func TestLimiterBlocksAfterThresholdAndRecovers(t *testing.T) {
	t.Parallel()
	limiter, clock := newTestLimiter(3, time.Second, time.Minute, 16)
	for attempt := 0; attempt < 2; attempt++ {
		if _, ok := limiter.Allow("share"); !ok {
			t.Fatalf("attempt %d refused before the threshold", attempt)
		}
		limiter.Fail("share")
	}
	if _, ok := limiter.Allow("share"); !ok {
		t.Fatal("third attempt refused before the threshold")
	}
	limiter.Fail("share")
	retryAfter, ok := limiter.Allow("share")
	if ok {
		t.Fatal("attempt allowed after the threshold")
	}
	if retryAfter != time.Second {
		t.Fatalf("retryAfter = %s, want 1s", retryAfter)
	}
	clock.advance(time.Second)
	if _, ok := limiter.Allow("share"); !ok {
		t.Fatal("attempt refused after the block expired")
	}
	limiter.Fail("share")
	if retryAfter, _ := limiter.Allow("share"); retryAfter != 2*time.Second {
		t.Fatalf("retryAfter = %s, want 2s", retryAfter)
	}
}

func TestLimiterCapsTheDelayAndForgetsOnSuccess(t *testing.T) {
	t.Parallel()
	limiter, _ := newTestLimiter(1, time.Second, 4*time.Second, 16)
	for attempt := 0; attempt < 8; attempt++ {
		limiter.Fail("phone")
	}
	retryAfter, ok := limiter.Allow("phone")
	if ok {
		t.Fatal("attempt allowed while blocked")
	}
	if retryAfter > 4*time.Second {
		t.Fatalf("retryAfter = %s, want at most 4s", retryAfter)
	}
	limiter.Succeed("phone")
	if _, ok := limiter.Allow("phone"); !ok {
		t.Fatal("success did not clear the failure state")
	}
}

func TestLimiterKeepsKeysIndependentAndBounded(t *testing.T) {
	t.Parallel()
	limiter, clock := newTestLimiter(1, time.Minute, time.Minute, 2)
	limiter.Fail("a")
	clock.advance(time.Second)
	limiter.Fail("b")
	if _, ok := limiter.Allow("a"); ok {
		t.Fatal("the first key was not blocked")
	}
	if _, ok := limiter.Allow("b"); ok {
		t.Fatal("the second key was not blocked")
	}
	if _, ok := limiter.Allow("unrelated"); !ok {
		t.Fatal("an unrelated key was affected")
	}
	clock.advance(time.Second)
	limiter.Fail("c")
	if _, ok := limiter.Allow("c"); ok {
		t.Fatal("the newest key was not blocked")
	}
	if _, ok := limiter.Allow("a"); !ok {
		t.Fatal("the oldest key should have been forgotten to bound memory")
	}
	if len(limiter.entries) > 2 {
		t.Fatalf("tracked %d keys, want at most 2", len(limiter.entries))
	}
}
