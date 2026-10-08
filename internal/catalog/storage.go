package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
)

// StorageDashboard is the aggregated storage view of one user: current totals,
// the last 30 days of growth, category and channel breakdowns, reclaimable space
// and the most recent activity entries.
type StorageDashboard struct {
	// Summary holds the current byte and entry counts of the drive.
	Summary StorageSummary
	// Growth is one point per day for the last 30 days, oldest first.
	Growth []StorageGrowthPoint
	// Categories breaks the active files down by category bucket.
	Categories []CategoryStatistic
	// Channels lists every channel of the user together with its stored bytes.
	Channels []StorageChannelStatistic
	// Cleanup estimates the space that could be reclaimed.
	Cleanup StorageCleanupStatistics
	// Activity holds up to eight recent events, newest first, limited to the
	// twelve event types ListRecentStorageActivity selects (file, upload, share
	// and channel events), so other recent events of the user are omitted.
	Activity []StorageActivity
}

// StorageSummary holds the byte and entry counts of one drive.
type StorageSummary struct {
	// LogicalBytes is the summed size of active files in bytes.
	LogicalBytes int64
	// ActiveFiles is the number of active file entries.
	ActiveFiles int64
	// ActiveFolders is the number of active folder entries.
	ActiveFolders int64
	// TrashedFiles is the number of trashed file entries, folders excluded.
	TrashedFiles int64
	// TrashBytes is the summed size of those trashed files in bytes.
	TrashBytes int64
}

// StorageGrowthPoint is one day of the 30-day growth series.
type StorageGrowthPoint struct {
	// Day identifies the calendar day the point covers.
	Day time.Time
	// AddedBytes is the size of the files created that day and still active, in
	// bytes.
	AddedBytes int64
	// LogicalBytes is the cumulative size of all active files up to and
	// including that day, in bytes.
	LogicalBytes int64
}

// StorageChannelStatistic describes one Telegram channel of the user and how much
// of the drive it holds.
type StorageChannelStatistic struct {
	// ChannelID is the Telegram channel ID.
	ChannelID int64
	// Name is the title configured for the channel.
	Name string
	// Selected reports whether new uploads prefer this channel.
	Selected bool
	// Health is the last recorded channel health: "unknown", "healthy",
	// "degraded" or "unavailable".
	Health string
	// LastCheckedAt is when the health was last probed, or nil if it never was.
	LastCheckedAt *time.Time
	// PartCount is the number of stored parts of the user's active files on this
	// channel.
	PartCount int64
	// StoredBytes is the summed stored size of those parts in bytes, which is the
	// on-Telegram figure rather than the plaintext size.
	StoredBytes int64
}

// StorageCleanupStatistics estimates the space reclaimable by emptying the trash
// and by dropping the parts of expired upload sessions.
type StorageCleanupStatistics struct {
	// TrashBytes is the stored size of the parts of trashed files, in bytes.
	TrashBytes int64
	// StaleUploadBytes is the stored size of the parts left behind by expired
	// upload sessions, in bytes.
	StaleUploadBytes int64
	// StaleUploads counts the expired upload sessions, whether or not they still
	// hold stored parts.
	StaleUploads int64
	// TotalReclaimableBytes is TrashBytes plus StaleUploadBytes.
	TotalReclaimableBytes int64
}

// StorageActivity is one entry of the recent-activity feed.
type StorageActivity struct {
	// ID is the user_events row ID; the feed is ordered by it, newest first.
	ID int64
	// Type is the event type, for example "file.created" or "share.deleted".
	Type string
	// ResourceType names the kind of resource the event refers to.
	ResourceType string
	// ResourceID is the resource identifier, empty when the event carries none.
	ResourceID string
	// Label is a human-readable name taken from the event payload, falling back
	// to Type when the payload has none.
	Label string
	// OccurredAt is the event time in UTC.
	OccurredAt time.Time
}

// StorageDashboard assembles the dashboard of userID from six queries: totals,
// cleanup estimates, the 30-day growth series, channel statistics, the eight most
// recent user events, and the category breakdown. It returns ErrInvalidOwner for
// a non-positive user ID and stops at the first failing query, so a partially
// built dashboard is never returned.
func (s *Service) StorageDashboard(ctx context.Context, userID int64) (StorageDashboard, error) {
	if userID <= 0 {
		return StorageDashboard{}, ErrInvalidOwner
	}
	totals, err := s.queries.GetStorageDashboardTotals(ctx, userID)
	if err != nil {
		return StorageDashboard{}, fmt.Errorf("get storage totals: %w", err)
	}
	cleanup, err := s.queries.GetStorageCleanupStatistics(ctx, userID)
	if err != nil {
		return StorageDashboard{}, fmt.Errorf("get storage cleanup statistics: %w", err)
	}
	growthRows, err := s.queries.ListStorageGrowth(ctx, userID)
	if err != nil {
		return StorageDashboard{}, fmt.Errorf("list storage growth: %w", err)
	}
	channelRows, err := s.queries.ListStorageChannelStatistics(ctx, userID)
	if err != nil {
		return StorageDashboard{}, fmt.Errorf("list storage channels: %w", err)
	}
	activityRows, err := s.queries.ListRecentStorageActivity(ctx, sqlcgen.ListRecentStorageActivityParams{
		UserID: userID, ActivityLimit: 8,
	})
	if err != nil {
		return StorageDashboard{}, fmt.Errorf("list storage activity: %w", err)
	}
	categories, err := s.CategoryStatistics(ctx, userID)
	if err != nil {
		return StorageDashboard{}, err
	}

	result := StorageDashboard{
		Summary: StorageSummary{
			LogicalBytes: totals.LogicalBytes,
			ActiveFiles:  totals.ActiveFiles, ActiveFolders: totals.ActiveFolders,
			TrashedFiles: totals.TrashedFiles, TrashBytes: totals.TrashBytes,
		},
		Categories: categories,
		Cleanup: StorageCleanupStatistics{
			TrashBytes: cleanup.TrashBytes, StaleUploadBytes: cleanup.StaleUploadBytes,
			StaleUploads:          cleanup.StaleUploads,
			TotalReclaimableBytes: cleanup.TrashBytes + cleanup.StaleUploadBytes,
		},
		Growth:   make([]StorageGrowthPoint, 0, len(growthRows)),
		Channels: make([]StorageChannelStatistic, 0, len(channelRows)),
		Activity: make([]StorageActivity, 0, len(activityRows)),
	}
	for _, row := range growthRows {
		result.Growth = append(result.Growth, StorageGrowthPoint{
			Day: row.Day.Time.UTC(), AddedBytes: row.AddedBytes, LogicalBytes: row.LogicalBytes,
		})
	}
	for _, row := range channelRows {
		item := StorageChannelStatistic{
			ChannelID: row.ChannelID, Name: row.Name, Selected: row.Selected, Health: row.Health,
			PartCount: row.PartCount, StoredBytes: row.StoredBytes,
		}
		if row.LastCheckedAt.Valid {
			value := row.LastCheckedAt.Time.UTC()
			item.LastCheckedAt = &value
		}
		result.Channels = append(result.Channels, item)
	}
	for _, row := range activityRows {
		item := StorageActivity{
			ID: row.ID, Type: row.EventType, ResourceType: row.ResourceType,
			OccurredAt: row.OccurredAt.Time.UTC(), Label: storageActivityLabel(row.EventType, row.Payload),
		}
		if row.ResourceID.Valid {
			item.ResourceID = row.ResourceID.String
		}
		result.Activity = append(result.Activity, item)
	}
	return result, nil
}

// storageActivityLabel picks a readable label for an activity entry from the
// "name" field of its JSON payload. A payload that is not a JSON object, or one
// without a usable name, falls back to the event type, so the feed always has
// something to display.
func storageActivityLabel(eventType string, payload []byte) string {
	var values map[string]any
	if json.Unmarshal(payload, &values) == nil {
		if name, ok := values["name"].(string); ok && name != "" {
			return name
		}
	}
	return eventType
}
