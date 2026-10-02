package botgateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/session"

	"github.com/tgdrive/teldrive/v2/internal/telethonsession"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
)

// botSessionStorage adapts one bot row to gotd's session.Storage interface: it
// keeps the session as a Telethon StringSession encrypted under the
// "bot-session" purpose, so a stored session stays portable and readable
// without the gotd version that wrote it. One value is bound to a single user
// and bot, and its in-memory copy is unsynchronized, so it must not be driven
// from several goroutines at once.
type botSessionStorage struct {
	// queries persists the refreshed session on the bot row.
	queries *sqlcgen.Queries
	// cipher seals and opens sessions under the "bot-session" purpose.
	cipher *secureblob.Cipher
	// userID scopes the update to the owning TelDrive user.
	userID int64
	// botID is the Telegram bot ID whose row is updated.
	botID int64
	// stored is the encrypted session as loaded from the row. It is replaced
	// after a successful store so a later LoadSession returns the newest value.
	stored []byte
}

// HasSession reports whether the bot row carried a non-empty session. It is
// safe on a nil receiver and reports false there, which makes gotd treat the
// bot as not yet authorized.
func (s *botSessionStorage) HasSession() bool {
	return s != nil && len(s.stored) > 0
}

// LoadSession decrypts the stored session and converts the Telethon
// StringSession into the gotd blob the client expects. It returns
// session.ErrNotFound when the row has no session, and a wrapped error when
// decryption or conversion fails, for example because the stored value was
// sealed under another purpose or names an unsupported data centre.
func (s *botSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if !s.HasSession() {
		return nil, session.ErrNotFound
	}
	plain, err := s.cipher.Open("bot-session", s.stored)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot session: %w", err)
	}
	return telethonsession.DecodeToGotd(ctx, string(plain))
}

// StoreSession converts raw gotd session bytes into a Telethon StringSession,
// encrypts it and writes it to the bot row in one UPDATE. It reports an error
// when the receiver is incomplete or raw is empty, when encoding or encryption
// fails, and when the UPDATE matches no row, which means the bot was deleted
// while it was in use. On success the in-memory copy is refreshed so a later
// LoadSession sees the stored session.
func (s *botSessionStorage) StoreSession(ctx context.Context, raw []byte) error {
	if s == nil || s.queries == nil || s.cipher == nil || s.userID <= 0 || s.botID <= 0 || len(raw) == 0 {
		return errors.New("invalid bot session storage")
	}
	encoded, err := telethonsession.EncodeGotd(ctx, raw)
	if err != nil {
		return err
	}
	ciphertext, err := s.cipher.Seal("bot-session", []byte(encoded))
	if err != nil {
		return fmt.Errorf("encrypt bot session: %w", err)
	}
	count, err := s.queries.UpdateBotSession(ctx, sqlcgen.UpdateBotSessionParams{
		Session: ciphertext,
		UserID:  s.userID,
		BotID:   s.botID,
	})
	if err != nil {
		return fmt.Errorf("persist bot session: %w", err)
	}
	if count == 0 {
		return errors.New("bot session row no longer exists")
	}
	s.stored = append(s.stored[:0], ciphertext...)
	return nil
}
