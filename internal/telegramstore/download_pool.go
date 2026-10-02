package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tgdrive/teldrive/v2/internal/cache"
)

// defaultDownloadClientIdleTimeout is how long a pooled download client may
// stay unreferenced before the reaper stops it. The reaper ticks at half this
// interval, and a later session restarts the stopped slot on demand.
const defaultDownloadClientIdleTimeout = 5 * time.Minute

// ErrDownloadClientPoolClosed reports that the pool was closed, either when a
// session is requested or while one waits for its client. Callers must test it
// with errors.Is.
var ErrDownloadClientPoolClosed = errors.New("Telegram download client pool is closed")

// DownloadClientPoolConfig tunes a download client pool. Non-positive values
// fall back to defaults, so the zero value is valid.
type DownloadClientPoolConfig struct {
	// Clients is the number of authenticated download clients kept warm per
	// user. Sessions are spread over these slots round robin, and several
	// concurrent sessions may share one slot. Values below one select one client.
	Clients int
	// ReadBuffers is the number of prefetched chunks buffered per download
	// stream. Values below one select defaultTelegramReadBuffers.
	ReadBuffers int
	// ReadParallel is the number of concurrent chunk fetches per stream and the
	// number of Telegram connections requested for each pooled client. Values
	// below one select defaultTelegramReadParallel.
	ReadParallel int
}

// DownloadClientPool keeps authenticated Telegram download clients running
// between requests, so consecutive requests do not each pay for a fresh
// client run. Clients are pooled per TelDrive user in config.Clients rotation
// slots; sessions lease a slot, share its client with other sessions, and
// release it when they close. The pool owns the client lifetime: it stops
// clients that stay idle and restarts them lazily, and Close stops all of
// them. It is safe for concurrent use.
type DownloadClientPool struct {
	// runner starts the background client of every slot. It is supplied by the
	// caller and must not be nil.
	runner Runner
	// config holds the settings with defaults already applied.
	config DownloadClientPoolConfig
	// globalCache is the document location cache handed to every session; nil
	// disables location caching for pooled sessions.
	globalCache cache.Cacher
	// ctx is the pool lifetime. It is derived from context.Background and is
	// cancelled by Close.
	ctx context.Context
	// cancel cancels ctx, which stops the reaper and every slot context.
	cancel context.CancelFunc

	// mu guards the fields below and every field of the entries they hold.
	mu sync.Mutex
	// entries maps a TelDrive user ID to its rotation slots. A nil slot marks a
	// client that stopped and will be started again by the next lease; the
	// placeholder keeps the other slots at their rotation positions.
	entries map[int64][]*downloadClientEntry
	// next is the per-user round-robin cursor, incremented on every lease so
	// consecutive sessions spread over the slots.
	next map[int64]uint64
	// closed reports that Close ran; a closed pool rejects new sessions.
	closed bool
	// done is closed by the reaper when it exits, which Close waits for.
	done chan struct{}
}

// downloadClientEntry is one rotation slot: a background client run plus the
// bookkeeping needed to hand its API to sessions and to stop it when idle.
type downloadClientEntry struct {
	// userID is the TelDrive user whose session starts the client.
	userID int64
	// clientID is the Telegram account ID reported by the runner, used to scope
	// the document location cache. It is zero until the client is running.
	clientID int64
	// ctx is the slot lifetime, derived from the pool context. Cancelling it
	// stops the client run.
	ctx context.Context
	// cancel cancels ctx; the idle reaper and Close use it to stop the client.
	cancel context.CancelFunc
	// ready is closed once the client is usable, or once the run ended without
	// becoming usable, whichever happens first.
	ready chan struct{}
	// done is closed after the run goroutine finished and removed the slot;
	// Close waits on it.
	done chan struct{}
	// api is the running client API. It is nil before ready closes and nil again
	// once the run ends.
	api *tg.Client
	// err is the terminal error of the client run; a non-nil err also marks the
	// slot as unhealthy for lease.
	err error
	// refs counts the sessions currently holding the slot. Zero makes the entry
	// idle and eligible for reaping.
	refs int
	// lastUsed is the time the last session released the slot, measured against
	// the idle timeout by reapIdle.
	lastUsed time.Time
}

// NewDownloadClientPool returns a pool that starts its clients through runner
// and hands c to every session as its document location cache; c may be nil.
// A nil runner is rejected with ErrInvalidRequest, and every other field of
// config is normalized to its default. The pool owns a background reaper
// goroutine and must be released with Close.
func NewDownloadClientPool(runner Runner, config DownloadClientPoolConfig, c cache.Cacher) (*DownloadClientPool, error) {
	if runner == nil {
		return nil, ErrInvalidRequest
	}
	if config.Clients <= 0 {
		config.Clients = 1
	}
	if config.ReadBuffers <= 0 {
		config.ReadBuffers = defaultTelegramReadBuffers
	}
	if config.ReadParallel <= 0 {
		config.ReadParallel = defaultTelegramReadParallel
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool := &DownloadClientPool{
		runner: runner, config: config, globalCache: c, ctx: ctx, cancel: cancel,
		entries: make(map[int64][]*downloadClientEntry), next: make(map[int64]uint64), done: make(chan struct{}),
	}
	go pool.reap()
	return pool, nil
}

// OpenDownloadSession leases a slot for the user, waits until its client is
// usable, and returns a session bound to it. It returns ErrInvalidRequest for
// a nil pool or non-positive user ID, the startup error when the client stops
// before becoming usable (ErrClientUnavailable when the runner reported no
// error), ErrDownloadClientPoolClosed when the pool is closed while waiting,
// and ctx.Err() when ctx ends first. Every failure path releases the lease,
// and the caller owns the returned session and must close it.
func (p *DownloadClientPool) OpenDownloadSession(ctx context.Context, userID int64) (DownloadSession, error) {
	if p == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	entry, created, err := p.lease(userID)
	if err != nil {
		return nil, err
	}
	if created {
		p.start(entry)
	}
	select {
	case <-entry.ready:
		p.mu.Lock()
		api, runErr := entry.api, entry.err
		p.mu.Unlock()
		if api == nil {
			p.release(entry)
			if runErr == nil {
				runErr = ErrClientUnavailable
			}
			return nil, runErr
		}
		return &gotdDownloadSession{
			clientID:             entry.clientID,
			clientFn:             func() (*tg.Client, error) { return p.client(entry, api) },
			closeFn:              func() error { p.release(entry); return nil },
			downloadReadBuffers:  p.config.ReadBuffers,
			downloadReadParallel: p.config.ReadParallel,
			globalCache:          p.globalCache,
		}, nil
	case <-ctx.Done():
		p.release(entry)
		return nil, ctx.Err()
	case <-p.ctx.Done():
		p.release(entry)
		return nil, ErrDownloadClientPoolClosed
	}
}

// lease picks the slot for the next session of userID, reusing a healthy one
// or installing a fresh entry in its place, and returns the entry with one
// reference already taken plus whether the caller must start it. It returns
// ErrDownloadClientPoolClosed once the pool is closed. The pool mutex is taken
// here, so callers must not hold it.
func (p *DownloadClientPool) lease(userID int64) (*downloadClientEntry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, ErrDownloadClientPoolClosed
	}

	entries := p.entries[userID]
	slot := int(p.next[userID] % uint64(p.config.Clients))
	p.next[userID]++
	if slot < len(entries) {
		entry := entries[slot]
		if entry != nil && entry.err == nil && entry.ctx.Err() == nil {
			entry.refs++
			return entry, false, nil
		}
	}

	entryCtx, cancel := context.WithCancel(p.ctx)
	entry := &downloadClientEntry{
		userID: userID, ctx: entryCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), refs: 1, lastUsed: time.Now(),
	}
	if slot < len(entries) {
		entries[slot] = entry
	} else {
		entries = append(entries, entry)
	}
	p.entries[userID] = entries
	return entry, true, nil
}

// start runs the entry's client in a background goroutine and returns without
// waiting. The goroutine publishes the client API and account ID, closes ready
// exactly once, waits for the slot context to end, and finally records the run
// error, clears the slot, and closes done. It is called once per freshly
// leased entry.
func (p *DownloadClientPool) start(entry *downloadClientEntry) {
	go func() {
		ready := sync.Once{}
		err := runWithConnections(entry.ctx, p.runner, entry.userID, OperationDownload, p.config.ReadParallel, func(runCtx context.Context, api *tg.Client) error {
			p.mu.Lock()
			entry.api = api
			entry.clientID, _ = ClientID(runCtx)
			p.mu.Unlock()
			ready.Do(func() { close(entry.ready) })
			<-runCtx.Done()
			return runCtx.Err()
		})
		p.mu.Lock()
		entry.api = nil
		entry.err = fmt.Errorf("run pooled Telegram download client: %w", err)
		p.removeLocked(entry)
		p.mu.Unlock()
		ready.Do(func() { close(entry.ready) })
		close(entry.done)
	}()
}

// client returns the running API of entry to a session that was opened for it.
// It returns ErrClientUnavailable when the pool is closed, the session already
// released the slot, the client stopped, or the slot now holds a different
// client, so a session can never use a replacement client it does not own.
func (p *DownloadClientPool) client(entry *downloadClientEntry, expected *tg.Client) (*tg.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || entry.refs <= 0 || entry.api == nil || entry.api != expected || entry.err != nil {
		return nil, ErrClientUnavailable
	}
	return entry.api, nil
}

// release drops one session reference from entry and, when the last one is
// gone, stamps lastUsed so the idle reaper can stop the client later. It never
// stops the client itself.
func (p *DownloadClientPool) release(entry *downloadClientEntry) {
	p.mu.Lock()
	if entry.refs > 0 {
		entry.refs--
		if entry.refs == 0 {
			entry.lastUsed = time.Now()
		}
	}
	p.mu.Unlock()
}

// reap is the idle reaper loop of the pool. It ticks at half the idle timeout,
// calls reapIdle on every tick, and closes done when the pool context is
// cancelled so Close can wait for it. It runs in its own goroutine for the
// lifetime of the pool.
func (p *DownloadClientPool) reap() {
	interval := defaultDownloadClientIdleTimeout / 2
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		close(p.done)
	}()
	for {
		select {
		case now := <-ticker.C:
			p.reapIdle(now)
		case <-p.ctx.Done():
			return
		}
	}
}

// reapIdle cancels the context of every slot that no session has held for at
// least defaultDownloadClientIdleTimeout, measured against now. Cancelling the
// slot context stops its client; the next lease starts a replacement. now is a
// parameter so tests can age slots without sleeping, and the pool mutex is
// taken here.
func (p *DownloadClientPool) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entries := range p.entries {
		for _, entry := range entries {
			if entry != nil && entry.refs == 0 && now.Sub(entry.lastUsed) >= defaultDownloadClientIdleTimeout {
				entry.cancel()
			}
		}
	}
}

// Close shuts the pool down: it marks the pool closed, cancels the pool
// context, which stops the reaper and every slot, and waits for the client runs
// it still tracks and for the reaper to finish, bounded by ctx. It returns
// ctx.Err() when ctx ends before the shutdown completes and nil otherwise. Close
// is idempotent; sessions opened earlier lose their client and report
// ErrClientUnavailable, and new sessions report ErrDownloadClientPoolClosed.
func (p *DownloadClientPool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cancel()
	}
	entries := make([]*downloadClientEntry, 0)
	for _, userEntries := range p.entries {
		for _, entry := range userEntries {
			if entry != nil {
				entries = append(entries, entry)
			}
		}
	}
	p.mu.Unlock()

	for _, entry := range entries {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// removeLocked clears the slot holding target, leaving a nil placeholder so
// the remaining slots keep their rotation positions. It must be called with
// the pool mutex held, and it does nothing when the slot has already been
// replaced.
func (p *DownloadClientPool) removeLocked(target *downloadClientEntry) {
	entries := p.entries[target.userID]
	for i, entry := range entries {
		if entry == target {
			entries[i] = nil
			p.entries[target.userID] = entries
			return
		}
	}
}
