package app

import (
	"context"
	"errors"

	"github.com/tgdrive/teldrive/v2/internal/authn"
	"github.com/tgdrive/teldrive/v2/internal/bots"
)

// ErrLocalTelegramLoginUnavailable reports that interactive Telegram login was
// requested while the filesystem backend is configured. That backend emulates
// storage only and has no MTProto account to authorize, so every localTelegramLogin
// method returns this sentinel; callers must test it with errors.Is and surface it
// as a permanent configuration error rather than retrying.
var ErrLocalTelegramLoginUnavailable = errors.New("Telegram login is unavailable with the filesystem backend")

// localTelegramLogin is the authn.TelegramLogin implementation used with the
// filesystem backend. It is stateless and exists only to reject login attempts
// with ErrLocalTelegramLoginUnavailable instead of leaving the dependency nil,
// which would turn a clear configuration error into a panic.
type localTelegramLogin struct{}

// Start always fails with ErrLocalTelegramLoginUnavailable, including for a blank
// phone number, because no login flow can be started on this backend.
func (localTelegramLogin) Start(context.Context, string) (authn.LoginStep, error) {
	return authn.LoginStep{}, ErrLocalTelegramLoginUnavailable
}

// StartQR always fails with ErrLocalTelegramLoginUnavailable; the filesystem
// backend never issues a QR login link.
func (localTelegramLogin) StartQR(context.Context) (authn.LoginStep, error) {
	return authn.LoginStep{}, ErrLocalTelegramLoginUnavailable
}

// PollQR always fails with ErrLocalTelegramLoginUnavailable, so a QR login state
// minted by another backend is rejected rather than silently advanced.
func (localTelegramLogin) PollQR(context.Context, []byte) (authn.LoginStep, error) {
	return authn.LoginStep{}, ErrLocalTelegramLoginUnavailable
}

// VerifyCode always fails with ErrLocalTelegramLoginUnavailable; no login code can
// exist on this backend because Start never succeeds.
func (localTelegramLogin) VerifyCode(context.Context, string, []byte, string) (authn.LoginStep, error) {
	return authn.LoginStep{}, ErrLocalTelegramLoginUnavailable
}

// VerifyPassword always fails with ErrLocalTelegramLoginUnavailable, so a
// two-step password cannot unlock a session that was never authorized.
func (localTelegramLogin) VerifyPassword(context.Context, []byte, string) (authn.LoginStep, error) {
	return authn.LoginStep{}, ErrLocalTelegramLoginUnavailable
}

// localBotVerifier is the bots.Verifier used with the filesystem backend. The
// emulated storage has no bot accounts, so it rejects every credential.
type localBotVerifier struct{}

// Verify always reports bots.ErrNotBot, whatever init data is supplied: with the
// filesystem backend there is no bot token to validate against, and treating a
// credential as valid would let any caller provision a bot whose session can
// never work.
func (localBotVerifier) Verify(context.Context, string) (bots.Identity, error) {
	return bots.Identity{}, bots.ErrNotBot
}

var _ authn.TelegramLogin = localTelegramLogin{}
var _ bots.Verifier = localBotVerifier{}
