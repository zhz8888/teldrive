// Package botgateway connects the bot registry to the Telegram storage
// boundary: it verifies bot tokens, resolves the bots that must join a new
// channel, promotes a verified bot into the user's existing channels, and runs
// uploads and downloads through a selected bot session. Selection happens per
// operation and bot sessions are stored as encrypted Telethon StringSession
// values, so an authorized bot survives restarts and is reused instead of
// logging in again.
package botgateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// GotdVerifier authenticates bot tokens against Telegram on a throwaway
// in-memory session, so verification neither touches the stored session of the
// user nor leaves an authorization behind. It implements bots.Verifier.
type GotdVerifier struct {
	// factory builds the gotd client used for the one-off login.
	factory *telegramstore.Factory
}

// NewGotdVerifier returns a verifier that logs in through factory. A nil
// factory is reported as bots.ErrInvalidInput. Every Verify call creates and
// tears down its own client, so the factory must be safe for concurrent use.
func NewGotdVerifier(factory *telegramstore.Factory) (*GotdVerifier, error) {
	if factory == nil {
		return nil, bots.ErrInvalidInput
	}
	return &GotdVerifier{factory: factory}, nil
}

// Verify logs in with token and returns the identity Telegram reports for it.
// Every failure is wrapped with "verify Telegram bot"; a credential that
// authenticates as a regular user is reported with bots.ErrNotBot inside that
// wrap, so callers must test it with errors.Is. The session is in-memory only,
// so nothing is written and the authorization cannot be resumed later.
func (v *GotdVerifier) Verify(ctx context.Context, token string) (bots.Identity, error) {
	memory := &session.StorageMemory{}
	client, err := v.factory.New(memory)
	if err != nil {
		return bots.Identity{}, err
	}
	var identity bots.Identity
	err = client.Run(ctx, func(runCtx context.Context) error {
		if _, err := client.Auth().Bot(runCtx, token); err != nil {
			return err
		}
		self, err := client.Self(runCtx)
		if err != nil {
			return err
		}
		if self == nil || !self.Bot {
			return bots.ErrNotBot
		}
		identity = bots.Identity{ID: self.ID, Username: self.Username}
		return nil
	})
	if err != nil {
		return bots.Identity{}, fmt.Errorf("verify Telegram bot: %w", err)
	}
	return identity, nil
}

// ChannelBotProvider resolves a user's enabled bots to Telegram input users, so
// a newly created channel can add them before it is handed out for uploads. It
// implements telegramstore.BotProvider; a user without enabled bots yields an
// empty list, which is valid.
type ChannelBotProvider struct {
	// queries reads the enabled bot rows of the user, in ascending bot ID
	// order.
	queries *sqlcgen.Queries
}

// NewChannelBotProvider returns a provider backed by pool. A nil pool is
// reported as bots.ErrInvalidInput, and the pool is not pinged.
func NewChannelBotProvider(pool *pgxpool.Pool) (*ChannelBotProvider, error) {
	if pool == nil {
		return nil, bots.ErrInvalidInput
	}
	return &ChannelBotProvider{queries: sqlcgen.New(pool)}, nil
}

// ChannelBots resolves every enabled bot of userID to a tg.InputUserClass by
// its stored username. Bots without a stored username are skipped, and a
// username that no longer resolves to the recorded bot ID fails the whole call
// with a message wrapping bots.ErrNotFound, so a caller never adds the wrong
// account to a channel. It returns bots.ErrInvalidInput for a nil receiver,
// query handle or API client, or a non-positive user ID; api must be running
// and authorized as the channel owner.
func (p *ChannelBotProvider) ChannelBots(ctx context.Context, userID int64, api *tg.Client) ([]tg.InputUserClass, error) {
	if p == nil || p.queries == nil || userID <= 0 || api == nil {
		return nil, bots.ErrInvalidInput
	}
	rows, err := p.queries.ListEnabledBots(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list enabled bots: %w", err)
	}
	users := make([]tg.InputUserClass, 0, len(rows))
	for _, row := range rows {
		if !row.Username.Valid || strings.TrimSpace(row.Username.String) == "" {
			continue
		}
		resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(row.Username.String, "@")})
		if err != nil {
			return nil, fmt.Errorf("resolve bot %d: %w", row.BotID, err)
		}
		found := false
		for _, item := range resolved.Users {
			user, ok := item.(*tg.User)
			if !ok || user.ID != row.BotID || !user.Bot {
				continue
			}
			users = append(users, user.AsInput())
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("resolve bot %d: %w", row.BotID, bots.ErrNotFound)
		}
	}
	return users, nil
}

var (
	_ bots.Verifier             = (*GotdVerifier)(nil)
	_ telegramstore.BotProvider = (*ChannelBotProvider)(nil)
)
