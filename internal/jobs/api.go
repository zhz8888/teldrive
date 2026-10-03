// Package jobs defines TelDrive's River and RiverPro background workers and the
// runtime that registers, starts and administers them. It covers the maintenance
// sweeps (upload cleanup, user event cleanup, trash cleanup, pending deletion
// purge and orphaned Telegram part cleanup), HTTP and local upload batches, and
// Telegram bot provisioning.
//
// Runtime is the package entry point: it owns the RiverPro client, the pgx pool
// and the configured database schema, and it implements the job management
// operations exposed by the HTTP layer. Constructing a Runtime does not start
// any worker; call Start to begin processing and Stop to shut the client down.
// A constructed Runtime is safe for concurrent use by multiple goroutines.
//
// Workers must be idempotent because River retries failed jobs, and the job
// kinds together with their JSON argument field names are persisted in
// river_job, so both must stay stable across releases.
package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/rivertype"
)

// ErrInvalidCursor reports that a job list cursor is not the opaque token
// produced by an earlier List call. Runtime.List returns it instead of silently
// treating the cursor as absent; callers must test it with errors.Is to map it
// to a client error rather than a server failure.
var ErrInvalidCursor = errors.New("invalid job cursor")

// JobError is one failed attempt of a job, as recorded by River on the job row.
type JobError struct {
	// Attempt is the 1-based attempt number that failed.
	Attempt int
	// At is the time the attempt failed.
	At time.Time
	// Error is the error message reported by the worker.
	Error string
	// Trace is the stack trace captured for the attempt; empty when River stored
	// none.
	Trace string
}

// Job is the read-only projection of a River job row handed to callers. It is a
// detached snapshot: mutating it does not affect the stored job, and the values
// it carries may be stale as soon as the job progresses.
type Job struct {
	// ID is the River job ID, unique across all kinds and queues.
	ID int64
	// State is the River lifecycle state, for example available, running,
	// completed, discarded or cancelled.
	State string
	// Attempt is the number of attempts already made, including the current one.
	Attempt int
	// MaxAttempts is the attempt budget; a job is discarded once it is exhausted.
	MaxAttempts int
	// AttemptedAt is when the current or last attempt started; nil while the job
	// has never been attempted.
	AttemptedAt *time.Time
	// CreatedAt is when the job was inserted.
	CreatedAt time.Time
	// FinalizedAt is when the job reached a terminal state (completed, cancelled
	// or discarded); nil while the job is still active.
	FinalizedAt *time.Time
	// ScheduledAt is when the job becomes or became eligible to run.
	ScheduledAt time.Time
	// Priority is the River priority; lower values run first.
	Priority int
	// Kind is the registered worker kind that executes the job.
	Kind string
	// Queue is the queue the job was inserted into.
	Queue string
	// Args holds the decoded job arguments keyed by JSON field name. Values stay
	// raw so unparsable arguments do not fail the listing; the HTTP layer redacts
	// sensitive keys before returning them to clients.
	Args map[string]json.RawMessage
	// Metadata holds River's bookkeeping metadata (for example
	// cancel_attempted_at), keyed by JSON field name.
	Metadata map[string]json.RawMessage
	// Output is the raw JSON the worker recorded on success; empty when the job
	// produced no output.
	Output json.RawMessage
	// Errors lists the failed attempts in chronological order.
	Errors []JobError
	// AttemptedBy lists the worker clients that picked the job up.
	AttemptedBy []string
	// Tags are the operator-supplied labels attached to the job.
	Tags []string
	// LastError repeats the message of the most recent failed attempt, or is
	// empty when no attempt has failed.
	LastError string
}

// UserID extracts the "user_id" argument used to scope listings and per-user
// access checks, and reports whether it could be read. It reports false when the
// argument is absent, is not a JSON integer or is not positive, which is how
// jobs that do not belong to a single user stay invisible to tenant-scoped
// callers.
func (j Job) UserID() (int64, bool) {
	raw, ok := j.Args["user_id"]
	if !ok {
		return 0, false
	}
	var userID int64
	if err := json.Unmarshal(raw, &userID); err != nil || userID <= 0 {
		return 0, false
	}
	return userID, true
}

// ListInput filters and paginates Runtime.List. Every filter is optional and the
// populated ones combine with AND.
type ListInput struct {
	// UserID restricts the page to jobs whose "user_id" argument equals it; zero
	// or less means no owner filter, which is the administrator scope.
	UserID int64
	// Cursor is the continuation token returned by a previous List call; empty
	// starts at the newest job.
	Cursor string
	// Limit is the maximum page size. Values outside the range 1..200 fall back
	// to 100.
	Limit int32
	// State restricts the page to one River job state; empty means any state.
	State string
	// Kind restricts the page to one job kind; empty means any kind.
	Kind string
	// Queue restricts the page to one queue; empty means any queue.
	Queue string
}

// Statistics counts jobs per River state. A zero field means no job was observed
// in that state, not that the state could not be counted.
type Statistics struct {
	// Available is the number of jobs ready to be worked.
	Available int64
	// Cancelled is the number of jobs cancelled before they could finish.
	Cancelled int64
	// Completed is the number of jobs that finished successfully.
	Completed int64
	// Discarded is the number of jobs that exhausted their attempts.
	Discarded int64
	// Pending is the number of jobs waiting on an external dependency.
	Pending int64
	// Retryable is the number of failed jobs waiting for another attempt.
	Retryable int64
	// Running is the number of jobs currently being worked.
	Running int64
	// Scheduled is the number of jobs waiting for their scheduled time.
	Scheduled int64
}

// List returns jobs ordered newest first together with the cursor for the next
// page, which is empty when this was the last page. It over-fetches one row to
// detect that case, so the returned cursor always belongs to the last item of
// the current page. It returns ErrInvalidCursor for a malformed input cursor and
// ErrRuntimeNotConfigured when the runtime has no client.
func (r *Runtime) List(ctx context.Context, input ListInput) ([]Job, string, error) {
	if r == nil || r.client == nil {
		return nil, "", ErrRuntimeNotConfigured
	}
	if input.Limit <= 0 {
		input.Limit = 100
	}
	if input.Limit > 200 {
		// A page larger than the cap is served at the cap rather than silently
		// reduced to the default, so a caller asking for more still gets the
		// largest page this API serves.
		input.Limit = 200
	}
	beforeID, err := decodeCursor(input.Cursor)
	if err != nil {
		return nil, "", err
	}

	params := river.NewJobListParams().
		OrderBy(river.JobListOrderByID, river.SortOrderDesc).
		First(int(input.Limit) + 1)
	if input.UserID > 0 {
		params = params.Where("args->>'user_id' = @user_id", river.NamedArgs{"user_id": strconv.FormatInt(input.UserID, 10)})
	}
	if beforeID > 0 {
		params = params.Where("id < @before_id", river.NamedArgs{"before_id": beforeID})
	}
	if input.State != "" {
		params = params.States(rivertype.JobState(input.State))
	}
	if input.Kind != "" {
		params = params.Kinds(input.Kind)
	}
	if input.Queue != "" {
		params = params.Queues(input.Queue)
	}

	result, err := r.client.JobList(ctx, params)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	items := make([]Job, 0, len(result.Jobs))
	for _, row := range result.Jobs {
		items = append(items, jobFromRiver(row))
	}
	var next string
	if len(items) > int(input.Limit) {
		items = items[:input.Limit]
		next = encodeCursor(items[len(items)-1].ID)
	}
	return items, next, nil
}

// GetForUser returns the job only when its "user_id" argument matches userID, so
// one user cannot read another user's job. A missing job and a job owned by
// somebody else both yield river.ErrNotFound, which keeps the endpoint from
// disclosing the existence of foreign jobs.
func (r *Runtime) GetForUser(ctx context.Context, id, userID int64) (Job, error) {
	if userID <= 0 {
		return Job{}, river.ErrNotFound
	}
	item, err := r.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	ownerID, ok := item.UserID()
	if !ok || ownerID != userID {
		return Job{}, river.ErrNotFound
	}
	return item, nil
}

// CancelForUser verifies ownership with GetForUser and then cancels the job,
// returning its row as it stands after the request. Cancelling a running job
// only asks its worker to stop, so the returned state can still be running.
// Ownership failures surface as river.ErrNotFound.
func (r *Runtime) CancelForUser(ctx context.Context, id, userID int64) (Job, error) {
	if _, err := r.GetForUser(ctx, id, userID); err != nil {
		return Job{}, err
	}
	return r.Cancel(ctx, id)
}

// RetryForUser verifies ownership with GetForUser and then hands the job back to
// its queue with Retry, which is how a user restarts a discarded job of their
// own. Ownership failures surface as river.ErrNotFound.
func (r *Runtime) RetryForUser(ctx context.Context, id, userID int64) (Job, error) {
	if _, err := r.GetForUser(ctx, id, userID); err != nil {
		return Job{}, err
	}
	return r.Retry(ctx, id)
}

// DeleteForUser verifies ownership with GetForUser and then permanently removes
// the job row. River refuses to delete a job that is currently running and that
// error is passed through unchanged, so callers should cancel active jobs first.
func (r *Runtime) DeleteForUser(ctx context.Context, id, userID int64) error {
	if _, err := r.GetForUser(ctx, id, userID); err != nil {
		return err
	}
	return r.Delete(ctx, id)
}

// StatisticsForUser counts the jobs whose "user_id" argument equals userID,
// grouped by River state in a single query. Only jobs carrying a valid user ID
// are visible here, so shared maintenance jobs are excluded. It returns
// ErrRuntimeNotConfigured when the runtime has no pool or userID is not positive.
func (r *Runtime) StatisticsForUser(ctx context.Context, userID int64) (Statistics, error) {
	if r == nil || r.pool == nil || userID <= 0 {
		return Statistics{}, ErrRuntimeNotConfigured
	}
	jobTable := pgx.Identifier{r.schema, "river_job"}.Sanitize()
	rows, err := r.pool.Query(ctx, "SELECT state::text, count(*) FROM "+jobTable+" WHERE args->>'user_id' = $1 GROUP BY state", strconv.FormatInt(userID, 10))
	if err != nil {
		return Statistics{}, fmt.Errorf("job statistics for user: %w", err)
	}
	defer rows.Close()
	var stats Statistics
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return Statistics{}, fmt.Errorf("scan job statistics for user: %w", err)
		}
		switch rivertype.JobState(state) {
		case rivertype.JobStateAvailable:
			stats.Available = count
		case rivertype.JobStateCancelled:
			stats.Cancelled = count
		case rivertype.JobStateCompleted:
			stats.Completed = count
		case rivertype.JobStateDiscarded:
			stats.Discarded = count
		case rivertype.JobStatePending:
			stats.Pending = count
		case rivertype.JobStateRetryable:
			stats.Retryable = count
		case rivertype.JobStateRunning:
			stats.Running = count
		case rivertype.JobStateScheduled:
			stats.Scheduled = count
		}
	}
	if err := rows.Err(); err != nil {
		return Statistics{}, fmt.Errorf("iterate job statistics for user: %w", err)
	}
	return stats, nil
}

// Statistics counts every job in the configured schema regardless of owner; it
// is the administrator-scoped counterpart of StatisticsForUser and includes
// maintenance jobs that carry no user ID.
func (r *Runtime) Statistics(ctx context.Context) (Statistics, error) {
	if r == nil || r.client == nil {
		return Statistics{}, ErrRuntimeNotConfigured
	}
	counts, err := r.client.Driver().GetExecutor().JobCountByAllStates(ctx, &riverdriver.JobCountByAllStatesParams{Schema: r.schema})
	if err != nil {
		return Statistics{}, fmt.Errorf("job statistics: %w", err)
	}
	return Statistics{
		Available: int64(counts[rivertype.JobStateAvailable]),
		Cancelled: int64(counts[rivertype.JobStateCancelled]),
		Completed: int64(counts[rivertype.JobStateCompleted]),
		Discarded: int64(counts[rivertype.JobStateDiscarded]),
		Pending:   int64(counts[rivertype.JobStatePending]),
		Retryable: int64(counts[rivertype.JobStateRetryable]),
		Running:   int64(counts[rivertype.JobStateRunning]),
		Scheduled: int64(counts[rivertype.JobStateScheduled]),
	}, nil
}

// Cancel requests cancellation of the job with the given ID and returns its row
// after the request. A job that is still running is not interrupted: it is
// flagged so the job rescuer cancels it if the worker never stops, and an
// already finalized job is returned unchanged. A missing job yields
// river.ErrNotFound.
func (r *Runtime) Cancel(ctx context.Context, id int64) (Job, error) {
	if r == nil || r.client == nil {
		return Job{}, ErrRuntimeNotConfigured
	}
	row, err := r.client.JobCancel(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return jobFromRiver(row), nil
}

// Retry puts a job back on its queue and returns the updated row: it sets the
// state back to available, schedules the job for immediate execution, clears the
// finalization time and grants one extra attempt when the budget was already
// exhausted, so a discarded job can run again.
//
// River's retry statement leaves a running job untouched, because its worker still
// owns it, and leaves an available job whose scheduled time has passed untouched,
// because it is already waiting to be worked. Retry reads the job first and
// reports those two states with ErrInvalidJobState instead of returning a success
// that changed nothing.
//
// The same transaction removes the "cancel_attempted_at" marker River stores
// when a running job is cancelled, so a retried job is not cancelled later by
// the job rescuer. The state change and the metadata cleanup therefore commit
// together. It returns ErrRuntimeNotConfigured when the runtime has no client or
// pool.
func (r *Runtime) Retry(ctx context.Context, id int64) (Job, error) {
	if r == nil || r.client == nil || r.pool == nil {
		return Job{}, ErrRuntimeNotConfigured
	}
	current, err := r.client.JobGet(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !retryRequeuesJob(current) {
		return Job{}, fmt.Errorf("%w: job %d is %s and retry would leave it unchanged", ErrInvalidJobState, id, current.State)
	}
	var row *rivertype.JobRow
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		row, err = r.client.JobRetryTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.State == rivertype.JobStateRunning {
			// The job started running between the read above and this
			// transaction, so the retry statement left it untouched and its
			// cancel_attempted_at marker must stay.
			return fmt.Errorf("%w: job %d is %s and retry would leave it unchanged", ErrInvalidJobState, id, row.State)
		}

		jobTable := pgx.Identifier{r.schema, "river_job"}.Sanitize()
		return tx.QueryRow(ctx,
			"UPDATE "+jobTable+" SET metadata = metadata - 'cancel_attempted_at' WHERE id = $1 RETURNING metadata",
			id,
		).Scan(&row.Metadata)
	})
	if err != nil {
		return Job{}, err
	}
	return jobFromRiver(row), nil
}

// retryRequeuesJob reports whether River's retry statement would actually move the
// job back to the available state. It mirrors the conditions of that statement: a
// running job is owned by its worker, and an available job whose scheduled time
// has already passed is queued for immediate execution, so both are skipped. It
// compares the scheduled time with the local clock, which is close enough to the
// database's for a pre-check; a job that changes state in between is caught by the
// running-state check inside the retry transaction.
func retryRequeuesJob(row *rivertype.JobRow) bool {
	switch row.State {
	case rivertype.JobStateRunning:
		return false
	case rivertype.JobStateAvailable:
		return row.ScheduledAt.After(time.Now())
	default:
		return true
	}
}

// Delete removes the job row permanently. River refuses to delete a job in the
// running state and returns rivertype.ErrJobRunning; callers should cancel such
// a job and delete it after the worker has stopped.
func (r *Runtime) Delete(ctx context.Context, id int64) error {
	if r == nil || r.client == nil {
		return ErrRuntimeNotConfigured
	}
	_, err := r.client.JobDelete(ctx, id)
	return err
}

// jobFromRiver converts a River job row into the caller-facing Job, deep-copying
// the slices and raw JSON so the result never aliases the row. Arguments and
// metadata that fail to decode are left out instead of failing the conversion,
// so one corrupt job cannot break a whole listing.
func jobFromRiver(row *rivertype.JobRow) Job {
	item := Job{
		ID: row.ID, State: string(row.State), Attempt: row.Attempt, MaxAttempts: row.MaxAttempts,
		AttemptedAt: row.AttemptedAt, CreatedAt: row.CreatedAt, FinalizedAt: row.FinalizedAt,
		ScheduledAt: row.ScheduledAt, Priority: row.Priority, Kind: row.Kind, Queue: row.Queue,
		Args: map[string]json.RawMessage{}, Metadata: map[string]json.RawMessage{},
		AttemptedBy: append([]string(nil), row.AttemptedBy...), Tags: append([]string(nil), row.Tags...),
	}
	_ = json.Unmarshal(row.EncodedArgs, &item.Args)
	_ = json.Unmarshal(row.Metadata, &item.Metadata)
	item.Output = append(json.RawMessage(nil), row.Output()...)
	item.Errors = make([]JobError, 0, len(row.Errors))
	for _, attemptError := range row.Errors {
		item.Errors = append(item.Errors, JobError{
			Attempt: attemptError.Attempt,
			At:      attemptError.At,
			Error:   attemptError.Error,
			Trace:   attemptError.Trace,
		})
	}
	if len(item.Errors) > 0 {
		item.LastError = item.Errors[len(item.Errors)-1].Error
	}
	return item
}

// encodeCursor renders a job ID as the opaque, URL-safe cursor accepted by the
// cursor field of List.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// decodeCursor parses a cursor produced by encodeCursor. An empty or blank
// cursor means "start from the newest job" and yields zero; anything that is not
// a base64-encoded positive job ID is rejected with ErrInvalidCursor so that a
// tampered cursor is reported as a client error instead of being ignored.
func decodeCursor(cursor string) (int64, error) {
	if strings.TrimSpace(cursor) == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrInvalidCursor
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidCursor
	}
	return id, nil
}
