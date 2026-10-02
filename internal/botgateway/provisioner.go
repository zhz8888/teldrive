package botgateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// provisionChannelPageSize is the number of channels listed per cursor page
// while promoting a bot, and the page size the cursor is advanced with.
const provisionChannelPageSize = 200

// ExistingChannelProvisioner ensures a bot cannot become upload-eligible until
// it has been invited to every channel currently registered for the user.
type ExistingChannelProvisioner struct {
	// queries lists the channels the user already owns, newest first.
	queries *sqlcgen.Queries
	// storage performs the Telegram promotion on the user's own session.
	storage telegramstore.BotInviter
}

// NewExistingChannelProvisioner returns a provisioner that promotes verified
// bots through storage. A nil pool or storage is reported as
// bots.ErrInvalidInput, and the pool is not pinged.
func NewExistingChannelProvisioner(pool *pgxpool.Pool, storage telegramstore.BotInviter) (*ExistingChannelProvisioner, error) {
	if pool == nil || storage == nil {
		return nil, bots.ErrInvalidInput
	}
	return &ExistingChannelProvisioner{queries: sqlcgen.New(pool), storage: storage}, nil
}

// ProvisionBot promotes the bot described by identity in every channel the user
// already owns. It walks the channels newest first in cursor pages, so a user
// with more channels than one page holds is provisioned completely. It stops at
// the first channel whose promotion fails and returns an error naming the bot
// and channel, so channels after that one are not attempted and the caller must
// treat the bot as only partially provisioned. It returns bots.ErrInvalidInput
// for a nil receiver or dependency, a non-positive user or bot ID, or a blank
// username.
func (p *ExistingChannelProvisioner) ProvisionBot(ctx context.Context, userID int64, identity bots.Identity) error {
	if p == nil || p.queries == nil || p.storage == nil || userID <= 0 || identity.ID <= 0 || strings.TrimSpace(identity.Username) == "" {
		return bots.ErrInvalidInput
	}
	var afterCreatedAt *time.Time
	var afterChannelID *int64
	for {
		var afterID pgtype.Int8
		if afterChannelID != nil {
			afterID = dbtypes.Int8(*afterChannelID)
		}
		channels, err := p.queries.ListChannels(ctx, sqlcgen.ListChannelsParams{
			UserID: userID, AfterCreatedAt: dbtypes.OptionalTime(afterCreatedAt),
			AfterChannelID: afterID, PageSize: provisionChannelPageSize,
		})
		if err != nil {
			return fmt.Errorf("list channels for bot provisioning: %w", err)
		}
		for _, channel := range channels {
			if err := p.storage.InviteBot(ctx, userID, channel.ChannelID, identity.Username); err != nil {
				return fmt.Errorf("provision bot %d in channel %d: %w", identity.ID, channel.ChannelID, err)
			}
		}
		if len(channels) < provisionChannelPageSize {
			return nil
		}
		// Continue after the last row of this page; the cursor pair breaks
		// ties between channels sharing created_at.
		last := channels[len(channels)-1]
		createdAt := last.CreatedAt.Time
		channelID := last.ChannelID
		afterCreatedAt, afterChannelID = &createdAt, &channelID
	}
}

var _ bots.Provisioner = (*ExistingChannelProvisioner)(nil)
