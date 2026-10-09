import { Accordion, Button, Card, Chip, Spinner, Typography } from "@heroui/react";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import BackIcon from "~icons/gravity-ui/arrow-left";
import RetryIcon from "~icons/gravity-ui/arrows-rotate-right";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import CheckIcon from "~icons/gravity-ui/check";
import ClockIcon from "~icons/gravity-ui/clock";
import PlayIcon from "~icons/gravity-ui/circle-play";
import PlusIcon from "~icons/gravity-ui/plus";
import XIcon from "~icons/gravity-ui/xmark";
import RefreshIcon from "~icons/gravity-ui/arrows-rotate-right";
import { TaskStatusChip, taskStatusLabel } from "../components/task-status-chip";
import type { components } from "@/api/schema";
import { $api as api, fetchClient } from "@/api/client";
import { unwrap } from "@/api/errors";
import { queryClient } from "@/api/query-client";
import { invalidateTaskQueries } from "@/api/tasks";
import { useI18n, type MessageKey, type MessageParams } from "@/lib/i18n";

/** One job as the job endpoint returns it. */
type TaskOut = components["schemas"]["Job"];
/** One recorded attempt failure, with its error text and stack trace. */
type TaskAttemptError = components["schemas"]["JobAttemptError"];

/** Translator signature for helpers that format outside a component body. */
type Translate = (key: MessageKey, params?: MessageParams) => string;

/**
 * States in which the worker still owns the job. They decide whether the page keeps
 * polling and whether the destructive button cancels the job or deletes its record.
 */
const ACTIVE_STATES = ["pending", "scheduled", "available", "running", "retryable"];

/** Interface wording for the states of an attempt; the stored values stay English. */
const ATTEMPT_LABEL_KEYS: Record<AttemptView["state"], MessageKey> = {
  completed: "routes.tasks.detail.attempt.completed",
  failed: "routes.tasks.detail.attempt.failed",
  running: "routes.tasks.detail.attempt.running",
  waiting: "routes.tasks.detail.attempt.waiting",
};

/** Interface wording for a job state; unknown states fall back to the API value. */
const STATUS_SUMMARY_KEYS: Record<string, MessageKey | undefined> = {
  pending: "routes.tasks.detail.summary.pending",
  scheduled: "routes.tasks.detail.summary.scheduled",
  available: "routes.tasks.detail.summary.available",
  retryable: "routes.tasks.detail.summary.retryable",
  running: "routes.tasks.detail.summary.running",
  completed: "routes.tasks.detail.summary.completed",
  discarded: "routes.tasks.detail.summary.discarded",
  cancelled: "routes.tasks.detail.summary.cancelled",
};

/**
 * `/tasks/$id` — one job's detail page. The loader resolves the job before the page
 * renders, so a navigation to an unknown id never reaches the component; the route
 * declares neither an error nor a not-found component, so that failure surfaces as a
 * router error rather than a friendly message.
 */
export const Route = createFileRoute("/tasks_/$id")({
  component: TaskDetailPage,
  pendingComponent: () => (
    <div className="flex items-center justify-center py-20">
      <Spinner size="lg" />
    </div>
  ),
  loader: async ({ params }) => {
    const qc = queryClient;
    // Warms the same cache entry the component reads, so the page renders without a
    // second request and without the route's pending component.
    await qc.ensureQueryData(
      api.queryOptions("get", "/v1/jobs/{jobId}", { params: { path: { jobId: params.id } } }),
    );
  },
});

/**
 * Detail view of a single job: its facts, its lifecycle timeline, the raw arguments and
 * output, the attempts that failed, and the retry/cancel/delete actions. While the job is
 * still active the page re-reads it every two seconds.
 */
function TaskDetailPage() {
  const { id } = Route.useParams();
  const navigate = useNavigate();
  const { t } = useI18n();
  const qc = queryClient;
  const _taskQuery = api.queryOptions("get", "/v1/jobs/{jobId}", {
    params: { path: { jobId: id } },
  });
  const { data } = api.useSuspenseQuery("get", "/v1/jobs/{jobId}", {
    params: { path: { jobId: id } },
  });
  const [retrying, setRetrying] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const task = data;

  // Polls only while the job is still owned by a worker; a finished job cannot change on
  // its own, so the page stops asking. The effect re-registers when the status changes,
  // which is also what stops the interval once the job settles.
  useEffect(() => {
    if (!task || !ACTIVE_STATES.includes(task.status)) return;
    const interval = window.setInterval(() => {
      qc.invalidateQueries({
        queryKey: ["get", "/v1/jobs/{jobId}", { params: { path: { jobId: id } } }],
      });
    }, 2_000);
    return () => window.clearInterval(interval);
  }, [id, qc, task?.status]);

  if (!task) {
    return (
      <div className="py-20 text-center text-sm text-muted">
        {t("routes.tasks.detail.notFound")}
      </div>
    );
  }

  /** Requeues the job after a failure, then re-reads it so the new attempt is visible. */
  const retry = async () => {
    setRetrying(true);
    try {
      await unwrap(fetchClient.POST("/v1/jobs/{jobId}/retry", { params: { path: { jobId: id } } }));
    } catch {
      toast.error(t("routes.tasks.toast.retryFailed"));
      return;
    } finally {
      setRetrying(false);
    }
    toast.success(t("routes.tasks.toast.retryQueued"));
    await invalidateTaskQueries(qc, id);
  };

  /**
   * Cancels an active job or deletes a finished one, then returns to the list. The detail
   * page has nothing left to show in the second case: its subject is gone.
   */
  const remove = async () => {
    setDeleting(true);
    try {
      if (ACTIVE_STATES.includes(task.status)) {
        await unwrap(
          fetchClient.POST("/v1/jobs/{jobId}/cancel", { params: { path: { jobId: id } } }),
        );
      } else {
        await unwrap(fetchClient.DELETE("/v1/jobs/{jobId}", { params: { path: { jobId: id } } }));
      }
    } catch {
      toast.error(
        t(
          ACTIVE_STATES.includes(task.status)
            ? "routes.tasks.toast.cancelFailed"
            : "routes.tasks.toast.removeFailed",
        ),
      );
      return;
    } finally {
      setDeleting(false);
    }
    toast.success(
      t(
        ACTIVE_STATES.includes(task.status)
          ? "routes.tasks.toast.cancelRequested"
          : "routes.tasks.toast.removed",
      ),
    );
    await invalidateTaskQueries(qc, id);
    navigate({
      to: "/tasks",
      search: { status: "running", query: "" },
    });
  };

  const refresh = () => {
    void invalidateTaskQueries(qc, id);
  };

  // Newest attempt first.
  const errors = [...(task.errors ?? [])].sort((a, b) => (b.attempt ?? 0) - (a.attempt ?? 0));
  // Broken files are recovered from the job output, which is free-form JSON.
  const brokenFiles = extractBrokenFiles(task.output);

  return (
    <div className="mx-auto flex max-w-[1500px] flex-col gap-5">
      <header className="flex flex-col gap-4 border-border border-b pb-5 lg:flex-row lg:items-start lg:justify-between">
        <div className="flex min-w-0 items-start gap-3">
          <Link
            to="/tasks"
            search={{ status: "running", query: "" }}
            className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg text-muted transition hover:bg-muted/40 hover:text-foreground"
            aria-label={t("routes.tasks.detail.back")}
          >
            <BackIcon className="size-4" />
          </Link>
          <div className="min-w-0">
            <Typography type="h1" className="truncate text-xl font-semibold sm:text-2xl">
              {task.description ?? defaultTaskDescription(task)}
            </Typography>
            <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
              <span className="font-medium text-foreground">{task.type}</span>
              <span className="font-mono">{t("routes.tasks.detail.id", { id: task.id })}</span>
              {task.parentId && (
                <Link
                  to="/tasks/$id"
                  params={{ id: task.parentId }}
                  className="hover:text-foreground"
                >
                  {t("routes.tasks.detail.parent", { id: task.parentId })}
                </Link>
              )}
            </div>
          </div>
        </div>

        <div className="flex flex-wrap gap-2 lg:justify-end">
          <Button size="sm" variant="tertiary" onPress={refresh}>
            <RefreshIcon className="size-3.5" /> {t("common.action.refresh")}
          </Button>
          {["cancelled", "discarded", "retryable"].includes(task.status) && (
            <Button size="sm" variant="primary" isPending={retrying} onPress={retry}>
              {t("common.action.retry")}
            </Button>
          )}
          <Button
            size="sm"
            variant={ACTIVE_STATES.includes(task.status) ? "danger" : "tertiary"}
            isPending={deleting}
            onPress={remove}
          >
            {ACTIVE_STATES.includes(task.status)
              ? t("common.action.cancel")
              : t("common.action.delete")}
          </Button>
        </div>
      </header>

      <Card className="overflow-hidden p-0">
        <Card.Header className="flex-col items-start gap-1 border-border border-b px-5 py-4">
          <Card.Title className="text-sm font-semibold">
            {t("routes.tasks.detail.execution.title")}
          </Card.Title>
          <Card.Description className="text-xs">
            {t("routes.tasks.detail.execution.description")}
          </Card.Description>
        </Card.Header>
        <div className="grid lg:grid-cols-[minmax(0,1fr)_minmax(20rem,0.85fr)]">
          <div className="border-border p-5 lg:border-r sm:p-6">
            <div className="grid gap-x-8 gap-y-5 sm:grid-cols-3">
              <Fact
                label={t("routes.tasks.detail.fact.state")}
                value={taskStatusLabel(task.status)}
              >
                <TaskStatusChip status={task.status} />
              </Fact>
              <Fact
                label={t("routes.tasks.detail.fact.attempt")}
                value={`${task.attempt} / ${task.maxAttempts}`}
              />
              <Fact label={t("routes.tasks.detail.fact.priority")} value={String(task.priority)} />
              <Fact label={t("routes.tasks.detail.fact.queue")} value={task.queue || "default"} />
              <Fact
                label={t("routes.tasks.detail.fact.tags")}
                value={task.tags?.length ? task.tags.join(", ") : "—"}
              />
              <Fact
                label={t("routes.tasks.detail.fact.created")}
                value={formatRelative(task.createdAt, t)}
              />
            </div>

            <div className="mt-6 border-border border-t pt-5">
              <div className="text-[11px] font-semibold text-muted uppercase tracking-[0.14em]">
                {t("routes.tasks.detail.currentStatus")}
              </div>
              <div className="mt-2 text-base font-semibold">
                {task.message || statusSummary(task.status, t)}
              </div>
            </div>
          </div>

          <div className="p-5 sm:p-6">
            <h2 className="text-sm font-semibold">{t("routes.tasks.detail.lifecycle")}</h2>
            <div className="mt-4">
              <Timeline task={task} />
            </div>
          </div>
        </div>
      </Card>

      <div className="grid gap-5 lg:grid-cols-2">
        <JsonPanel
          title={t("routes.tasks.detail.arguments.title")}
          value={task.args}
          empty={t("routes.tasks.detail.arguments.empty")}
        />
        <JsonPanel
          title={t("routes.tasks.detail.output.title")}
          value={task.output}
          empty={t("routes.tasks.detail.output.empty")}
          downloadName={`task-${task.id}-output.json`}
        />
      </div>

      {brokenFiles.length > 0 && (
        <BrokenFilesCard files={brokenFiles} truncated={isOutputTruncated(task.output)} />
      )}

      <Card className="overflow-hidden p-0">
        <div className="flex items-center justify-between border-border border-b px-5 py-4">
          <div>
            <h2 className="text-sm font-semibold">{t("routes.tasks.detail.attempts.title")}</h2>
            <p className="mt-1 text-xs text-muted">
              {t("routes.tasks.detail.attempts.description")}
            </p>
          </div>
          <Chip size="sm" variant="tertiary">
            {/* The attempt counter and the recorded failures can disagree, so the header
                shows whichever of the two is larger. */}
            {Math.max(task.attempt ?? 0, errors.length)}
          </Chip>
        </div>
        <div className="divide-y divide-border">
          {buildAttempts(task, errors).map((attempt) => (
            <AttemptRow key={`${attempt.attempt}-${attempt.state}`} attempt={attempt} />
          ))}
        </div>
      </Card>
    </div>
  );
}

/** One fact in the execution grid: a small caption and either a value or custom content. */
function Fact({
  label,
  value,
  children,
}: {
  label: string;
  value: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="min-w-0">
      <div className="text-[10px] font-semibold text-muted uppercase tracking-wide">{label}</div>
      <div className="mt-2 truncate text-sm font-medium">{children ?? value}</div>
    </div>
  );
}

/**
 * Lifecycle timeline of one job. The steps are fixed and every one is rendered; only their
 * state varies, so the reader always sees the whole path and where the job stopped. The
 * error and retry steps are inserted conditionally, and the last step is the terminal one.
 */
function Timeline({ task }: { task: TaskOut }) {
  const { t } = useI18n();
  const errors = task.errors ?? [];
  const steps = [
    {
      label: t("routes.tasks.detail.timeline.created"),
      at: task.createdAt,
      state: "done",
      kind: "created",
    },
    {
      label: t("routes.tasks.detail.timeline.scheduled"),
      at: task.scheduledAt,
      state: timelineState(task, "scheduled"),
      kind: "scheduled",
    },
    {
      label: t("routes.tasks.detail.timeline.running"),
      at: task.startedAt,
      state: timelineState(task, "running"),
      kind: "running",
    },
    ...(errors.length > 0
      ? [
          {
            label: t("routes.tasks.detail.timeline.errored"),
            at: errors.at(-1)?.at,
            state: task.status === "discarded" ? "error" : "done",
            kind: "error",
          },
        ]
      : []),
    ...(task.status === "retryable"
      ? [
          {
            label: t("routes.tasks.detail.timeline.awaitingRetry"),
            at: task.scheduledAt,
            state: "current",
            kind: "retry",
          },
        ]
      : []),
    {
      label:
        task.status === "cancelled"
          ? t("routes.tasks.detail.timeline.cancelled")
          : task.status === "discarded"
            ? t("routes.tasks.detail.timeline.discarded")
            : t("routes.tasks.detail.timeline.complete"),
      at: task.completedAt,
      state: ["completed", "cancelled", "discarded"].includes(task.status)
        ? task.status === "discarded"
          ? "error"
          : "done"
        : "future",
      kind:
        task.status === "cancelled"
          ? "cancelled"
          : task.status === "discarded"
            ? "discarded"
            : "completed",
    },
  ];

  return (
    <ol>
      {steps.map((step, index) => (
        <li key={step.label} className="relative flex gap-3 pb-5 last:pb-0">
          {index < steps.length - 1 && (
            <span className="absolute top-6 bottom-0 left-[9px] w-px bg-border" />
          )}
          <span
            className={`relative mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full ${timelineIconClass(step.kind, step.state)}`}
          >
            <TimelineIcon kind={step.kind} className="size-3.5" />
          </span>
          <div className="min-w-0">
            <div className="text-sm font-medium">{step.label}</div>
            <div className="mt-0.5 text-xs text-muted">
              {step.at ? `${formatRelative(step.at, t)} · ${formatDate(step.at)}` : "—"}
            </div>
          </div>
        </li>
      ))}
    </ol>
  );
}

/** One row of the attempts list, assembled from the recorded errors and the job itself. */
type AttemptView = {
  /** Attempt number, 1-based; 0 stands for a job that has not run yet. */
  attempt: number;
  /** Outcome shown for the attempt; "waiting" is a job that has not been picked up yet. */
  state: "completed" | "failed" | "running" | "waiting";
  /** When the attempt finished, started or was scheduled, as far as it is known. */
  at?: string;
  /** Error text of a failed attempt. */
  error?: string;
  /** Stack trace of a failed attempt, shown behind a disclosure. */
  trace?: string;
  /** Worker that handled the attempt, when the job records it. */
  worker?: string;
};

/**
 * Worker that handled one attempt, looked up by the attempt number rather than by a
 * position in another array: River appends to `attemptedBy` in attempt order, while the
 * attempts list is rendered newest first. A job whose attempt counter is ahead of the
 * workers it recorded (an attempt that has not started yet) or a row without an attempt
 * number therefore gets no worker instead of another attempt's.
 */
function attemptWorker(task: TaskOut, attempt: number): string | undefined {
  const index = attempt - 1;
  const workers = task.attemptedBy ?? [];
  return index >= 0 && index < workers.length ? workers[index] : undefined;
}

/**
 * Merges the recorded failures with the job's own progress into the rows the attempts
 * list shows: one row per failure, plus a row for the current attempt when it is newer
 * than the last recorded failure — or when a completed job's final attempt succeeded.
 * A job with nothing recorded still gets a synthetic "waiting" row, so the panel is never
 * empty, and the result is ordered newest attempt first.
 */
function buildAttempts(task: TaskOut, errors: TaskAttemptError[]): AttemptView[] {
  const attempts: AttemptView[] = errors.map((error) => {
    // The attempt number is what identifies the row in the job's own bookkeeping, so it is
    // also the key the worker is read by; the sorting of `errors` must not shift it.
    const attempt = error.attempt ?? 0;
    return {
      attempt,
      state: "failed",
      at: error.at,
      error: error.error,
      trace: error.trace,
      worker: attemptWorker(task, attempt),
    };
  });

  const latestErrorAttempt = errors.reduce((max, error) => Math.max(max, error.attempt ?? 0), 0);
  if ((task.attempt ?? 0) > latestErrorAttempt) {
    attempts.push({
      attempt: task.attempt ?? 0,
      state:
        task.status === "completed"
          ? "completed"
          : task.status === "running"
            ? "running"
            : "waiting",
      at: task.completedAt ?? task.startedAt ?? task.scheduledAt,
      worker: attemptWorker(task, task.attempt ?? 0),
    });
  } else if (task.status === "completed" && (task.attempt ?? 0) > 0) {
    attempts.push({
      attempt: task.attempt ?? 0,
      state: "completed",
      at: task.completedAt,
      worker: attemptWorker(task, task.attempt ?? 0),
    });
  }

  if (attempts.length === 0) {
    attempts.push({ attempt: 0, state: "waiting", at: task.scheduledAt });
  }

  return attempts.sort((a, b) => b.attempt - a.attempt);
}

/** One attempt: its outcome, when it happened, the worker, and the error behind a disclosure. */
function AttemptRow({ attempt }: { attempt: AttemptView }) {
  const { t } = useI18n();
  return (
    <div className="px-5 py-4">
      <div className="flex items-start gap-3">
        <span className={`mt-1 size-2.5 shrink-0 rounded-full ${attemptDot(attempt.state)}`} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="text-sm font-semibold">
              {t(ATTEMPT_LABEL_KEYS[attempt.state])}{" "}
              <span className="font-normal text-muted">
                {t("routes.tasks.detail.attempt.number", { number: attempt.attempt })}
              </span>
            </div>
            <div className="text-xs text-muted">
              {attempt.at ? formatRelative(attempt.at, t) : "—"}
            </div>
          </div>
          {attempt.worker && (
            <div className="mt-1 font-mono text-[11px] text-muted">{attempt.worker}</div>
          )}
          {attempt.error && <div className="mt-3 text-sm text-danger">{attempt.error}</div>}
          {attempt.trace && (
            <Accordion hideSeparator className="mt-3">
              <Accordion.Item id="trace">
                <Accordion.Heading>
                  <Accordion.Trigger className="text-xs font-medium text-muted hover:text-foreground">
                    {t("routes.tasks.detail.attempt.trace")}
                    <Accordion.Indicator />
                  </Accordion.Trigger>
                </Accordion.Heading>
                <Accordion.Panel>
                  <Accordion.Body>
                    <pre className="max-h-72 overflow-auto rounded-lg bg-muted/15 p-3 text-[11px] leading-relaxed whitespace-pre-wrap">
                      {attempt.trace}
                    </pre>
                  </Accordion.Body>
                </Accordion.Panel>
              </Accordion.Item>
            </Accordion>
          )}
        </div>
      </div>
    </div>
  );
}

/**
 * Read-only JSON viewer: pretty-prints the value with its keys in a stable order, offers a
 * download when a filename is given, and shows `empty` instead of an empty document. The
 * `tall` flag is for the panel that is not side by side with another one.
 */
function JsonPanel({
  title,
  value,
  empty,
  tall = false,
  downloadName,
}: {
  title: string;
  value?: unknown;
  empty: string;
  tall?: boolean;
  downloadName?: string;
}) {
  const { t } = useI18n();
  return (
    <Card className="overflow-hidden p-0">
      <div className="flex items-center justify-between gap-2 border-border border-b px-5 py-3.5">
        <h2 className="text-sm font-semibold">{title}</h2>
        {downloadName && hasJsonValue(value) && (
          <Button
            size="sm"
            variant="tertiary"
            onPress={() => downloadJsonFile(downloadName, value)}
          >
            <DownloadIcon className="size-3.5" /> {t("common.action.download")}
          </Button>
        )}
      </div>
      {hasJsonValue(value) ? (
        <pre
          className={`${tall ? "max-h-136" : "max-h-72"} overflow-auto bg-muted/10 p-4 text-[11px] leading-relaxed whitespace-pre-wrap wrap-break-word`}
        >
          {JSON.stringify(sortJsonKeys(value), null, 2)}
        </pre>
      ) : (
        <div className="p-4 text-sm text-muted">{empty}</div>
      )}
    </Card>
  );
}

/**
 * One entry of the job output's `brokenFiles` list. Every field is `unknown` on purpose:
 * the shape comes from unstructured worker output, so each one is type-checked at the
 * point of use rather than trusted here.
 */
type BrokenFileEntry = {
  /** Id of the file the missing parts belong to. */
  fileId?: unknown;
  /** Display name of the file, when the worker recorded one. */
  name?: unknown;
  /** Declared size in bytes, validated by `formatBytes` before it is shown. */
  size?: unknown;
  /** Storage channel the file's parts live in. */
  channelId?: unknown;
  /** Message ids whose parts could not be found; the count is what the card shows. */
  missingMessageIds?: unknown;
};

/** Reads `output.brokenFiles` out of a job's free-form output, ignoring anything unusable. */
function extractBrokenFiles(output: unknown): BrokenFileEntry[] {
  if (!output || typeof output !== "object" || Array.isArray(output)) return [];
  const list = (output as Record<string, unknown>).brokenFiles;
  if (!Array.isArray(list)) return [];
  return list.filter(
    (item): item is BrokenFileEntry => !!item && typeof item === "object" && !Array.isArray(item),
  );
}

/**
 * Whether the worker dropped the broken-file list to stay inside the job-output budget.
 * The counts in the same output stay exact when it does, but the list itself is empty.
 */
function isOutputTruncated(output: unknown): boolean {
  if (!output || typeof output !== "object" || Array.isArray(output)) return false;
  return (output as Record<string, unknown>).brokenTruncated === true;
}

/** Number of missing message ids on one entry; a malformed value counts as none. */
function missingPartCount(entry: BrokenFileEntry): number {
  return Array.isArray(entry.missingMessageIds) ? entry.missingMessageIds.length : 0;
}

/**
 * Files the job reported as damaged, with how many of their parts are missing. The caller
 * renders the card only when at least one entry survived, and `truncated` adds the note
 * that the worker dropped the list for size.
 */
function BrokenFilesCard({ files, truncated }: { files: BrokenFileEntry[]; truncated: boolean }) {
  const { t } = useI18n();
  return (
    <Card className="overflow-hidden p-0">
      <div className="flex flex-wrap items-center justify-between gap-2 border-border border-b px-5 py-3.5">
        <div>
          <h2 className="text-sm font-semibold">{t("routes.tasks.detail.brokenFiles.title")}</h2>
          <p className="mt-0.5 text-xs text-muted">
            {t("routes.tasks.detail.brokenFiles.description")}
            {truncated ? ` ${t("routes.tasks.detail.brokenFiles.truncated")}` : ""}
          </p>
        </div>
        <Button
          size="sm"
          variant="tertiary"
          onPress={() => downloadJsonFile("broken-files.json", files)}
        >
          <DownloadIcon className="size-3.5" /> {t("routes.tasks.detail.brokenFiles.download")}
        </Button>
      </div>
      <div className="divide-y divide-border">
        {files.map((file, index) => (
          <div
            key={typeof file.fileId === "string" ? file.fileId : index}
            className="flex flex-wrap items-center gap-x-6 gap-y-1 px-5 py-3"
          >
            <span className="min-w-0 flex-1 truncate text-sm font-medium">
              {typeof file.name === "string" && file.name
                ? file.name
                : t("routes.tasks.detail.brokenFiles.unnamed")}
            </span>
            <span className="text-xs text-muted">{formatBytes(file.size)}</span>
            <span className="text-xs text-muted">
              {t("routes.tasks.detail.brokenFiles.missingParts", {
                count: missingPartCount(file),
              })}
            </span>
          </div>
        ))}
      </div>
    </Card>
  );
}

/** Saves a value as a pretty-printed JSON file through a temporary object URL. */
function downloadJsonFile(filename: string, value: unknown) {
  const blob = new Blob([JSON.stringify(value, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  // The download begins asynchronously, so the object URL has to outlive this
  // tick: revoking it here can cancel the transfer in browsers that have not
  // started reading the blob yet.
  window.setTimeout(() => URL.revokeObjectURL(url), 1_000);
}

/**
 * Formats a value from free-form output as a size. Anything that is not a finite,
 * non-negative number reads as an em dash, and the unit list stops at TB.
 */
function formatBytes(value: unknown): string {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "—";
  if (value === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

/**
 * Rebuilds the value with object keys sorted alphabetically, recursively. Two payloads
 * that differ only in key order then render identically; array order is preserved, since
 * it is part of the data.
 */
function sortJsonKeys(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortJsonKeys);
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(
    Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, item]) => [key, sortJsonKeys(item)]),
  );
}

/**
 * Visual state of one timeline step: `current` when the job is sitting on it, `done`
 * once the job has moved past it, `future` for a step that has not been reached. A job
 * that ended by cancellation or discard is decided by whether it ever started, because
 * the ordering list below has no entry for those two statuses.
 */
function timelineState(task: TaskOut, stage: string) {
  const order = ["pending", "scheduled", "available", "running", "retryable", "completed"];
  if (task.status === stage) return "current";
  if (["cancelled", "discarded"].includes(task.status)) return task.startedAt ? "done" : "future";
  return order.indexOf(task.status) > order.indexOf(stage) ? "done" : "future";
}

/** Icon for a step kind; the varied terminal outcomes share the cross. */
function TimelineIcon({ kind, className }: { kind: string; className?: string }) {
  if (kind === "created") return <PlusIcon className={className} />;
  if (kind === "scheduled") return <ClockIcon className={className} />;
  if (kind === "running") return <PlayIcon className={className} />;
  if (kind === "retry") return <RetryIcon className={className} />;
  if (kind === "completed") return <CheckIcon className={className} />;
  return <XIcon className={className} />;
}

/** Colour pair of a timeline marker; an unreached step is muted whatever its kind. */
function timelineIconClass(kind: string, state: string) {
  if (state === "future") return "bg-muted/25 text-muted";
  if (kind === "created") return "bg-accent/15 text-accent";
  if (kind === "scheduled") return "bg-warning/15 text-warning";
  if (kind === "running") return "bg-accent/15 text-accent";
  if (kind === "retry") return "bg-warning/15 text-warning";
  if (kind === "completed") return "bg-success/15 text-success";
  if (kind === "cancelled") return "bg-muted/30 text-muted";
  return "bg-danger/15 text-danger";
}

/** Dot colour of an attempt row; a not-yet-run attempt keeps the warning colour. */
function attemptDot(state: AttemptView["state"]) {
  if (state === "completed") return "bg-success";
  if (state === "failed") return "bg-danger";
  if (state === "running") return "bg-accent";
  return "bg-warning";
}

/**
 * One-line explanation of a status, falling back to the shared status label for a value
 * this page has no wording for.
 */
function statusSummary(status: string, t: Translate) {
  const key = STATUS_SUMMARY_KEYS[status];
  return key ? t(key) : taskStatusLabel(status);
}

/** Heading fallback for a job the creator left without a description: its kind. */
function defaultTaskDescription(task: TaskOut) {
  return task.type;
}

/** Whether a JSON panel has anything to show; empty containers count as nothing. */
function hasJsonValue(value: unknown) {
  if (value == null) return false;
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === "object") return Object.keys(value).length > 0;
  return true;
}

/** Full local date-time, paired with the relative age in the timeline and attempt rows. */
function formatDate(value: string) {
  return new Date(value).toLocaleString();
}

/**
 * Age of an instant as "just now", minutes, hours or days ago. Unlike the list page there
 * is no week cutoff, so every age is expressed in days.
 */
function formatRelative(value: string, t: Translate) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 60) return t("routes.tasks.relative.justNow");
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return t("routes.tasks.relative.minutesAgo", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t("routes.tasks.relative.hoursAgo", { count: hours });
  const days = Math.floor(hours / 24);
  return t("routes.tasks.relative.daysAgo", { count: days });
}
