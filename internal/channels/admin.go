// Package channels owns the Telegram storage channels TelDrive uploads into: it
// tracks the channel selected per user, reserves capacity for a batch of parts,
// rolls over to a freshly created channel when the selected one is full or
// unavailable, and lists, selects, deletes and syncs channels for the API. All
// Telegram calls go through a Creator, so the package never talks to Telegram
// itself, and the sentinel errors below explain why an allocation was refused.
package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

var (
	// ErrSelectedChannel reports that a channel could not become, or stop being,
	// the single selected upload channel: Delete was asked to remove the channel
	// currently selected, and a creation or selection that races another caller
	// lost on the partial unique index that keeps one selection per user. Callers
	// must test it with errors.Is, and the API answers it with its own generic
	// conflict message, so this text is what the server logs.
	ErrSelectedChannel = errors.New("selected channel cannot be deleted or claimed by a concurrent request")
	// ErrChannelInUse reports that a channel still holds referenced file or
	// upload parts, so deleting it would orphan stored objects. Callers must
	// test it with errors.Is.
	ErrChannelInUse = errors.New("channel still contains referenced objects")
)

// ListInput selects one cursor page of a user's channels, newest first. UserID
// is mandatory; the After fields are the exclusive cursor taken from the
// previous page, and Limit is clamped by List.
type ListInput struct {
	// UserID is the TelDrive user whose channels are listed and must be
	// positive.
	UserID int64
	// AfterCreatedAt is the created_at of the last row of the previous page;
	// nil starts at the newest channel. It is only meaningful together with
	// AfterChannelID.
	AfterCreatedAt *time.Time
	// AfterChannelID is the channel_id of the last row of the previous page and
	// breaks ties between rows sharing created_at; nil starts at the newest
	// channel.
	AfterChannelID *int64
	// Limit is the requested page size. Zero or negative values default to 100
	// and values above 200 are clamped to 200.
	Limit int32
}

// List returns one cursor page of the user's channels, newest first, ordered by
// (created_at, channel_id) descending. Limit defaults to 100 and is capped at
// 200; a page shorter than the effective limit means the user's channels are
// exhausted. It returns ErrInvalidOwner for a non-positive user ID and a
// wrapped query error otherwise.
func (s *Service) List(ctx context.Context, in ListInput) ([]*sqlcgen.Channel, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidOwner
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	var afterID pgtype.Int8
	if in.AfterChannelID != nil {
		afterID = dbtypes.Int8(*in.AfterChannelID)
	}
	rows, err := s.queries.ListChannels(ctx, sqlcgen.ListChannelsParams{
		UserID: in.UserID, AfterCreatedAt: dbtypes.OptionalTime(in.AfterCreatedAt),
		AfterChannelID: afterID, PageSize: in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	return rows, nil
}

// Create creates a Telegram channel named name for the user, registers it, and
// optionally makes it the selected upload channel, all in one transaction. A
// name that is blank after trimming is replaced by a generated
// "<prefix>_<utc timestamp>" one, and a name Telegram returns empty is replaced
// by the requested name. It returns ErrInvalidOwner for a non-positive user ID
// or a nil creator, and deletes a freshly created Telegram channel again whenever
// the row cannot be written. A creation that loses the race for the single
// selected channel to a concurrent one is reported as ErrSelectedChannel and
// deletes its own Telegram channel as well. Unlike Resolve, it never consults the
// part limit or Config.AutoCreate.
func (s *Service) Create(ctx context.Context, userID int64, name string, selected bool) (*sqlcgen.Channel, error) {
	if userID <= 0 || s.creator == nil {
		return nil, ErrInvalidOwner
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("%s_%s", strings.TrimSpace(s.config.NamePrefix), s.now().UTC().Format("20060102_150405"))
	}
	remote, err := s.creator.Create(ctx, userID, name)
	if err != nil {
		return nil, fmt.Errorf("create Telegram channel: %w", err)
	}
	if remote.ID == 0 {
		return nil, errors.New("Telegram returned an empty channel id")
	}
	if strings.TrimSpace(remote.Name) == "" {
		remote.Name = name
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)
	if selected {
		if err := q.ClearSelectedChannel(ctx, userID); err != nil {
			s.compensateDelete(userID, remote.ID)
			return nil, fmt.Errorf("clear selected channel: %w", err)
		}
	}
	row, err := q.CreateChannel(ctx, sqlcgen.CreateChannelParams{ChannelID: remote.ID, UserID: userID, Name: remote.Name})
	if err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("create channel record: %w", err)
	}
	if selected {
		row, err = q.SelectChannel(ctx, sqlcgen.SelectChannelParams{UserID: userID, ChannelID: remote.ID})
		if err != nil {
			s.compensateDelete(userID, remote.ID)
			// Two concurrent creations of a selected channel for the same user can
			// both pass the clear above and then collide on the partial unique index
			// that keeps one selection per user, which is a conflict rather than a
			// fault. The Telegram channel created for the losing row is deleted
			// again, because the rollback below leaves no row pointing at it.
			if isUniqueViolation(err) {
				return nil, ErrSelectedChannel
			}
			return nil, fmt.Errorf("select created channel: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("commit created channel: %w", err)
	}
	return row, nil
}

// Select makes channelID the user's selected upload channel, clearing any
// previous selection in the same transaction so that at most one channel is
// ever selected. It returns ErrInvalidChannel for a non-positive user or
// channel ID or for a channel the user does not own, and ErrChannelUnhealthy
// when the channel is marked unavailable.
func (s *Service) Select(ctx context.Context, userID, channelID int64) (*sqlcgen.Channel, error) {
	if userID <= 0 || channelID == 0 {
		return nil, ErrInvalidChannel
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin channel selection: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)
	channel, err := q.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{UserID: userID, ChannelID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidChannel
	}
	if err != nil {
		return nil, fmt.Errorf("get channel for selection: %w", err)
	}
	if channel.Health == sqlcgen.ChannelHealthUnavailable {
		return nil, ErrChannelUnhealthy
	}
	if err := q.ClearSelectedChannel(ctx, userID); err != nil {
		return nil, fmt.Errorf("clear selected channel: %w", err)
	}
	channel, err = q.SelectChannel(ctx, sqlcgen.SelectChannelParams{UserID: userID, ChannelID: channelID})
	if err != nil {
		// Two callers selecting a channel for the same user at once can both
		// pass the clear and then collide on the partial unique index that keeps
		// one selection per user, which is a conflict rather than a fault.
		if isUniqueViolation(err) {
			return nil, ErrSelectedChannel
		}
		return nil, fmt.Errorf("select channel: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit channel selection: %w", err)
	}
	return channel, nil
}

// Delete removes the channel's row and its Telegram channel. The selected
// channel is refused with ErrSelectedChannel, a channel that still has
// referenced file or upload parts is refused with ErrChannelInUse, and an
// unknown or foreign channel is reported as ErrInvalidChannel. The row delete
// runs in a transaction that is committed only after Telegram accepted the
// deletion, so a delete that matches no row (for example because the channel
// was selected or removed concurrently) rolls back and reports
// ErrInvalidChannel without touching Telegram. The remaining window is a commit
// that fails after Telegram already deleted the channel: the row then survives
// with its Telegram channel gone, which a retry repairs because deleting a
// channel that no longer resolves succeeds.
//
// The reference count and the row delete share that transaction, and the count is
// taken again after the delete, because nothing in the schema stops a part from
// being inserted or claimed for this channel in between: the second count sees a
// reference created after the first one, and the deferred rollback then restores
// the row and the call reports ErrChannelInUse instead of destroying a channel
// that is in use. Telegram is only asked to delete the channel once the row is
// gone and the re-check passed, so a refused delete never destroys anything. The
// window that remains is a reference inserted after the re-check but before the
// commit, which the row delete cannot see; closing it would need a foreign key on
// the part tables or a table lock, neither of which this package can add.
func (s *Service) Delete(ctx context.Context, userID, channelID int64) error {
	if userID <= 0 || channelID == 0 || s.creator == nil {
		return ErrInvalidChannel
	}
	channel, err := s.queries.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{UserID: userID, ChannelID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidChannel
	}
	if err != nil {
		return fmt.Errorf("get channel for deletion: %w", err)
	}
	if channel.Selected {
		return ErrSelectedChannel
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin channel deletion: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)
	references, err := q.CountChannelReferences(ctx, channelID)
	if err != nil {
		return fmt.Errorf("count channel references: %w", err)
	}
	if references > 0 {
		return ErrChannelInUse
	}
	count, err := q.DeleteChannel(ctx, sqlcgen.DeleteChannelParams{UserID: userID, ChannelID: channelID})
	if err != nil {
		return fmt.Errorf("delete channel record: %w", err)
	}
	if count == 0 {
		return ErrInvalidChannel
	}
	references, err = q.CountChannelReferences(ctx, channelID)
	if err != nil {
		return fmt.Errorf("recount channel references after the delete: %w", err)
	}
	if references > 0 {
		return ErrChannelInUse
	}
	if err := s.creator.Delete(ctx, userID, channelID); err != nil {
		return fmt.Errorf("delete Telegram channel: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit channel deletion: %w", err)
	}
	return nil
}
