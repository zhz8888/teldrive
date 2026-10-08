// Package logingateway implements the Telegram login flows on top of gotd.
//
// The flows are stateless on the server: each step receives an opaque state
// blob produced by the previous step, rebuilds an in-memory gotd session from
// it, performs one round trip to Telegram, and returns a new state blob. A
// half-finished login therefore consumes no storage and can be resumed by any
// instance, and the state is only as trustworthy as the transport carrying it.
//
// Two flows are supported: a phone code sent by Telegram, and a QR token that the
// user confirms from an already signed-in device. Both may end at a two-step
// verification password prompt.
package logingateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	"github.com/zhz8888/teldrive/v2/internal/telethonsession"
)

// loginState is the opaque state a login step hands back to the caller and
// expects to receive again on the next step.
//
// It carries the partially authorised gotd session so the flow can continue
// without server-side storage. Exactly one of the flow markers is set: a phone
// code hash for the SMS flow, or the QR URL and its expiry for the QR flow.
type loginState struct {
	// Session is the gotd session blob, base64url-encoded. It holds the auth key
	// negotiated so far, which is what makes the login resumable.
	Session string `json:"session"`

	// PhoneCodeHash is the identifier Telegram returned for the sent login code,
	// required to verify it. Empty in the QR flow.
	PhoneCodeHash string `json:"phone_code_hash,omitempty"`

	// QRURL is the tg://login link the client renders as a QR code. Empty in the
	// SMS flow.
	QRURL string `json:"qr_url,omitempty"`

	// QRExpiresAt is when QRURL stops being accepted; the client should request a
	// new one after this instant.
	QRExpiresAt time.Time `json:"qr_expires_at"`
}

// qrExportResult is the internal outcome of one QR export call, before it is
// turned into a client-visible login step.
type qrExportResult struct {
	// User is set when the token was accepted, meaning the login is complete.
	User *tg.User

	// URL is the fresh QR link to render when the token is still pending.
	URL string

	// ExpiresAt is when URL stops being accepted.
	ExpiresAt time.Time

	// PasswordRequired reports that Telegram accepted the token but the account
	// has a two-step verification password, so the caller must call VerifyPassword.
	PasswordRequired bool
}

// GotdTelegramLogin implements authn.TelegramLogin against the real Telegram API.
// It is stateless and safe to share between requests; all per-login data lives in
// the state blobs passed through the interface.
type GotdTelegramLogin struct {
	// factory builds throwaway clients whose session lives in an in-memory store,
	// so a login attempt never touches persistent session storage.
	factory *telegramstore.Factory
}

// New returns a login gateway backed by factory. A nil factory is rejected with
// telegramstore.ErrTelegramConfiguration rather than producing a gateway that
// would fail on the first request.
func New(factory *telegramstore.Factory) (*GotdTelegramLogin, error) {
	if factory == nil {
		return nil, telegramstore.ErrTelegramConfiguration
	}
	return &GotdTelegramLogin{factory: factory}, nil
}

// Start sends a login code to phone and returns a state carrying the resulting
// phone code hash.
//
// An account that is already authorised, or one Telegram demands payment for,
// is reported as an error instead of a state: continuing would silently produce
// a step the caller cannot complete. The client is executed for this one round
// trip only, and its session is serialised into the returned state.
func (g *GotdTelegramLogin) Start(ctx context.Context, phone string) (authn.LoginStep, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return authn.LoginStep{}, authn.ErrLoginStateInvalid
	}
	memory := &session.StorageMemory{}
	client, err := g.factory.New(memory)
	if err != nil {
		return authn.LoginStep{}, err
	}
	var codeHash string
	if err := client.Run(ctx, func(runCtx context.Context) error {
		sent, err := client.Auth().SendCode(runCtx, phone, auth.SendCodeOptions{})
		if err != nil {
			return err
		}
		switch value := sent.(type) {
		case *tg.AuthSentCode:
			codeHash = value.PhoneCodeHash
		case *tg.AuthSentCodeSuccess:
			return errors.New("Telegram session is already authorized")
		case *tg.AuthSentCodePaymentRequired:
			return errors.New("Telegram requires payment before sending a login code")
		default:
			return fmt.Errorf("unexpected Telegram send-code response %T", sent)
		}
		return nil
	}); err != nil {
		return authn.LoginStep{}, fmt.Errorf("send Telegram login code: %w", err)
	}
	state, err := encodeLoginState(memory, codeHash)
	if err != nil {
		return authn.LoginStep{}, err
	}
	return authn.LoginStep{State: state}, nil
}

// StartQR begins a QR login and returns the link to render.
//
// When the account is already authorised the step comes back completed rather
// than pending, so a caller that has a valid session does not have to special
// case it. The returned state carries no phone code hash, which is what marks it
// as a QR flow for PollQR.
func (g *GotdTelegramLogin) StartQR(ctx context.Context) (authn.LoginStep, error) {
	memory := &session.StorageMemory{}
	client, err := g.factory.New(memory)
	if err != nil {
		return authn.LoginStep{}, err
	}
	var result qrExportResult
	if err := client.Run(ctx, func(runCtx context.Context) error {
		var exportErr error
		result, exportErr = g.exportQR(runCtx, client)
		return exportErr
	}); err != nil {
		return authn.LoginStep{}, fmt.Errorf("start Telegram QR login: %w", err)
	}
	if result.User != nil {
		return completeLoginStep(memory, result.User)
	}
	state, err := encodeState(memory, loginState{QRURL: result.URL, QRExpiresAt: result.ExpiresAt})
	if err != nil {
		return authn.LoginStep{}, err
	}
	return authn.LoginStep{
		State: state, PasswordRequired: result.PasswordRequired,
		QRURL: result.URL, QRExpiresAt: result.ExpiresAt,
	}, nil
}

// PollQR advances a QR login by re-exporting the token.
//
// A state that carries a phone code hash belongs to the SMS flow and is rejected
// as invalid, so the two flows cannot be crossed. When Telegram hands back a
// fresh link the state is updated and returned; when the user has confirmed on
// their device the step comes back completed with the session.
//
// Polling is cheap but not free: each call performs one round trip, and Telegram
// rate-limits this method, so callers should poll no faster than the QR code's
// own refresh cadence.
func (g *GotdTelegramLogin) PollQR(ctx context.Context, encodedState []byte) (authn.LoginStep, error) {
	state, memory, err := decodeLoginState(encodedState)
	if err != nil || state.PhoneCodeHash != "" {
		return authn.LoginStep{}, authn.ErrLoginStateInvalid
	}
	client, err := g.factory.New(memory)
	if err != nil {
		return authn.LoginStep{}, err
	}
	var result qrExportResult
	if err := client.Run(ctx, func(runCtx context.Context) error {
		var exportErr error
		result, exportErr = g.exportQR(runCtx, client)
		return exportErr
	}); err != nil {
		return authn.LoginStep{}, fmt.Errorf("poll Telegram QR login: %w", err)
	}
	if result.User != nil {
		return completeLoginStep(memory, result.User)
	}
	if result.URL != "" {
		state.QRURL = result.URL
		state.QRExpiresAt = result.ExpiresAt
	}
	next, err := encodeState(memory, state)
	if err != nil {
		return authn.LoginStep{}, err
	}
	return authn.LoginStep{
		State: next, PasswordRequired: result.PasswordRequired,
		QRURL: state.QRURL, QRExpiresAt: state.QRExpiresAt,
	}, nil
}

// exportQR performs one AuthExportLoginToken call and normalises the three
// possible outcomes into qrExportResult: a pending token with a link, a completed
// authorisation with the user, or a DC migration.
//
// A SESSION_PASSWORD_NEEDED response is not an error but a marker that the flow
// moved to the two-step password step, and it is reported through
// PasswordRequired. The migration case exists because Telegram may answer on a
// different data centre than the one the client connected to; the token must then
// be imported there.
func (g *GotdTelegramLogin) exportQR(ctx context.Context, client *telegram.Client) (qrExportResult, error) {
	appID, appHash, ok := g.factory.AppCredentials()
	if !ok || client == nil {
		return qrExportResult{}, telegramstore.ErrTelegramConfiguration
	}
	result, err := client.API().AuthExportLoginToken(ctx, &tg.AuthExportLoginTokenRequest{
		APIID: appID, APIHash: appHash,
	})
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") || errors.Is(err, auth.ErrPasswordAuthNeeded) {
		return qrExportResult{PasswordRequired: true}, nil
	}
	if err != nil {
		return qrExportResult{}, err
	}
	switch value := result.(type) {
	case *tg.AuthLoginToken:
		token := qrlogin.NewToken(value.Token, value.Expires)
		return qrExportResult{URL: token.URL(), ExpiresAt: token.Expires()}, nil
	case *tg.AuthLoginTokenSuccess:
		user, err := authorizationUser(value.Authorization)
		if err != nil {
			return qrExportResult{}, err
		}
		return qrExportResult{User: user}, nil
	case *tg.AuthLoginTokenMigrateTo:
		imported, err := importMigratedQRToken(
			ctx,
			value.DCID,
			value.Token,
			client.MigrateTo,
			client.API().AuthImportLoginToken,
		)
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") || errors.Is(err, auth.ErrPasswordAuthNeeded) {
			return qrExportResult{PasswordRequired: true}, nil
		}
		if err != nil {
			return qrExportResult{}, fmt.Errorf("migrate Telegram QR login to DC %d: %w", value.DCID, err)
		}
		success, ok := imported.(*tg.AuthLoginTokenSuccess)
		if !ok {
			return qrExportResult{}, fmt.Errorf("unexpected Telegram QR import response %T", imported)
		}
		user, err := authorizationUser(success.Authorization)
		if err != nil {
			return qrExportResult{}, err
		}
		return qrExportResult{User: user}, nil
	default:
		return qrExportResult{}, fmt.Errorf("unexpected Telegram QR export response %T", result)
	}
}

// importMigratedQRToken moves the client to the data centre that issued the QR
// token and imports it there.
//
// The migrate and import functions are injected rather than taken from a client
// so the sequence can be tested without a network, and so this helper stays
// independent of gotd's client type. A missing argument is reported as an invalid
// login state, which is the same error a caller sees for a corrupt state blob.
func importMigratedQRToken(
	ctx context.Context,
	dcID int,
	token []byte,
	migrate func(context.Context, int) error,
	importToken func(context.Context, []byte) (tg.AuthLoginTokenClass, error),
) (tg.AuthLoginTokenClass, error) {
	if dcID <= 0 || len(token) == 0 || migrate == nil || importToken == nil {
		return nil, authn.ErrLoginStateInvalid
	}
	if err := migrate(ctx, dcID); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	result, err := importToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	return result, nil
}

// authorizationUser extracts the account from an authorisation result, rejecting
// anything that is not a resolved user. Telegram can answer an authorisation
// request with a "not yet" variant, and treating that as success would hand the
// caller a session with no identity attached.
func authorizationUser(value tg.AuthAuthorizationClass) (*tg.User, error) {
	authorization, ok := value.(*tg.AuthAuthorization)
	if !ok {
		return nil, authn.ErrLoginStateInvalid
	}
	user, ok := authorization.User.AsNotEmpty()
	if !ok || user.ID <= 0 {
		return nil, authn.ErrLoginStateInvalid
	}
	return user, nil
}

// VerifyCode completes the SMS flow by signing in with the code the user received.
//
// Telegram reports a two-step password requirement through the same successful
// sign-in path, so it is translated into authn.ErrPasswordRequired and returned as
// a normal step carrying PasswordRequired, with the phone code hash preserved so
// VerifyPassword can continue. Bad or expired codes become authn.ErrCodeInvalid.
//
// Those two code errors are recognised by matching Telegram's error text, because
// the library exposes no typed error for them; the check is therefore coupled to
// Telegram's wording, not to its API shape.
func (g *GotdTelegramLogin) VerifyCode(ctx context.Context, phone string, encodedState []byte, code string) (authn.LoginStep, error) {
	state, memory, err := decodeLoginState(encodedState)
	if err != nil || state.PhoneCodeHash == "" {
		return authn.LoginStep{}, authn.ErrLoginStateInvalid
	}
	client, err := g.factory.New(memory)
	if err != nil {
		return authn.LoginStep{}, err
	}
	var user *tg.User
	err = client.Run(ctx, func(runCtx context.Context) error {
		if _, err := client.Auth().SignIn(runCtx, phone, strings.TrimSpace(code), state.PhoneCodeHash); err != nil {
			if errors.Is(err, auth.ErrPasswordAuthNeeded) {
				return authn.ErrPasswordRequired
			}
			if strings.Contains(err.Error(), "PHONE_CODE_INVALID") || strings.Contains(err.Error(), "PHONE_CODE_EXPIRED") {
				return authn.ErrCodeInvalid
			}
			return err
		}
		user, err = client.Self(runCtx)
		return err
	})
	if errors.Is(err, authn.ErrPasswordRequired) {
		next, encodeErr := encodeLoginState(memory, state.PhoneCodeHash)
		if encodeErr != nil {
			return authn.LoginStep{}, encodeErr
		}
		return authn.LoginStep{State: next, PasswordRequired: true}, nil
	}
	if err != nil {
		return authn.LoginStep{}, fmt.Errorf("verify Telegram login code: %w", err)
	}
	return completeLoginStep(memory, user)
}

// VerifyPassword completes the two-step verification step for a flow that reached
// PasswordRequired.
//
// The password is forwarded to Telegram and never stored or cached; a wrong
// password becomes authn.ErrPasswordInvalid. Like VerifyCode, that mapping relies
// on matching Telegram's error text.
func (g *GotdTelegramLogin) VerifyPassword(ctx context.Context, encodedState []byte, password string) (authn.LoginStep, error) {
	_, memory, err := decodeLoginState(encodedState)
	if err != nil {
		return authn.LoginStep{}, err
	}
	client, err := g.factory.New(memory)
	if err != nil {
		return authn.LoginStep{}, err
	}
	var user *tg.User
	err = client.Run(ctx, func(runCtx context.Context) error {
		if _, err := client.Auth().Password(runCtx, password); err != nil {
			// gotd already maps PASSWORD_HASH_INVALID to auth.ErrPasswordInvalid,
			// so the textual code never reaches the old string match; both forms
			// are accepted so a rejected password keeps mapping to 401 even if
			// that conversion changes.
			if errors.Is(err, auth.ErrPasswordInvalid) || tgerr.Is(err, "PASSWORD_HASH_INVALID") {
				return authn.ErrPasswordInvalid
			}
			return err
		}
		user, err = client.Self(runCtx)
		return err
	})
	if err != nil {
		return authn.LoginStep{}, fmt.Errorf("verify Telegram password: %w", err)
	}
	return completeLoginStep(memory, user)
}

// encodeLoginState encodes the SMS-flow state for one phone code hash.
func encodeLoginState(memory *session.StorageMemory, phoneCodeHash string) ([]byte, error) {
	return encodeState(memory, loginState{PhoneCodeHash: phoneCodeHash})
}

// encodeState serialises the in-memory session into state together with the flow
// markers and returns the JSON blob handed to the caller.
//
// The session is base64url-encoded because it is a binary blob and the state may
// travel through places that are not binary safe. The blob contains the auth key
// negotiated with Telegram, so it is a credential: callers must not log it or
// store it anywhere less protected than the session it stands for.
func encodeState(memory *session.StorageMemory, state loginState) ([]byte, error) {
	data, err := memory.Bytes(nil)
	if err != nil {
		return nil, fmt.Errorf("serialize Telegram login session: %w", err)
	}
	state.Session = base64.RawURLEncoding.EncodeToString(data)
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("serialize Telegram login state: %w", err)
	}
	return encoded, nil
}

// decodeLoginState reverses encodeState and restores the in-memory session the
// next step will run on.
//
// Every structural problem — malformed JSON, a missing session, bad base64, or a
// blob the session store refuses — is reported as authn.ErrLoginStateInvalid, so
// a caller cannot tell a tampered state from a truncated one. The store call uses
// a background context on purpose: restoring a session is local work that must not
// be abandoned halfway because the request was cancelled.
func decodeLoginState(encoded []byte) (loginState, *session.StorageMemory, error) {
	var state loginState
	if err := json.Unmarshal(encoded, &state); err != nil || state.Session == "" {
		return loginState{}, nil, authn.ErrLoginStateInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(state.Session)
	if err != nil {
		return loginState{}, nil, authn.ErrLoginStateInvalid
	}
	memory := &session.StorageMemory{}
	if err := memory.StoreSession(context.Background(), data); err != nil {
		return loginState{}, nil, authn.ErrLoginStateInvalid
	}
	return state, memory, nil
}

// completeLoginStep turns an authorised session into a finished login step: the
// account identity plus the session re-encoded as a Telethon StringSession, which
// is the form TelDrive stores for later uploads and downloads.
//
// The display name is assembled from the two name parts and trimmed, so an
// account with only one of them does not end up with a leading or trailing space.
// A user without a positive ID is rejected rather than returned, because a session
// with no identity cannot be stored against anyone.
func completeLoginStep(memory *session.StorageMemory, user *tg.User) (authn.LoginStep, error) {
	if user == nil || user.ID <= 0 {
		return authn.LoginStep{}, authn.ErrLoginStateInvalid
	}
	raw, err := memory.Bytes(nil)
	if err != nil {
		return authn.LoginStep{}, fmt.Errorf("serialize authorized Telegram session: %w", err)
	}
	encoded, err := telethonsession.EncodeGotd(context.Background(), raw)
	if err != nil {
		return authn.LoginStep{}, fmt.Errorf("encode authorized Telegram session as Telethon: %w", err)
	}
	displayName := strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName))
	return authn.LoginStep{
		User:    &authn.TelegramUser{ID: user.ID, DisplayName: displayName, Username: user.Username, Premium: user.Premium},
		Session: []byte(encoded),
	}, nil
}

var _ authn.TelegramLogin = (*GotdTelegramLogin)(nil)
