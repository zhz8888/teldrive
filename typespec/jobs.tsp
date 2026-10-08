using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/**
 * Lifecycle state of a River job. A job becomes available or scheduled, turns
 * running while a worker holds it, and ends in completed, cancelled or
 * discarded.
 */
enum JobState {
  /** Queued and ready to be picked up by a worker. */
  available: "available",
  /** Cancelled before it could finish. */
  cancelled: "cancelled",
  /** Finished successfully. */
  completed: "completed",
  /** Gave up after exhausting its attempts. */
  discarded: "discarded",
  /** Waiting on an external dependency before it can run. */
  pending: "pending",
  /** Failed and waiting for another attempt. */
  retryable: "retryable",
  /** Currently being worked on. */
  running: "running",
  /** Waiting for its scheduled time to arrive. */
  scheduled: "scheduled",
}

/** One failed attempt of a job, as recorded by River. */
model JobAttemptError {
  /** 1-based number of the attempt that failed. */
  attempt: int32;
  /** Time the attempt failed. */
  at: utcDateTime;
  /** Error message reported by the worker. */
  error: string;
  /** Stack trace captured for the attempt; absent when River stored none. */
  trace?: string;
}

/** A River job as the management API exposes it; the values are a snapshot and may already be stale. */
model Job {
  /** River job ID, unique across all kinds and queues. */
  id: string;
  /** Lifecycle state of the job. */
  status: JobState;
  /** Worker kind that executes the job, for example "teldrive_cleanup_uploads". */
  type: string;
  /** Identifier of the job this one is grouped under; absent unless the producer set one. */
  parentId?: string;
  /** Queue the job was inserted into. */
  queue: string;
  /** Human-readable description taken from the job metadata; absent when none is set. */
  description?: string;
  /** Message taken from the job metadata, falling back to the most recent attempt error. */
  message?: string;
  /** Number of attempts already made, including the current one. */
  attempt: int32;
  /** Attempt budget; the job is discarded once it is exhausted. */
  maxAttempts: int32;
  /** River priority, 1 being the highest and 4 the lowest. */
  priority: int32;
  /** Operator-supplied labels attached to the job. */
  tags: string[];
  /** Job arguments keyed by JSON field name; credential-like values are redacted. */
  args: Record<unknown>;
  /** JSON object the worker recorded on success; absent when it recorded none. */
  output?: Record<unknown>;
  /** Failed attempts in chronological order. */
  errors: JobAttemptError[];
  /** Worker clients that picked the job up. */
  attemptedBy: string[];
  /** Time the job was inserted. */
  createdAt: utcDateTime;
  /** Time the job became or becomes eligible to run. */
  scheduledAt: utcDateTime;
  /** Time the current or last attempt started; absent while the job has never been attempted. */
  startedAt?: utcDateTime;
  /** Time the job reached a terminal state; absent while it is still active. */
  completedAt?: utcDateTime;
}

/** Paging metadata of a job listing. */
model JobPageMeta {
  /** Opaque cursor for the next page; absent when this was the last page. */
  nextCursor?: Cursor;
}

/** One page of jobs. */
model JobPage {
  /** Jobs of the page, newest first. */
  tasks: Job[];
  /** Cursor for the following page. */
  meta: JobPageMeta;
}

/**
 * Filters of a job listing; cursor and limit come from CursorQuery. The limit
 * defaults to 100, and a larger value is served as 200 at most.
 */
model JobListQuery is CursorQuery {
  /** Keep only jobs in this state; absent lists every state. */
  @query
  status?: JobState;

  /** Keep only jobs of this worker kind; absent lists every kind. */
  @query
  type?: string;

  /** Keep only jobs of this queue; absent lists every queue. */
  @query
  queue?: string;
}

/** Number of jobs per lifecycle state; a zero field means no job was observed in that state. */
model JobStatistics {
  /** Jobs queued and ready to be worked. */
  available: int64;
  /** Jobs cancelled before they could finish. */
  cancelled: int64;
  /** Jobs that finished successfully. */
  completed: int64;
  /** Jobs that exhausted their attempts. */
  discarded: int64;
  /** Jobs waiting on an external dependency. */
  pending: int64;
  /** Failed jobs waiting for another attempt. */
  retryable: int64;
  /** Jobs currently being worked. */
  running: int64;
  /** Jobs waiting for their scheduled time. */
  scheduled: int64;
}

/** State whose finalized jobs a purge deletes. */
model JobPurgeQuery {
  /** Only cancelled, completed and discarded are accepted; any other state is rejected with 409. */
  @query
  status: JobState;
}

/** Outcome of a job purge. */
model JobPurgeResult {
  /** Number of job rows deleted. */
  count: int64;
}

/** A one-off maintenance job to enqueue. */
model JobCreate {
  /**
   * Worker kind to run: one of the cleanup sweeps this deployment registered a
   * worker for ("teldrive_cleanup_uploads", "teldrive_cleanup_user_events",
   * "teldrive_cleanup_trash", "teldrive_purge_pending_files" or
   * "teldrive_cleanup_orphaned_telegram_parts"). Any other kind is rejected with
   * 422.
   */
  type: string;
  /** Job arguments keyed by JSON field name; the worker applies its own defaults to absent fields. */
  args: Record<unknown>;
  /** Target queue; absent selects River's default queue. */
  queue?: string;
  /** River priority from 1 (highest) to 4 (lowest); absent or not positive selects the default. */
  priority?: int32;
  /** Attempt budget; absent or not positive selects River's default. */
  maxAttempts?: int32;
  /** Labels attached to the job. */
  tags?: string[];
}

/** One River queue with the counters the job dashboard shows. */
model JobQueue {
  /** Queue name; River only tracks queues that have held at least one job. */
  name: string;
  /** Whether the queue is paused and therefore hands out no new jobs. */
  paused: boolean;
  /** Jobs ready to run in this queue. */
  available: int64;
  /** Jobs of this queue currently being worked. */
  running: int64;
  /** Failed jobs of this queue waiting for another attempt; the administrator listing leaves it 0. */
  retryable: int64;
  /** Jobs of this queue waiting for their scheduled time; the administrator listing leaves it 0. */
  scheduled: int64;
}

/** Queues visible to the caller. */
model JobQueueList {
  /** Every known queue for an administrator, otherwise only the queues holding the caller's jobs. */
  queues: JobQueue[];
}

/** A stored periodic job definition with its runtime bookkeeping. */
model PeriodicJob {
  /** Durable identifier; update, pause, resume and delete refer to it. */
  id: string;
  /** Worker kind each run uses. */
  kind: string;
  /** Arguments inserted with every run, keyed by JSON field name. */
  args: Record<unknown>;
  /** Queue the runs are inserted into. */
  queue: string;
  /** River priority of the runs, 1 being the highest. */
  priority: int32;
  /** Attempt budget of each run. */
  maxAttempts: int32;
  /** Labels attached to each run. */
  tags: string[];
  /** Cron expression, or River's "@every <duration>" shorthand, that produces the runs. */
  cronExpression: string;
  /** IANA timezone the expression is evaluated in; never blank. */
  cronTimezone: string;
  /** Time the next run is due. */
  nextRunAt: utcDateTime;
  /** Whether the schedule is suspended. */
  paused: boolean;
  /** Time the schedule was paused; absent while it is active. */
  pausedAt?: utcDateTime;
  /** Time the definition was first stored. */
  createdAt: utcDateTime;
  /** Time the definition was last changed. */
  updatedAt: utcDateTime;
}

/** The stored periodic job definitions. */
model PeriodicJobList {
  /** Definitions known to the instance, at most 1000. */
  jobs: PeriodicJob[];
}

/** A built-in periodic job offered by the catalog, with the defaults a client prefills. */
model PeriodicJobTemplate {
  /** Worker kind the schedule inserts. */
  kind: string;
  /** Name shown in the console. */
  label: string;
  /** What the schedule does. */
  description: string;
  /** Identifier the periodic job is created with. */
  defaultId: string;
  /** Arguments the schedule is created with. */
  defaultArgs: Record<unknown>;
  /** Queue the runs are inserted into. */
  defaultQueue: string;
  /** Recommended schedule, a cron expression or "@every <duration>". */
  recommendedCron: string;
}

/** The built-in schedules this deployment is able to run. */
model PeriodicJobCatalog {
  /** One template per enabled feature; a schedule whose worker is unavailable is absent. */
  templates: PeriodicJobTemplate[];
}

/** A periodic job definition to store. */
model PeriodicJobCreate {
  /** Identifier to create; it must not be in use yet. */
  id: string;
  /** Worker kind each run uses; the same cleanup sweep kinds JobCreate accepts. */
  kind: string;
  /** Arguments inserted with every run, keyed by JSON field name. */
  args: Record<unknown>;
  /** Queue the runs are inserted into; blank selects River's default queue. */
  queue: string;
  /** River priority of the runs, 1 being the highest; 0 or less selects the default. */
  priority: int32;
  /** Attempt budget of each run; 0 or less selects the default. */
  maxAttempts: int32;
  /** Labels attached to each run. */
  tags: string[];
  /** Cron expression, or River's "@every <duration>" shorthand, that produces the runs. */
  cronExpression: string;
  /** IANA timezone the expression is evaluated in; blank is stored as UTC. */
  cronTimezone: string;
  /** Store the schedule paused instead of active; absent stores it active. */
  paused?: boolean;
}

/**
 * Replacement state of a periodic job. Every field is taken from the body, so
 * leaving paused unset resumes a paused schedule, and a blank queue, a
 * non-positive priority or maxAttempts, or a blank cronTimezone selects the
 * default instead of keeping the stored value.
 */
model PeriodicJobUpdate {
  /** Worker kind each run uses; the same cleanup sweep kinds JobCreate accepts. */
  kind: string;
  /** Arguments inserted with every run, keyed by JSON field name. */
  args: Record<unknown>;
  /** Queue the runs are inserted into; blank selects River's default queue. */
  queue: string;
  /** River priority of the runs, 1 being the highest; 0 or less selects the default. */
  priority: int32;
  /** Attempt budget of each run; 0 or less selects the default. */
  maxAttempts: int32;
  /** Labels attached to each run. */
  tags: string[];
  /** Cron expression, or River's "@every <duration>" shorthand, that produces the runs. */
  cronExpression: string;
  /** IANA timezone the expression is evaluated in; blank is stored as UTC. */
  cronTimezone: string;
  /** Leave the schedule paused instead of active; absent resumes a paused schedule. */
  paused?: boolean;
}

@tag("Jobs")
@route("/v1/jobs")
@useAuth(TelDriveAuth)
namespace Jobs {
  /**
   * List jobs newest first. Administrators see every job; other callers only see
   * the jobs that carry their own user ID. A cursor that is not the opaque token
   * of an earlier page is rejected with 422.
   */
  @get
  @operationId("listJobs")
  op list(...JobListQuery):
    | JsonResponse<200, JobPage>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /**
   * Enqueue a one-off maintenance job and return it. The admin or owner role is
   * required, and a job kind this deployment has no worker for is rejected with
   * 422 before anything is written.
   */
  @post
  @operationId("createJob")
  op create(@body body: JobCreate):
    | JsonResponse<201, Job>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /**
   * Count jobs per state: cluster-wide for administrators, and restricted to the
   * caller's own jobs for everyone else, which hides maintenance jobs that carry
   * no user ID.
   */
  @get
  @route("/statistics")
  @operationId("getJobStatistics")
  op statistics(): JsonResponse<200, JobStatistics> | ErrorResponse<401>;

  /**
   * List the River queues with their paused flag and counters: every queue known
   * to the instance for administrators, otherwise only the queues holding the
   * caller's jobs.
   */
  @get
  @route("/queues")
  @operationId("listJobQueues")
  op queues(): JsonResponse<200, JobQueueList> | ErrorResponse<401>;

  /**
   * Pause a queue so it hands out no new jobs; jobs already running finish.
   * Requires the admin or owner role, and a queue that has never held a job is
   * reported as 404.
   */
  @post
  @route("/queues/{queue}/pause")
  @operationId("pauseJobQueue")
  op pauseQueue(@path queue: string):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /**
   * Let a paused queue hand out jobs again. Requires the admin or owner role, and
   * a queue that River has never seen is reported as 404.
   */
  @post
  @route("/queues/{queue}/resume")
  @operationId("resumeJobQueue")
  op resumeQueue(@path queue: string):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /**
   * Permanently delete the caller's jobs in one finalized state and report how
   * many rows were removed; administrators purge across all users. Only
   * cancelled, completed and discarded are accepted, and any other state is
   * rejected with 409, so a job that may still run cannot be deleted here.
   */
  @delete
  @route("/purge")
  @operationId("purgeJobs")
  op purge(...JobPurgeQuery):
    | JsonResponse<200, JobPurgeResult>
    | ErrorResponse<401>
    | ErrorResponse<409>;

  /**
   * Return one job by its decimal ID. Non-administrators only reach their own
   * jobs, so an unknown or foreign ID is reported as 404 without disclosing which
   * IDs exist.
   */
  @get
  @route("/{jobId}")
  @operationId("getJob")
  op get(@path jobId: string):
    | JsonResponse<200, Job>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /**
   * Request cancellation of a job and return its record. A running job is only
   * flagged for the job rescuer rather than interrupted, so the returned state can
   * still be running; the same visibility rule as get applies.
   */
  @post
  @route("/{jobId}/cancel")
  @operationId("cancelJob")
  op cancel(@path jobId: string):
    | JsonResponse<200, Job>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;

  /**
   * Put a job back on its queue and return the updated record, granting one extra
   * attempt when the budget was already exhausted. A job River would leave
   * untouched, because it is running or already queued, is rejected with 409.
   */
  @post
  @route("/{jobId}/retry")
  @operationId("retryJob")
  op retry(@path jobId: string):
    | JsonResponse<200, Job>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;

  /**
   * Remove a job permanently; an active job is cancelled instead, because River
   * refuses to delete a job a worker may still hold. Non-administrators can only
   * delete their own jobs, and an unknown ID is reported as 404.
   */
  @delete
  @route("/{jobId}")
  @operationId("deleteJob")
  op delete(@path jobId: string):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;
}

@tag("Periodic Jobs")
@route("/v1/periodic-jobs")
@useAuth(TelDriveAuth)
namespace PeriodicJobs {
  /**
   * List every stored periodic job with its schedule, queue and pause state. The
   * admin or owner role is required: a schedule drives maintenance for every
   * account on the instance.
   */
  @get
  @operationId("listPeriodicJobs")
  op list():
    | JsonResponse<200, PeriodicJobList>
    | ErrorResponse<401>
    | ErrorResponse<403>;

  /**
   * Store a new periodic job definition and return it. The admin or owner role is
   * required, an identifier already in use is reported as 409, and a kind this
   * deployment has no worker for as 422 before anything is written.
   */
  @post
  @operationId("createPeriodicJob")
  op create(@body body: PeriodicJobCreate):
    | JsonResponse<201, PeriodicJob>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /**
   * List the built-in schedule templates with their kind, label, default
   * arguments, queue and recommended cron expression, which clients use to create
   * periodic jobs. The admin or owner role is required, and templates of features
   * this deployment cannot run are absent.
   */
  @get
  @route("/catalog")
  @operationId("getPeriodicJobCatalog")
  op catalog():
    | JsonResponse<200, PeriodicJobCatalog>
    | ErrorResponse<401>
    | ErrorResponse<403>;

  /**
   * Restore the built-in schedules to their catalog defaults and return them:
   * every stored definition is deleted and recreated active, so operator edits
   * are lost while the runs the old definitions already inserted stay in the job
   * table. The admin or owner role is required.
   */
  @post
  @route("/reset")
  @operationId("resetPeriodicJobs")
  op reset():
    | JsonResponse<200, PeriodicJobList>
    | ErrorResponse<401>
    | ErrorResponse<403>;

  /**
   * Replace the definition identified by the path ID and return the stored job.
   * The admin or owner role is required, an unknown ID is reported as 404, and a
   * kind this deployment has no worker for as 422.
   */
  @put
  @route("/{periodicJobId}")
  @operationId("updatePeriodicJob")
  op update(@path periodicJobId: string, @body body: PeriodicJobUpdate):
    | JsonResponse<200, PeriodicJob>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /**
   * Remove a schedule permanently; the runs it already inserted stay in the job
   * table and keep their history. The admin or owner role is required, and an
   * unknown ID is reported as 404.
   */
  @delete
  @route("/{periodicJobId}")
  @operationId("deletePeriodicJob")
  op delete(@path periodicJobId: string):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>;

  /**
   * Suspend a schedule so it inserts no further runs while keeping its
   * configuration, and return the updated definition. The admin or owner role is
   * required, and an unknown ID is reported as 404.
   */
  @post
  @route("/{periodicJobId}/pause")
  @operationId("pausePeriodicJob")
  op pause(@path periodicJobId: string):
    | JsonResponse<200, PeriodicJob>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>;

  /**
   * Reactivate a paused schedule and return the updated definition. Occurrences
   * skipped while it was paused are not replayed one by one, but a schedule whose
   * stored next run has already passed fires one catch-up run. The admin or owner
   * role is required, and an unknown ID is reported as 404.
   */
  @post
  @route("/{periodicJobId}/resume")
  @operationId("resumePeriodicJob")
  op resume(@path periodicJobId: string):
    | JsonResponse<200, PeriodicJob>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>;
}
