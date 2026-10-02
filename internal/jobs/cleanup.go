package jobs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

const (
	// UploadCleanupSweepKind is the River job kind of the periodic sweep that
	// expires abandoned upload sessions and deletes the Telegram messages holding
	// their stored parts.
	UploadCleanupSweepKind = "teldrive_cleanup_uploads"
	// CleanupQueue is the River queue shared by every maintenance sweep; keeping
	// cleanups on their own queue stops them from competing with interactive work.
	CleanupQueue = "maintenance"
)

// ErrUploadCleanupNotConfigured is returned by Work when the worker was built
// without a pool or Telegram storage. It reports a wiring problem, so a River
// retry cannot succeed until the process is reconfigured.
var ErrUploadCleanupNotConfigured = errors.New("upload cleanup worker is not configured")

// CleanupSweepKind is the original name of UploadCleanupSweepKind, kept so that
// schedules and persisted jobs written by older releases still resolve.
//
// Deprecated: use UploadCleanupSweepKind instead.
const CleanupSweepKind = UploadCleanupSweepKind

// ErrCleanupNotConfigured is the original name of ErrUploadCleanupNotConfigured,
// kept for callers compiled against it.
//
// Deprecated: use ErrUploadCleanupNotConfigured instead.
var ErrCleanupNotConfigured = ErrUploadCleanupNotConfigured

// UploadCleanupSweepArgs is the empty payload of an upload cleanup sweep: the
// sweep takes no parameters and always drains every expired session it finds.
type UploadCleanupSweepArgs struct{}

// CleanupSweepArgs is the original name of UploadCleanupSweepArgs, kept so that
// persisted jobs still decode.
//
// Deprecated: use UploadCleanupSweepArgs instead.
type CleanupSweepArgs = UploadCleanupSweepArgs

// Kind reports the River job kind handled by UploadCleanupWorker.
func (UploadCleanupSweepArgs) Kind() string { return UploadCleanupSweepKind }

// InsertOpts schedules the sweep on the maintenance queue with three attempts and
// priority 2, so within that queue it is ordered after the priority-1 purges and
// trash cleanups. Upload jobs run on the separate uploads queue, so their ordering
// against this sweep comes from queue isolation rather than from priority.
func (UploadCleanupSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       CleanupQueue,
		MaxAttempts: 3,
		Priority:    2,
	}
}

// UploadCleanupWorker deletes the Telegram messages that hold the parts of
// expired and aborted upload sessions, and then removes the session rows. It is
// driven by a periodic sweep and every run is idempotent: work a failed run left
// behind is simply picked up by the next one.
type UploadCleanupWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[UploadCleanupSweepArgs]
	// pool is only checked for nil: Work rejects a worker that has no pool rather
	// than panicking inside a query.
	pool *pgxpool.Pool
	// queries expires stale sessions and lists the parts that still need deleting.
	queries *sqlcgen.Queries
	// storage deletes the Telegram messages holding the expired parts.
	storage telegramstore.Storage
}

// cleanupChannel identifies one Telegram channel and the user who owns it, so the
// parts of several sessions can be deleted per channel with a single request.
type cleanupChannel struct {
	userID    int64
	channelID int64
}

// cleanupPartRecord is one upload part in the JSON payload passed to
// DeleteUploadPartsForCleanup. The exported fields must keep their JSON names
// because the database reads that payload.
type cleanupPartRecord struct {
	// UploadID is the session the part belongs to.
	UploadID uuid.UUID `json:"upload_id"`
	// PartNo is the 1-based part number inside the upload.
	PartNo int32 `json:"part_no"`
	// MessageID is the Telegram message holding the part. It also acts as a guard:
	// the row is only deleted while it still points at the message that was just
	// deleted from Telegram.
	MessageID int64 `json:"message_id"`
}

// NewUploadCleanupWorker returns a sweep worker backed by pool and storage. Both
// are required: Work returns ErrUploadCleanupNotConfigured when either is missing.
func NewUploadCleanupWorker(pool *pgxpool.Pool, storage telegramstore.Storage) *UploadCleanupWorker {
	return &UploadCleanupWorker{pool: pool, queries: sqlcgen.New(pool), storage: storage}
}

// Timeout allows two hours, because a single run drains every expired session
// instead of processing one bounded page.
func (w *UploadCleanupWorker) Timeout(*river.Job[UploadCleanupSweepArgs]) time.Duration {
	return 2 * time.Hour
}

// Work drains expired upload sessions until there is nothing left to clean.
//
// Each round first expires the sessions whose expires_at has passed and then
// fetches the sessions that still have parts with a Telegram message, in pages of
// at most 1000 rows, so a large backlog is never processed inside one long
// transaction. The loop stops only when both queries come back empty, which makes
// the sweep safe to retry. It returns ErrUploadCleanupNotConfigured when the
// worker is not wired up.
func (w *UploadCleanupWorker) Work(ctx context.Context, job *river.Job[UploadCleanupSweepArgs]) error {
	if w.pool == nil || w.storage == nil {
		return ErrUploadCleanupNotConfigured
	}
	for {
		expired, err := w.queries.ExpireUploadSessions(ctx)
		if err != nil {
			return fmt.Errorf("expire upload sessions: %w", err)
		}
		sessions, err := w.queries.ListUploadSessionsPendingCleanup(ctx)
		if err != nil {
			return fmt.Errorf("list upload cleanup sessions: %w", err)
		}
		if err := w.cleanupUploads(ctx, sessions); err != nil {
			return err
		}
		if len(expired) == 0 && len(sessions) == 0 {
			return nil
		}
	}
}

// cleanupUploads deletes the Telegram messages of the given sessions and then
// removes the part rows that referenced them.
//
// Parts are grouped per owner and channel, so every channel is contacted once,
// and the groups are sorted so a failure is reproducible. The messages are
// deleted before the rows on purpose: a crash in between leaves the rows in place
// with their message IDs, so the next run repeats the Telegram delete, while
// DeleteUploadPartsForCleanup only removes rows whose message ID still matches the
// one that was deleted. A row-count mismatch means a part changed concurrently,
// and the whole sweep fails so River retries it.
func (w *UploadCleanupWorker) cleanupUploads(ctx context.Context, sessions []*sqlcgen.UploadSession) error {
	if len(sessions) == 0 {
		return nil
	}
	userByUpload := make(map[uuid.UUID]int64, len(sessions))
	uploadIDs := make([]pgtype.UUID, 0, len(sessions))
	for _, session := range sessions {
		uploadID, ok := dbtypes.GoogleUUID(session.ID)
		if !ok {
			return errors.New("cleanup session has invalid upload id")
		}
		userByUpload[uploadID] = session.UserID
		uploadIDs = append(uploadIDs, session.ID)
	}
	parts, err := w.queries.ListUploadPartsForCleanupMany(ctx, uploadIDs)
	if err != nil {
		return fmt.Errorf("list upload cleanup parts: %w", err)
	}
	byChannel := make(map[cleanupChannel][]*sqlcgen.UploadPart)
	for _, part := range parts {
		if !part.MessageID.Valid || part.MessageID.Int64 <= 0 {
			continue
		}
		uploadID, ok := dbtypes.GoogleUUID(part.UploadID)
		if !ok {
			return errors.New("cleanup part has invalid upload id")
		}
		userID, ok := userByUpload[uploadID]
		if !ok {
			return errors.New("cleanup part has no upload session")
		}
		key := cleanupChannel{userID: userID, channelID: part.ChannelID}
		byChannel[key] = append(byChannel[key], part)
	}
	channels := make([]cleanupChannel, 0, len(byChannel))
	for channel := range byChannel {
		channels = append(channels, channel)
	}
	slices.SortFunc(channels, func(a, b cleanupChannel) int {
		return cmp.Or(cmp.Compare(a.userID, b.userID), cmp.Compare(a.channelID, b.channelID))
	})
	for _, channel := range channels {
		channelParts := byChannel[channel]
		messageIDs := make([]int64, 0, len(channelParts))
		records := make([]cleanupPartRecord, 0, len(channelParts))
		for _, part := range channelParts {
			messageIDs = append(messageIDs, part.MessageID.Int64)
			uploadID, _ := dbtypes.GoogleUUID(part.UploadID)
			records = append(records, cleanupPartRecord{UploadID: uploadID, PartNo: part.PartNo, MessageID: part.MessageID.Int64})
		}
		if err := w.storage.DeleteMessages(ctx, channel.userID, channel.channelID, messageIDs); err != nil {
			return fmt.Errorf("delete Telegram upload messages for user %d channel %d: %w", channel.userID, channel.channelID, err)
		}
		encoded, err := json.Marshal(records)
		if err != nil {
			return fmt.Errorf("encode cleaned upload parts: %w", err)
		}
		deleted, err := w.queries.DeleteUploadPartsForCleanup(ctx, encoded)
		if err != nil {
			return fmt.Errorf("delete upload parts after Telegram cleanup: %w", err)
		}
		if deleted != int64(len(records)) {
			return fmt.Errorf("upload cleanup deleted %d of %d parts; %d parts changed during cleanup", deleted, len(records), int64(len(records))-deleted)
		}
	}
	return nil
}
