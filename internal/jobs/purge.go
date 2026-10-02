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

const (
	// PurgeSweepKind is the River job kind of the periodic sweep that finishes the
	// deletion of files left in the deletion_pending state.
	PurgeSweepKind = "teldrive_purge_pending_files"
	// PurgeQueue is the River queue the purge sweep runs on. It points at
	// CleanupQueue today, so purges share the maintenance queue.
	PurgeQueue = CleanupQueue
)

// ErrPurgeNotConfigured is returned by Work when the worker has no pool or no
// purge service, which means the runtime did not wire it up.
var ErrPurgeNotConfigured = errors.New("pending-file purge worker is not configured")

// PurgeService is the deletion behavior the purge sweep depends on; it is
// implemented by fileops.Service, which also owns the Telegram message deletion.
type PurgeService interface {
	// Purge deletes the file identified by fileID and its whole subtree for
	// userID, releasing the Telegram messages that back it. It reports
	// fileops.ErrNotFound when the root is already gone and fileops.ErrNotTrashed
	// when the root is neither trashed nor deletion_pending.
	Purge(context.Context, int64, uuid.UUID) error
	// PurgeMany deletes several roots in one call and is the form the sweep uses.
	// Implementations may skip roots that another purge already holds a lock on,
	// so a nil error does not guarantee that every root was removed.
	PurgeMany(context.Context, int64, []uuid.UUID) error
}

// PurgeSweepArgs is the empty payload of a purge sweep: the sweep always looks up
// every deletion_pending root itself.
type PurgeSweepArgs struct{}

// Kind reports the River job kind handled by PendingFilePurgeWorker.
func (PurgeSweepArgs) Kind() string { return PurgeSweepKind }

// InsertOpts runs the sweep on the purge queue with three attempts and the
// highest maintenance priority, because the files it deletes still occupy
// Telegram storage.
func (PurgeSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: PurgeQueue, MaxAttempts: 3, Priority: 1}
}

// PendingFilePurgeWorker retries the final deletion of files that stayed in the
// deletion_pending state, for example because Telegram was unreachable while the
// user deleted them or because the process died mid-purge.
type PendingFilePurgeWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[PurgeSweepArgs]
	// pool is only checked for nil: Work rejects a worker that has no pool rather
	// than panicking inside a query.
	pool *pgxpool.Pool
	// queries lists the roots whose deletion never finished.
	queries *sqlcgen.Queries
	// service performs the actual deletion.
	service PurgeService
}

// NewPendingFilePurgeWorker returns a sweep worker backed by pool and service.
// Both are required: Work returns ErrPurgeNotConfigured when either is nil.
func NewPendingFilePurgeWorker(pool *pgxpool.Pool, service PurgeService) *PendingFilePurgeWorker {
	return &PendingFilePurgeWorker{pool: pool, queries: sqlcgen.New(pool), service: service}
}

// Timeout allows two hours, because one run drains every pending root rather than
// a single page.
func (w *PendingFilePurgeWorker) Timeout(*river.Job[PurgeSweepArgs]) time.Duration {
	return 2 * time.Hour
}

// Work repeatedly lists deletion_pending roots, at most 1000 per page, and asks
// the purge service to delete them, until the listing comes back empty.
//
// Roots are grouped per user and purged in user order, so one call carries all of
// a user's pending roots. The sweep is idempotent: a root stays deletion_pending
// until its deletion actually completes, so an interrupted run is picked up
// again, and River retries the job when a purge fails. A root whose own parent is
// still deletion_pending is not listed by the query and is handled once its
// parent is gone.
func (w *PendingFilePurgeWorker) Work(ctx context.Context, job *river.Job[PurgeSweepArgs]) error {
	if w == nil || w.pool == nil || w.service == nil {
		return ErrPurgeNotConfigured
	}
	for {
		rows, err := w.queries.ListDeletionPendingRoots(ctx)
		if err != nil {
			return fmt.Errorf("list deletion-pending roots: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		byUser := make(map[int64][]uuid.UUID)
		for _, item := range rows {
			fileID, ok := dbtypes.GoogleUUID(item.FileID)
			if !ok {
				return fmt.Errorf("list deletion-pending roots: invalid file ID")
			}
			byUser[item.UserID] = append(byUser[item.UserID], fileID)
		}
		userIDs := slices.Sorted(maps.Keys(byUser))
		for _, userID := range userIDs {
			if err := w.service.PurgeMany(ctx, userID, byUser[userID]); err != nil {
				return fmt.Errorf("retry deletion-pending files for user %d: %w", userID, err)
			}
		}
	}
}
