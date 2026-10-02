package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// BotProvisionKind is the River job kind that verifies pending Telegram bots and
// promotes them to administrators of the user's channels.
const BotProvisionKind = "teldrive_provision_bots"

// ErrBotProvisionNotConfigured reports that the worker cannot run because it is
// missing its queries, bot service or Telegram inviter, or because the job
// carries no positive user ID. Callers must test it with errors.Is.
var ErrBotProvisionNotConfigured = errors.New("bot provisioning worker is not configured")

// BotProvisionArgs is the persisted payload of one bot provisioning job. Its
// JSON field names are stored in river_job, so they must stay stable while older
// jobs may still be queued.
type BotProvisionArgs struct {
	// UserID is the TelDrive user that owns the bots and channels.
	UserID int64 `json:"user_id"`
	// BotIDs lists the Telegram bot IDs to verify and promote; the worker ignores
	// non-positive values and duplicates.
	BotIDs []int64 `json:"bot_ids"`
}

// Kind reports the River job kind handled by BotProvisionWorker.
func (BotProvisionArgs) Kind() string { return BotProvisionKind }

// InsertOpts pins provisioning jobs to the maintenance queue, gives them three
// attempts and deduplicates them by arguments, so requesting the same user and
// bot set twice joins the pending job instead of provisioning the bots twice.
func (BotProvisionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CleanupQueue, MaxAttempts: 3, Priority: 2, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// BotProvisionWorker verifies pending Telegram bots and promotes each of them to
// administrator in every channel the user owns. It is registered only when the
// storage backend implements telegramstore.BotInviter; otherwise the runtime
// leaves bot provisioning disabled.
type BotProvisionWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[BotProvisionArgs]
	// queries reads the user's channels.
	queries *sqlcgen.Queries
	// bots verifies pending bot tokens and records provisioning failures.
	bots *bots.Service
	// inviter performs the actual Telegram membership calls.
	inviter telegramstore.BotInviter
}

// NewBotProvisionWorker returns a worker backed by pool, botService and inviter.
// All three are required: Work fails with ErrBotProvisionNotConfigured when one
// is missing.
func NewBotProvisionWorker(pool *pgxpool.Pool, botService *bots.Service, inviter telegramstore.BotInviter) *BotProvisionWorker {
	return &BotProvisionWorker{queries: sqlcgen.New(pool), bots: botService, inviter: inviter}
}

// Timeout allows 30 minutes per run, since the worker verifies each requested
// bot against Telegram and then promotes it in all of the user's channels with
// three promotions in flight.
func (w *BotProvisionWorker) Timeout(*river.Job[BotProvisionArgs]) time.Duration {
	return 30 * time.Minute
}

// Work verifies the requested bots and promotes each of them to administrator in
// the user's channels, using at most the 200 most recently created channels.
//
// Bots are handled one after another. The promotions for a single bot run
// concurrently, at most three at a time; the first error is remembered, and once
// all in-flight promotions finish the bot is marked as failed and that error is
// returned, so River retries the whole job. Retries re-verify and re-promote the
// bots that already succeeded, which the idempotent activation path tolerates. A
// job whose BotIDs contain no positive value succeeds without doing anything.
func (w *BotProvisionWorker) Work(ctx context.Context, job *river.Job[BotProvisionArgs]) error {
	if w == nil || w.queries == nil || w.bots == nil || w.inviter == nil || job.Args.UserID <= 0 {
		return ErrBotProvisionNotConfigured
	}
	botIDs := normalizedBotIDs(job.Args.BotIDs)
	if len(botIDs) == 0 {
		return nil
	}
	slog.InfoContext(ctx, "Starting bot provisioning job", "job_id", job.ID, "user_id", job.Args.UserID, "bot_count", len(botIDs))
	channels, err := w.queries.ListChannels(ctx, sqlcgen.ListChannelsParams{UserID: job.Args.UserID, PageSize: 200})
	if err != nil {
		return fmt.Errorf("list channels for bot provisioning: %w", err)
	}
	for _, botID := range botIDs {
		row, verifyErr := w.bots.VerifyPending(ctx, job.Args.UserID, botID)
		if verifyErr != nil {
			_ = w.bots.MarkProvisionFailure(ctx, job.Args.UserID, botID, verifyErr)
			return fmt.Errorf("verify pending bot %d: %w", botID, verifyErr)
		}
		username := strings.TrimSpace(row.Username.String)
		var wg sync.WaitGroup
		var inviteErr error
		var inviteMu sync.Mutex
		sem := make(chan struct{}, 3)
		for _, channel := range channels {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()

				if err := w.inviter.InviteBot(ctx, job.Args.UserID, channel.ChannelID, username); err != nil {
					inviteMu.Lock()
					if inviteErr == nil {
						inviteErr = err
					}
					inviteMu.Unlock()
				}
			})
		}
		wg.Wait()
		if inviteErr != nil {
			_ = w.bots.MarkProvisionFailure(ctx, job.Args.UserID, botID, inviteErr)
			return fmt.Errorf("provision bot %d: %w", botID, inviteErr)
		}
		slog.InfoContext(ctx, "Telegram bot provisioned", "job_id", job.ID, "user_id", job.Args.UserID, "bot_id", botID, "bot_username", username, "channel_count", len(channels))
	}
	return nil
}

// normalizedBotIDs removes non-positive IDs and keeps only the first occurrence
// of each remaining ID. It always returns a newly allocated, non-nil slice, so
// callers may store or reorder the result without affecting the input.
func normalizedBotIDs(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
