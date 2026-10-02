package channels

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

var (
	// ErrInvalidOwner reports a non-positive TelDrive user ID passed to a
	// channel operation. Callers must test it with errors.Is.
	ErrInvalidOwner = errors.New("invalid channel owner")
	// ErrInvalidChannel reports a channel that is unknown to the user or was
	// addressed with a non-positive ID; the API also maps it to 404 for
	// malformed list cursors, which is why it must be tested with errors.Is.
	ErrInvalidChannel = errors.New("channel does not belong to user")
	// ErrNoSelected reports that the user has no selected channel and
	// Config.AutoCreate is false, so there is nothing to allocate from.
	// Callers must test it with errors.Is.
	ErrNoSelected = errors.New("no selected channel")
	// ErrChannelFull reports that the addressed channel already stores
	// Config.PartLimit parts, so it cannot accept another one. Callers must
	// test it with errors.Is.
	ErrChannelFull = errors.New("channel capacity reached")
	// ErrAutoCreateOff reports that the selected channel is full or
	// unavailable but Config.AutoCreate is false, so rollover was refused
	// instead of creating a replacement. Callers must test it with errors.Is.
	ErrAutoCreateOff = errors.New("automatic channel creation is disabled")
	// ErrChannelUnhealthy reports a channel whose health column reads
	// "unavailable", which excludes it from both explicit and automatic
	// selection. ResolveMany also uses the same state internally to stop
	// reusing a channel it has just filled during one allocation. Callers must
	// test it with errors.Is.
	ErrChannelUnhealthy = errors.New("channel is unavailable")
)

// RemoteChannel is the minimum Telegram metadata required for durable storage.
type RemoteChannel struct {
	// ID is the Telegram channel ID, which is unique across all accounts.
	ID int64 `json:"channel_id"`
	// Name is the display name reported by Telegram. Sync stores it as given,
	// and the creation paths substitute the requested name when Telegram
	// returns none.
	Name string `json:"name"`
}

// Creator performs Telegram-side channel lifecycle operations. Implementations
// are expected to add the configured upload bots before returning from Create.
type Creator interface {
	// Create creates a Telegram channel named name for the user and returns its
	// metadata. An implementation that cannot make the channel usable, for
	// example because a bot cannot be added, must fail instead of returning it.
	Create(ctx context.Context, userID int64, name string) (RemoteChannel, error)
	// Delete removes the Telegram channel. A channel that no longer resolves is
	// expected to count as already deleted and return nil, because the
	// deletion compensations run after failures and must not fail again.
	Delete(ctx context.Context, userID, channelID int64) error
}

// Config is the channel allocation policy of a Service.
type Config struct {
	// PartLimit is the maximum number of stored Telegram messages a channel may
	// hold. Values at or below zero disable the limit, so no channel is ever
	// considered full and rollover only happens for unavailable channels.
	PartLimit int64
	// AutoCreate enables rollover: when the selected channel is full or
	// unavailable, Resolve and ResolveMany create and select a replacement
	// instead of failing. The admin methods never create channels implicitly
	// and ignore this flag.
	AutoCreate bool
	// NamePrefix is the prefix of generated channel names, which end with the
	// UTC creation timestamp. NewService replaces a blank prefix with
	// "storage".
	NamePrefix string
}

// Service owns Telegram storage channel allocation for every user. It keeps no
// per-request state and is safe for concurrent use. The advisory lock taken by
// ResolveMany and rollover is scoped to the user inside the database rather
// than to the process, so several TelDrive instances sharing one database
// cannot create two selected channels for the same user.
type Service struct {
	// pool is the connection pool that hosts the rollover transaction and the
	// allocation advisory lock.
	pool *pgxpool.Pool
	// queries is the sqlc handle bound to pool for statements outside a
	// transaction.
	queries *sqlcgen.Queries
	// creator performs the Telegram-side calls; a nil creator makes channel
	// creation fail with an explicit error instead of panicking.
	creator Creator
	// config is the allocation policy copied from the Config passed to
	// NewService.
	config Config
	// now is the service clock used to name generated channels; tests replace
	// it to make names deterministic.
	now func() time.Time
}

// NewService returns a channel service that creates Telegram channels through
// creator under the given policy. A blank Config.NamePrefix is replaced with
// "storage". The pool is not pinged, so connectivity problems surface on first
// use, and a nil creator only fails the operations that need it.
func NewService(pool *pgxpool.Pool, creator Creator, cfg Config) *Service {
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = "storage"
	}
	return &Service{
		pool:    pool,
		queries: sqlcgen.New(pool),
		creator: creator,
		config:  cfg,
		now:     time.Now,
	}
}

// Resolve returns an owned channel suitable for one more uploaded part.
// Explicit channel requests never roll over silently.
func (s *Service) Resolve(ctx context.Context, userID, requestedChannelID int64) (int64, error) {
	if userID <= 0 {
		return 0, ErrInvalidOwner
	}
	if requestedChannelID != 0 {
		channel, err := s.queries.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{
			UserID:    userID,
			ChannelID: requestedChannelID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrInvalidChannel
		}
		if err != nil {
			return 0, fmt.Errorf("get requested channel: %w", err)
		}
		if channel.Health == sqlcgen.ChannelHealthUnavailable {
			return 0, ErrChannelUnhealthy
		}
		full, err := s.limitReached(ctx, requestedChannelID)
		if err != nil {
			return 0, err
		}
		if full {
			return 0, ErrChannelFull
		}
		return requestedChannelID, nil
	}

	selected, err := s.queries.GetSelectedChannel(ctx, userID)
	if err == nil {
		if selected.Health != sqlcgen.ChannelHealthUnavailable {
			full, countErr := s.limitReached(ctx, selected.ChannelID)
			if countErr != nil {
				return 0, countErr
			}
			if !full {
				return selected.ChannelID, nil
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("get selected channel: %w", err)
	}

	if !s.config.AutoCreate {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNoSelected
		}
		return 0, ErrAutoCreateOff
	}
	return s.rollover(ctx, userID)
}

// ResolveMany reserves channel capacity for count parts while holding the
// rollover lock. The returned channel IDs correspond to parts in input order.
func (s *Service) ResolveMany(ctx context.Context, userID int64, count int) (channelIDs []int64, err error) {
	if userID <= 0 {
		return nil, ErrInvalidOwner
	}
	if count <= 0 {
		return []int64{}, nil
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire channel allocation connection: %w", err)
	}
	defer conn.Release()
	lockID := advisoryLockID(userID)
	queries := sqlcgen.New(conn)
	if err := queries.AcquireAdvisoryLock(ctx, lockID); err != nil {
		return nil, fmt.Errorf("acquire channel allocation lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, unlockErr := queries.ReleaseAdvisoryLock(unlockCtx, lockID); unlockErr != nil && err == nil {
			err = fmt.Errorf("release channel allocation lock: %w", unlockErr)
		}
	}()

	selected, selectedErr := queries.GetSelectedChannel(ctx, userID)
	if selectedErr != nil && !errors.Is(selectedErr, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get selected channel for allocation: %w", selectedErr)
	}
	for len(channelIDs) < count {
		var channelID int64
		used := int64(0)
		if selectedErr == nil && selected.Health != sqlcgen.ChannelHealthUnavailable {
			channelID = selected.ChannelID
			if s.config.PartLimit > 0 {
				used, err = queries.CountChannelStoredMessages(ctx, channelID)
				if err != nil {
					return nil, fmt.Errorf("count channel parts: %w", err)
				}
			}
		}
		capacity := count - len(channelIDs)
		if channelID != 0 && s.config.PartLimit > 0 {
			capacity = int(max(s.config.PartLimit-used, 0))
			capacity = min(capacity, count-len(channelIDs))
		}
		if channelID != 0 && capacity > 0 {
			for range capacity {
				channelIDs = append(channelIDs, channelID)
			}
			if len(channelIDs) < count {
				selected.Health = sqlcgen.ChannelHealthUnavailable
			}
			continue
		}
		if !s.config.AutoCreate {
			if errors.Is(selectedErr, pgx.ErrNoRows) {
				return nil, ErrNoSelected
			}
			return nil, ErrAutoCreateOff
		}
		selected, err = s.createSelectedChannel(ctx, conn, queries, userID)
		if err != nil {
			return nil, err
		}
		selectedErr = nil
	}
	return channelIDs, nil
}

// createSelectedChannel creates a Telegram channel through the configured
// creator, registers it for the user and selects it, all in one transaction on
// conn. Callers must already hold the user's advisory lock: the transaction
// clears any previous selection and replaces it, so running it concurrently
// would leave a stray selected row. Every failure after the Telegram channel
// exists deletes it again, so a channel the database does not know about is not
// left behind.
func (s *Service) createSelectedChannel(ctx context.Context, conn *pgxpool.Conn, queries *sqlcgen.Queries, userID int64) (*sqlcgen.Channel, error) {
	if s.creator == nil {
		return nil, errors.New("Telegram channel creator is not configured")
	}
	name := fmt.Sprintf("%s_%s", strings.TrimSpace(s.config.NamePrefix), s.now().UTC().Format("20060102_150405"))
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
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("begin channel allocation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	q := queries.WithTx(tx)
	if err := q.ClearSelectedChannel(ctx, userID); err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("clear selected channel: %w", err)
	}
	if _, err := q.CreateChannel(ctx, sqlcgen.CreateChannelParams{ChannelID: remote.ID, UserID: userID, Name: remote.Name}); err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("create channel record: %w", err)
	}
	selected, err := q.SelectChannel(ctx, sqlcgen.SelectChannelParams{UserID: userID, ChannelID: remote.ID})
	if err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("select channel record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.compensateDelete(userID, remote.ID)
		return nil, fmt.Errorf("commit channel allocation: %w", err)
	}
	return selected, nil
}

// limitReached reports whether the channel already stores Config.PartLimit
// parts. It reports false when the limit is disabled and returns the count
// error unchanged when the query fails.
func (s *Service) limitReached(ctx context.Context, channelID int64) (bool, error) {
	return s.limitReachedWith(ctx, s.queries, channelID)
}

// limitReachedWith is limitReached against an explicit query handle, so callers
// that already own a connection (inside a transaction, or while holding the
// allocation advisory lock) count through that same connection. It reports
// false when Config.PartLimit is not positive and therefore never queries.
func (s *Service) limitReachedWith(ctx context.Context, queries *sqlcgen.Queries, channelID int64) (bool, error) {
	if s.config.PartLimit <= 0 {
		return false, nil
	}
	count, err := queries.CountChannelStoredMessages(ctx, channelID)
	if err != nil {
		return false, fmt.Errorf("count channel parts: %w", err)
	}
	return count >= s.config.PartLimit, nil
}

// rollover creates and selects a new channel for a user whose selected channel
// is full or unavailable. It takes the per-user advisory lock before doing
// anything, then rechecks the selection under the lock, because another process
// may have completed the rollover while this caller waited; when the recheck
// finds a usable channel no Telegram channel is created. The lock is released
// on a background context with a five second deadline, and a release failure is
// reported only when no earlier error is pending, so it cannot mask the
// original cause.
func (s *Service) rollover(ctx context.Context, userID int64) (channelID int64, err error) {
	if s.creator == nil {
		return 0, errors.New("Telegram channel creator is not configured")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire rollover connection: %w", err)
	}
	defer conn.Release()

	lockID := advisoryLockID(userID)
	lockedQueries := sqlcgen.New(conn)
	if err := lockedQueries.AcquireAdvisoryLock(ctx, lockID); err != nil {
		return 0, fmt.Errorf("acquire channel rollover lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, unlockErr := lockedQueries.ReleaseAdvisoryLock(unlockCtx, lockID); unlockErr != nil && err == nil {
			err = fmt.Errorf("release channel rollover lock: %w", unlockErr)
		}
	}()

	// Another process may have completed rollover while this caller waited.
	selected, selectedErr := lockedQueries.GetSelectedChannel(ctx, userID)
	if selectedErr == nil && selected.Health != sqlcgen.ChannelHealthUnavailable {
		full, countErr := s.limitReachedWith(ctx, lockedQueries, selected.ChannelID)
		if countErr != nil {
			return 0, countErr
		}
		if !full {
			return selected.ChannelID, nil
		}
	} else if selectedErr != nil && !errors.Is(selectedErr, pgx.ErrNoRows) {
		return 0, fmt.Errorf("recheck selected channel: %w", selectedErr)
	}

	name := fmt.Sprintf("%s_%s", strings.TrimSpace(s.config.NamePrefix), s.now().UTC().Format("20060102_150405"))
	remote, err := s.creator.Create(ctx, userID, name)
	if err != nil {
		return 0, fmt.Errorf("create Telegram channel: %w", err)
	}
	if remote.ID == 0 {
		return 0, errors.New("Telegram returned an empty channel id")
	}
	if strings.TrimSpace(remote.Name) == "" {
		remote.Name = name
	}

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		s.compensateDelete(userID, remote.ID)
		return 0, fmt.Errorf("begin channel rollover transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	q := lockedQueries.WithTx(tx)
	if err := q.ClearSelectedChannel(ctx, userID); err != nil {
		s.compensateDelete(userID, remote.ID)
		return 0, fmt.Errorf("clear selected channel: %w", err)
	}
	if _, err := q.CreateChannel(ctx, sqlcgen.CreateChannelParams{
		ChannelID: remote.ID,
		UserID:    userID,
		Name:      remote.Name,
	}); err != nil {
		s.compensateDelete(userID, remote.ID)
		return 0, fmt.Errorf("create channel record: %w", err)
	}
	if _, err := q.SelectChannel(ctx, sqlcgen.SelectChannelParams{
		UserID:    userID,
		ChannelID: remote.ID,
	}); err != nil {
		s.compensateDelete(userID, remote.ID)
		return 0, fmt.Errorf("select channel record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.compensateDelete(userID, remote.ID)
		return 0, fmt.Errorf("commit channel rollover: %w", err)
	}
	return remote.ID, nil
}

// compensateDelete deletes a Telegram channel that was created but could not be
// recorded, using a fresh 15-second timeout because the caller's context is
// already failing. The delete error is deliberately dropped: the compensation
// must not replace the original failure, and a channel that is gone already
// counts as deleted.
func (s *Service) compensateDelete(userID, channelID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.creator.Delete(ctx, userID, channelID)
}

// advisoryLockID derives the PostgreSQL advisory lock key that serializes
// channel allocation for one user. The key is a SHA-256 digest of the user ID
// under the "teldrive/channel-rollover/" domain prefix, so it is stable across
// processes and instances and cannot collide with the login flow locks taken by
// internal/authn, which hash under their own prefix.
func advisoryLockID(userID int64) int64 {
	var input [8]byte
	binary.BigEndian.PutUint64(input[:], uint64(userID))
	digest := sha256.Sum256(append([]byte("teldrive/channel-rollover/"), input[:]...))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
