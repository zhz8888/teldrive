package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

// EventCleanupKind is the River job kind of the periodic sweep that deletes user
// events older than the retention window carried in its arguments.
const EventCleanupKind = "teldrive_cleanup_user_events"

// ErrEventCleanupNotConfigured is returned by Work when the worker was built
// without its pool, queries or clock, which means the runtime did not wire it up.
var ErrEventCleanupNotConfigured = errors.New("event cleanup worker is not configured")

// EventCleanupArgs carries the retention window of one user-event sweep. It is
// persisted as JSON in river_job, so the field name must stay stable while older
// jobs may still be queued.
type EventCleanupArgs struct {
	// Retention is the Go duration string that decides which events are deleted;
	// Work rejects values that do not parse or are not positive.
	Retention string `json:"retention"`
}

// Kind reports the River job kind handled by EventCleanupWorker.
func (EventCleanupArgs) Kind() string { return EventCleanupKind }

// InsertOpts runs the sweep on the maintenance queue with three attempts and
// priority 2, so within that queue it is ordered after the priority-1 purges and
// trash cleanups.
func (EventCleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CleanupQueue, MaxAttempts: 3, Priority: 2}
}

// EventCleanupWorker deletes user events that are older than the configured
// retention, deletes expired Telegram login flows, and records how many rows it
// removed. Each run issues one bounded DELETE per table, so retrying after a
// failure simply deletes whatever is left.
type EventCleanupWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[EventCleanupArgs]
	// pool is only checked for nil: Work rejects a worker that has no pool rather
	// than panicking inside a query.
	pool *pgxpool.Pool
	// queries performs the retention delete.
	queries *sqlcgen.Queries
	// now returns the current time; tests replace it to pin the cutoff.
	now func() time.Time
}

// NewEventCleanupWorker returns a worker backed by pool and using the system
// clock for the cutoff. The pool is required: Work returns
// ErrEventCleanupNotConfigured when it is missing, rather than failing inside the
// first query.
func NewEventCleanupWorker(pool *pgxpool.Pool) *EventCleanupWorker {
	return &EventCleanupWorker{pool: pool, queries: sqlcgen.New(pool), now: time.Now}
}

// Timeout allows 30 minutes for the retention DELETE, which can touch every row
// of a large user_events table.
func (w *EventCleanupWorker) Timeout(*river.Job[EventCleanupArgs]) time.Duration {
	return 30 * time.Minute
}

// Work deletes every user event older than the retention window in the job
// arguments, deletes every Telegram login flow whose deadline has passed, and
// records the deleted row counts and the cutoff as the job output, so an operator
// can see what a sweep did without querying the database.
//
// Both deletes are bounded single statements and independent of each other: the
// event delete uses the cutoff the arguments select, while the flow delete uses
// the database clock and does not depend on the retention. Neither is skipped
// when the other has nothing to remove, and both are idempotent, so a retry after
// a partial failure is safe.
//
// It fails with ErrEventCleanupNotConfigured when the worker is not wired up, and
// rejects a retention that does not parse or is not positive.
func (w *EventCleanupWorker) Work(ctx context.Context, job *river.Job[EventCleanupArgs]) error {
	if w == nil || w.pool == nil || w.queries == nil || w.now == nil {
		return ErrEventCleanupNotConfigured
	}
	retention, err := time.ParseDuration(job.Args.Retention)
	if err != nil || retention <= 0 {
		return fmt.Errorf("invalid event retention %q", job.Args.Retention)
	}
	cutoff := w.now().UTC().Add(-retention)
	deleted, err := w.queries.DeleteUserEventsBefore(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return fmt.Errorf("delete expired user events: %w", err)
	}
	// Every StartLogin and StartQR stores a login flow that is only meaningful
	// until it expires, and nothing else removes them, so the sweep that already
	// maintains the event stream drops them here as well. Without it the table
	// grows without bound and keeps the sealed phone number and login state of
	// abandoned logins forever.
	flowsDeleted, err := w.queries.DeleteExpiredTelegramLoginFlows(ctx)
	if err != nil {
		return fmt.Errorf("delete expired Telegram login flows: %w", err)
	}
	if job.JobRow != nil {
		return river.RecordOutput(ctx, struct {
			Deleted           int64     `json:"deleted"`
			LoginFlowsDeleted int64     `json:"login_flows_deleted"`
			Cutoff            time.Time `json:"cutoff"`
		}{Deleted: deleted, LoginFlowsDeleted: flowsDeleted, Cutoff: cutoff})
	}
	return nil
}
