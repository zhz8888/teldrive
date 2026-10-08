import { Card, Chip, Spinner, Typography } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import ChannelIcon from "~icons/gravity-ui/database";
import CleanupIcon from "~icons/gravity-ui/trash-bin";
import { $api } from "@/api/client";
import { queryClient } from "@/api/query-client";
import type { components } from "@/api/schema";
import { LinkButton } from "@/components/link-button";
import { Page, PageHeader } from "@/components/page";
import { useI18n, type MessageKey, type MessageParams } from "@/lib/i18n";

/** One entry of the recent-activity feed, as the storage endpoint returns it. */
type StorageActivity = components["schemas"]["StorageActivity"];
/** One day of the storage-growth series, carrying the total logical size at that day. */
type StorageGrowthPoint = components["schemas"]["StorageGrowthPoint"];
/** Translator signature for helpers that format outside a component body. */
type Translate = (key: MessageKey, params?: MessageParams) => string;

/**
 * File categories to their labels. A category the server reports but this table does not
 * know falls back to the raw value instead of rendering nothing.
 */
const CATEGORY_LABEL_KEYS: Record<string, MessageKey | undefined> = {
  archive: "routes.storage.category.archive",
  audio: "routes.storage.category.audio",
  document: "routes.storage.category.document",
  image: "routes.storage.category.image",
  video: "routes.storage.category.video",
  other: "routes.storage.category.other",
};

/** Activity event types to their labels; unknown types are shown verbatim, as above. */
const ACTIVITY_LABEL_KEYS: Record<string, MessageKey | undefined> = {
  "file.created": "routes.storage.activityType.fileCreated",
  "file.trashed": "routes.storage.activityType.fileTrashed",
  "file.restored": "routes.storage.activityType.fileRestored",
  "file.purged": "routes.storage.activityType.filePurged",
  "upload.completed": "routes.storage.activityType.uploadCompleted",
  "upload.aborted": "routes.storage.activityType.uploadAborted",
  "upload.expired": "routes.storage.activityType.uploadExpired",
  "share.created": "routes.storage.activityType.shareCreated",
  "share.deleted": "routes.storage.activityType.shareDeleted",
  "channel.created": "routes.storage.activityType.channelCreated",
  "channel.updated": "routes.storage.activityType.channelUpdated",
  "channel.deleted": "routes.storage.activityType.channelDeleted",
};

/**
 * `/storage` — one screen of drive statistics. The loader warms the single stats query so
 * the numbers are already cached by the time the component reads them, and the whole page
 * shares that one response.
 */
export const Route = createFileRoute("/storage")({
  component: StoragePage,
  pendingComponent: () => (
    <div className="flex items-center justify-center py-20">
      <Spinner size="lg" />
    </div>
  ),
  loader: () => queryClient.ensureQueryData($api.queryOptions("get", "/v1/storage/stats")),
});

/**
 * Renders the stats payload: headline counters, the growth chart, the composition and
 * channel breakdowns, and the cleanup and activity lists. Every figure is a server-side
 * aggregate — nothing is computed here beyond the per-row percentages.
 */
function StoragePage() {
  const { t } = useI18n();
  const { data } = $api.useSuspenseQuery("get", "/v1/storage/stats");
  const summary = data.summary;
  const configuredChannels = data.channels.length;
  const selectedChannels = data.channels.filter((channel) => channel.selected).length;
  // Parts across every channel, used as the denominator of each channel's share.
  const totalChannelParts = data.channels.reduce((total, channel) => total + channel.partCount, 0);

  return (
    <Page>
      <PageHeader title={t("routes.storage.title")} description={t("routes.storage.description")} />

      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label={t("routes.storage.stat.totalStored")}
          value={formatBytes(summary.logicalBytes)}
          detail={t("routes.storage.stat.addedToday", {
            size: formatBytes(data.growth.at(-1)?.addedBytes ?? 0),
          })}
        />
        <StatCard
          label={t("routes.storage.stat.activeFiles")}
          value={summary.activeFiles.toLocaleString()}
          detail={t("routes.storage.stat.folders", {
            count: summary.activeFolders.toLocaleString(),
          })}
        />
        <StatCard
          label={t("routes.storage.stat.trash")}
          value={formatBytes(summary.trashBytes)}
          detail={t("routes.storage.stat.files", { count: summary.trashedFiles.toLocaleString() })}
        />
        <StatCard
          label={t("routes.storage.stat.channels")}
          value={configuredChannels.toLocaleString()}
          detail={t("routes.storage.stat.selected", { count: selectedChannels.toLocaleString() })}
        />
        <StatCard
          label={t("routes.storage.stat.reclaimable")}
          value={formatBytes(data.cleanup.totalReclaimableBytes)}
          detail={t("routes.storage.stat.staleUploads", {
            count: data.cleanup.staleUploads.toLocaleString(),
          })}
        />
      </div>

      <Card className="gap-5 p-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <Typography type="h2" className="text-base font-semibold">
              {t("routes.storage.growth.title")}
            </Typography>
            <Typography.Paragraph className="text-sm text-muted">
              {t("routes.storage.growth.description")}
            </Typography.Paragraph>
          </div>
          <Chip variant="tertiary">{t("routes.storage.growth.range")}</Chip>
        </div>
        <StorageGrowthChart points={data.growth} />
      </Card>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card className="gap-5 p-5">
          <div>
            <Typography type="h2" className="text-base font-semibold">
              {t("routes.storage.composition.title")}
            </Typography>
            <Typography.Paragraph className="text-sm text-muted">
              {t("routes.storage.composition.description")}
            </Typography.Paragraph>
          </div>
          <div className="grid gap-4">
            {data.categories.map((category) => {
              const percent =
                summary.logicalBytes > 0 ? (category.totalSize / summary.logicalBytes) * 100 : 0;
              const categoryKey = CATEGORY_LABEL_KEYS[category.category];
              return (
                <div key={category.category} className="grid gap-2">
                  <div className="flex items-center justify-between gap-4 text-sm">
                    <span className="font-medium">
                      {categoryKey ? t(categoryKey) : category.category}
                    </span>
                    <span className="text-muted">
                      {t("routes.storage.composition.row", {
                        size: formatBytes(category.totalSize),
                        count: category.totalFiles.toLocaleString(),
                      })}
                    </span>
                  </div>
                  <ProgressTrack
                    value={percent}
                    label={t("routes.storage.composition.progress", {
                      category: category.category,
                    })}
                  />
                </div>
              );
            })}
            {data.categories.length === 0 && (
              <EmptyCopy>{t("routes.storage.composition.empty")}</EmptyCopy>
            )}
          </div>
        </Card>

        <Card className="gap-5 p-5">
          <div className="flex items-start justify-between gap-3">
            <div>
              <Typography type="h2" className="text-base font-semibold">
                {t("routes.storage.channels.title")}
              </Typography>
              <Typography.Paragraph className="text-sm text-muted">
                {t("routes.storage.channels.description")}
              </Typography.Paragraph>
            </div>
            <LinkButton to="/settings/channels" size="sm" variant="tertiary">
              {t("routes.storage.channels.manage")}
            </LinkButton>
          </div>
          <div className="divide-y divide-border rounded-xl border border-border">
            {data.channels.map((channel) => {
              const percent =
                totalChannelParts > 0 ? (channel.partCount / totalChannelParts) * 100 : 0;
              return (
                <div key={channel.channelId} className="grid gap-3 p-4">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <ChannelIcon className="size-4 shrink-0 text-muted" />
                        <span className="truncate text-sm font-semibold">{channel.name}</span>
                        {channel.selected && (
                          <Chip size="sm" variant="tertiary">
                            {t("routes.storage.channels.selected")}
                          </Chip>
                        )}
                      </div>
                      <div className="mt-1 text-xs text-muted">
                        {t("routes.storage.channels.parts", {
                          parts: channel.partCount.toLocaleString(),
                          percent: percent.toFixed(1),
                        })}
                      </div>
                    </div>
                  </div>
                  <ProgressTrack
                    value={percent}
                    label={t("routes.storage.channels.progress", { name: channel.name })}
                  />
                </div>
              );
            })}
            {data.channels.length === 0 && (
              <EmptyCopy>{t("routes.storage.channels.empty")}</EmptyCopy>
            )}
          </div>
        </Card>
      </div>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card className="gap-5 p-5">
          <div className="flex items-start justify-between gap-3">
            <div>
              <Typography type="h2" className="text-base font-semibold">
                {t("routes.storage.cleanup.title")}
              </Typography>
              <Typography.Paragraph className="text-sm text-muted">
                {t("routes.storage.cleanup.description")}
              </Typography.Paragraph>
            </div>
            <CleanupIcon className="size-5 text-muted" />
          </div>
          <div className="divide-y divide-border rounded-xl border border-border">
            <MetricRow
              label={t("routes.storage.cleanup.trash")}
              value={formatBytes(data.cleanup.trashBytes)}
            />
            <MetricRow
              label={t("routes.storage.cleanup.staleUploads")}
              value={formatBytes(data.cleanup.staleUploadBytes)}
              detail={t("routes.storage.cleanup.sessions", {
                count: data.cleanup.staleUploads.toLocaleString(),
              })}
            />
            <MetricRow
              label={t("routes.storage.cleanup.totalReclaimable")}
              value={formatBytes(data.cleanup.totalReclaimableBytes)}
              strong
            />
          </div>
          <div className="flex items-center justify-between gap-4">
            <p className="text-xs text-muted">{t("routes.storage.cleanup.note")}</p>
            <LinkButton to="/trash" size="sm" variant="primary">
              {t("routes.storage.cleanup.review")}
            </LinkButton>
          </div>
        </Card>

        <Card className="gap-5 p-5">
          <div>
            <Typography type="h2" className="text-base font-semibold">
              {t("routes.storage.activity.title")}
            </Typography>
            <Typography.Paragraph className="text-sm text-muted">
              {t("routes.storage.activity.description")}
            </Typography.Paragraph>
          </div>
          <div className="divide-y divide-border rounded-xl border border-border">
            {data.activity.map((activity) => (
              <ActivityRow key={activity.id} activity={activity} />
            ))}
            {data.activity.length === 0 && (
              <EmptyCopy>{t("routes.storage.activity.empty")}</EmptyCopy>
            )}
          </div>
        </Card>
      </div>
    </Page>
  );
}

/**
 * Horizontal bar for a percentage that may be fractional or slightly out of range: the
 * drawn width is clamped to 0-100% while `aria-valuenow` reports the rounded real value.
 */
function ProgressTrack({ value, label }: { value: number; label: string }) {
  const width = `${Math.max(0, Math.min(100, value))}%`;
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(value)}
      className="h-2 overflow-hidden rounded-full bg-default/40"
    >
      <div className="h-full rounded-full bg-accent transition-[width]" style={{ width }} />
    </div>
  );
}

/** Headline tile: a label, the primary figure and one line of context under it. */
function StatCard({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <Card className="gap-1 p-4">
      <div className="text-[11px] font-semibold uppercase tracking-[0.12em] text-muted">
        {label}
      </div>
      <div className="text-2xl font-semibold tracking-tight">{value}</div>
      <div className="text-xs text-muted">{detail}</div>
    </Card>
  );
}

/** Label/value line of the cleanup panel; `strong` marks the total row. */
function MetricRow({
  label,
  value,
  detail,
  strong,
}: {
  label: string;
  value: string;
  detail?: string;
  strong?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-4 px-4 py-3">
      <div>
        <div className={strong ? "text-sm font-semibold" : "text-sm"}>{label}</div>
        {detail && <div className="text-xs text-muted">{detail}</div>}
      </div>
      <div className={strong ? "font-mono text-sm font-semibold" : "font-mono text-sm"}>
        {value}
      </div>
    </div>
  );
}

/** One activity-feed line: the event label, its subject, and how long ago it happened. */
function ActivityRow({ activity }: { activity: StorageActivity }) {
  const { t } = useI18n();
  const labelKey = ACTIVITY_LABEL_KEYS[activity.type];
  return (
    <div className="flex items-start justify-between gap-4 px-4 py-3">
      <div className="min-w-0">
        <div className="text-sm font-medium">{labelKey ? t(labelKey) : activity.type}</div>
        <div className="truncate text-xs text-muted">{activity.label}</div>
      </div>
      <time className="shrink-0 text-xs text-muted" dateTime={activity.occurredAt}>
        {formatRelative(activity.occurredAt, t)}
      </time>
    </div>
  );
}

/**
 * Line chart of the growth series, drawn straight into an SVG viewBox rather than through
 * a chart library. The y-scale is relative to the series' own minimum and maximum, so the
 * chart shows the shape of the change; a flat series still divides by a range of 1.
 */
function StorageGrowthChart({ points }: { points: StorageGrowthPoint[] }) {
  const { t } = useI18n();
  if (points.length === 0) return <EmptyCopy>{t("routes.storage.growth.empty")}</EmptyCopy>;
  const width = 960;
  const height = 220;
  const padding = 18;
  const values = points.map((point) => point.logicalBytes);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = Math.max(1, max - min);
  const coordinates = points.map((point, index) => {
    // `Math.max(1, ...)` guards the divisor when the series holds a single point.
    const x = padding + (index / Math.max(1, points.length - 1)) * (width - padding * 2);
    const y = height - padding - ((point.logicalBytes - min) / range) * (height - padding * 2);
    return [x, y] as const;
  });
  const path = coordinates
    .map(([x, y], index) => `${index === 0 ? "M" : "L"}${x.toFixed(1)} ${y.toFixed(1)}`)
    .join(" ");

  return (
    <div className="grid gap-3">
      <div className="overflow-hidden rounded-xl border border-border bg-surface-secondary/40 p-3">
        <svg
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label={t("routes.storage.growth.chartLabel")}
          className="h-56 w-full"
        >
          <title>{t("routes.storage.growth.chartLabel")}</title>
          {[0.25, 0.5, 0.75].map((ratio) => (
            <line
              key={ratio}
              x1={padding}
              x2={width - padding}
              y1={height * ratio}
              y2={height * ratio}
              className="stroke-border"
              strokeWidth="1"
            />
          ))}
          <path
            d={path}
            fill="none"
            className="stroke-accent"
            strokeWidth="3"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          {coordinates.at(-1) && (
            <circle
              cx={coordinates.at(-1)?.[0]}
              cy={coordinates.at(-1)?.[1]}
              r="5"
              className="fill-accent"
            />
          )}
        </svg>
      </div>
      <div className="flex items-center justify-between text-xs text-muted">
        <span>{new Date(points[0].day).toLocaleDateString()}</span>
        <span>{formatBytes(points.at(-1)?.logicalBytes ?? 0)}</span>
        <span>{new Date(points.at(-1)?.day ?? points[0].day).toLocaleDateString()}</span>
      </div>
    </div>
  );
}

/** Placeholder line shown in place of a chart, list or breakdown that has no data. */
function EmptyCopy({ children }: { children: React.ReactNode }) {
  return <div className="px-4 py-8 text-center text-sm text-muted">{children}</div>;
}

/**
 * Formats a byte count with binary units. Unlike the settings page's version the decimal
 * count adapts to the magnitude: none at or above 100, one at or above 10, two below.
 */
function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const power = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** power;
  return `${value.toFixed(value >= 100 || power === 0 ? 0 : value >= 10 ? 1 : 2)} ${units[power]}`;
}

/**
 * Renders an instant as "just now", a bucket of elapsed minutes/hours/days, or a plain
 * date once it is a week old. A timestamp in the future counts as "just now" rather than
 * producing a negative age.
 */
function formatRelative(value: string, t: Translate) {
  const delta = Math.max(0, Date.now() - new Date(value).getTime());
  const minutes = Math.floor(delta / 60_000);
  if (minutes < 1) return t("routes.storage.relative.justNow");
  if (minutes < 60) return t("routes.storage.relative.minutesAgo", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t("routes.storage.relative.hoursAgo", { count: hours });
  const days = Math.floor(hours / 24);
  return days < 7
    ? t("routes.storage.relative.daysAgo", { count: days })
    : new Date(value).toLocaleDateString();
}
