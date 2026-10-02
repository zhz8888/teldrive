package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/divyam234/riverpro"
	prodriver "github.com/divyam234/riverpro/driver"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
)

// ErrInvalidJobState reports that an operation was refused because of the state
// the target job is in, so nothing would change: a purge of a state that is not
// finalized (cancelled, completed or discarded), or a retry of a job River leaves
// untouched because it is running or already queued. Callers must test it with
// errors.Is and answer with a client conflict error rather than a server failure.
var ErrInvalidJobState = errors.New("invalid job state")

// CreateInput describes a one-off job inserted by an administrator. Only the
// built-in cleanup sweep kinds are accepted; leaving Queue, Priority and
// MaxAttempts at their zero values takes the River defaults.
type CreateInput struct {
	// Kind is the worker kind to run. It must be one of the cleanup sweep kinds
	// this runtime registered a worker for, as checked by Runtime.Create.
	Kind string
	// Args holds the job arguments as raw JSON keyed by JSON field name; nil is
	// stored as an empty object.
	Args map[string]json.RawMessage
	// Queue is the target queue; empty selects River's default queue.
	Queue string
	// Priority is the River priority, 1 being the highest and 4 the lowest; zero
	// or less selects the default.
	Priority int
	// MaxAttempts is the attempt budget; zero or less selects River's default.
	MaxAttempts int
	// Tags are optional labels used for filtering.
	Tags []string
}

// Queue is one River queue together with the counters the job dashboard shows.
// The counters are a snapshot taken while the listing runs and may already be
// stale when the caller renders them.
type Queue struct {
	// Name is the queue name; only queues that have held at least one job are
	// tracked by River.
	Name string
	// Paused reports whether the queue is paused, that is its jobs are not handed
	// out until it is resumed.
	Paused bool
	// Available is the number of jobs ready to run in this queue.
	Available int64
	// Running is the number of jobs of this queue currently being worked.
	Running int64
	// Retryable is the number of failed jobs waiting for another attempt. Only
	// the per-user listing fills it in; the administrator listing leaves it zero
	// because River's queue counter does not report that state.
	Retryable int64
	// Scheduled is the number of jobs waiting for their scheduled time. Only the
	// per-user listing fills it in; the administrator listing leaves it zero
	// because River's queue counter does not report that state.
	Scheduled int64
}

// PeriodicSchedule is when a periodic job runs. CronExpression accepts a
// five-field cron expression or River's "@every <duration>" shorthand, and
// CronTimezone is an IANA name such as "UTC"; a blank timezone becomes UTC.
type PeriodicSchedule struct {
	// CronExpression is the schedule, for example "0 0 * * *" or "@every 12h".
	CronExpression string
	// CronTimezone is the timezone the expression is evaluated in; blank means
	// UTC.
	CronTimezone string
}

// PeriodicJob is a stored periodic job definition plus its runtime bookkeeping,
// as returned by the management API. All timestamps are normalized to UTC.
type PeriodicJob struct {
	// ID is the durable identifier; update, pause, resume and delete refer to it.
	ID string
	// Kind is the worker kind that each run uses.
	Kind string
	// Args are the arguments inserted with every run, as raw JSON keyed by JSON
	// field name.
	Args map[string]json.RawMessage
	// Queue is the queue the runs are inserted into.
	Queue string
	// Priority is the River priority of the runs, 1 being the highest.
	Priority int
	// MaxAttempts is the attempt budget of each run.
	MaxAttempts int
	// Tags are the labels attached to each run.
	Tags []string
	// Schedule is the cron expression and timezone that produce the runs.
	Schedule PeriodicSchedule
	// NextRunAt is when the next run is due, in UTC.
	NextRunAt time.Time
	// Paused reports whether the schedule is suspended; PausedAt then carries the
	// time it was suspended.
	Paused bool
	// PausedAt is when the schedule was paused, or nil while it is active.
	PausedAt *time.Time
	// CreatedAt is when the definition was first stored, in UTC.
	CreatedAt time.Time
	// UpdatedAt is when the definition was last changed, in UTC.
	UpdatedAt time.Time
}

// PeriodicJobInput is the desired state of a periodic job for CreatePeriodicJob
// and UpdatePeriodicJob. Zero-valued Queue, Priority, MaxAttempts and timezone
// are replaced with defaults before the definition is stored, so the stored
// definition is always fully populated.
type PeriodicJobInput struct {
	// ID is the identifier of the definition to create or update.
	ID string
	// Kind is the worker kind that each run uses; it must be one of the cleanup
	// sweep kinds this runtime registered a worker for.
	Kind string
	// Args holds the arguments inserted with every run, as raw JSON keyed by JSON
	// field name; nil is stored as an empty object.
	Args map[string]json.RawMessage
	// Queue is the queue the runs are inserted into; blank selects the default
	// queue.
	Queue string
	// Priority is the River priority of the runs, 1 being the highest; zero or
	// less selects the default.
	Priority int
	// MaxAttempts is the attempt budget of each run; zero or less selects the
	// default.
	MaxAttempts int
	// Tags are the labels attached to each run.
	Tags []string
	// Schedule is the requested cron expression and timezone.
	Schedule PeriodicSchedule
	// Paused requests that the definition be left or put in the paused state.
	Paused bool
}

// PeriodicTemplate is a built-in periodic job offered by the catalog, together
// with the defaults the console prefills when an operator schedules it. An
// implementation feature that is disabled removes its templates from the
// catalog entirely.
type PeriodicTemplate struct {
	// ID is the identifier the periodic job is created with.
	ID string
	// Label is the human readable name shown in the console.
	Label string
	// Description explains what the schedule does.
	Description string
	// Kind is the worker kind the schedule inserts.
	Kind string
	// DefaultArgs are the arguments the schedule is created with.
	DefaultArgs map[string]json.RawMessage
	// DefaultQueue is the queue the runs are inserted into.
	DefaultQueue string
	// DefaultPriority is the River priority of the runs, 1 being the highest.
	DefaultPriority int
	// DefaultMaxAttempts is the attempt budget of each run.
	DefaultMaxAttempts int
	// DefaultTags are the labels attached to each run.
	DefaultTags []string
	// DefaultCronExpression is the recommended schedule.
	DefaultCronExpression string
	// DefaultCronTimezone is the timezone the recommended schedule is evaluated
	// in.
	DefaultCronTimezone string
}

// Get returns the job with the given ID. It returns river.ErrNotFound for an
// unknown ID so callers can map it to a 404, and ErrRuntimeNotConfigured when
// the runtime has no client.
func (r *Runtime) Get(ctx context.Context, id int64) (Job, error) {
	if r == nil || r.client == nil {
		return Job{}, ErrRuntimeNotConfigured
	}
	row, err := r.client.JobGet(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return jobFromRiver(row), nil
}

// ListQueues returns every queue River knows about, at most 1000, sorted by
// name, with its paused flag and counters. River can only count available and
// running jobs per queue in one query, so Retryable and Scheduled stay zero
// here; ListQueuesForUser computes all four states from the job table instead.
func (r *Runtime) ListQueues(ctx context.Context) ([]Queue, error) {
	if r == nil || r.client == nil {
		return nil, ErrRuntimeNotConfigured
	}
	executor := r.client.Driver().GetExecutor()
	rows, err := executor.QueueList(ctx, &riverdriver.QueueListParams{Max: 1000, Schema: r.schema})
	if err != nil {
		return nil, fmt.Errorf("list queues: %w", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	counts, err := executor.JobCountByQueueAndState(ctx, &riverdriver.JobCountByQueueAndStateParams{
		QueueNames: names,
		Schema:     r.schema,
	})
	if err != nil {
		return nil, fmt.Errorf("count queue jobs: %w", err)
	}
	countByQueue := make(map[string]*riverdriver.JobCountByQueueAndStateResult, len(counts))
	for _, count := range counts {
		countByQueue[count.Queue] = count
	}
	result := make([]Queue, 0, len(rows))
	for _, row := range rows {
		queue := Queue{Name: row.Name, Paused: row.PausedAt != nil}
		if count := countByQueue[row.Name]; count != nil {
			queue.Available = int64(count.CountAvailable)
			queue.Running = int64(count.CountRunning)
		}
		result = append(result, queue)
	}
	slices.SortFunc(result, func(a, b Queue) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

// ListQueuesForUser returns only the queues that hold at least one job belonging
// to userID, with available, running, retryable and scheduled counts, and merges
// in the global paused flag from ListQueues so a user can see why their jobs are
// not moving. Jobs without a user_id are invisible here. It returns
// ErrRuntimeNotConfigured when the runtime has no pool or userID is not positive.
func (r *Runtime) ListQueuesForUser(ctx context.Context, userID int64) ([]Queue, error) {
	if r == nil || r.pool == nil || userID <= 0 {
		return nil, ErrRuntimeNotConfigured
	}
	jobTable := pgx.Identifier{r.schema, "river_job"}.Sanitize()
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT queue,
       count(*) FILTER (WHERE state::text = 'available'),
       count(*) FILTER (WHERE state::text = 'running'),
       count(*) FILTER (WHERE state::text = 'retryable'),
       count(*) FILTER (WHERE state::text = 'scheduled')
FROM %s
WHERE args->>'user_id' = $1
GROUP BY queue
ORDER BY queue`, jobTable), fmt.Sprintf("%d", userID))
	if err != nil {
		return nil, fmt.Errorf("list user queues: %w", err)
	}
	defer rows.Close()
	result := make([]Queue, 0)
	for rows.Next() {
		var queue Queue
		if err := rows.Scan(&queue.Name, &queue.Available, &queue.Running, &queue.Retryable, &queue.Scheduled); err != nil {
			return nil, fmt.Errorf("scan user queue: %w", err)
		}
		result = append(result, queue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user queues: %w", err)
	}
	global, err := r.ListQueues(ctx)
	if err == nil {
		paused := make(map[string]bool, len(global))
		for _, queue := range global {
			paused[queue.Name] = queue.Paused
		}
		for index := range result {
			result[index].Paused = paused[result[index].Name]
		}
	}
	return result, nil
}

// PauseQueue stops the named queue from handing out new jobs; jobs that are
// already running are allowed to finish. The name reaches River unchanged, so
// River's all-queues wildcard "*" works too. A queue that has never held a job
// yields river.ErrNotFound, because River only tracks queues it has seen.
func (r *Runtime) PauseQueue(ctx context.Context, name string) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	return r.client.QueuePause(ctx, name, nil)
}

// ResumeQueue lets the named queue hand out jobs again and, like PauseQueue,
// returns river.ErrNotFound when the queue has never held a job and
// ErrRuntimeNotConfigured when the runtime has no client.
func (r *Runtime) ResumeQueue(ctx context.Context, name string) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	return r.client.QueueResume(ctx, name, nil)
}

// Purge permanently deletes every job in the given finalized state and reports
// how many rows were removed. Any other state is rejected with
// ErrInvalidJobState before SQL runs, so a purge can never delete a job that is
// still going to run. It returns ErrRuntimeNotConfigured when the runtime has no
// pool, since River has no purge API.
func (r *Runtime) Purge(ctx context.Context, state string) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, ErrRuntimeNotConfigured
	}
	switch state {
	case "cancelled", "completed", "discarded":
	default:
		return 0, ErrInvalidJobState
	}
	jobTable := pgx.Identifier{r.schema, "river_job"}.Sanitize()
	command, err := r.pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE state::text = $1", jobTable), state)
	if err != nil {
		return 0, fmt.Errorf("purge %s jobs: %w", state, err)
	}
	return command.RowsAffected(), nil
}

// PurgeForUser is Purge restricted to the jobs whose "user_id" argument equals
// userID, which is how a user clears their own history without touching anybody
// else's. It applies the same state whitelist and returns the number of deleted
// rows, or ErrRuntimeNotConfigured when the runtime has no pool or userID is not
// positive.
func (r *Runtime) PurgeForUser(ctx context.Context, userID int64, state string) (int64, error) {
	if r == nil || r.pool == nil || userID <= 0 {
		return 0, ErrRuntimeNotConfigured
	}
	switch state {
	case "cancelled", "completed", "discarded":
	default:
		return 0, ErrInvalidJobState
	}
	jobTable := pgx.Identifier{r.schema, "river_job"}.Sanitize()
	command, err := r.pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE state::text = $1 AND args->>'user_id' = $2", jobTable), state, fmt.Sprintf("%d", userID))
	if err != nil {
		return 0, fmt.Errorf("purge %s jobs for user: %w", state, err)
	}
	return command.RowsAffected(), nil
}

// ListPeriodicJobs returns every durable periodic job definition, at most 1000.
// A row whose arguments cannot be decoded fails the whole listing instead of
// being skipped, so callers never receive a silently incomplete catalog.
func (r *Runtime) ListPeriodicJobs(ctx context.Context) ([]PeriodicJob, error) {
	if r == nil || r.client == nil {
		return nil, ErrRuntimeNotConfigured
	}
	rows, err := r.client.PeriodicJobList(ctx, &riverpro.PeriodicJobListOpts{Max: 1000})
	if err != nil {
		return nil, fmt.Errorf("list periodic jobs: %w", err)
	}
	result := make([]PeriodicJob, 0, len(rows))
	for _, row := range rows {
		job, err := periodicJobFromRiver(row)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, nil
}

// ResetPeriodicJobs restores the built-in schedules to their catalog defaults.
// It deletes every stored definition, repeating the listing until it comes back
// empty so nothing survives an iteration, and then recreates the templates
// returned by PeriodicJobCatalog, active rather than paused. Operator edits made
// through the API are therefore lost, and runs already inserted by the old
// definitions stay in the job table.
//
// The whole reset holds the runtime mutex so it cannot interleave with Start,
// which registers the same IDs. It returns ErrRuntimeNotConfigured when the
// runtime has no client.
func (r *Runtime) ResetPeriodicJobs(ctx context.Context) ([]PeriodicJob, error) {
	if r == nil || r.client == nil {
		return nil, ErrRuntimeNotConfigured
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		rows, err := r.client.PeriodicJobList(ctx, &riverpro.PeriodicJobListOpts{Max: 1000})
		if err != nil {
			return nil, fmt.Errorf("list periodic jobs for reset: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if _, err := r.client.PeriodicJobDelete(ctx, row.ID); err != nil {
				return nil, fmt.Errorf("delete periodic job %q during reset: %w", row.ID, err)
			}
		}
	}

	templates := r.PeriodicJobCatalog()
	result := make([]PeriodicJob, 0, len(templates))
	for _, template := range templates {
		job, err := r.CreatePeriodicJob(ctx, PeriodicJobInput{
			ID:          template.ID,
			Kind:        template.Kind,
			Args:        template.DefaultArgs,
			Queue:       template.DefaultQueue,
			Priority:    template.DefaultPriority,
			MaxAttempts: template.DefaultMaxAttempts,
			Tags:        append([]string(nil), template.DefaultTags...),
			Schedule: PeriodicSchedule{
				CronExpression: template.DefaultCronExpression,
				CronTimezone:   template.DefaultCronTimezone,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("restore default periodic job %q: %w", template.ID, err)
		}
		result = append(result, job)
	}
	return result, nil
}

// PeriodicJobCatalog returns the built-in periodic jobs this runtime is able to
// run, with the schedule and arguments each is created with. The catalog depends
// on the enabled features: the trash cleanup and pending deletion purge entries
// appear only when purge support is configured, and the orphaned Telegram part
// cleanup only when the storage backend can list documents. A template that is
// restricted is simply absent, so the console cannot schedule a sweep whose
// worker was never registered. Each call builds and returns fresh values.
func (r *Runtime) PeriodicJobCatalog() []PeriodicTemplate {
	templates := []PeriodicTemplate{
		{
			ID: uploadCleanupPeriodicID, Label: "Upload cleanup", Description: "Remove abandoned upload sessions and temporary parts.",
			Kind: UploadCleanupSweepKind, DefaultArgs: rawArgs(UploadCleanupSweepArgs{}),
			DefaultQueue: CleanupQueue, DefaultPriority: 2, DefaultMaxAttempts: 3,
			DefaultCronExpression: uploadCleanupDefaultCron, DefaultCronTimezone: maintenanceTimezone,
		},
		{
			ID: eventCleanupPeriodicID, Label: "User event cleanup", Description: "Delete replayable user events older than the configured retention period.",
			Kind: EventCleanupKind, DefaultArgs: rawArgs(EventCleanupArgs{Retention: eventCleanupDefaultRetention}),
			DefaultQueue: CleanupQueue, DefaultPriority: 2, DefaultMaxAttempts: 3,
			DefaultCronExpression: eventCleanupDefaultCron, DefaultCronTimezone: maintenanceTimezone,
		},
	}
	if r.purgeEnabled {
		templates = append(templates,
			PeriodicTemplate{
				ID: trashCleanupPeriodicID, Label: "Trash cleanup", Description: "Permanently remove trashed files after their retention period.",
				Kind: TrashCleanupSweepKind, DefaultArgs: rawArgs(TrashCleanupSweepArgs{Retention: "720h"}),
				DefaultQueue: CleanupQueue, DefaultPriority: 1, DefaultMaxAttempts: 3,
				DefaultCronExpression: trashCleanupDefaultCron, DefaultCronTimezone: maintenanceTimezone,
			},
			PeriodicTemplate{
				ID: purgePeriodicID, Label: "Pending deletion cleanup", Description: "Finish permanent deletion for files already marked deletion-pending.",
				Kind: PurgeSweepKind, DefaultArgs: rawArgs(PurgeSweepArgs{}),
				DefaultQueue: PurgeQueue, DefaultPriority: 1, DefaultMaxAttempts: 3,
				DefaultCronExpression: pendingDeletionCleanupDefaultCron, DefaultCronTimezone: maintenanceTimezone,
			},
		)
	}
	if r.orphanCleanupEnabled {
		templates = append(templates, PeriodicTemplate{
			ID: orphanCleanupPeriodicID, Label: "Orphaned Telegram-part cleanup",
			Description: "Delete old Telegram documents that are not referenced by file or upload parts.",
			Kind:        OrphanCleanupKind, DefaultArgs: rawArgs(OrphanCleanupArgs{}),
			DefaultQueue: CleanupQueue, DefaultPriority: 3, DefaultMaxAttempts: 3,
			DefaultCronExpression: orphanCleanupDefaultCron, DefaultCronTimezone: maintenanceTimezone,
		})
	}
	return templates
}

// CreatePeriodicJob stores a new periodic job definition and returns it. The ID
// must not be in use yet; RiverPro reports a duplicate as
// riverpro.ErrPeriodicJobAlreadyExists, which the console maps to a conflict.
// Blank queue, non-positive priority or attempt budget and a blank timezone fall
// back to defaults. The kind must name a cleanup sweep this runtime registered a
// worker for, the same set Create accepts, so a schedule can never be stored for
// a job that would fail with an unknown job kind on every run; the check happens
// before any write. It returns ErrRuntimeNotConfigured when the runtime has no
// client.
func (r *Runtime) CreatePeriodicJob(ctx context.Context, input PeriodicJobInput) (PeriodicJob, error) {
	if r == nil || r.client == nil {
		return PeriodicJob{}, ErrRuntimeNotConfigured
	}
	kind := strings.TrimSpace(input.Kind)
	if err := r.validateCleanupJobKind(kind); err != nil {
		return PeriodicJob{}, err
	}
	args, err := newRawPeriodicJobArgs(kind, input.Args)
	if err != nil {
		return PeriodicJob{}, err
	}
	row, err := r.client.PeriodicJobInsert(ctx, &riverpro.PeriodicJobInsertOpts{
		ID: input.ID, JobArgs: args, Queue: defaultQueue(input.Queue), Priority: defaultPriority(input.Priority),
		MaxAttempts: defaultMaxAttempts(input.MaxAttempts), Tags: append([]string(nil), input.Tags...),
		Schedule: &riverpro.PeriodicJobSchedule{
			CronExpression: input.Schedule.CronExpression,
			CronTimezone:   defaultTimezone(input.Schedule.CronTimezone),
		},
		Paused: input.Paused,
	})
	if err != nil {
		return PeriodicJob{}, fmt.Errorf("create periodic job %q: %w", input.ID, err)
	}
	return periodicJobFromRiver(row)
}

// UpdatePeriodicJob applies input to the stored definition with the given ID and
// returns the updated row. It reads the current definition first and sends only
// the fields that actually differ, so an unchanged update issues no write at
// all; a pause or resume is a separate RiverPro call issued only when the stored
// state differs from input.Paused. The kind must name a cleanup sweep this
// runtime registered a worker for, the same set Create accepts, so an existing
// schedule can never be rewritten to a job that would fail with an unknown job
// kind on every run; like Create, the check happens before anything is read or
// written. It returns river.ErrNotFound for an unknown ID and
// ErrRuntimeNotConfigured when the runtime has no client.
func (r *Runtime) UpdatePeriodicJob(ctx context.Context, id string, input PeriodicJobInput) (PeriodicJob, error) {
	if r == nil || r.client == nil {
		return PeriodicJob{}, ErrRuntimeNotConfigured
	}
	kind := strings.TrimSpace(input.Kind)
	if err := r.validateCleanupJobKind(kind); err != nil {
		return PeriodicJob{}, err
	}
	current, err := r.client.PeriodicJobGet(ctx, id)
	if err != nil {
		return PeriodicJob{}, err
	}
	args, encodedArgs, err := rawPeriodicJobArgsAndJSON(kind, input.Args)
	if err != nil {
		return PeriodicJob{}, err
	}
	queue := defaultQueue(input.Queue)
	priority := defaultPriority(input.Priority)
	maxAttempts := defaultMaxAttempts(input.MaxAttempts)
	tags := append([]string(nil), input.Tags...)
	timezone := defaultTimezone(input.Schedule.CronTimezone)
	opts := &riverpro.PeriodicJobUpdateOpts{}
	if current.Kind != kind || !jsonBytesEqual(current.Args, encodedArgs) {
		opts.JobArgs = args
	}
	if current.Queue != queue {
		opts.Queue = &queue
	}
	if current.Priority != priority {
		opts.Priority = &priority
	}
	if current.MaxAttempts != maxAttempts {
		opts.MaxAttempts = &maxAttempts
	}
	if !reflect.DeepEqual(current.Tags, tags) {
		opts.Tags = &tags
	}
	currentCron := ""
	if current.CronExpression != nil {
		currentCron = *current.CronExpression
	}
	if currentCron != input.Schedule.CronExpression || current.CronTimezone != timezone {
		opts.Schedule = &riverpro.PeriodicJobSchedule{CronExpression: input.Schedule.CronExpression, CronTimezone: timezone}
	}
	row := current
	if opts.JobArgs != nil || opts.Queue != nil || opts.Priority != nil || opts.MaxAttempts != nil || opts.Tags != nil || opts.Schedule != nil {
		row, err = r.client.PeriodicJobUpdate(ctx, id, opts)
		if err != nil {
			return PeriodicJob{}, fmt.Errorf("update periodic job %q: %w", id, err)
		}
	}
	if input.Paused && row.PausedAt == nil {
		row, err = r.client.PeriodicJobPause(ctx, id)
	} else if !input.Paused && row.PausedAt != nil {
		row, err = r.client.PeriodicJobResume(ctx, id)
	}
	if err != nil {
		return PeriodicJob{}, err
	}
	return periodicJobFromRiver(row)
}

// DeletePeriodicJob removes a periodic job definition permanently. Only the
// schedule disappears; the jobs it already inserted stay in the job table and
// keep their history.
func (r *Runtime) DeletePeriodicJob(ctx context.Context, id string) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	_, err := r.client.PeriodicJobDelete(ctx, id)
	return err
}

// PausePeriodicJob suspends a schedule so it inserts no further runs and returns
// the updated definition. Runs that were already inserted are unaffected, and
// pausing an already paused definition is harmless. An unknown ID yields
// river.ErrNotFound.
func (r *Runtime) PausePeriodicJob(ctx context.Context, id string) (PeriodicJob, error) {
	if r == nil || r.client == nil {
		return PeriodicJob{}, ErrRuntimeNotConfigured
	}
	row, err := r.client.PeriodicJobPause(ctx, id)
	if err != nil {
		return PeriodicJob{}, err
	}
	return periodicJobFromRiver(row)
}

// ResumePeriodicJob reactivates a paused schedule and returns the updated
// definition. Resuming only clears paused_at and leaves next_run_at untouched, so a
// schedule whose stored next run has already passed is picked up by the next
// enqueuer pass and fires one catch-up run. The occurrences skipped while it was
// paused are still not replayed one by one, because each following tick is computed
// from the current time. Runs that were already inserted are unaffected, resuming an
// active definition is harmless, and an unknown ID yields river.ErrNotFound.
func (r *Runtime) ResumePeriodicJob(ctx context.Context, id string) (PeriodicJob, error) {
	if r == nil || r.client == nil {
		return PeriodicJob{}, ErrRuntimeNotConfigured
	}
	row, err := r.client.PeriodicJobResume(ctx, id)
	if err != nil {
		return PeriodicJob{}, err
	}
	return periodicJobFromRiver(row)
}

// rawPeriodicJobArgs carries a periodic job's arguments as pre-encoded JSON so
// the management API can create and update periodic jobs whose concrete
// argument type is chosen by the operator at runtime. River only needs a kind
// and a JSON payload, so the arguments never have to be decoded into a Go type.
type rawPeriodicJobArgs struct {
	// kind is the River job kind reported to the client.
	kind string
	// raw is the encoded argument object inserted verbatim.
	raw json.RawMessage
}

// Kind reports the worker kind the wrapped arguments belong to.
func (a rawPeriodicJobArgs) Kind() string { return a.kind }

// MarshalJSON returns a copy of the pre-encoded arguments, so the JSON the
// operator submitted is stored byte for byte and the returned slice never
// aliases the wrapper's buffer.
func (a rawPeriodicJobArgs) MarshalJSON() ([]byte, error) { return append([]byte(nil), a.raw...), nil }

// newRawPeriodicJobArgs validates kind and encodes args for insertion. It
// returns the error of rawPeriodicJobArgsAndJSON, which rejects a blank kind and
// reports argument encoding failures.
func newRawPeriodicJobArgs(kind string, args map[string]json.RawMessage) (rawPeriodicJobArgs, error) {
	jobArgs, _, err := rawPeriodicJobArgsAndJSON(kind, args)
	return jobArgs, err
}

// rawPeriodicJobArgsAndJSON is newRawPeriodicJobArgs that also returns the
// encoded JSON. UpdatePeriodicJob needs those bytes to compare the submitted
// arguments with the stored ones without encoding them a second time, which
// would hide real differences behind key-order noise. A blank kind is rejected,
// and nil args encode as an empty object rather than null so the stored job
// always has a JSON object.
func rawPeriodicJobArgsAndJSON(kind string, args map[string]json.RawMessage) (rawPeriodicJobArgs, []byte, error) {
	if strings.TrimSpace(kind) == "" {
		return rawPeriodicJobArgs{}, nil, errors.New("periodic job kind is required")
	}
	if args == nil {
		args = map[string]json.RawMessage{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return rawPeriodicJobArgs{}, nil, fmt.Errorf("encode periodic job args: %w", err)
	}
	return rawPeriodicJobArgs{kind: kind, raw: encoded}, encoded, nil
}

// periodicJobFromRiver converts a RiverPro periodic job row into the API-facing
// PeriodicJob. It decodes the stored argument JSON into an empty object when the
// row carries none, unwraps the optional cron expression and normalizes every
// timestamp to UTC. A nil row or undecodable arguments return an error instead
// of a partially filled definition.
func periodicJobFromRiver(row *prodriver.PeriodicJob) (PeriodicJob, error) {
	if row == nil {
		return PeriodicJob{}, errors.New("periodic job row is nil")
	}
	args := map[string]json.RawMessage{}
	if len(row.Args) > 0 {
		if err := json.Unmarshal(row.Args, &args); err != nil {
			return PeriodicJob{}, fmt.Errorf("decode periodic job %q args: %w", row.ID, err)
		}
	}
	cron := ""
	if row.CronExpression != nil {
		cron = *row.CronExpression
	}
	return PeriodicJob{
		ID: row.ID, Kind: row.Kind, Args: args, Queue: row.Queue, Priority: row.Priority,
		MaxAttempts: row.MaxAttempts, Tags: append([]string(nil), row.Tags...),
		Schedule:  PeriodicSchedule{CronExpression: cron, CronTimezone: row.CronTimezone},
		NextRunAt: row.NextRunAt.UTC(), PausedAt: utcTimePtr(row.PausedAt),
		Paused: row.PausedAt != nil, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

// rawJobArgs is rawPeriodicJobArgs for one-off jobs inserted by an
// administrator, which likewise pick their argument type at runtime.
type rawJobArgs struct {
	// kind is the River job kind reported to the client.
	kind string
	// raw is the encoded argument object inserted verbatim.
	raw json.RawMessage
}

// Kind reports the worker kind the wrapped arguments belong to.
func (a rawJobArgs) Kind() string { return a.kind }

// MarshalJSON returns a copy of the pre-encoded arguments, so the submitted JSON
// is stored byte for byte and the returned slice never aliases the wrapper's
// buffer.
func (a rawJobArgs) MarshalJSON() ([]byte, error) { return append([]byte(nil), a.raw...), nil }

// Create inserts a one-off administrative job and returns it. Only the cleanup
// sweep kinds are accepted (upload cleanup, user event cleanup, trash cleanup,
// pending deletion purge and orphaned Telegram part cleanup) because those are
// the sweeps that are safe to trigger by hand; any other kind is rejected with
// an error naming it. A sweep that belongs to a feature this runtime was not
// configured with, such as the trash cleanup without a purge service, is refused
// the same way before River can fail the insert with an unknown job kind. Blank
// queue, non-positive priority or attempt budget fall back to the River defaults.
// It returns ErrRuntimeNotConfigured when the runtime has no client.
func (r *Runtime) Create(ctx context.Context, input CreateInput) (Job, error) {
	if r == nil || r.client == nil {
		return Job{}, ErrRuntimeNotConfigured
	}
	kind := strings.TrimSpace(input.Kind)
	if err := r.validateCleanupJobKind(kind); err != nil {
		return Job{}, err
	}
	encoded, err := json.Marshal(input.Args)
	if err != nil {
		return Job{}, fmt.Errorf("encode job args: %w", err)
	}
	result, err := r.client.Insert(ctx, rawJobArgs{kind: kind, raw: encoded}, &river.InsertOpts{
		Queue: defaultQueue(input.Queue), Priority: defaultPriority(input.Priority),
		MaxAttempts: defaultMaxAttempts(input.MaxAttempts), Tags: append([]string(nil), input.Tags...),
	})
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return jobFromRiver(result.Job), nil
}

// validateCleanupJobKind reports whether kind names a cleanup sweep this runtime
// registered a worker for, and otherwise the reason it cannot be used. The upload
// cleanup and user event cleanup workers are always registered; the trash cleanup
// and pending deletion purge need a purge service, and the orphaned Telegram part
// cleanup needs a storage backend that can list documents. Create and
// CreatePeriodicJob share it, so neither can store a definition for a job this
// runtime would only fail on later with an unknown job kind.
func (r *Runtime) validateCleanupJobKind(kind string) error {
	switch kind {
	case UploadCleanupSweepKind, EventCleanupKind:
		return nil
	case TrashCleanupSweepKind, PurgeSweepKind:
		if r.purgeEnabled {
			return nil
		}
		return fmt.Errorf("job kind %q is unavailable: this runtime was built without a purge service", kind)
	case OrphanCleanupKind:
		if r.orphanCleanupEnabled {
			return nil
		}
		return fmt.Errorf("job kind %q is unavailable: this runtime's storage backend cannot list documents", kind)
	default:
		return fmt.Errorf("unsupported job kind %q", kind)
	}
}

// rawArgs renders a built-in argument value as the raw JSON object stored in
// river_job, which is what the catalog templates hand to the console. Both the
// marshal and the unmarshal error are ignored on purpose: the argument types are
// package-owned structs that always encode, and a failure would leave the caller
// with an empty object instead of a broken template.
func rawArgs(value any) map[string]json.RawMessage {
	encoded, _ := json.Marshal(value)
	result := map[string]json.RawMessage{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

// defaultQueue returns value, or River's default queue when value is blank.
func defaultQueue(value string) string {
	if strings.TrimSpace(value) == "" {
		return river.QueueDefault
	}
	return value
}

// defaultPriority returns value, or River's default priority when value is not
// positive. River priorities run from 1 (highest) to 4 (lowest).
func defaultPriority(value int) int {
	if value <= 0 {
		return river.PriorityDefault
	}
	return value
}

// defaultMaxAttempts returns value, or River's default attempt budget when value
// is not positive.
func defaultMaxAttempts(value int) int {
	if value <= 0 {
		return river.MaxAttemptsDefault
	}
	return value
}

// defaultTimezone returns value, or "UTC" when value is blank. Schedules are
// always stored with an explicit timezone because RiverPro requires one.
func defaultTimezone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "UTC"
	}
	return value
}

// utcTimePtr normalizes an optional timestamp to UTC and returns nil for nil, so
// a "never happened" field stays empty instead of becoming the zero time.
func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

// jsonBytesEqual reports whether two JSON documents are semantically equal,
// ignoring key order and whitespace. Documents that do not parse fall back to a
// byte comparison, which errs towards reporting a difference and so never
// silently skips an update. UpdatePeriodicJob uses it to detect whether the
// stored arguments really changed.
func jsonBytesEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return string(left) == string(right)
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
