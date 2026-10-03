package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/divyam234/riverpro"
	"github.com/divyam234/riverpro/driver/riverpropgxv5"
	"github.com/divyam234/riverpro/riverencrypt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

const (
	// uploadCleanupPeriodicID is the durable periodic job ID of the upload
	// cleanup sweep. It is stable across restarts so an operator's schedule is
	// reused instead of being recreated.
	uploadCleanupPeriodicID = "teldrive-upload-cleanup"
	// eventCleanupPeriodicID is the durable periodic job ID of the replayable
	// user event cleanup sweep.
	eventCleanupPeriodicID = "teldrive-user-event-cleanup"
	// trashCleanupPeriodicID is the durable periodic job ID of the trash cleanup
	// sweep that expires trashed files.
	trashCleanupPeriodicID = "teldrive-trash-cleanup"
	// purgePeriodicID is the durable periodic job ID of the sweep that finishes
	// permanent deletion of files already marked deletion-pending.
	purgePeriodicID = "teldrive-pending-file-purge"
	// orphanCleanupPeriodicID is the durable periodic job ID of the sweep that
	// deletes Telegram documents no file or upload part references any more.
	orphanCleanupPeriodicID = "teldrive-orphaned-telegram-part-cleanup"
	// uploadCleanupDefaultCron runs the upload cleanup sweep every 12 hours.
	uploadCleanupDefaultCron = "@every 12h"
	// eventCleanupDefaultCron runs the user event cleanup once a day at midnight.
	eventCleanupDefaultCron = "0 0 * * *"
	// eventCleanupDefaultRetention is how long replayable user events are kept,
	// expressed as a Go duration string that EventCleanupArgs parses.
	eventCleanupDefaultRetention = "48h"
	// trashCleanupDefaultCron runs the trash cleanup sweep every 12 hours.
	trashCleanupDefaultCron = "@every 12h"
	// pendingDeletionCleanupDefaultCron runs the pending deletion purge every 12
	// hours.
	pendingDeletionCleanupDefaultCron = "@every 12h"
	// orphanCleanupDefaultCron runs the orphaned Telegram part cleanup every 14
	// days; the sweep is intentionally rare because it scans channel history.
	orphanCleanupDefaultCron = "@every 336h"
	// maintenanceTimezone is the timezone the cron expressions above are
	// evaluated in. UTC keeps the schedule independent of the host clock.
	maintenanceTimezone = "UTC"
	// maintenanceWorkers caps how many maintenance queue jobs run concurrently.
	maintenanceWorkers = 2
	// cleanupPeriodicID is retained as a compatibility alias for
	// uploadCleanupPeriodicID.
	//
	// Deprecated: use uploadCleanupPeriodicID instead.
	cleanupPeriodicID = uploadCleanupPeriodicID // deprecated compatibility name
)

// ErrRuntimeNotConfigured reports that a Runtime method was called on a nil
// runtime or on one that is missing the client, pool or optional feature the
// operation needs, for example InsertPurge on a runtime built without a purge
// service. It is an expected condition rather than a fault, so callers test it
// with errors.Is and may safely ignore it.
var ErrRuntimeNotConfigured = errors.New("job runtime is not configured")

// Runtime owns the RiverPro client that executes TelDrive's background jobs and
// exposes the job management operations used by the HTTP layer. The composition
// root creates exactly one instance and shares it with every request, so all
// methods are safe for concurrent use; Start and Stop are additionally
// serialized so the runtime is started or stopped at most once at a time.
type Runtime struct {
	// client is the RiverPro client; nil only for a zero-value Runtime.
	client *riverpro.Client[pgx.Tx]
	// pool backs the queries River's own API cannot express: per-user statistics,
	// purges and the transactional job retry.
	pool *pgxpool.Pool
	// schema is the PostgreSQL schema that holds the River tables.
	schema string
	// purgeEnabled reports whether a PurgeService was supplied, in which case the
	// pending deletion purge and trash cleanup workers exist and their periodic
	// jobs are scheduled.
	purgeEnabled bool
	// orphanCleanupEnabled reports whether the storage backend can list its
	// documents, which the orphaned Telegram part cleanup requires.
	orphanCleanupEnabled bool
	// botProvisionEnabled reports whether storage, bot service and encryptor are
	// all available for Telegram bot provisioning.
	botProvisionEnabled bool
	// uploadEnabled reports whether the catalog, upload and transfer services the
	// upload workers need were supplied.
	uploadEnabled bool
	// mu serializes Start and Stop; the River client itself is concurrency safe.
	mu sync.Mutex
	// started reports whether Start completed and Stop has not run since.
	started bool
	// cancel cancels the context handed to the client on Start; nil before Start
	// and after a completed Stop.
	cancel context.CancelFunc
}

// NewRuntime builds a runtime on the "teldrive" schema with the workers that
// only need the pool and storage. Bot provisioning and the upload workers stay
// disabled because they need extra collaborators; see
// NewRuntimeWithSchemaAndBotProvision and NewRuntimeWithServices. Purge support
// is enabled when a purgeServices implementation is given. It returns
// ErrRuntimeNotConfigured when pool or storage is nil and wraps any worker
// registration failure.
func NewRuntime(pool *pgxpool.Pool, storage telegramstore.Storage, purgeServices ...PurgeService) (*Runtime, error) {
	return NewRuntimeWithSchema(pool, storage, "teldrive", purgeServices...)
}

// NewRuntimeWithSchema is NewRuntime for a deployment that stores its tables in
// a non-default schema. The 7 day session TTL makes the orphaned part cleanup
// treat unreferenced Telegram documents older than 7 days as deletable.
func NewRuntimeWithSchema(pool *pgxpool.Pool, storage telegramstore.Storage, schema string, purgeServices ...PurgeService) (*Runtime, error) {
	return newRuntimeWithSchema(pool, storage, schema, nil, nil, 7*24*time.Hour, nil, purgeServices)
}

// NewRuntimeWithSchemaAndBotProvision also enables Telegram bot provisioning,
// which requires storage to implement telegramstore.BotInviter as well as a
// non-nil botService and encryptor; the encryptor is installed as a River hook
// so bot tokens are encrypted at rest in the job arguments. uploadSessionTTL
// becomes the minimum age of an unreferenced Telegram document before the orphan
// cleanup may delete it, falling back to 7 days when it is not positive. The
// upload workers stay disabled.
func NewRuntimeWithSchemaAndBotProvision(pool *pgxpool.Pool, storage telegramstore.Storage, schema string, botService *bots.Service, encryptor riverencrypt.Encryptor, uploadSessionTTL time.Duration, purgeServices ...PurgeService) (*Runtime, error) {
	return newRuntimeWithSchema(pool, storage, schema, botService, encryptor, uploadSessionTTL, nil, purgeServices)
}

// UploaderServices bundles the collaborators of the upload workers. A field that
// is left nil disables the feature that needs it: without Catalog, Uploads or
// Pipeline no upload worker is registered at all.
type UploaderServices struct {
	// Catalog resolves destination folders and records the imported files.
	Catalog *catalog.Service
	// Uploads creates and resumes the upload sessions used by the transfer
	// pipeline.
	Uploads *uploads.Service
	// Pipeline performs the actual part transfers.
	Pipeline *transfer.Pipeline
	// HTTPClient is used for HTTP sources and for fetching remote objects; nil
	// selects a default client.
	HTTPClient *http.Client
	// ActiveKeyVersion is the encryption key version new uploads are written
	// with.
	ActiveKeyVersion int32
	// MaxChunkSize is the largest part a background import may ask for, and
	// normally the uploads service's configured max-part-size: the chunk the
	// worker picks becomes the session's part size, which that service
	// refuses if it is too large. Clamping here turns a configuration the
	// operator chose into a smaller chunk instead of a failed import.
	// A non-positive value leaves the package default in place.
	MaxChunkSize int64
	// LocalImportRoots are the directories a local import may read from. An empty
	// list disables local imports entirely, and a requested path outside every
	// root is rejected, which is the security boundary for local imports.
	LocalImportRoots []string
}

// NewRuntimeWithServices is the composition root's constructor: it enables the
// upload workers when uploader carries a catalog, upload service and pipeline,
// and enables everything NewRuntimeWithSchemaAndBotProvision enables. It returns
// ErrRuntimeNotConfigured when pool or storage is nil and wraps any worker
// registration failure.
func NewRuntimeWithServices(pool *pgxpool.Pool, storage telegramstore.Storage, schema string, botService *bots.Service, encryptor riverencrypt.Encryptor, uploadSessionTTL time.Duration, uploader UploaderServices, purgeServices ...PurgeService) (*Runtime, error) {
	return newRuntimeWithSchema(pool, storage, schema, botService, encryptor, uploadSessionTTL, &uploader, purgeServices)
}

// newRuntimeWithSchema validates the dependencies, registers every worker whose
// collaborators are available and builds the RiverPro client with durable
// periodic jobs enabled.
//
// Registration is feature gated: without a purge service the pending deletion
// purge and trash cleanup workers are absent, without a document lister the
// orphan cleanup is absent, and without bot service, encryptor and an inviter
// the bot provisioning worker is absent. The maintenance queue runs at most
// maintenanceWorkers jobs concurrently and the upload queue at most two, with a
// 30 second soft stop so in-flight jobs get a chance to finish. A nil pool or
// storage yields ErrRuntimeNotConfigured, and the first worker registration
// failure aborts construction with the client unbuilt.
func newRuntimeWithSchema(pool *pgxpool.Pool, storage telegramstore.Storage, schema string, botService *bots.Service, encryptor riverencrypt.Encryptor, uploadSessionTTL time.Duration, uploader *UploaderServices, purgeServices []PurgeService) (*Runtime, error) {
	if pool == nil || storage == nil {
		return nil, ErrRuntimeNotConfigured
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, NewUploadCleanupWorker(pool, storage)); err != nil {
		return nil, fmt.Errorf("register upload cleanup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, NewEventCleanupWorker(pool)); err != nil {
		return nil, fmt.Errorf("register event cleanup worker: %w", err)
	}
	var purgeService PurgeService
	if len(purgeServices) > 0 {
		purgeService = purgeServices[0]
	}
	if purgeService != nil {
		if err := river.AddWorkerSafely(workers, NewPendingFilePurgeWorker(pool, purgeService)); err != nil {
			return nil, fmt.Errorf("register purge worker: %w", err)
		}
		if err := river.AddWorkerSafely(workers, NewTrashCleanupWorker(pool, purgeService)); err != nil {
			return nil, fmt.Errorf("register trash cleanup worker: %w", err)
		}
	}
	lister, orphanCleanupEnabled := storage.(telegramstore.DocumentMessageLister)
	if orphanCleanupEnabled {
		minimumAge := 7 * 24 * time.Hour
		if uploadSessionTTL > 0 {
			minimumAge = uploadSessionTTL
		}
		if err := river.AddWorkerSafely(workers, NewOrphanedTelegramPartsCleanupWorker(pool, storage, lister, minimumAge)); err != nil {
			return nil, fmt.Errorf("register orphan cleanup worker: %w", err)
		}
	}
	inviter, hasInviter := storage.(telegramstore.BotInviter)
	botProvisionEnabled := hasInviter && botService != nil && encryptor != nil
	if botProvisionEnabled {
		if err := river.AddWorkerSafely(workers, NewBotProvisionWorker(pool, botService, inviter)); err != nil {
			return nil, fmt.Errorf("register bot provisioning worker: %w", err)
		}
	}
	uploadEnabled := uploader != nil && uploader.Catalog != nil && uploader.Uploads != nil && uploader.Pipeline != nil
	if uploadEnabled {
		if uploader.HTTPClient == nil {
			uploader.HTTPClient = NewUploadHTTPClient()
		}
		if err := river.AddWorkerSafely(workers, NewUploadBatchWorker(uploader.HTTPClient, uploader.Catalog, uploader.MaxChunkSize, uploader.LocalImportRoots)); err != nil {
			return nil, fmt.Errorf("register upload batch worker: %w", err)
		}
		if err := river.AddWorkerSafely(workers, NewUploadSourceWorker(pool, uploader.Catalog, uploader.Uploads, uploader.Pipeline, uploader.HTTPClient, uploader.ActiveKeyVersion, uploader.MaxChunkSize)); err != nil {
			return nil, fmt.Errorf("register upload source worker: %w", err)
		}
	}
	riverConfig := river.Config{
		Schema:  schema,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			CleanupQueue: {MaxWorkers: maintenanceWorkers},
			UploadQueue:  {MaxWorkers: 2},
		},
		SoftStopTimeout: 30 * time.Second,
	}
	if botProvisionEnabled {
		riverConfig.Hooks = append(riverConfig.Hooks, riverencrypt.NewEncryptHookConfig(&riverencrypt.EncryptHookConfig{
			Encryptor:       encryptor,
			JobKindsInclude: []string{BotProvisionKind},
		}))
	}
	client, err := riverpro.NewClient(riverpropgxv5.New(pool), &riverpro.Config{
		Config: riverConfig,
		DurablePeriodicJobs: riverpro.DurablePeriodicJobsConfig{
			Enabled:      true,
			PollInterval: time.Second,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create RiverPro client: %w", err)
	}
	return &Runtime{
		client: client, pool: pool, schema: schema,
		purgeEnabled: purgeService != nil, orphanCleanupEnabled: orphanCleanupEnabled, botProvisionEnabled: botProvisionEnabled, uploadEnabled: uploadEnabled,
	}, nil
}

// pausePeriodicJobIfPresent pauses a fixed schedule that exists and is running,
// so a feature this runtime did not enable stops enqueueing jobs no worker can
// handle. A schedule that was never created needs no work, and one an operator
// paused already is left as it is.
func (r *Runtime) pausePeriodicJobIfPresent(ctx context.Context, id string) error {
	row, err := r.client.PeriodicJobGet(ctx, id)
	if errors.Is(err, river.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load periodic job %q: %w", id, err)
	}
	if row.PausedAt != nil {
		return nil
	}
	if _, err := r.client.PeriodicJobPause(ctx, id); err != nil {
		return fmt.Errorf("pause periodic job %q of a disabled feature: %w", id, err)
	}
	return nil
}

// Start registers the durable periodic jobs and starts the RiverPro client's
// workers. It is idempotent: once started it returns nil without touching the
// client, and concurrent callers are serialized by the runtime mutex.
//
// The periodic jobs are created only for the features this runtime enabled, and
// only when they do not exist yet: riverpro.ErrPeriodicJobAlreadyExists is
// treated as success, so restarting a server keeps an operator's edits to the
// schedule instead of resetting them. They are written before the client starts
// and therefore survive a failed start. The cron expressions run in UTC and
// order the sweeps by urgency: trash cleanup and the pending deletion purge are
// priority 1, upload and event cleanup priority 2, and the expensive orphan
// cleanup priority 3. The supplied context becomes the parent of the workers'
// context; cancelling it or calling Stop shuts them down.
func (r *Runtime) Start(ctx context.Context) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	uploadCleanupArgs, err := json.Marshal(UploadCleanupSweepArgs{})
	if err != nil {
		return fmt.Errorf("marshal upload cleanup periodic args: %w", err)
	}
	if _, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
		ID:          uploadCleanupPeriodicID,
		Kind:        UploadCleanupSweepKind,
		Args:        uploadCleanupArgs,
		Queue:       CleanupQueue,
		Priority:    2,
		MaxAttempts: 3,
		Schedule: &riverpro.PeriodicJobSchedule{
			CronExpression: uploadCleanupDefaultCron,
			CronTimezone:   maintenanceTimezone,
		},
	}); err != nil && !errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
		return fmt.Errorf("upsert upload cleanup periodic job: %w", err)
	}
	eventCleanupArgs, err := json.Marshal(EventCleanupArgs{Retention: eventCleanupDefaultRetention})
	if err != nil {
		return fmt.Errorf("marshal event cleanup periodic args: %w", err)
	}
	if _, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
		ID:          eventCleanupPeriodicID,
		Kind:        EventCleanupKind,
		Args:        eventCleanupArgs,
		Queue:       CleanupQueue,
		Priority:    2,
		MaxAttempts: 3,
		Schedule: &riverpro.PeriodicJobSchedule{
			CronExpression: eventCleanupDefaultCron,
			CronTimezone:   maintenanceTimezone,
		},
	}); err != nil && !errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
		return fmt.Errorf("upsert event cleanup periodic job: %w", err)
	}
	if r.purgeEnabled {
		trashCleanupArgs, err := json.Marshal(TrashCleanupSweepArgs{Retention: "720h"})
		if err != nil {
			return fmt.Errorf("marshal trash cleanup periodic args: %w", err)
		}
		if _, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
			ID:          trashCleanupPeriodicID,
			Kind:        TrashCleanupSweepKind,
			Args:        trashCleanupArgs,
			Queue:       CleanupQueue,
			Priority:    1,
			MaxAttempts: 3,
			Schedule: &riverpro.PeriodicJobSchedule{
				CronExpression: trashCleanupDefaultCron,
				CronTimezone:   maintenanceTimezone,
			},
		}); err != nil && !errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
			return fmt.Errorf("upsert trash cleanup periodic job: %w", err)
		}

		purgeArgs, err := json.Marshal(PurgeSweepArgs{})
		if err != nil {
			return fmt.Errorf("marshal purge periodic args: %w", err)
		}
		if _, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
			ID:          purgePeriodicID,
			Kind:        PurgeSweepKind,
			Args:        purgeArgs,
			Queue:       PurgeQueue,
			Priority:    1,
			MaxAttempts: 3,
			Schedule: &riverpro.PeriodicJobSchedule{
				CronExpression: pendingDeletionCleanupDefaultCron,
				CronTimezone:   maintenanceTimezone,
			},
		}); err != nil && !errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
			return fmt.Errorf("upsert purge periodic job: %w", err)
		}
	}
	if r.orphanCleanupEnabled {
		orphanCleanupArgs, err := json.Marshal(OrphanCleanupArgs{})
		if err != nil {
			return fmt.Errorf("marshal orphan cleanup periodic args: %w", err)
		}
		if _, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
			ID:          orphanCleanupPeriodicID,
			Kind:        OrphanCleanupKind,
			Args:        orphanCleanupArgs,
			Queue:       CleanupQueue,
			Priority:    3,
			MaxAttempts: 3,
			Schedule: &riverpro.PeriodicJobSchedule{
				CronExpression: orphanCleanupDefaultCron,
				CronTimezone:   maintenanceTimezone,
			},
		}); err != nil && !errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
			return fmt.Errorf("upsert orphan cleanup periodic job: %w", err)
		}
	}
	// A feature that was switched off after its schedule existed leaves a durable
	// row behind, because this code only ever inserts. RiverPro enqueues from that
	// row without checking that a worker exists, so the job fails with an unknown
	// kind on every run and fills the task list with failures. Pausing the
	// schedules of the features this runtime did not enable keeps the rows and any
	// operator edit to them while stopping the enqueueing.
	for _, schedule := range []struct {
		id      string
		enabled bool
	}{
		{trashCleanupPeriodicID, r.purgeEnabled},
		{purgePeriodicID, r.purgeEnabled},
		{orphanCleanupPeriodicID, r.orphanCleanupEnabled},
	} {
		if schedule.enabled {
			continue
		}
		if err := r.pausePeriodicJobIfPresent(ctx, schedule.id); err != nil {
			return err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := r.client.Start(runCtx); err != nil {
		cancel()
		return fmt.Errorf("start RiverPro client: %w", err)
	}
	r.cancel = cancel
	r.started = true
	return nil
}

// Stop stops the RiverPro client, which stops handing out jobs and waits for the
// in-flight ones, cancelling whatever is still running once the 30 second soft
// stop timeout expires; the workers' context is cancelled afterwards, once the
// client has actually stopped. It returns nil when the runtime was never started
// or is already stopped, so it is safe to call during shutdown unconditionally.
//
// If ctx is cancelled before the client has drained, Stop returns the context
// error and leaves the runtime marked as started with its cancellation still in
// place, so a later call can retry the shutdown.
func (r *Runtime) Stop(ctx context.Context) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	if err := r.client.Stop(ctx); err != nil {
		return fmt.Errorf("stop RiverPro client: %w", err)
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.started = false
	return nil
}

// InsertUploadCleanup enqueues an upload cleanup sweep immediately, in addition
// to the periodic schedule. It is used when an operator wants the sweep to run
// now, for example after changing the upload session TTL, and returns
// ErrRuntimeNotConfigured when the runtime has no client.
func (r *Runtime) InsertUploadCleanup(ctx context.Context) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	if _, err := r.client.Insert(ctx, UploadCleanupSweepArgs{}, nil); err != nil {
		return fmt.Errorf("insert upload cleanup sweep: %w", err)
	}
	return nil
}

// InsertCleanup is kept for callers using the previous generic name.
func (r *Runtime) InsertCleanup(ctx context.Context) error {
	return r.InsertUploadCleanup(ctx)
}

// InsertPurge enqueues a pending deletion purge sweep immediately, so a user who
// empties the trash does not have to wait for the periodic run. Purge is only
// available when the runtime was built with a purge service; otherwise it
// returns ErrRuntimeNotConfigured, which callers may ignore.
func (r *Runtime) InsertPurge(ctx context.Context) error {
	if r == nil || r.client == nil || !r.purgeEnabled {
		return ErrRuntimeNotConfigured
	}
	if _, err := r.client.Insert(ctx, PurgeSweepArgs{}, nil); err != nil {
		return fmt.Errorf("insert purge sweep: %w", err)
	}
	return nil
}

// InsertBotProvision enqueues a bot provisioning job for userID and returns the
// ID of the job that will do the work, formatted as a decimal string, or an
// empty string when there is nothing to do.
//
// It requires bot provisioning to be enabled and a positive user ID, and returns
// ErrRuntimeNotConfigured otherwise. Non-positive and duplicate bot IDs are
// dropped; when none survive the call is a successful no-op. Insertion is
// deduplicated by job arguments, so a repeated request while a job is still
// pending returns that pending job's ID instead of queueing a second one.
func (r *Runtime) InsertBotProvision(ctx context.Context, userID int64, botIDs []int64) (string, error) {
	if r == nil || r.client == nil || !r.botProvisionEnabled || userID <= 0 {
		return "", ErrRuntimeNotConfigured
	}
	botIDs = normalizedBotIDs(botIDs)
	if len(botIDs) == 0 {
		return "", nil
	}
	result, err := r.client.Insert(ctx, BotProvisionArgs{UserID: userID, BotIDs: botIDs}, nil)
	if err != nil {
		return "", fmt.Errorf("insert bot provisioning job: %w", err)
	}
	return fmt.Sprintf("%d", result.Job.ID), nil
}

// InsertUploadBatch enqueues an upload batch on the upload queue and returns the
// stored job, so the caller can report its ID and let the client poll progress.
// A random batch ID is generated when args carries none, without modifying the
// caller's copy. It returns ErrRuntimeNotConfigured when the upload workers were
// not configured or when the job would be meaningless, that is without a
// positive user ID or without any source.
func (r *Runtime) InsertUploadBatch(ctx context.Context, args UploadBatchArgs) (Job, error) {
	if r == nil || r.client == nil || !r.uploadEnabled || args.UserID <= 0 || len(args.Sources) == 0 {
		return Job{}, ErrRuntimeNotConfigured
	}
	if strings.TrimSpace(args.BatchID) == "" {
		args.BatchID = uuid.NewString()
	}
	result, err := r.client.Insert(ctx, args, nil)
	if err != nil {
		return Job{}, fmt.Errorf("insert upload batch: %w", err)
	}
	return jobFromRiver(result.Job), nil
}
