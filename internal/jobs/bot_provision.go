package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

// BotProvisionKind is the River job kind that verifies pending Telegram bots and
// promotes them to administrators of the user's channels.
const BotProvisionKind = "teldrive_provision_bots"

// botChannelPageSize is how many of a user's channels one ListChannels page
// holds. The worker walks the (created_at, channel_id) cursor until a page comes
// back short, so an account with more channels than this is still provisioned
// completely.
const botChannelPageSize = 200

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
//
// The unique state set deliberately covers only the states a job can still run
// from. River's default set also counts completed jobs, which would silently
// swallow every repeat request for the same user and bot set until the job
// cleaner removed the finished job; callers insert a new job to recover a bot
// that a previous run left behind, so a finished job must not block the insert.
func (BotProvisionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: CleanupQueue, MaxAttempts: 3, Priority: 2,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
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
// every channel the user owns, paging through the channel list until it is
// exhausted.
//
// Bots are handled one after another. The promotions for a single bot run
// concurrently, at most three at a time. A channel whose promotion fails does not
// stop the remaining channels: every channel is attempted, the failures are
// logged, and the bot is marked as failed with all of them. Promotion is
// idempotent, so a channel that already carries the bot is left untouched, and
// the aggregated error the job returns makes River retry only the channels that
// are still missing the bot as administrator.
//
// A bot is only activated, and thereby made an upload candidate, once it was
// promoted into every channel. A run that failed in even one channel leaves the
// row disabled and records the failure, so the allocator never picks a bot that
// cannot upload into the channel it would be given. A job whose BotIDs contain no
// positive value succeeds without doing anything.
func (w *BotProvisionWorker) Work(ctx context.Context, job *river.Job[BotProvisionArgs]) error {
	if w == nil || w.queries == nil || w.bots == nil || w.inviter == nil || job.Args.UserID <= 0 {
		return ErrBotProvisionNotConfigured
	}
	botIDs := normalizedBotIDs(job.Args.BotIDs)
	if len(botIDs) == 0 {
		return nil
	}
	slog.InfoContext(ctx, "Starting bot provisioning job", "job_id", job.ID, "user_id", job.Args.UserID, "bot_count", len(botIDs))
	channels, err := w.userChannels(ctx, job.Args.UserID)
	if err != nil {
		return err
	}
	botErrors := make([]error, 0, len(botIDs))
	for _, botID := range botIDs {
		identity, verifyErr := w.bots.VerifyPending(ctx, job.Args.UserID, botID)
		if verifyErr != nil {
			_ = w.bots.MarkProvisionFailure(ctx, job.Args.UserID, botID, verifyErr)
			return fmt.Errorf("verify pending bot %d: %w", botID, verifyErr)
		}
		username := strings.TrimSpace(identity.Username)
		inviteErrors := w.promoteBot(ctx, job.Args.UserID, username, channels)
		if len(inviteErrors) > 0 {
			slog.WarnContext(ctx, "Telegram bot was not promoted in every channel", "job_id", job.ID, "user_id", job.Args.UserID, "bot_id", botID, "channel_count", len(channels), "failed_channel_count", len(inviteErrors), "error", errors.Join(inviteErrors...))
			botErr := fmt.Errorf("provision bot %d: %w", botID, errors.Join(inviteErrors...))
			_ = w.bots.MarkProvisionFailure(ctx, job.Args.UserID, botID, botErr)
			botErrors = append(botErrors, botErr)
			continue
		}
		// Activation comes last and only here: enabling the bot earlier would
		// make the upload allocator pick a bot that is not in every channel yet.
		if _, activateErr := w.bots.ActivateVerified(ctx, job.Args.UserID, botID, username); activateErr != nil {
			botErr := fmt.Errorf("activate bot %d: %w", botID, activateErr)
			_ = w.bots.MarkProvisionFailure(ctx, job.Args.UserID, botID, botErr)
			botErrors = append(botErrors, botErr)
			continue
		}
		slog.InfoContext(ctx, "Telegram bot provisioned", "job_id", job.ID, "user_id", job.Args.UserID, "bot_id", botID, "bot_username", username, "channel_count", len(channels))
	}
	if len(botErrors) > 0 {
		return fmt.Errorf("provision Telegram bots for user %d: %w", job.Args.UserID, errors.Join(botErrors...))
	}
	return nil
}

// userChannels returns every channel owned by userID, newest first. It follows
// the (created_at, channel_id) cursor of ListChannels until a page is shorter
// than botChannelPageSize, so a user with more channels than one page is
// provisioned completely instead of only in the most recent page.
func (w *BotProvisionWorker) userChannels(ctx context.Context, userID int64) ([]*sqlcgen.Channel, error) {
	channels := make([]*sqlcgen.Channel, 0, botChannelPageSize)
	for {
		var afterCreatedAt pgtype.Timestamptz
		var afterChannelID pgtype.Int8
		if len(channels) > 0 {
			last := channels[len(channels)-1]
			afterCreatedAt = last.CreatedAt
			afterChannelID = pgtype.Int8{Int64: last.ChannelID, Valid: true}
		}
		page, err := w.queries.ListChannels(ctx, sqlcgen.ListChannelsParams{
			UserID: userID, AfterCreatedAt: afterCreatedAt, AfterChannelID: afterChannelID, PageSize: botChannelPageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("list channels for bot provisioning: %w", err)
		}
		channels = append(channels, page...)
		if len(page) < botChannelPageSize {
			return channels, nil
		}
	}
}

// promoteBot invites username into every channel, at most three at a time, and
// returns one error per channel that failed, in completion order. Every channel
// is attempted exactly once and a failure never cancels the remaining ones, so
// the caller can retry the job and only the failed channels still need the
// promotion.
func (w *BotProvisionWorker) promoteBot(ctx context.Context, userID int64, username string, channels []*sqlcgen.Channel) []error {
	var (
		inviteMu   sync.Mutex
		inviteErrs []error
	)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for _, channel := range channels {
		// The slot is taken before the goroutine exists, so a user with many
		// channels does not start one goroutine per channel that then waits for a
		// turn; only three are ever alive.
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()

			if err := w.inviter.InviteBot(ctx, userID, channel.ChannelID, username); err != nil {
				inviteMu.Lock()
				inviteErrs = append(inviteErrs, fmt.Errorf("channel %d: %w", channel.ChannelID, err))
				inviteMu.Unlock()
			}
		})
	}
	wg.Wait()
	return inviteErrs
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
