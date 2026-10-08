using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Current totals of one drive. */
model StorageSummary {
  /** Summed size of the active files, in bytes. */
  logicalBytes: int64;
  /** Number of active file entries. */
  activeFiles: int64;
  /** Number of active folder entries. */
  activeFolders: int64;
  /** Number of trashed file entries; trashed folders are not counted. */
  trashedFiles: int64;
  /** Summed size of those trashed files, in bytes. */
  trashBytes: int64;
}

/** One day of the thirty-day growth series. */
model StorageGrowthPoint {
  /** Calendar day the point covers, in UTC. */
  day: plainDate;
  /** Size of the files created that day and still active, in bytes. */
  addedBytes: int64;
  /** Cumulative size of all active files up to and including that day, in bytes. */
  logicalBytes: int64;
}

/** One Telegram channel of the account and how much of the drive it holds. */
model StorageChannelStatistic {
  /** Telegram channel ID. */
  channelId: int64;
  /** Channel title. */
  name: string;
  /** True for the channel new uploads currently target; at most one channel per account. */
  selected: boolean;
  /**
   * Last recorded health as a plain string: `unknown`, `healthy`, `degraded` or
   * `unavailable`.
   */
  health: string;
  /** When the health was last recorded; omitted when the channel was never checked. */
  lastCheckedAt?: utcDateTime;
  /** Number of stored parts of the account's active files on this channel. */
  partCount: int64;
  /**
   * Summed stored size of those parts, in bytes: the on-Telegram figure, not the
   * plaintext size.
   */
  storedBytes: int64;
}

/** Space that emptying the trash and dropping stale upload parts would reclaim. */
model StorageCleanupStatistics {
  /** Stored size of the parts of trashed files, in bytes. */
  trashBytes: int64;
  /** Stored size of the parts left behind by expired upload sessions, in bytes. */
  staleUploadBytes: int64;
  /** Number of expired upload sessions, whether or not they still hold stored parts. */
  staleUploads: int64;
  /** `trashBytes` plus `staleUploadBytes`, in bytes. */
  totalReclaimableBytes: int64;
}

/** One entry of the recent-activity feed. */
model StorageActivity {
  /** Event row ID; the feed is ordered by it, newest first. */
  id: int64;
  /** Event type, for example `file.created` or `share.deleted`. */
  type: string;
  /** Kind of resource the event is about, for example `file` or `channel`. */
  resourceType: string;
  /** Identifier of that resource; omitted when the event names no single resource. */
  resourceId?: string;
  /** Human-readable name taken from the event payload, falling back to the event type. */
  label: string;
  /** When the event happened, in UTC. */
  occurredAt: utcDateTime;
}

/** Aggregated storage view of one account. */
model StorageDashboard {
  /** Current byte and entry totals. */
  summary: StorageSummary;
  /** One point per day for the last thirty days, oldest first. */
  growth: StorageGrowthPoint[];
  /** Active files broken down by category bucket. */
  categories: FileCategoryStatistics[];
  /** Every channel of the account with the bytes it stores. */
  channels: StorageChannelStatistic[];
  /** Reclaimable space estimates. */
  cleanup: StorageCleanupStatistics;
  /**
   * Up to eight recent events, newest first, limited to file, upload, share and channel
   * events.
   */
  activity: StorageActivity[];
}

@tag("Storage")
@route("/v1/storage")
@useAuth(TelDriveAuth)
namespace Storage {
  /** Return the authenticated user's storage statistics. */
  @get
  @route("/stats")
  @operationId("getStorageStats")
  op stats(): JsonResponse<200, StorageDashboard> | ErrorResponse<401>;
}
