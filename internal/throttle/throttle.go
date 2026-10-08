// Package throttle bounds how often a caller may repeat a failing action for the
// same key, such as guessing a share password or a Telegram login code.
//
// The limiter is deliberately small and in-process: it makes online guessing
// expensive and stops one account from driving repeated Telegram sends, but it is
// not a shared quota. A deployment with several instances therefore grants one
// budget per instance, which still bounds the rate a single client can reach
// through any one of them. State is kept for at most Capacity keys, so an
// attacker who invents keys cannot grow it without bound.
package throttle

import (
	"sync"
	"time"
)

// Limiter tracks consecutive failures per key.
type Limiter struct {
	// mu guards entries, which every exported method reads or writes.
	mu sync.Mutex
	// entries holds the failure state of the keys seen so far and is bounded by
	// capacity, so invented keys cannot grow it without limit.
	entries map[string]*entry
	// failures is how many consecutive failures a key may accumulate before the
	// limiter starts refusing attempts.
	failures int
	// base is the first block applied after the threshold, doubled for every
	// further failure up to max.
	base time.Duration
	// max caps one block, so a longer failure streak cannot lock a key out
	// indefinitely.
	max time.Duration
	// capacity bounds how many keys are remembered at once.
	capacity int
	// now is the clock, injectable so tests do not sleep.
	now func() time.Time
}

// entry is the failure state of one key.
type entry struct {
	// failures counts consecutive failures since the last success.
	failures int
	// blocked is the instant until which attempts are refused.
	blocked time.Time
	// seen is the instant of the last failure, used to prune old keys.
	seen time.Time
}

// New returns a Limiter that refuses a key after failures consecutive failures,
// first for base and then for twice the previous delay, never longer than max.
// Capacity bounds how many keys are tracked; the least recently failed keys are
// forgotten first when it is exceeded.
func New(failures int, base, max time.Duration, capacity int) *Limiter {
	if failures < 1 {
		failures = 1
	}
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	if capacity < 1 {
		capacity = 1
	}
	return &Limiter{
		entries:  make(map[string]*entry),
		failures: failures,
		base:     base,
		max:      max,
		capacity: capacity,
		now:      time.Now,
	}
}

// Allow reports whether key may be attempted now. When it may not, the returned
// duration is how long the caller should wait before retrying.
func (l *Limiter) Allow(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, ok := l.entries[key]
	if !ok {
		return 0, true
	}
	now := l.now()
	if now.Before(item.blocked) {
		return item.blocked.Sub(now), false
	}
	return 0, true
}

// Fail records one failed attempt for key and starts or extends its block once
// the threshold is reached.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	item, ok := l.entries[key]
	if !ok {
		item = &entry{}
		l.entries[key] = item
		l.prune(now, key)
	}
	item.failures++
	item.seen = now
	if item.failures >= l.failures {
		item.blocked = now.Add(l.delay(item.failures))
	}
}

// Succeed forgets the failures recorded for key, so a caller that eventually
// presents the right secret starts from a clean budget.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// delay returns how long a key with the given number of consecutive failures is
// blocked: base after the threshold, doubled for each further failure, capped at
// max.
func (l *Limiter) delay(failures int) time.Duration {
	delay := l.base
	for i := l.failures; i < failures; i++ {
		if delay >= l.max {
			break
		}
		delay *= 2
	}
	if delay > l.max {
		delay = l.max
	}
	return delay
}

// prune drops remembered keys until the map fits its capacity, keeping key. It
// first forgets keys that are neither blocked nor recently failed, and if that is
// not enough it forgets the least recently failed keys, including blocked ones:
// bounded memory is worth more than a block that a long-running process would
// otherwise never release.
func (l *Limiter) prune(now time.Time, keep string) {
	if len(l.entries) <= l.capacity {
		return
	}
	stale := now.Add(-l.max)
	for name, item := range l.entries {
		if name == keep || now.Before(item.blocked) || item.seen.After(stale) {
			continue
		}
		delete(l.entries, name)
	}
	for len(l.entries) > l.capacity {
		oldestName := ""
		var oldest time.Time
		for name, item := range l.entries {
			if name == keep {
				continue
			}
			if oldestName == "" || item.seen.Before(oldest) {
				oldestName, oldest = name, item.seen
			}
		}
		if oldestName == "" {
			return
		}
		delete(l.entries, oldestName)
	}
}
