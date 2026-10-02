// Package telegramstore stores TelDrive file data in Telegram as document
// messages and reads it back as byte ranges. GotdStorage and GotdAccount are
// the production implementations built on gotd; the package interfaces
// (Storage, MetadataReader, DownloadSessionOpener, DocumentMessageLister,
// BotInviter, and Account) keep the rest of the server independent of them, and
// Runner concentrates Telegram authentication, client lifetime, bot selection,
// retries, rate limiting, and flood waiting in one place. Errors callers are
// expected to classify are exported as sentinel values and must be tested with
// errors.Is.
package telegramstore

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

// maxProfilePhotoBytes caps the bytes buffered for one profile photo download.
// A larger photo fails the download instead of growing the buffer further.
const maxProfilePhotoBytes = 10 * 1024 * 1024

// DiscoveredChannel is a Telegram channel the account can use as a storage
// channel.
type DiscoveredChannel struct {
	// ID is the Telegram channel ID, the identifier recorded with every part
	// that lands in the channel.
	ID int64
	// Name is the channel title as Telegram reports it, used for display and as
	// the primary sort key of the discovery result.
	Name string
}

// ProfilePhoto is a downloaded Telegram profile photo together with the photo
// ID it was fetched for.
type ProfilePhoto struct {
	// Content holds the encoded photo bytes, copied out of the download buffer,
	// so it stays valid after the call returns.
	Content []byte
	// PhotoID is the Telegram photo ID. It changes whenever the user uploads a
	// new photo, so it can serve as a cache validator.
	PhotoID int64
}

// Account exposes the Telegram account-level data the TelDrive API serves on
// behalf of one user.
type Account interface {
	// DiscoverChannels lists the channels the user may store files in, that is
	// the channels they created or administer with the right to add admins.
	// Duplicates are collapsed and the result is sorted by title and then by
	// channel ID, so the order is stable across calls. It returns
	// ErrInvalidRequest for a non-positive user ID and the session error
	// otherwise.
	DiscoverChannels(ctx context.Context, userID int64) ([]DiscoveredChannel, error)
	// ProfilePhoto returns the user's current Telegram profile photo. The
	// boolean is false with a nil error when the account has no usable photo; a
	// download that exceeds the size cap fails with ErrProfilePhotoTooLarge and
	// a failure on the Telegram side with its own error, both wrapped with the
	// download context. The content is a copy owned by the caller.
	ProfilePhoto(ctx context.Context, userID int64) (ProfilePhoto, bool, error)
}

// GotdAccount is the production Account implementation. It holds only a
// Runner and keeps no other state, so it is safe for concurrent use: every
// call runs on a session of the user passed to it.
type GotdAccount struct {
	// runner executes every Telegram call and owns authentication, retries, and
	// client lifetime. NewGotdAccount rejects a nil runner.
	runner Runner
}

// NewGotdAccount returns an Account that runs its Telegram calls through
// runner. A nil runner is rejected with ErrClientUnavailable, so a non-nil
// result always has a usable runner.
func NewGotdAccount(runner Runner) (*GotdAccount, error) {
	if runner == nil {
		return nil, ErrClientUnavailable
	}
	return &GotdAccount{runner: runner}, nil
}

// DiscoverChannels lists the user's usable storage channels by walking the
// dialog list in batches of 100 and keeping the channels the user created or
// administers with the right to add admins. Left channels and non-channel
// peers are skipped, duplicates are folded by channel ID, and the result is
// sorted by title and then by channel ID. It runs as a management operation,
// which the runner refuses to serve from a bot session, and a nil receiver,
// missing runner, or non-positive user ID returns ErrInvalidRequest.
func (a *GotdAccount) DiscoverChannels(ctx context.Context, userID int64) ([]DiscoveredChannel, error) {
	if a == nil || a.runner == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	channels := make(map[int64]DiscoveredChannel)
	err := a.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		iterator := query.GetDialogs(api).BatchSize(100).Iter()
		for iterator.Next(runCtx) {
			element := iterator.Value()
			peer, ok := element.Peer.(*tg.InputPeerChannel)
			if !ok {
				continue
			}
			channel, ok := element.Entities.Channel(peer.ChannelID)
			if !ok || channel == nil || channel.ID == 0 || channel.Left {
				continue
			}
			rights, hasRights := channel.GetAdminRights()
			if !channel.Creator && (!hasRights || !rights.AddAdmins) {
				continue
			}
			channels[channel.ID] = DiscoveredChannel{ID: channel.ID, Name: channel.Title}
		}
		if err := iterator.Err(); err != nil {
			return fmt.Errorf("iterate Telegram dialogs: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := slices.SortedFunc(maps.Values(channels), func(a, b DiscoveredChannel) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return result, nil
}

// ProfilePhoto downloads the small variant of the user's profile photo on a
// download session and returns it with the Telegram photo ID. It reports
// found=false and a nil error when Telegram returns no user, no photo, or a
// photo without an ID. A download that exceeds maxProfilePhotoBytes fails with
// ErrProfilePhotoTooLarge and a Telegram failure with its own error, both
// wrapped with the download context. A nil receiver, missing runner, or
// non-positive user ID returns ErrInvalidRequest.
func (a *GotdAccount) ProfilePhoto(ctx context.Context, userID int64) (ProfilePhoto, bool, error) {
	if a == nil || a.runner == nil || userID <= 0 {
		return ProfilePhoto{}, false, ErrInvalidRequest
	}
	var result ProfilePhoto
	var found bool
	err := a.runner.Run(ctx, userID, OperationDownload, func(runCtx context.Context, api *tg.Client) error {
		users, err := api.UsersGetUsers(runCtx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("get Telegram self: %w", err)
		}
		if len(users) == 0 {
			return nil
		}
		user, ok := users[0].AsNotEmpty()
		if !ok || user.Photo == nil {
			return nil
		}
		photo, ok := user.Photo.AsNotEmpty()
		if !ok || photo.PhotoID == 0 {
			return nil
		}
		location := &tg.InputPeerPhotoFileLocation{
			Big: false, Peer: user.AsInputPeer(), PhotoID: photo.PhotoID,
		}
		var buffer bytes.Buffer
		writer := &boundedWriter{writer: &buffer, remaining: maxProfilePhotoBytes}
		if _, err := downloader.NewDownloader().Download(api, location).Stream(runCtx, writer); err != nil {
			return fmt.Errorf("download Telegram profile photo: %w", err)
		}
		result = ProfilePhoto{Content: append([]byte(nil), buffer.Bytes()...), PhotoID: photo.PhotoID}
		found = true
		return nil
	})
	if err != nil {
		return ProfilePhoto{}, false, err
	}
	return result, found, nil
}

// ErrProfilePhotoTooLarge reports a Telegram profile photo download that would
// exceed maxProfilePhotoBytes. ProfilePhoto wraps it with the download context,
// so callers classify it with errors.Is.
var ErrProfilePhotoTooLarge = errors.New("Telegram profile photo exceeds size limit")

// boundedWriter buffers a download while enforcing a byte budget. It caps
// profile photo downloads: the first write that would cross the budget fails
// without storing anything, so the buffer never exceeds the limit.
type boundedWriter struct {
	// writer receives the accepted bytes. It is not written to once the budget
	// is exhausted.
	writer *bytes.Buffer
	// remaining is the number of bytes still accepted, counted down by the bytes
	// actually written.
	remaining int64
}

// Write appends p to the buffer while it fits in the remaining budget. It
// returns ErrProfilePhotoTooLarge and writes nothing when p alone would exceed
// the budget, which is the only failure mode of this writer.
func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrProfilePhotoTooLarge
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

var _ Account = (*GotdAccount)(nil)
