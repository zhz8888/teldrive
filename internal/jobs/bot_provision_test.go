package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

func TestBotProvisionWorkerRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	var missing *BotProvisionWorker
	if err := missing.Work(context.Background(), &river.Job[BotProvisionArgs]{Args: BotProvisionArgs{UserID: 1001, BotIDs: []int64{777}}}); !errors.Is(err, ErrBotProvisionNotConfigured) {
		t.Fatalf("nil worker Work() error = %v, want ErrBotProvisionNotConfigured", err)
	}
	// sqlcgen.New accepts a nil pool, so only the bot service and the inviter are
	// actually missing here and the guard has to notice that.
	worker := NewBotProvisionWorker(nil, nil, nil)
	if err := worker.Work(context.Background(), &river.Job[BotProvisionArgs]{Args: BotProvisionArgs{UserID: 1001, BotIDs: []int64{777}}}); !errors.Is(err, ErrBotProvisionNotConfigured) {
		t.Fatalf("unconfigured worker Work() error = %v, want ErrBotProvisionNotConfigured", err)
	}
}

func TestBotProvisionArgsIdentifyTheMaintenanceQueue(t *testing.T) {
	t.Parallel()
	if got := (BotProvisionArgs{}).Kind(); got != BotProvisionKind {
		t.Fatalf("Kind() = %q, want %q", got, BotProvisionKind)
	}
	opts := (BotProvisionArgs{}).InsertOpts()
	if opts.Queue != CleanupQueue {
		t.Fatalf("InsertOpts().Queue = %q, want %q", opts.Queue, CleanupQueue)
	}
	if opts.MaxAttempts != 3 {
		t.Fatalf("InsertOpts().MaxAttempts = %d, want 3", opts.MaxAttempts)
	}
	// Deduplicating by arguments is what stops a repeated request from promoting
	// the same bot set twice while a job is still queued.
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false, want true")
	}
}

func TestBotProvisionTimeoutAllowsThirtyMinutes(t *testing.T) {
	t.Parallel()
	if got := (&BotProvisionWorker{}).Timeout(nil); got != 30*time.Minute {
		t.Fatalf("Timeout() = %s, want 30m", got)
	}
}

func TestNormalizedBotIDsKeepsOrderDropsNoiseAndDuplicates(t *testing.T) {
	t.Parallel()
	got := normalizedBotIDs([]int64{7, 0, -3, 7, 9, 9, 2})
	want := []int64{7, 9, 2}
	if len(got) != len(want) {
		t.Fatalf("normalizedBotIDs() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("normalizedBotIDs() = %v, want %v", got, want)
		}
	}
	// A job carrying nothing to provision still gets a non-nil slice, so callers
	// may range over the result without a nil check.
	if empty := normalizedBotIDs(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("normalizedBotIDs(nil) = %v, want a non-nil empty slice", empty)
	}
	if all := normalizedBotIDs([]int64{0, -1}); all == nil || len(all) != 0 {
		t.Fatalf("normalizedBotIDs(all invalid) = %v, want a non-nil empty slice", all)
	}
}

func TestPromoteBotAttemptsEveryChannelAndReportsEachFailure(t *testing.T) {
	t.Parallel()
	channels := []*sqlcgen.Channel{
		{ChannelID: 9001}, {ChannelID: 9002}, {ChannelID: 9003},
		{ChannelID: 9004}, {ChannelID: 9005}, {ChannelID: 9006},
	}
	inviter := &probeInviter{byChannel: map[int64]error{9002: errors.New("telegram refused"), 9005: errors.New("telegram refused")}}
	worker := &BotProvisionWorker{inviter: inviter}

	errs := worker.promoteBot(context.Background(), 1001, "storage_bot", channels)

	// One failure never cancels the rest, so the caller can retry and have only
	// the still-missing channels attempted again.
	if len(errs) != 2 {
		t.Fatalf("promoteBot() errors = %v, want one per failed channel", errs)
	}
	if inviter.calls() != len(channels) {
		t.Fatalf("InviteBot calls = %d, want %d", inviter.calls(), len(channels))
	}
	if inviter.peakConcurrency() > 3 {
		t.Fatalf("peak concurrency = %d, want at most 3", inviter.peakConcurrency())
	}
	if inviter.peakConcurrency() < 2 {
		t.Fatalf("peak concurrency = %d, want the promotions to overlap", inviter.peakConcurrency())
	}
}

func TestPromoteBotWithNoChannelsSucceedsWithoutCallingTelegram(t *testing.T) {
	t.Parallel()
	inviter := &probeInviter{}
	errs := (&BotProvisionWorker{inviter: inviter}).promoteBot(context.Background(), 1001, "storage_bot", nil)
	if len(errs) != 0 {
		t.Fatalf("promoteBot() errors = %v, want none", errs)
	}
	if inviter.calls() != 0 {
		t.Fatalf("InviteBot calls = %d, want 0", inviter.calls())
	}
}

// probeInviter is a BotInviter that fails the channels named in byChannel and
// records how many promotions overlapped, so a test can assert both that every
// channel was attempted and that the three-slot limit held.
type probeInviter struct {
	byChannel map[int64]error

	mu       sync.Mutex
	inFlight int
	peak     int
	attempts map[int64]int
}

func (p *probeInviter) InviteBot(_ context.Context, _ int64, channelID int64, _ string) error {
	p.mu.Lock()
	if p.attempts == nil {
		p.attempts = map[int64]int{}
	}
	p.attempts[channelID]++
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()

	// Hold the slot long enough for the other two to be taken, so an unbounded
	// implementation would show a peak above three.
	time.Sleep(2 * time.Millisecond)

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return p.byChannel[channelID]
}

func (p *probeInviter) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, count := range p.attempts {
		total += count
	}
	return total
}

func (p *probeInviter) peakConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}
