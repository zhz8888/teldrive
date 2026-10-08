package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

// OrphanCleanupKind is the River job kind of the periodic sweep that deletes
// Telegram documents no active file references any more.
const OrphanCleanupKind = "teldrive_cleanup_orphaned_telegram_parts"

// ErrOrphanCleanupNotConfigured is returned by Work when the worker was built
// without its pool, document lister or Telegram storage, which means the runtime
// did not wire it up. Callers must test it with errors.Is.
var ErrOrphanCleanupNotConfigured = errors.New("orphan cleanup worker is not configured")

// maxOrphanOutputBytes bounds the recorded job output well below River's
// 32MB limit, so a badly damaged channel degrades to counters instead of
// failing the sweep.
const maxOrphanOutputBytes = 16 << 20

// OrphanCleanupArgs is the empty payload of an orphan sweep: the age threshold
// comes from the worker configuration, not from the job.
type OrphanCleanupArgs struct{}

// Kind reports the River job kind handled by OrphanedTelegramPartsCleanupWorker.
func (OrphanCleanupArgs) Kind() string { return OrphanCleanupKind }

// InsertOpts pins the sweep to the maintenance queue with three attempts and
// priority 3, so within that queue it is ordered after the priority-1 purges and
// trash cleanups as well as the priority-2 upload and event sweeps.
func (OrphanCleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CleanupQueue, MaxAttempts: 3, Priority: 3}
}

// OrphanCleanupOutput is recorded as the River job output when the sweep
// completes, so operators can inspect per-run counters without log access.
type OrphanCleanupOutput struct {
	// Channels is the number of storage channels that were inspected.
	Channels int `json:"channels"`
	// Scanned is the number of Telegram documents listed across those channels.
	Scanned int `json:"scanned"`
	// Deleted is the number of unreferenced documents that were deleted.
	Deleted int `json:"deleted"`
	// Cutoff is the age threshold of the run: only documents created before it
	// were deletion candidates.
	Cutoff time.Time `json:"cutoff"`
	// CompletedAt is the UTC time the run finished collecting its counters.
	CompletedAt time.Time `json:"completedAt"`
	// BrokenFiles lists active files with DB-referenced messages missing from
	// Telegram, so owners know what to re-upload. The list is uncapped;
	// limitBrokenFilesSize degrades to counters when it would exceed the
	// job-output size budget.
	BrokenFiles []BrokenFile `json:"brokenFiles"`
	// BrokenTotal counts active files with at least one missing part, including
	// those omitted from BrokenFiles, so it stays exact when the list is dropped.
	BrokenTotal int `json:"brokenTotal"`
	// BrokenTruncated reports that BrokenFiles was dropped to stay inside the
	// job-output budget; BrokenTotal is still complete.
	BrokenTruncated bool `json:"brokenTruncated"`
}

// BrokenFile is one active file with at least one referenced part message
// absent from its Telegram channel.
type BrokenFile struct {
	// FileID is the file UUID as a string, so the output stays readable JSON.
	FileID string `json:"fileId"`
	// Name is the file name its owner sees.
	Name string `json:"name"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// ChannelID is the Telegram channel that should still hold the file's parts.
	ChannelID int64 `json:"channelId"`
	// MissingMessageIDs lists the referenced message IDs that the channel no
	// longer returns, sorted ascending.
	MissingMessageIDs []int64 `json:"missingMessageIds"`
}

// findBrokenFiles returns files with referenced messages absent from the
// Telegram listing, grouped by file and sorted by name.
func findBrokenFiles(seen map[int64]struct{}, rows []*sqlcgen.ListChannelReferencedPartsRow, channelID int64) []BrokenFile {
	type pending struct {
		name string
		size int64
		ids  []int64
	}
	byFile := make(map[string]*pending)
	order := make([]string, 0)
	for _, row := range rows {
		if _, ok := seen[row.MessageID]; ok {
			continue
		}
		fileID, ok := dbtypes.GoogleUUID(row.FileID)
		if !ok {
			continue
		}
		key := fileID.String()
		entry, ok := byFile[key]
		if !ok {
			entry = &pending{name: row.FileName, size: row.FileSize.Int64}
			byFile[key] = entry
			order = append(order, key)
		}
		entry.ids = append(entry.ids, row.MessageID)
	}
	files := make([]BrokenFile, 0, len(order))
	for _, key := range order {
		entry := byFile[key]
		slices.Sort(entry.ids)
		files = append(files, BrokenFile{FileID: key, Name: entry.name, Size: entry.size, ChannelID: channelID, MissingMessageIDs: entry.ids})
	}
	slices.SortFunc(files, func(a, b BrokenFile) int {
		if result := strings.Compare(a.Name, b.Name); result != 0 {
			return result
		}
		return strings.Compare(a.FileID, b.FileID)
	})
	return files
}

// limitBrokenFilesSize drops the broken-file list, keeping counters, when
// the marshaled output would exceed maxBytes. The sweep still completes
// with full totals instead of failing on oversized job output.
func limitBrokenFilesSize(output OrphanCleanupOutput, maxBytes int) OrphanCleanupOutput {
	if len(output.BrokenFiles) == 0 {
		return output
	}
	raw, err := json.Marshal(output)
	if err != nil || len(raw) <= maxBytes {
		return output
	}
	output.BrokenFiles = []BrokenFile{}
	output.BrokenTruncated = true
	return output
}

// OrphanedTelegramPartsCleanupWorker deletes Telegram documents that no active
// file references any more and reports the active files whose referenced parts
// have gone missing. Documents younger than minimumAge are never candidates, so
// an upload that is still running cannot be mistaken for an orphan.
type OrphanedTelegramPartsCleanupWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[OrphanCleanupArgs]
	// pool is only checked for nil: Work rejects a worker that has no pool rather
	// than panicking inside a query.
	pool *pgxpool.Pool
	// queries lists the channels to inspect and the message IDs they reference.
	queries *sqlcgen.Queries
	// lister paginates the documents stored in a channel.
	lister telegramstore.DocumentMessageLister
	// storage deletes the orphaned documents.
	storage telegramstore.Storage
	// minimumAge is the grace period that keeps recently created documents from
	// being treated as orphans; zero makes every unreferenced document a
	// candidate.
	minimumAge time.Duration
}

// NewOrphanedTelegramPartsCleanupWorker returns a sweep worker that ignores
// documents younger than minimumAge. pool, storage and lister are all required:
// Work returns ErrOrphanCleanupNotConfigured when one of them is missing, rather
// than panicking inside the first query or delete.
func NewOrphanedTelegramPartsCleanupWorker(pool *pgxpool.Pool, storage telegramstore.Storage, lister telegramstore.DocumentMessageLister, minimumAge time.Duration) *OrphanedTelegramPartsCleanupWorker {
	return &OrphanedTelegramPartsCleanupWorker{pool: pool, queries: sqlcgen.New(pool), storage: storage, lister: lister, minimumAge: minimumAge}
}

// Timeout allows four hours: the sweep lists every page of every channel,
// resolving references as it goes, before it can record its output.
func (w *OrphanedTelegramPartsCleanupWorker) Timeout(*river.Job[OrphanCleanupArgs]) time.Duration {
	return 4 * time.Hour
}

// Work deletes the Telegram documents that no active file references any more and
// records both the cleanup counters and the active files whose parts are missing.
//
// Every channel that stores upload parts is paged through in requests of at most
// 100 documents. Only documents created before now-minimumAge are deletion
// candidates, which keeps the part a concurrent upload has just written, but not
// yet recorded, from being deleted. Each page is checked against the message IDs
// the database still references, and a page whose cursor does not advance aborts
// the run instead of looping forever. Messages are deleted page by page, so a run
// that fails halfway leaves the earlier pages deleted; the retry lists whatever is
// still there and converges. Once a channel has been fully listed, the referenced
// parts that never appeared in it are reported as broken files.
//
// It returns ErrOrphanCleanupNotConfigured when the worker is missing its pool,
// lister or Telegram storage, so a misconfigured runtime fails with a sentinel
// error instead of panicking inside the sweep.
func (w *OrphanedTelegramPartsCleanupWorker) Work(ctx context.Context, job *river.Job[OrphanCleanupArgs]) error {
	if w == nil || w.pool == nil || w.queries == nil || w.lister == nil || w.storage == nil {
		return ErrOrphanCleanupNotConfigured
	}
	channels, err := w.queries.ListChannelsForOrphanCleanup(ctx)
	if err != nil {
		return fmt.Errorf("list channels for orphan cleanup: %w", err)
	}
	cutoff := time.Now().UTC().Add(-w.minimumAge)
	var scanned, deleted, brokenTotal int
	brokenFiles := make([]BrokenFile, 0)
	for _, channel := range channels {
		channelScanned, channelDeleted, channelBroken := 0, 0, 0
		seen := make(map[int64]struct{})
		beforeID := int64(0)
		for {
			page, err := w.lister.ListDocumentMessages(ctx, telegramstore.ListDocumentMessagesRequest{
				UserID: channel.UserID, ChannelID: channel.ChannelID, BeforeID: beforeID, Limit: 100,
			})
			if err != nil {
				return fmt.Errorf("list Telegram documents for channel %d: %w", channel.ChannelID, err)
			}
			scanned += len(page.Messages)
			channelScanned += len(page.Messages)
			for _, message := range page.Messages {
				seen[message.ID] = struct{}{}
			}
			candidateIDs := make([]int64, 0, len(page.Messages))
			for _, message := range page.Messages {
				if message.CreatedAt.Before(cutoff) {
					candidateIDs = append(candidateIDs, message.ID)
				}
			}
			if len(candidateIDs) > 0 {
				referenced, err := w.queries.ListReferencedMessageIDs(ctx, sqlcgen.ListReferencedMessageIDsParams{
					TargetChannelID: channel.ChannelID, MessageIds: candidateIDs,
				})
				if err != nil {
					return fmt.Errorf("list referenced messages for channel %d: %w", channel.ChannelID, err)
				}
				refs := make(map[int64]struct{}, len(referenced))
				for _, id := range referenced {
					refs[id] = struct{}{}
				}
				orphans := candidateIDs[:0]
				for _, id := range candidateIDs {
					if _, ok := refs[id]; !ok {
						orphans = append(orphans, id)
					}
				}
				if len(orphans) > 0 {
					if err := w.storage.DeleteMessages(ctx, channel.UserID, channel.ChannelID, orphans); err != nil {
						return fmt.Errorf("delete orphaned Telegram documents from channel %d: %w", channel.ChannelID, err)
					}
					deleted += len(orphans)
					channelDeleted += len(orphans)
				}
			}
			if page.Exhausted {
				break
			}
			if page.BeforeID <= 0 || page.BeforeID == beforeID {
				return fmt.Errorf("list Telegram documents for channel %d: pagination did not advance", channel.ChannelID)
			}
			beforeID = page.BeforeID
		}
		referenced, err := w.queries.ListChannelReferencedParts(ctx, sqlcgen.ListChannelReferencedPartsParams{
			TargetChannelID: channel.ChannelID, TargetUserID: channel.UserID,
		})
		if err != nil {
			return fmt.Errorf("list referenced parts for channel %d: %w", channel.ChannelID, err)
		}
		channelBrokenFiles := findBrokenFiles(seen, referenced, channel.ChannelID)
		brokenFiles = append(brokenFiles, channelBrokenFiles...)
		brokenTotal += len(channelBrokenFiles)
		channelBroken = len(channelBrokenFiles)
		slog.DebugContext(ctx, "orphaned Telegram part cleanup: channel completed",
			"user_id", channel.UserID, "channel_id", channel.ChannelID,
			"scanned", channelScanned, "deleted", channelDeleted, "broken", channelBroken)
	}
	slog.InfoContext(ctx, "orphaned Telegram part cleanup completed", "channels", len(channels), "scanned", scanned, "deleted", deleted, "broken", brokenTotal, "cutoff", cutoff)
	output := limitBrokenFilesSize(OrphanCleanupOutput{Channels: len(channels), Scanned: scanned, Deleted: deleted, Cutoff: cutoff, CompletedAt: time.Now().UTC(),
		BrokenFiles: brokenFiles, BrokenTotal: brokenTotal}, maxOrphanOutputBytes)
	if output.BrokenTruncated {
		slog.WarnContext(ctx, "orphaned Telegram part cleanup: broken-file list exceeds output budget, recording counters only",
			"broken", brokenTotal)
	}
	if job.JobRow != nil {
		return river.RecordOutput(ctx, output)
	}
	return nil
}
