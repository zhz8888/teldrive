package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// TrashCleanupSweepKind is the River job kind of the periodic sweep that
// permanently deletes files that have been in the trash longer than the retention
// window.
const TrashCleanupSweepKind = "teldrive_cleanup_trash"

// ErrTrashCleanupNotConfigured is returned by Work when the worker has no pool or
// no purge service, which means the runtime did not wire it up.
var ErrTrashCleanupNotConfigured = errors.New("trash cleanup worker is not configured")

// TrashCleanupSweepArgs carries the retention window of one trash sweep. It is
// persisted as JSON in river_job, so the field name must stay stable while older
// jobs may still be queued.
type TrashCleanupSweepArgs struct {
	// Retention is the Go duration string after which a trashed file becomes
	// eligible for permanent deletion. An empty value means the 720h (30 day)
	// default, which is what the periodic schedule inserts.
	Retention string `json:"retention,omitempty"`
}

// Kind reports the River job kind handled by TrashCleanupWorker.
func (TrashCleanupSweepArgs) Kind() string { return TrashCleanupSweepKind }

// InsertOpts runs the sweep on the maintenance queue with three attempts and the
// highest priority, because expired trash still occupies Telegram storage.
func (TrashCleanupSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CleanupQueue, MaxAttempts: 3, Priority: 1}
}

// TrashCleanupWorker hands the trashed roots that outlived the retention window
// to the purge service, which is what actually frees their storage.
type TrashCleanupWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[TrashCleanupSweepArgs]
	// pool is only checked for nil: Work rejects a worker that has no pool rather
	// than panicking inside a query.
	pool *pgxpool.Pool
	// queries lists the expired trash roots.
	queries *sqlcgen.Queries
	// service performs the permanent deletion.
	service PurgeService
	// now returns the current time; tests replace it to pin the cutoff.
	now func() time.Time
}

// NewTrashCleanupWorker returns a sweep worker backed by pool and service, using
// the system clock for the cutoff. pool and service are required: Work returns
// ErrTrashCleanupNotConfigured when either is nil.
func NewTrashCleanupWorker(pool *pgxpool.Pool, service PurgeService) *TrashCleanupWorker {
	return &TrashCleanupWorker{pool: pool, queries: sqlcgen.New(pool), service: service, now: time.Now}
}

// Timeout allows two hours, because one run purges every expired root instead of
// a single page.
func (w *TrashCleanupWorker) Timeout(*river.Job[TrashCleanupSweepArgs]) time.Duration {
	return 2 * time.Hour
}

// Work permanently deletes the trashed roots that have outlived the retention
// window, at most 1000 rows per page, until no expired root is left.
//
// The cutoff is computed once per run, and the listing only returns roots whose
// own parent is not trashed as well, so a deleted folder subtree is purged once
// from its top. The sweep is idempotent: a purged root disappears from the
// listing, and a page that fails is retried by River with whatever is left. It
// returns ErrTrashCleanupNotConfigured when the worker is not wired up, and
// rejects a retention that does not parse or is not positive.
func (w *TrashCleanupWorker) Work(ctx context.Context, job *river.Job[TrashCleanupSweepArgs]) error {
	if w == nil || w.pool == nil || w.service == nil {
		return ErrTrashCleanupNotConfigured
	}
	retentionText := job.Args.Retention
	if retentionText == "" {
		retentionText = "720h"
	}
	retention, err := time.ParseDuration(retentionText)
	if err != nil || retention <= 0 {
		return fmt.Errorf("invalid trash retention %q", retentionText)
	}
	deletedBefore := dbtypes.Time(w.now().Add(-retention))
	for {
		rows, err := w.queries.ListTrashedRootsBefore(ctx, deletedBefore)
		if err != nil {
			return fmt.Errorf("list expired trash roots: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		byUser := make(map[int64][]uuid.UUID)
		for _, item := range rows {
			fileID, ok := dbtypes.GoogleUUID(item.FileID)
			if !ok {
				return errors.New("expired trash root has invalid file ID")
			}
			byUser[item.UserID] = append(byUser[item.UserID], fileID)
		}
		userIDs := slices.Sorted(maps.Keys(byUser))
		for _, userID := range userIDs {
			if err := w.service.PurgeMany(ctx, userID, byUser[userID]); err != nil {
				return fmt.Errorf("purge expired trash files for user %d: %w", userID, err)
			}
		}
	}
}
