import { Button, Card, Chip, Input, ListBox, Popover, Select, Spinner } from "@heroui/react";
import { createFileRoute, Link, stripSearchParams } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import PrevIcon from "~icons/gravity-ui/chevron-left";
import NextIcon from "~icons/gravity-ui/chevron-right";
import FilterIcon from "~icons/gravity-ui/funnel";
import AddIcon from "~icons/gravity-ui/plus";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import { ConfirmDialog } from "../components/dialogs/confirm-dialog";
import { EmptyState, Page, PageHeader, PageToolbar } from "../components/page";
import { TaskLauncher } from "../components/task-launcher";
import { TaskStatusChip } from "../components/task-status-chip";
import type { components } from "@/api/schema";
import { $api as api, fetchClient } from "@/api/client";
import { unwrap } from "@/api/errors";
import { queryClient } from "@/api/query-client";
import { invalidateTaskQueries } from "@/api/tasks";
import { useI18n, type MessageKey, type MessageParams } from "@/lib/i18n";

/** One job as the jobs endpoint returns it. */
type TaskOut = components["schemas"]["Job"];
/** Per-status job counters, used for the tab badges and the cleanup confirmation. */
type TaskCounts = components["schemas"]["JobStatistics"];
/** One River queue with its pause state and pending/running counts. */
type TaskQueueOut = components["schemas"]["JobQueue"];
/** Validated `/tasks` URL state: which status is listed and the client-side text filter. */
type TaskSearch = {
  /** Status whose jobs are listed; the server filters on it. */
  status: string;
  /** Free-text filter applied to the rows already loaded, not sent to the server. */
  query: string;
};

/** Translator signature for helpers that format outside a component body. */
type Translate = (key: MessageKey, params?: MessageParams) => string;

/** Jobs per page. The loader's prefetch has to ask for the same limit to hit its cache entry. */
const PAGE_SIZE = 20;
/**
 * Statuses that are still owned by the worker, where the row's destructive action means
 * "cancel" rather than "delete". Anything else is finished and can be removed outright.
 */
const CANCELLABLE_STATUSES = ["pending", "scheduled", "available", "running", "retryable"];

/** Status filter options in display order; doubles as the allow-list in `validateSearch`. */
const STATUS_TABS = [
  { key: "pending", labelKey: "routes.tasks.status.pending" },
  { key: "scheduled", labelKey: "routes.tasks.status.scheduled" },
  { key: "available", labelKey: "routes.tasks.status.available" },
  { key: "running", labelKey: "routes.tasks.status.running" },
  { key: "retryable", labelKey: "routes.tasks.status.retryable" },
  { key: "cancelled", labelKey: "routes.tasks.status.cancelled" },
  { key: "discarded", labelKey: "routes.tasks.status.discarded" },
  { key: "completed", labelKey: "routes.tasks.status.completed" },
] as const satisfies readonly { key: string; labelKey: MessageKey }[];

/**
 * `/tasks` — the job console. The status lives in the URL, so a filtered view can be
 * linked and reloaded; the text filter does not, because it only narrows the rows already
 * fetched. The loader warms the running page plus the counters and queue list, and the
 * middleware drops parameters that equal their defaults to keep the URL short.
 */
export const Route = createFileRoute("/tasks")({
  validateSearch: (search: Record<string, unknown>): TaskSearch => ({
    // An unknown status falls back to "running" rather than listing everything: the
    // endpoint requires a status, and the default tab is the one that shows live work.
    status:
      typeof search.status === "string" &&
      [
        "pending",
        "scheduled",
        "available",
        "running",
        "retryable",
        "cancelled",
        "discarded",
        "completed",
      ].includes(search.status)
        ? search.status
        : "running",
    query: typeof search.query === "string" ? search.query : "",
  }),
  search: {
    middlewares: [
      // Removes `status=running` and `query=` from the URL once they hold the defaults,
      // so the canonical `/tasks` link never carries redundant parameters.
      stripSearchParams({
        status: "running" as components["schemas"]["JobState"],
        query: "",
      }),
    ],
  },
  component: TasksPage,
  pendingComponent: () => (
    <div className="flex items-center justify-center py-16">
      <Spinner size="lg" />
    </div>
  ),
  loader: async () => {
    const qc = queryClient;
    // Prefetches exactly the three queries the page renders on entry — the default status
    // tab's first page, the counters and the queues — so the first paint has no spinners.
    await Promise.all([
      qc.ensureQueryData(
        api.queryOptions("get", "/v1/jobs", {
          params: {
            query: { limit: PAGE_SIZE, status: "running" as components["schemas"]["JobState"] },
          },
        }),
      ),
      qc.ensureQueryData(api.queryOptions("get", "/v1/jobs/statistics")),
      qc.ensureQueryData(api.queryOptions("get", "/v1/jobs/queues")),
    ]);
  },
});

/**
 * Job console: a status-filtered, cursor-paginated list of jobs with per-row retry,
 * cancel/delete and a queue pause/resume panel. The list and the counters are polled, so
 * the page shows work progressing without a manual refresh.
 */
function TasksPage() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  const { t } = useI18n();
  const qc = queryClient;
  // Keyset pagination state: the cursor of the page on screen, and the stack of cursors
  // that preceded it, which is what makes going back possible at all (the API has no
  // offset paging). Both are reset whenever the status changes.
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorHistory, setCursorHistory] = useState<(string | undefined)[]>([]);
  const { data } = api.useSuspenseQuery("get", "/v1/jobs", {
    params: {
      query: {
        cursor,
        limit: PAGE_SIZE,
        status: search.status as components["schemas"]["JobState"],
      },
    },
  });
  const _statesQuery = api.queryOptions("get", "/v1/jobs/statistics");
  const { data: statesData } = api.useSuspenseQuery("get", "/v1/jobs/statistics");
  const _queuesQuery = api.queryOptions("get", "/v1/jobs/queues");
  const { data: queuesData } = api.useSuspenseQuery("get", "/v1/jobs/queues");

  // Whether the job composer dialog is open.
  const [composerOpen, setComposerOpen] = useState(false);
  // Rows with an in-flight action; only the matching row shows a pending button.
  const [retryingId, setRetryingId] = useState<string | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  // Status whose finished jobs the confirmation dialog offers to purge; null closes it.
  const [cleanupStatus, setCleanupStatus] = useState<
    "completed" | "retryable" | "discarded" | null
  >(null);
  const [cleaning, setCleaning] = useState(false);

  const tasks = data.tasks ?? [];
  const meta = data.meta;
  const taskStats = statesData;
  const taskQueues = queuesData.queues ?? [];
  // Drives both the live indicator and the polling rate below.
  const hasActiveTasks =
    (taskStats?.scheduled ?? 0) +
      (taskStats?.available ?? 0) +
      (taskStats?.running ?? 0) +
      (taskStats?.retryable ?? 0) >
    0;

  // Polling of the three queries on screen. It runs every 2 seconds while work is
  // outstanding and backs off to 10 seconds once nothing is queued or running, so an idle
  // console is not a busy one. The effect re-registers when the cursor or status changes,
  // which retargets the listing invalidation at the page actually being viewed.
  useEffect(() => {
    const interval = window.setInterval(
      () => {
        qc.invalidateQueries({
          queryKey: [
            "get",
            "/v1/jobs",
            {
              params: {
                query: {
                  cursor,
                  limit: PAGE_SIZE,
                  status: search.status as components["schemas"]["JobState"],
                },
              },
            },
          ],
        });
        qc.invalidateQueries({ queryKey: ["get", "/v1/jobs/statistics"] });
        qc.invalidateQueries({ queryKey: ["get", "/v1/jobs/queues"] });
      },
      hasActiveTasks ? 2_000 : 10_000,
    );
    return () => window.clearInterval(interval);
  }, [cursor, hasActiveTasks, qc, search.status]);

  // The text filter is applied to the loaded page only: the endpoint has no free-text
  // parameter, so it narrows what is on screen rather than asking for other matches.
  const normalizedQuery = search.query.trim().toLowerCase();
  const filteredTasks = tasks.filter((task) => {
    const statusMatches = task.status === search.status;
    const textMatches =
      !normalizedQuery ||
      [
        task.id,
        task.type,
        task.queue,
        task.description,
        task.message,
        task.output ? JSON.stringify(task.output) : undefined,
      ]
        .filter(Boolean)
        .some((value) => String(value).toLowerCase().includes(normalizedQuery));
    return statusMatches && textMatches;
  });
  /**
   * Writes a new filter into the URL. Switching status waits for that status' first page
   * first, so the table is not replaced by the route's pending component while the new
   * listing loads; the cursor is reset either way, since it belongs to the old listing.
   */
  const setSearch = async (next: Partial<TaskSearch>) => {
    const nextSearch = { ...search, ...next };

    if (next.status && next.status !== search.status) {
      await qc.ensureQueryData(
        api.queryOptions("get", "/v1/jobs", {
          params: {
            query: {
              limit: PAGE_SIZE,
              status: nextSearch.status as components["schemas"]["JobState"],
            },
          },
        }),
      );
    }

    setCursor(undefined);
    setCursorHistory([]);
    await navigate({ search: nextSearch, replace: true });
  };

  /** Pushes the current cursor onto the stack and moves to the next page. */
  const goNext = () => {
    if (!meta?.nextCursor) return;
    setCursorHistory((history) => [...history, cursor]);
    setCursor(meta.nextCursor);
  };

  /** Pops the stack; its depth is also the page number shown between the two buttons. */
  const goPrevious = () => {
    setCursorHistory((history) => {
      const previous = history.at(-1);
      setCursor(previous);
      return history.slice(0, -1);
    });
  };

  /** Invalidates the listing, counters and queues after any change to the jobs. */
  const refreshTasks = () => {
    void invalidateTaskQueries(qc);
  };

  /** Requeues one job. The row stays pending until the request settles, not until it runs. */
  const retryTask = async (task: TaskOut) => {
    setRetryingId(task.id);
    try {
      await unwrap(
        fetchClient.POST("/v1/jobs/{jobId}/retry", { params: { path: { jobId: task.id } } }),
      );
    } catch {
      toast.error(t("routes.tasks.toast.retryFailed"));
      return;
    } finally {
      setRetryingId(null);
    }
    toast.success(t("routes.tasks.toast.retryQueued"));
    refreshTasks();
  };

  /**
   * The row's destructive action: a job still owned by a worker is cancelled, so the
   * worker can stop it; a finished one is deleted. The same test picks the toast copy.
   */
  const deleteTask = async (task: TaskOut) => {
    setDeletingId(task.id);
    try {
      if (CANCELLABLE_STATUSES.includes(task.status)) {
        await unwrap(
          fetchClient.POST("/v1/jobs/{jobId}/cancel", { params: { path: { jobId: task.id } } }),
        );
      } else {
        await unwrap(
          fetchClient.DELETE("/v1/jobs/{jobId}", { params: { path: { jobId: task.id } } }),
        );
      }
    } catch {
      toast.error(
        t(
          CANCELLABLE_STATUSES.includes(task.status)
            ? "routes.tasks.toast.cancelFailed"
            : "routes.tasks.toast.removeFailed",
        ),
      );
      return;
    } finally {
      setDeletingId(null);
    }
    toast.success(
      t(
        CANCELLABLE_STATUSES.includes(task.status)
          ? "routes.tasks.toast.cancelRequested"
          : "routes.tasks.toast.removed",
      ),
    );
    refreshTasks();
  };

  /** Purges every job in one finished status; the pagination restarts on the new listing. */
  const cleanTasks = async () => {
    if (!cleanupStatus) return;
    setCleaning(true);
    try {
      const response = await unwrap(
        fetchClient.DELETE("/v1/jobs/purge", { params: { query: { status: cleanupStatus } } }),
      );
      const status = cleanupStatus;
      setCleanupStatus(null);
      setCursor(undefined);
      setCursorHistory([]);
      toast.success(t("routes.tasks.toast.cleaned", { count: response.count, status }));
      refreshTasks();
    } catch {
      toast.error(t("routes.tasks.toast.cleanFailed", { status: cleanupStatus }));
    } finally {
      setCleaning(false);
    }
  };

  return (
    <Page>
      <PageHeader
        title={t("routes.tasks.title")}
        description={t("routes.tasks.description")}
        actions={
          <Button
            size="sm"
            variant="primary"
            className="bg-accent text-accent-foreground"
            onPress={() => setComposerOpen(true)}
          >
            <AddIcon className="size-3.5" /> {t("routes.tasks.action.new")}
          </Button>
        }
      />

      {composerOpen && (
        <TaskLauncher
          onClose={() => setComposerOpen(false)}
          onQueued={() => {
            refreshTasks();
            setComposerOpen(false);
          }}
        />
      )}

      <PageToolbar className="items-stretch">
        <div className="flex min-w-0 flex-1 flex-col gap-2 md:flex-row md:items-center">
          <Input
            aria-label={t("routes.tasks.search.label")}
            placeholder={t("routes.tasks.search.placeholder")}
            value={search.query}
            onChange={(event) => setSearch({ query: event.currentTarget.value })}
            className="min-w-0 flex-1"
          />
          <div className="flex flex-wrap items-center gap-2">
            <TaskStatusSelect
              value={search.status}
              counts={taskStats}
              onChange={(status) => setSearch({ status })}
            />
            <QueueManager queues={taskQueues} onChanged={refreshTasks} />
            <div className="w-[4.75rem] shrink-0">
              {/* The purge button only exists for a status whose jobs are finished, and it
                  is an inline IIFE so that narrowing stays local to the JSX. */}
              {(() => {
                const purgeStatus = ["completed", "retryable", "discarded"].includes(search.status)
                  ? (search.status as "completed" | "retryable" | "discarded")
                  : null;
                return purgeStatus ? (
                  <Button
                    size="sm"
                    variant="danger-soft"
                    className="w-full"
                    isDisabled={(taskStats?.[purgeStatus] ?? 0) === 0}
                    onPress={() => setCleanupStatus(purgeStatus)}
                  >
                    <TrashIcon className="size-3.5" /> {t("routes.tasks.action.clean")}
                  </Button>
                ) : null;
              })()}
            </div>
          </div>
        </div>
      </PageToolbar>

      <Card className="overflow-hidden p-0">
        <div className="flex items-center justify-between border-border border-b px-4 py-3">
          <div className="text-sm font-semibold">{t("routes.tasks.activity.title")}</div>
          {hasActiveTasks && (
            <span className="flex items-center gap-2 text-xs text-muted">
              <span className="size-2 animate-pulse rounded-full bg-accent" />
              {t("routes.tasks.activity.live")}
            </span>
          )}
        </div>
        {filteredTasks.length > 0 ? (
          <ListBox
            aria-label={t("routes.tasks.title")}
            selectionMode="none"
            className="w-full min-w-0 divide-y divide-border overflow-hidden p-0"
          >
            {filteredTasks.map((task) => (
              <TaskRow
                key={task.id}
                task={task}
                onRetry={() => retryTask(task)}
                onDelete={() => deleteTask(task)}
                retryPending={retryingId === task.id}
                deletePending={deletingId === task.id}
              />
            ))}
          </ListBox>
        ) : (
          <EmptyState
            title={t("routes.tasks.empty.title")}
            description={t("routes.tasks.empty.description")}
            action={
              <Button size="sm" variant="primary" onPress={() => setComposerOpen(true)}>
                {t("routes.tasks.action.new")}
              </Button>
            }
          />
        )}
      </Card>

      {(cursorHistory.length > 0 || Boolean(meta?.nextCursor)) && (
        <div className="flex items-center justify-end gap-2">
          <Button
            size="sm"
            variant="tertiary"
            isDisabled={cursorHistory.length === 0}
            onPress={goPrevious}
          >
            <PrevIcon className="size-3.5" /> {t("routes.tasks.pagination.previous")}
          </Button>
          <span className="min-w-20 text-center text-xs text-muted">
            {t("routes.tasks.pagination.page", { page: cursorHistory.length + 1 })}
          </span>
          <Button size="sm" variant="tertiary" isDisabled={!meta?.nextCursor} onPress={goNext}>
            {t("common.action.next")} <NextIcon className="size-3.5" />
          </Button>
        </div>
      )}

      <ConfirmDialog
        open={cleanupStatus != null}
        onOpenChange={(open) => !open && setCleanupStatus(null)}
        onConfirm={cleanTasks}
        title={t("routes.tasks.confirm.clean.title", { status: cleanupStatus ?? "" })}
        message={t("routes.tasks.confirm.clean.message", {
          count: cleanupStatus ? (taskStats?.[cleanupStatus] ?? 0) : 0,
          status: cleanupStatus ?? "",
        })}
        confirmLabel={t("routes.tasks.action.clean")}
        isPending={cleaning}
      />
    </Page>
  );
}

/**
 * One job line: identity, queue, duration and age, plus the retry and cancel/delete
 * actions. The pending flags come from the page so that only the row being acted on
 * shows a spinner.
 */
function TaskRow({
  task,
  onRetry,
  onDelete,
  retryPending,
  deletePending,
}: {
  task: TaskOut;
  onRetry: () => void;
  onDelete: () => void;
  retryPending: boolean;
  deletePending: boolean;
}) {
  const { t } = useI18n();
  const title = `${task.type} #${task.id}`;
  // Only runs that did not finish successfully offer a retry: cancelled, discarded and
  // retryable.
  const canRetry = ["cancelled", "discarded", "retryable"].includes(task.status);

  return (
    <ListBox.Item
      id={task.id}
      textValue={`${title} ${task.type} ${task.status}`}
      className="group block w-full min-w-0 max-w-full overflow-hidden transform-none! active:transform-none! data-pressed:transform-none!"
    >
      <div className="relative grid min-w-0 gap-3 px-3 py-3 sm:px-4 sm:py-4 lg:grid-cols-[minmax(0,1fr)_6rem_auto] lg:items-center lg:gap-4">
        <div className="min-w-0 pr-11 lg:pr-0">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5 sm:gap-2">
            <TaskStatusChip status={task.status} />
            {task.parentId ? (
              <Chip size="sm" variant="tertiary">
                {t("routes.tasks.row.childTask")}
              </Chip>
            ) : null}
          </div>

          <Link
            to="/tasks/$id"
            params={{ id: task.id }}
            className="mt-1.5 block truncate text-sm font-semibold text-foreground hover:text-accent hover:underline sm:mt-2"
          >
            {title}
          </Link>

          <div className="mt-1.5 flex min-w-0 items-center gap-2 overflow-hidden text-[11px] text-muted sm:flex-wrap sm:gap-x-3 sm:gap-y-1">
            <span className="shrink-0">
              {t("routes.tasks.row.queue", { queue: task.queue || "default" })}
            </span>
            <span className="shrink-0">{taskDuration(task)}</span>
            <span className="truncate" title={formatDate(task.createdAt)}>
              {formatRelativeDate(task.createdAt, t)}
            </span>
            {(task.attempt ?? 0) > 0 ? (
              <span className="hidden shrink-0 sm:inline lg:hidden">
                {t("routes.tasks.row.attemptOf", {
                  attempt: task.attempt,
                  total: task.maxAttempts || "—",
                })}
              </span>
            ) : null}
          </div>
        </div>

        <div className="hidden lg:block">
          <div className="text-[10px] font-medium uppercase tracking-wide text-muted">
            {t("routes.tasks.row.attempts")}
          </div>
          <div className="mt-1 text-xs font-semibold tabular-nums">
            {task.attempt ?? 0} / {task.maxAttempts || "—"}
          </div>
        </div>

        <div className="absolute top-3 right-3 flex items-center justify-end gap-1.5 lg:static">
          {canRetry ? (
            <Button
              size="sm"
              variant="primary"
              isPending={retryPending}
              onPress={onRetry}
              aria-label={t("routes.tasks.row.retryLabel", { title })}
            >
              {t("common.action.retry")}
            </Button>
          ) : null}
          {task.status === "running" ? (
            <Button
              size="sm"
              variant="danger-soft"
              isPending={deletePending}
              onPress={onDelete}
              aria-label={t("routes.tasks.row.cancelLabel", { title })}
            >
              {t("common.action.cancel")}
            </Button>
          ) : (
            <Button
              size="sm"
              variant="tertiary"
              isIconOnly
              isPending={deletePending}
              onPress={onDelete}
              aria-label={t("routes.tasks.row.deleteLabel", { title })}
            >
              <TrashIcon className="size-3.5" />
            </Button>
          )}
        </div>
      </div>
    </ListBox.Item>
  );
}

/**
 * Status filter as a select whose trigger shows the count for the chosen status. An
 * unrecognised value falls back to the first tab, matching `validateSearch`.
 */
function TaskStatusSelect({
  value,
  counts,
  onChange,
}: {
  value: string;
  counts?: TaskCounts;
  onChange: (value: string) => void;
}) {
  const { t } = useI18n();
  const selected = STATUS_TABS.find((item) => item.key === value) ?? STATUS_TABS[0];

  return (
    <Select
      aria-label={t("routes.tasks.status.label")}
      selectedKey={value}
      onSelectionChange={(key) => onChange(String(key))}
      className="w-44 shrink-0"
    >
      <Select.Trigger className="h-8 min-h-8 py-1.5">
        <Select.Value>
          <div className="flex min-w-0 items-center justify-between gap-3">
            <span className="truncate">{t(selected.labelKey)}</span>
            <span className="text-xs tabular-nums text-muted">{counts?.[selected.key] ?? 0}</span>
          </div>
        </Select.Value>
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {STATUS_TABS.map((item) => (
            <ListBox.Item key={item.key} id={item.key} textValue={t(item.labelKey)}>
              <div className="flex w-full items-center justify-between gap-4">
                <TaskStatusChip status={item.key} />
                <span className="text-xs font-medium tabular-nums text-muted">
                  {counts?.[item.key] ?? 0}
                </span>
              </div>
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/**
 * Queue pause/resume popover. Pausing a queue stops new work from being picked up while
 * jobs already running finish; the caller refreshes the listing afterwards.
 */
function QueueManager({ queues, onChanged }: { queues: TaskQueueOut[]; onChanged: () => void }) {
  const { t } = useI18n();
  // Name of the queue whose toggle is in flight, so only that row shows a pending button.
  const [pendingQueue, setPendingQueue] = useState<string | null>(null);

  const toggleQueue = async (queue: TaskQueueOut) => {
    setPendingQueue(queue.name);
    const endpoint = queue.paused
      ? "/v1/jobs/queues/{queue}/resume"
      : "/v1/jobs/queues/{queue}/pause";
    try {
      await unwrap(fetchClient.POST(endpoint, { params: { path: { queue: queue.name } } }));
    } catch {
      toast.error(
        t(
          queue.paused
            ? "routes.tasks.queues.toast.resumeFailed"
            : "routes.tasks.queues.toast.pauseFailed",
          { name: queue.name },
        ),
      );
      return;
    } finally {
      setPendingQueue(null);
    }
    toast.success(
      t(queue.paused ? "routes.tasks.queues.toast.resumed" : "routes.tasks.queues.toast.paused", {
        name: queue.name,
      }),
    );
    onChanged();
  };

  return (
    <Popover>
      <Button size="sm" variant="tertiary">
        <FilterIcon className="size-3.5" /> {t("routes.tasks.queues.button")}
      </Button>
      <Popover.Content placement="bottom end" offset={8} className="w-[min(92vw,28rem)]">
        <Popover.Dialog className="p-0">
          <div className="border-border border-b px-4 py-3">
            <Popover.Heading className="text-sm font-semibold">
              {t("routes.tasks.queues.heading")}
            </Popover.Heading>
            <p className="mt-0.5 text-xs text-muted">{t("routes.tasks.queues.description")}</p>
          </div>
          <div className="max-h-80 divide-y divide-border overflow-y-auto">
            {queues.length > 0 ? (
              queues.map((queue) => (
                <div key={queue.name} className="flex items-center gap-3 px-4 py-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-sm font-medium">{queue.name}</span>
                      <span
                        className={`size-2 rounded-full ${queue.paused ? "bg-warning" : "bg-success"}`}
                      />
                      <span className="text-[10px] text-muted">
                        {queue.paused
                          ? t("routes.tasks.queues.paused")
                          : t("routes.tasks.queues.active")}
                      </span>
                    </div>
                    <div className="mt-1 flex gap-3 text-[11px] text-muted">
                      <span>{t("routes.tasks.queues.available", { count: queue.available })}</span>
                      <span>{t("routes.tasks.queues.running", { count: queue.running })}</span>
                    </div>
                  </div>
                  <Button
                    size="sm"
                    variant={queue.paused ? "primary" : "tertiary"}
                    isPending={pendingQueue === queue.name}
                    onPress={() => toggleQueue(queue)}
                  >
                    {queue.paused
                      ? t("routes.tasks.queues.resume")
                      : t("routes.tasks.queues.pause")}
                  </Button>
                </div>
              ))
            ) : (
              <div className="px-4 py-8 text-center text-sm text-muted">
                {t("routes.tasks.queues.empty")}
              </div>
            )}
          </div>
        </Popover.Dialog>
      </Popover.Content>
    </Popover>
  );
}

/** Full local date-time, used as the `title` of the relative timestamp. */
function formatDate(value: string) {
  return new Date(value).toLocaleString();
}
/**
 * Age of an instant as "just now", minutes, hours or days, falling back to a plain date
 * after a week. A future timestamp reads as "just now" instead of a negative age.
 */
function formatRelativeDate(value: string, t: Translate) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 60) return t("routes.tasks.relative.justNow");
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return t("routes.tasks.relative.minutesAgo", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t("routes.tasks.relative.hoursAgo", { count: hours });
  const days = Math.floor(hours / 24);
  return days < 7
    ? t("routes.tasks.relative.daysAgo", { count: days })
    : new Date(value).toLocaleDateString();
}

/**
 * How long the job has been going, or took. Start is the first attempt's timestamp when
 * there is one and the creation time otherwise; end is the completion time, or now for a
 * job that has not finished, so the figure grows as the polls re-render the row.
 */
function taskDuration(task: TaskOut) {
  const start = task.startedAt
    ? new Date(task.startedAt).getTime()
    : new Date(task.createdAt).getTime();
  const end = task.completedAt ? new Date(task.completedAt).getTime() : Date.now();
  const seconds = Math.max(0, Math.floor((end - start) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}
