package channels

import (
	"context"
	"errors"

	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// TelegramCreator adapts the shared Telegram storage boundary to channel
// rollover. Bot provisioning and authentication remain inside the storage
// implementation's Runner.
type TelegramCreator struct {
	// Storage performs the Telegram calls on the user's own session. It must be
	// set before use; a nil value makes both methods fail with an error instead
	// of panicking.
	Storage telegramstore.Storage
}

// Create creates a Telegram channel named name for the user and returns the
// channel ID and name reported by the storage layer. It fails when Storage is
// nil and otherwise passes the storage error through unchanged, so a channel
// that could not be made usable is never reported as created.
func (c TelegramCreator) Create(ctx context.Context, userID int64, name string) (RemoteChannel, error) {
	if c.Storage == nil {
		return RemoteChannel{}, errors.New("Telegram storage is not configured")
	}
	channel, err := c.Storage.CreateChannel(ctx, userID, name)
	if err != nil {
		return RemoteChannel{}, err
	}
	return RemoteChannel{ID: channel.ID, Name: channel.Name}, nil
}

// Delete removes the Telegram channel on the user's own session. It fails when
// Storage is nil and otherwise passes the storage error through unchanged,
// where a channel that no longer resolves already counts as deleted.
func (c TelegramCreator) Delete(ctx context.Context, userID, channelID int64) error {
	if c.Storage == nil {
		return errors.New("Telegram storage is not configured")
	}
	return c.Storage.DeleteChannel(ctx, userID, channelID)
}
