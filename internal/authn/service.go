// Package authn authenticates callers and issues the credentials TelDrive
// accepts.
//
// Telegram logins run as short-lived flows persisted in PostgreSQL. StartLogin
// or StartQR mints a flow, the client then submits the code with VerifyCode or
// polls the QR challenge with PollQR until the phone approves on Telegram, and
// an account with a two-step password must pass VerifyPassword before the flow
// completes and hands out a session. A session is later proven either by a
// signed access token (AuthenticateBearer) or by an API key
// (AuthenticateAPIKey); both resolve to a principal.Identity.
//
// Work on a single flow is serialized by a PostgreSQL advisory lock keyed by
// the flow ID, so concurrent requests, including ones served by other
// instances, cannot interleave their Telegram calls. Session and API key
// secrets are stored only as digests, and every rejection is reported as a
// sentinel error that callers must match with errors.Is.
package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
)

var (
	// ErrInvalidInput reports malformed caller input such as a blank phone
	// number, code or password, a non-positive user ID, or an unsupported role.
	// It maps to HTTP 422 and never describes a rejected credential.
	ErrInvalidInput = errors.New("invalid authentication input")

	// ErrFlowNotFound reports a Telegram login flow that is unknown, expired,
	// already completed, or deleted while the request was in flight. Those
	// cases are deliberately indistinguishable so callers cannot probe which
	// flows exist; match it with errors.Is.
	ErrFlowNotFound = errors.New("Telegram login flow not found or expired")

	// ErrInvalidCredential reports a credential that is absent, malformed,
	// expired, revoked, or otherwise unusable. Bearer, refresh and API key
	// authentication all collapse their rejection reasons into this value, so
	// callers must match it with errors.Is rather than inspect the cause.
	ErrInvalidCredential = errors.New("invalid authentication credential")

	// ErrUserNotAllowed reports a Telegram account that signed in successfully
	// but is missing from Config.AllowedUsers. It maps to HTTP 401 so the
	// response reveals nothing about the allowlist contents.
	ErrUserNotAllowed = errors.New("Telegram user is not allowed")

	// ErrSessionNotFound reports a session that is unknown, owned by another
	// user, or already revoked. It maps to HTTP 404.
	ErrSessionNotFound = errors.New("session not found")

	// ErrUserNotFound reports an account that does not exist. It maps to HTTP
	// 404 and is deliberately distinct from ErrSessionNotFound, so a missing
	// account is never reported as a missing session; the user-administration
	// methods return it instead of reusing the session sentinel.
	ErrUserNotFound = errors.New("user not found")

	// ErrOwnerProtected reports an account that holds the owner role, which the
	// user-administration statements refuse to modify so a deployment cannot
	// demote or disable its last administrator. It is returned instead of
	// ErrUserNotFound, so "not allowed" is never reported as "does not exist".
	ErrOwnerProtected = errors.New("owner account is protected")

	// ErrAPIKeyNotFound reports an API key that is unknown, owned by another
	// user, or already revoked. It maps to HTTP 404.
	ErrAPIKeyNotFound = errors.New("API key not found")
)

// Config carries the settings a Service is built from. The values are copied at
// construction and never reloaded, so a running process keeps the key and the
// lifetimes it started with.
type Config struct {
	// SigningKey is the secret used to sign and verify access tokens. NewService
	// requires at least 32 bytes; replacing it invalidates every access token in
	// circulation while refresh tokens, which are database-backed, keep working.
	SigningKey string
	// Issuer is embedded in each access token and required again during bearer
	// authentication, so tokens minted by another deployment are rejected.
	Issuer string
	// AllowedUsers restricts the Telegram usernames allowed to complete a login.
	// NewService normalizes and deduplicates it, and an empty list permits every
	// account.
	AllowedUsers []string
	// AccessTokenTTL is how long an issued access token stays valid. It must be
	// positive and is reported to clients in seconds.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL is the session lifetime: it bounds both the refresh token
	// and the sessions row the token belongs to, and must be positive.
	RefreshTokenTTL time.Duration
	// LoginFlowTTL is how long a Telegram login flow may be polled before it is
	// reported as not found, and must be positive.
	LoginFlowTTL time.Duration
}

// Service authenticates callers and manages their sessions and API keys.
//
// It is safe for concurrent use: apart from the test seams noted below the
// struct is immutable once built, and coordination that spans requests is
// delegated to PostgreSQL, so several instances may share one database.
type Service struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
	// cipher seals the Telegram session, phone number and login state before
	// storage, binding each value to its column so a ciphertext moved elsewhere
	// fails to open.
	cipher *secureblob.Cipher
	// login is the Telegram gateway. Production wires the gotd implementation;
	// a deployment without Telegram credentials wires a stub that fails every
	// call.
	login  TelegramLogin
	config Config
	// random is the entropy source for opaque tokens. It is crypto/rand in
	// production and only injectable so tests can pin token values.
	random io.Reader
	// now supplies the clock used for every expiry decision, so tests can move
	// time without sleeping.
	now func() time.Time
}

// FlowResult describes a phone login flow that has been started and is waiting
// for the code Telegram sent to the user.
type FlowResult struct {
	// ID is the opaque flow identifier to pass back to VerifyCode or
	// VerifyPassword. It is meaningful only while the flow is unexpired and
	// unfinished.
	ID uuid.UUID
	// ExpiresAt is the UTC instant after which the flow is reported as not
	// found. It is the stored flow deadline, not the lifetime of the login code
	// Telegram sent.
	ExpiresAt time.Time
	// PasswordRequired reports whether the account's two-step password is still
	// needed. While true the client must call VerifyPassword and must not start
	// the flow over.
	PasswordRequired bool
}

// QRFlowResult describes a QR login flow that is waiting for the phone to scan
// and approve the code.
type QRFlowResult struct {
	// ID is the opaque flow identifier to pass back to PollQR.
	ID uuid.UUID
	// ExpiresAt is the UTC instant after which the flow is reported as not found
	// and polling must stop.
	ExpiresAt time.Time
	// QRURL is the tg:// link to render as a QR code. It is empty once the flow
	// reached the two-step password prompt.
	QRURL string
	// QRExpiresAt is when Telegram stops accepting the QR token, after which a
	// poll returns a fresh link and expiry. It is the zero time together with an
	// empty QRURL once the password prompt was reached.
	QRExpiresAt time.Time
	// PasswordRequired reports that the phone approved the QR code and the
	// account's two-step password is now required. PollQR stops contacting
	// Telegram while this is set.
	PasswordRequired bool
}

// TokenPair is the credential set issued by a completed login or a token
// refresh: a short-lived access token plus the long-lived refresh token that
// replaces it.
type TokenPair struct {
	// AccessToken is the signed bearer token to send on API requests.
	AccessToken string
	// RefreshToken is the opaque secret the client stores. It is rotated on every
	// refresh, so the value that was just used becomes unusable and must be
	// discarded.
	RefreshToken string
	// ExpiresIn is the access token lifetime in seconds, as reported to clients
	// for cookie max-age and refresh scheduling.
	ExpiresIn int32
}

// AccessRenewal is the result of renewing an access token without rotating the
// refresh token.
//
// It exists for the browser session middleware, which renews silently on
// ordinary requests; rotating the single-use refresh token there would make
// concurrent requests invalidate each other.
type AccessRenewal struct {
	// AccessToken is the replacement signed bearer token.
	AccessToken string
	// ExpiresIn is the replacement token's lifetime in seconds.
	ExpiresIn int32
}

// VerifyResult is the outcome of advancing a login flow. Exactly one field is
// set: Flow or QRFlow while the login still needs input, and Tokens once the
// session was created.
type VerifyResult struct {
	// Flow carries the pending phone flow when the two-step password is
	// required.
	Flow *FlowResult
	// QRFlow carries the pending QR flow on every poll that has not completed.
	QRFlow *QRFlowResult
	// Tokens holds the freshly issued credentials when the login completed. It is
	// nil while the flow is still pending.
	Tokens *TokenPair
}

// APIKeyCreated is a newly minted API key.
//
// The plaintext secret is returned exactly once, here: only a digest is stored,
// so it can never be retrieved again.
type APIKeyCreated struct {
	// Row is the stored key metadata, including the short non-secret prefix used
	// to recognize the key in listings.
	Row *sqlcgen.ApiKey
	// Secret is the full plaintext API key. It must be shown to the user
	// immediately because it cannot be recovered.
	Secret string
}

// ListAPIKeysInput selects one page of API keys for a single user, newest first.
type ListAPIKeysInput struct {
	// UserID is the owner whose keys are listed; it must be positive.
	UserID int64
	// AfterCreatedAt is the exclusive upper bound of the page, copied from the
	// last row of the previous page. Both cursor fields must be set together;
	// leaving this nil returns the first page and makes AfterID irrelevant.
	AfterCreatedAt *time.Time
	// AfterID breaks ties between keys created at the same instant as
	// AfterCreatedAt. Setting it without AfterCreatedAt selects no rows.
	AfterID *uuid.UUID
	// Limit is the maximum number of rows to return; values at or below zero
	// default to 100 and values above 200 are clamped to 200.
	Limit int32
}

// ListSessionsInput selects one page of active sessions for a single user,
// newest first.
type ListSessionsInput struct {
	// UserID is the owner whose sessions are listed; it must be positive.
	UserID int64
	// AfterCreatedAt is the exclusive upper bound of the page, copied from the
	// last row of the previous page. Both cursor fields must be set together;
	// leaving this nil returns the first page and makes AfterID irrelevant.
	AfterCreatedAt *time.Time
	// AfterID breaks ties between sessions created at the same instant as
	// AfterCreatedAt. Setting it without AfterCreatedAt selects no rows.
	AfterID *uuid.UUID
	// Limit is the maximum number of rows to return; values at or below zero
	// default to 100 and values above 200 are clamped to 200.
	Limit int32
}

// accessClaims is the verified payload of an access token. Only the session and
// the role snapshot travel besides the standard registered claims.
type accessClaims struct {
	// SessionID is the session the token was issued for. Bearer authentication
	// requires that session to still be active, which is what lets revocation
	// take effect before the token expires.
	SessionID uuid.UUID `json:"sid"`
	// Roles is the role snapshot taken when the token was signed; a later role
	// change only applies once the token is replaced.
	Roles []string `json:"roles,omitempty"`
	// RegisteredClaims supplies the issuer, subject, issued-at and expiry claims
	// that the parser enforces.
	jwt.RegisteredClaims
}

// RefreshTokenTTL returns the configured session lifetime so callers can give
// the refresh cookie the same expiry as the session row it belongs to. A nil
// receiver reports zero.
func (s *Service) RefreshTokenTTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.config.RefreshTokenTTL
}

// NewService builds the authentication service from its database, cipher and
// Telegram gateway dependencies.
//
// The configuration is validated here and never reloaded: the signing key must
// be at least 32 bytes, the issuer and all three lifetimes must be set, and
// every dependency must be non-nil. AllowedUsers is normalized to lowercase
// names without a leading "@" and deduplicated. An unusable configuration
// reports ErrInvalidInput and no service, so a process either starts with
// working authentication settings or does not start at all.
func NewService(pool *pgxpool.Pool, cipher *secureblob.Cipher, login TelegramLogin, cfg Config) (*Service, error) {
	if pool == nil || cipher == nil || login == nil || len(cfg.SigningKey) < 32 || strings.TrimSpace(cfg.Issuer) == "" || cfg.AccessTokenTTL <= 0 || cfg.RefreshTokenTTL <= 0 || cfg.LoginFlowTTL <= 0 {
		return nil, ErrInvalidInput
	}
	cfg.AllowedUsers = normalizeAllowedUsers(cfg.AllowedUsers)
	return &Service{
		pool: pool, queries: sqlcgen.New(pool), cipher: cipher, login: login, config: cfg,
		random: rand.Reader, now: time.Now,
	}, nil
}

// StartLogin begins a phone login by asking the Telegram gateway to send a code
// to phone, and persists the flow the client later continues with VerifyCode.
//
// The phone number and the gateway state are sealed before they reach the
// database, and the flow expires after Config.LoginFlowTTL. A blank phone number
// reports ErrInvalidInput; gateway failures are returned unchanged instead of
// being folded into a sentinel.
func (s *Service) StartLogin(ctx context.Context, phone string) (*FlowResult, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil, ErrInvalidInput
	}
	step, err := s.login.Start(ctx, phone)
	if err != nil {
		return nil, err
	}
	phoneCiphertext, err := s.cipher.Seal("login-phone", []byte(phone))
	if err != nil {
		return nil, err
	}
	stateCiphertext, err := s.cipher.Seal("login-state", step.State)
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	expires := s.now().UTC().Add(s.config.LoginFlowTTL)
	row, err := s.queries.CreateTelegramLoginFlow(ctx, sqlcgen.CreateTelegramLoginFlowParams{
		ID: dbtypes.UUID(id), Method: sqlcgen.TelegramLoginMethodPhone,
		PhoneNumberCiphertext: phoneCiphertext, TelegramStateCiphertext: stateCiphertext,
		PasswordRequired: step.PasswordRequired, ExpiresAt: dbtypes.Time(expires),
	})
	if err != nil {
		return nil, fmt.Errorf("create Telegram login flow: %w", err)
	}
	return &FlowResult{ID: id, ExpiresAt: row.ExpiresAt.Time, PasswordRequired: row.PasswordRequired}, nil
}

// StartQR begins a QR login and returns the link to render as a QR code, along
// with the flow ID that PollQR expects.
//
// A gateway response that already carries an authorized user, holds no
// resumable state, or (unless a password is pending) carries no QR link is
// rejected as ErrLoginStateInvalid rather than completed here, so an approved
// login always goes through PollQR. The flow expires after Config.LoginFlowTTL.
func (s *Service) StartQR(ctx context.Context) (*QRFlowResult, error) {
	step, err := s.login.StartQR(ctx)
	if err != nil {
		return nil, err
	}
	if step.User != nil || len(step.State) == 0 || (!step.PasswordRequired && strings.TrimSpace(step.QRURL) == "") {
		return nil, ErrLoginStateInvalid
	}
	stateCiphertext, err := s.cipher.Seal("login-state", step.State)
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	expires := s.now().UTC().Add(s.config.LoginFlowTTL)
	row, err := s.queries.CreateTelegramLoginFlow(ctx, sqlcgen.CreateTelegramLoginFlowParams{
		ID: dbtypes.UUID(id), Method: sqlcgen.TelegramLoginMethodQr,
		TelegramStateCiphertext: stateCiphertext, PasswordRequired: step.PasswordRequired,
		ExpiresAt: dbtypes.Time(expires),
	})
	if err != nil {
		return nil, fmt.Errorf("create Telegram QR login flow: %w", err)
	}
	return &QRFlowResult{
		ID: id, ExpiresAt: row.ExpiresAt.Time, QRURL: step.QRURL,
		QRExpiresAt: step.QRExpiresAt, PasswordRequired: row.PasswordRequired,
	}, nil
}

// PollQR advances a QR login flow: the client calls it repeatedly until the
// phone approves the QR code and the result carries Tokens.
//
// While the code is still pending, the result repeats the current QR link and
// its expiry. Once the phone approves an account that has a two-step password,
// the flow is pinned to PasswordRequired and later polls return immediately
// without contacting Telegram, because every export would mint a new login token
// and overwrite the state VerifyPassword has to resume. The advisory lock taken
// by withFlowLock keeps that pin from being raced by a concurrent poll.
//
// A nil flow ID, or a flow that is not a QR login, reports ErrInvalidInput; an
// unknown, expired or already completed flow reports ErrFlowNotFound. Both must
// be matched with errors.Is.
func (s *Service) PollQR(ctx context.Context, flowID uuid.UUID) (*VerifyResult, error) {
	if flowID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	return s.withFlowLock(ctx, flowID, func(conn *pgxpool.Conn, flow *sqlcgen.TelegramLoginFlow) (*VerifyResult, error) {
		if flow.Method != sqlcgen.TelegramLoginMethodQr {
			return nil, ErrInvalidInput
		}
		// A flow that already reached the two-step password prompt must not be
		// polled again. Every auth.exportLoginToken call mints a fresh login
		// token, and only the token the phone actually scanned carries the
		// pending-password state, so one more export reports
		// PasswordRequired=false and overwrites both the flag and the session
		// state that checkPassword has to resume. Polls are serialized per flow
		// by the advisory lock, so the flag read here cannot be rolled back by a
		// concurrent poll afterwards either.
		if flow.PasswordRequired {
			id, _ := dbtypes.GoogleUUID(flow.ID)
			return &VerifyResult{QRFlow: &QRFlowResult{
				ID: id, ExpiresAt: flow.ExpiresAt.Time, PasswordRequired: true,
			}}, nil
		}
		state, err := s.cipher.Open("login-state", flow.TelegramStateCiphertext)
		if err != nil {
			return nil, err
		}
		step, err := s.login.PollQR(ctx, state)
		if err != nil {
			return nil, err
		}
		if step.User != nil {
			return s.completeLogin(ctx, conn, flowID, step)
		}
		if len(step.State) == 0 || (!step.PasswordRequired && strings.TrimSpace(step.QRURL) == "") {
			return nil, ErrLoginStateInvalid
		}
		ciphertext, err := s.cipher.Seal("login-state", step.State)
		if err != nil {
			return nil, err
		}
		updated, err := sqlcgen.New(conn).UpdateTelegramLoginFlowState(ctx, sqlcgen.UpdateTelegramLoginFlowStateParams{
			TelegramStateCiphertext: ciphertext, PasswordRequired: step.PasswordRequired, ID: flow.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFlowNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("update Telegram QR login flow: %w", err)
		}
		id, _ := dbtypes.GoogleUUID(updated.ID)
		return &VerifyResult{QRFlow: &QRFlowResult{
			ID: id, ExpiresAt: updated.ExpiresAt.Time, QRURL: step.QRURL,
			QRExpiresAt: step.QRExpiresAt, PasswordRequired: updated.PasswordRequired,
		}}, nil
	})
}

// VerifyCode continues a phone login flow with the code Telegram sent the user.
// It returns Flow with PasswordRequired set when the account also has a two-step
// password, or Tokens when the login completed.
//
// The flow must be a phone flow whose sealed state still decrypts, otherwise
// ErrInvalidInput or ErrLoginStateInvalid is returned. Unknown, expired and
// already completed flows report ErrFlowNotFound. A wrong or expired code is
// reported by the gateway as ErrCodeInvalid; failed attempts are neither counted
// nor throttled by this service.
func (s *Service) VerifyCode(ctx context.Context, flowID uuid.UUID, code string) (*VerifyResult, error) {
	if flowID == uuid.Nil || strings.TrimSpace(code) == "" {
		return nil, ErrInvalidInput
	}
	return s.withFlowLock(ctx, flowID, func(conn *pgxpool.Conn, flow *sqlcgen.TelegramLoginFlow) (*VerifyResult, error) {
		if flow.Method != sqlcgen.TelegramLoginMethodPhone {
			return nil, ErrInvalidInput
		}
		phone, state, err := s.decryptFlow(flow)
		if err != nil || phone == "" {
			return nil, ErrLoginStateInvalid
		}
		step, err := s.login.VerifyCode(ctx, phone, state, code)
		if err != nil {
			return nil, err
		}
		if step.PasswordRequired {
			return s.persistPendingFlow(ctx, conn, flow, step)
		}
		return s.completeLogin(ctx, conn, flowID, step)
	})
}

// VerifyPassword completes a flow that stopped at the two-step password prompt
// and returns the issued token pair.
//
// The flow must already have PasswordRequired set, otherwise ErrInvalidInput is
// returned. The password is checked by Telegram against the account rather than
// against anything stored here, so this service cannot enforce local lockouts or
// count failed attempts; a rejected password is reported as ErrPasswordInvalid,
// which maps to HTTP 401. Success marks the flow completed in the same
// transaction that creates the session, so a flow can never be completed twice.
func (s *Service) VerifyPassword(ctx context.Context, flowID uuid.UUID, password string) (*VerifyResult, error) {
	if flowID == uuid.Nil || password == "" {
		return nil, ErrInvalidInput
	}
	return s.withFlowLock(ctx, flowID, func(conn *pgxpool.Conn, flow *sqlcgen.TelegramLoginFlow) (*VerifyResult, error) {
		if !flow.PasswordRequired {
			return nil, ErrInvalidInput
		}
		_, state, err := s.decryptFlow(flow)
		if err != nil {
			return nil, err
		}
		step, err := s.login.VerifyPassword(ctx, state, password)
		if err != nil {
			return nil, err
		}
		return s.completeLogin(ctx, conn, flowID, step)
	})
}

// withFlowLock runs fn while holding the PostgreSQL advisory lock that
// serializes work on one login flow, passing fn the pooled connection and the
// freshly loaded flow row.
//
// The lock is session-scoped and keyed by the flow ID, so it also excludes other
// service instances sharing the database. The flow is read only after the lock
// is taken and must still be unexpired and unfinished, otherwise ErrFlowNotFound
// is returned before fn runs. One pooled connection is held for the whole call,
// including the Telegram round trip; acquisition blocks without a timeout, and
// the release runs on a background context with a five second deadline so a
// cancelled request still unlocks.
func (s *Service) withFlowLock(ctx context.Context, flowID uuid.UUID, fn func(*pgxpool.Conn, *sqlcgen.TelegramLoginFlow) (*VerifyResult, error)) (result *VerifyResult, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire login flow connection: %w", err)
	}
	defer conn.Release()
	lockID := flowLockID(flowID)
	queries := sqlcgen.New(conn)
	if err := queries.AcquireAdvisoryLock(ctx, lockID); err != nil {
		return nil, fmt.Errorf("lock login flow: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := queries.ReleaseAdvisoryLock(unlockCtx, lockID)
		if unlockErr != nil && err == nil {
			err = fmt.Errorf("unlock login flow: %w", unlockErr)
		}
	}()
	flow, err := queries.GetTelegramLoginFlow(ctx, dbtypes.UUID(flowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFlowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get Telegram login flow: %w", err)
	}
	return fn(conn, flow)
}

// decryptFlow opens the sealed state of a stored flow and, for phone flows, the
// phone number as well.
//
// The phone number is empty for QR flows. A nil flow or a missing phone
// ciphertext reports ErrLoginStateInvalid, while a value that fails to decrypt is
// returned as secureblob.ErrInvalidCiphertext, so callers can tell "this flow
// never carried that field" from "this flow cannot be resumed".
func (s *Service) decryptFlow(flow *sqlcgen.TelegramLoginFlow) (string, []byte, error) {
	if flow == nil {
		return "", nil, ErrLoginStateInvalid
	}
	var phone string
	if flow.Method == sqlcgen.TelegramLoginMethodPhone {
		if len(flow.PhoneNumberCiphertext) == 0 {
			return "", nil, ErrLoginStateInvalid
		}
		plain, err := s.cipher.Open("login-phone", flow.PhoneNumberCiphertext)
		if err != nil {
			return "", nil, err
		}
		phone = string(plain)
	}
	state, err := s.cipher.Open("login-state", flow.TelegramStateCiphertext)
	if err != nil {
		return "", nil, err
	}
	return phone, state, nil
}

// persistPendingFlow records that a phone flow has reached the two-step password
// prompt: the gateway state needed to resume is resealed and password_required
// is set to true.
//
// It must run inside withFlowLock and reuse that lock's connection so the update
// belongs to the same serialized section. A flow that expired or completed in the
// meantime reports ErrFlowNotFound.
func (s *Service) persistPendingFlow(ctx context.Context, conn *pgxpool.Conn, flow *sqlcgen.TelegramLoginFlow, step LoginStep) (*VerifyResult, error) {
	ciphertext, err := s.cipher.Seal("login-state", step.State)
	if err != nil {
		return nil, err
	}
	updated, err := sqlcgen.New(conn).UpdateTelegramLoginFlowState(ctx, sqlcgen.UpdateTelegramLoginFlowStateParams{
		TelegramStateCiphertext: ciphertext, PasswordRequired: true, ID: flow.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFlowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update Telegram login flow: %w", err)
	}
	id, _ := dbtypes.GoogleUUID(updated.ID)
	return &VerifyResult{Flow: &FlowResult{ID: id, ExpiresAt: updated.ExpiresAt.Time, PasswordRequired: true}}, nil
}

// completeLogin finishes an approved login: it stores the Telegram session,
// creates the TelDrive session, marks the flow completed and mints a token pair.
//
// The user row is upserted under a transaction-scoped advisory lock, so the very
// first account to register becomes the owner exactly once even when two logins
// race. Every write, and the role lookup behind the access token, happens inside
// one transaction, and the token is minted before that transaction commits: a
// failure rolls the whole login back, so a flow is never left completed with no
// usable session, and the caller can retry it. A missing user or Telegram
// session reports ErrLoginStateInvalid, and an account outside
// Config.AllowedUsers reports ErrUserNotAllowed after Telegram already accepted
// it.
func (s *Service) completeLogin(ctx context.Context, conn *pgxpool.Conn, flowID uuid.UUID, step LoginStep) (*VerifyResult, error) {
	if step.User == nil || step.User.ID <= 0 || len(step.Session) == 0 {
		return nil, ErrLoginStateInvalid
	}
	if !s.userAllowed(step.User.Username) {
		return nil, ErrUserNotAllowed
	}
	telegramSession, err := s.cipher.Seal("telegram-session", step.Session)
	if err != nil {
		return nil, err
	}
	refreshToken, refreshHash, err := s.newOpaqueToken("tdr_")
	if err != nil {
		return nil, err
	}
	sessionID := uuid.New()
	expires := s.now().UTC().Add(s.config.RefreshTokenTTL)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin login transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)

	if err := q.AcquireUserBootstrapLock(ctx); err != nil {
		return nil, fmt.Errorf("lock user bootstrap: %w", err)
	}
	if _, err := q.UpsertUser(ctx, sqlcgen.UpsertUserParams{
		UserID: step.User.ID, DisplayName: dbtypes.OptionalText(nonEmpty(step.User.DisplayName)),
		Username: dbtypes.OptionalText(nonEmpty(step.User.Username)), Premium: step.User.Premium,
	}); err != nil {
		return nil, fmt.Errorf("upsert authenticated user: %w", err)
	}
	if _, err := q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID: dbtypes.UUID(sessionID), UserID: step.User.ID,
		TelegramSession: telegramSession, RefreshTokenHash: refreshHash,
		ExpiresAt: dbtypes.Time(expires),
	}); err != nil {
		return nil, fmt.Errorf("create authenticated session: %w", err)
	}
	if _, err := q.CompleteTelegramLoginFlow(ctx, dbtypes.UUID(flowID)); err != nil {
		return nil, fmt.Errorf("complete Telegram login flow: %w", err)
	}
	// Minting runs on the transaction's queries so the role lookup sees the user
	// row this transaction just upserted, and any failure here rolls the flow
	// completion and the new session back together instead of leaving the caller
	// without credentials for an already completed flow.
	access, err := s.issueAccessToken(ctx, q, step.User.ID, sessionID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit authenticated session: %w", err)
	}
	return &VerifyResult{Tokens: &TokenPair{AccessToken: access, RefreshToken: refreshToken, ExpiresIn: ttlSeconds(s.config.AccessTokenTTL)}}, nil
}

// RenewAccess exchanges a refresh token for a new access token without rotating
// the refresh token.
//
// It is meant for the browser middleware, which renews silently on ordinary
// requests; rotating there would make concurrent requests race for the same
// single-use token. The session must still be active, so an unknown, revoked or
// expired token reports ErrInvalidCredential. The session's last-used timestamp
// is then refreshed on a best-effort basis, and a failure to record it does not
// fail the renewal.
func (s *Service) RenewAccess(ctx context.Context, refreshToken string) (*AccessRenewal, error) {
	refreshHash := hashToken(strings.TrimSpace(refreshToken))
	if len(refreshHash) == 0 {
		return nil, ErrInvalidCredential
	}
	sessionRow, err := s.queries.GetSessionByRefreshTokenHash(ctx, refreshHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidCredential
	}
	if err != nil {
		return nil, fmt.Errorf("get refresh session: %w", err)
	}
	sessionID, ok := dbtypes.GoogleUUID(sessionRow.ID)
	if !ok {
		return nil, ErrSessionNotFound
	}
	access, err := s.issueAccessToken(ctx, s.queries, sessionRow.UserID, sessionID)
	if err != nil {
		return nil, err
	}
	_ = s.queries.TouchSession(ctx, sessionRow.ID)
	return &AccessRenewal{AccessToken: access, ExpiresIn: ttlSeconds(s.config.AccessTokenTTL)}, nil
}

// Refresh rotates a refresh token and returns a new token pair.
//
// The rotation is a compare-and-swap on the stored digest: a token that is
// unknown, already rotated, revoked or expired reports ErrInvalidCredential, so
// two concurrent refreshes with the same value cannot both succeed and a replayed
// token is rejected. Callers must replace the refresh token they sent with the
// one returned here.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	oldHash := hashToken(strings.TrimSpace(refreshToken))
	if len(oldHash) == 0 {
		return nil, ErrInvalidCredential
	}
	sessionRow, err := s.queries.GetSessionByRefreshTokenHash(ctx, oldHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidCredential
	}
	if err != nil {
		return nil, fmt.Errorf("get refresh session: %w", err)
	}
	sessionID, ok := dbtypes.GoogleUUID(sessionRow.ID)
	if !ok {
		return nil, ErrSessionNotFound
	}
	newToken, newHash, err := s.newOpaqueToken("tdr_")
	if err != nil {
		return nil, err
	}
	if _, err := s.queries.RotateSessionRefreshToken(ctx, sqlcgen.RotateSessionRefreshTokenParams{
		NewRefreshTokenHash: newHash, SessionID: sessionRow.ID, OldRefreshTokenHash: oldHash,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidCredential
	} else if err != nil {
		return nil, fmt.Errorf("rotate refresh token: %w", err)
	}
	access, err := s.issueAccessToken(ctx, s.queries, sessionRow.UserID, sessionID)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: newToken, ExpiresIn: ttlSeconds(s.config.AccessTokenTTL)}, nil
}

// AuthenticateBearer resolves a signed access token into a principal.Identity
// with Source "bearer".
//
// Parsing pins the signing method, requires the configured issuer and a
// non-expired token, and rejects anything that does not verify against the
// configured key. The token must name a session that is still active for the same
// user, which is how Logout and session revocation take effect before the token
// expires; the session's last-used timestamp is then touched best-effort, and
// roles are re-read from the user row instead of being trusted from the token.
// Every rejection except a database failure reports ErrInvalidCredential.
func (s *Service) AuthenticateBearer(ctx context.Context, raw string) (principal.Identity, error) {
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(strings.TrimSpace(raw), claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrInvalidCredential
		}
		return []byte(s.config.SigningKey), nil
	}, jwt.WithIssuer(s.config.Issuer), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || claims.Subject == "" || claims.SessionID == uuid.Nil {
		return principal.Identity{}, ErrInvalidCredential
	}
	userID, err := parseUserID(claims.Subject)
	if err != nil {
		return principal.Identity{}, ErrInvalidCredential
	}
	if _, err := s.queries.GetActiveSession(ctx, sqlcgen.GetActiveSessionParams{SessionID: dbtypes.UUID(claims.SessionID), UserID: userID}); errors.Is(err, pgx.ErrNoRows) {
		return principal.Identity{}, ErrInvalidCredential
	} else if err != nil {
		return principal.Identity{}, err
	}
	_ = s.queries.TouchSession(ctx, dbtypes.UUID(claims.SessionID))
	roles, err := s.rolesForUser(ctx, s.queries, userID)
	if err != nil {
		return principal.Identity{}, ErrInvalidCredential
	}
	return principal.Identity{UserID: userID, SessionID: claims.SessionID, Roles: roles, Source: "bearer"}, nil
}

// AuthenticateAPIKey resolves an API key into a principal.Identity with Source
// "api_key" and the zero session ID.
//
// The presented secret is hashed and looked up, so the plaintext is never stored
// or compared directly, and a key that is unknown, revoked or expired reports
// ErrInvalidCredential. Every rejection except a database failure reports that
// sentinel, so an outage stays a server error instead of being reported as a bad
// key. The key's last-used timestamp is updated best-effort, and roles are read
// from the owning user row, so disabling or revoking that user takes effect on
// the next request.
func (s *Service) AuthenticateAPIKey(ctx context.Context, raw string) (principal.Identity, error) {
	hash := hashToken(strings.TrimSpace(raw))
	if len(hash) == 0 {
		return principal.Identity{}, ErrInvalidCredential
	}
	row, err := s.queries.GetActiveAPIKeyByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return principal.Identity{}, ErrInvalidCredential
	}
	if err != nil {
		return principal.Identity{}, err
	}
	_ = s.queries.TouchAPIKey(ctx, row.ID)
	roles, err := s.rolesForUser(ctx, s.queries, row.UserID)
	if err != nil {
		return principal.Identity{}, ErrInvalidCredential
	}
	return principal.Identity{UserID: row.UserID, Roles: roles, Source: "api_key"}, nil
}

// GetUser returns the stored account row for userID, which the administrative
// endpoints use. A non-positive ID reports ErrInvalidInput, and an unknown
// account reports ErrUserNotFound, which the handlers map to HTTP 404.
func (s *Service) GetUser(ctx context.Context, userID int64) (*sqlcgen.User, error) {
	if userID <= 0 {
		return nil, ErrInvalidInput
	}
	row, err := s.queries.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return row, nil
}

// Logout revokes the session the caller authenticated with, so its access token
// stops being accepted before its own expiry.
//
// Only a bearer identity carries a revocable TelDrive session: an API key, an
// event ticket or a missing identity reports ErrSessionNotFound. The revocation
// matches the row whether or not it was already revoked, so repeated logouts are
// harmless.
func (s *Service) Logout(ctx context.Context, identity principal.Identity) error {
	if identity.UserID <= 0 || identity.SessionID == uuid.Nil || identity.Source != "bearer" {
		return ErrSessionNotFound
	}
	count, err := s.queries.RevokeSession(ctx, sqlcgen.RevokeSessionParams{SessionID: dbtypes.UUID(identity.SessionID), UserID: identity.UserID})
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if count == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// ListSessions returns one page of a user's active sessions, newest first, for
// the session management UI.
//
// Revoked and expired sessions are never listed. Limit defaults to 100 and is
// clamped to 200, and the cursor is keyset based, so AfterCreatedAt and AfterID
// must be copied from the last row of the previous page and set together; leaving
// both nil returns the first page. A non-positive user ID reports ErrInvalidInput.
func (s *Service) ListSessions(ctx context.Context, in ListSessionsInput) ([]*sqlcgen.Session, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidInput
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	rows, err := s.queries.ListSessions(ctx, sqlcgen.ListSessionsParams{
		UserID: in.UserID, AfterCreatedAt: dbtypes.OptionalTime(in.AfterCreatedAt),
		AfterID: dbtypes.OptionalUUID(in.AfterID), PageSize: in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return rows, nil
}

// RevokeSession revokes one session of userID. The session must be owned by that
// user, so a foreign or unknown ID reports ErrSessionNotFound rather than
// revealing whether the session exists at all.
//
// Revoking an already revoked session is not an error: the row still matches and
// keeps its original revocation timestamp, which makes the call idempotent. A
// non-positive user ID or the nil UUID reports ErrInvalidInput.
func (s *Service) RevokeSession(ctx context.Context, userID int64, sessionID uuid.UUID) error {
	if userID <= 0 || sessionID == uuid.Nil {
		return ErrInvalidInput
	}
	count, err := s.queries.RevokeSession(ctx, sqlcgen.RevokeSessionParams{
		SessionID: dbtypes.UUID(sessionID), UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if count == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// CreateAPIKey mints an API key for userID and returns the plaintext secret once.
//
// The name must be non-empty and at most 120 characters, and an expiry, when
// given, must be in the future; anything else reports ErrInvalidInput. Only a
// digest of the secret is stored, together with a short prefix kept for
// recognition, so the full value cannot be recovered after this call returns.
func (s *Service) CreateAPIKey(ctx context.Context, userID int64, name string, expiresAt *time.Time) (*APIKeyCreated, error) {
	name = strings.TrimSpace(name)
	if userID <= 0 || name == "" || len(name) > 120 || (expiresAt != nil && !expiresAt.After(s.now())) {
		return nil, ErrInvalidInput
	}
	secret, hash, err := s.newOpaqueToken("tdk_")
	if err != nil {
		return nil, err
	}
	prefix := secret
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	row, err := s.queries.CreateAPIKey(ctx, sqlcgen.CreateAPIKeyParams{
		ID: dbtypes.UUID(uuid.New()), UserID: userID, Name: name, KeyPrefix: prefix,
		SecretHash: hash, ExpiresAt: dbtypes.OptionalTime(expiresAt),
	})
	if err != nil {
		return nil, fmt.Errorf("create API key: %w", err)
	}
	return &APIKeyCreated{Row: row, Secret: secret}, nil
}

// ListAPIKeys returns one page of a user's usable API keys, newest first.
//
// Like ListSessions it hides the keys that can no longer authenticate: revoked
// keys and keys whose expiry has passed are dropped, because the statement itself
// returns every stored row of the account. The filter runs in Go, so the page is
// refilled from the following stored rows until Limit usable keys are collected
// or the account's rows run out; a caller therefore still receives a full page,
// and a short page means nothing usable is left. Limit defaults to 100 and is
// clamped to 200, and AfterCreatedAt together with AfterID forms the keyset
// cursor over the stored rows; a non-positive user ID reports ErrInvalidInput.
func (s *Service) ListAPIKeys(ctx context.Context, in ListAPIKeysInput) ([]*sqlcgen.ApiKey, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidInput
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	now := s.now()
	keys := make([]*sqlcgen.ApiKey, 0, in.Limit)
	afterCreatedAt, afterID := in.AfterCreatedAt, in.AfterID
	for len(keys) < int(in.Limit) {
		pageSize := in.Limit - int32(len(keys))
		rows, err := s.queries.ListAPIKeys(ctx, sqlcgen.ListAPIKeysParams{
			UserID: in.UserID, AfterCreatedAt: dbtypes.OptionalTime(afterCreatedAt),
			AfterID: dbtypes.OptionalUUID(afterID), PageSize: pageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("list API keys: %w", err)
		}
		for _, row := range rows {
			if row.RevokedAt.Valid || (row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(now)) {
				continue
			}
			keys = append(keys, row)
		}
		if len(rows) < int(pageSize) {
			break
		}
		// Continue below the last stored row, which is what keeps the cursor
		// correct even when that row was filtered out of the result.
		lastID, ok := dbtypes.GoogleUUID(rows[len(rows)-1].ID)
		if !ok {
			break
		}
		createdAt := rows[len(rows)-1].CreatedAt.Time
		afterCreatedAt, afterID = &createdAt, &lastID
	}
	return keys, nil
}

// RevokeAPIKey revokes one API key of userID.
//
// The key must belong to that user and still be active, so an unknown, foreign or
// already revoked key reports ErrAPIKeyNotFound; unlike RevokeSession this call
// is therefore not idempotent. A non-positive user ID or the nil UUID reports
// ErrInvalidInput.
func (s *Service) RevokeAPIKey(ctx context.Context, userID int64, keyID uuid.UUID) error {
	if userID <= 0 || keyID == uuid.Nil {
		return ErrInvalidInput
	}
	count, err := s.queries.RevokeAPIKey(ctx, sqlcgen.RevokeAPIKeyParams{ID: dbtypes.UUID(keyID), UserID: userID})
	if err != nil {
		return fmt.Errorf("revoke API key: %w", err)
	}
	if count == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// ListUsers returns accounts for the admin console, oldest first.
//
// A non-empty search matches display name, username or user ID, while an empty
// one lists every account. The page size is fixed at 500 with no cursor, so a
// larger installation is truncated silently.
func (s *Service) ListUsers(ctx context.Context, search string) ([]*sqlcgen.User, error) {
	rows, err := s.queries.ListUsers(ctx, sqlcgen.ListUsersParams{
		Search: dbtypes.OptionalText(nonEmpty(strings.TrimSpace(search))), PageSize: 500,
	})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return rows, nil
}

// SearchUsers finds accounts that a share can be granted to, excluding the caller
// so a user cannot share with itself and skipping disabled accounts.
//
// The search string is required and matches display name, username or user ID;
// results are ordered by username and capped at 20 rows with no cursor. A
// non-positive actor ID or a blank search reports ErrInvalidInput.
func (s *Service) SearchUsers(ctx context.Context, actorID int64, search string) ([]*sqlcgen.User, error) {
	search = strings.TrimSpace(search)
	if actorID <= 0 || search == "" {
		return nil, ErrInvalidInput
	}
	rows, err := s.queries.SearchUsersForShare(ctx, sqlcgen.SearchUsersForShareParams{
		ExcludeUserID: actorID, Search: search, PageSize: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	return rows, nil
}

// UpdateUserRole changes an account's role between "admin" and "user" and returns
// the updated row.
//
// Only those two roles are accepted. The account is loaded first so its two
// refusals stay distinguishable: an unknown account reports ErrUserNotFound,
// while an owner account reports ErrOwnerProtected instead of looking like a
// missing user, because the statement itself would quietly touch no row. The
// change applies from the next token issue or renewal onwards, since access
// tokens already in circulation carry a role snapshot.
func (s *Service) UpdateUserRole(ctx context.Context, userID int64, role sqlcgen.UserRole) (*sqlcgen.User, error) {
	if userID <= 0 || (role != sqlcgen.UserRoleAdmin && role != sqlcgen.UserRoleUser) {
		return nil, ErrInvalidInput
	}
	user, err := s.queries.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user for role update: %w", err)
	}
	if user.Role == sqlcgen.UserRoleOwner {
		return nil, ErrOwnerProtected
	}
	row, err := s.queries.UpdateUserRole(ctx, sqlcgen.UpdateUserRoleParams{Role: role, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// The statement only refuses owner accounts: the role was validated
		// above, and no account can be promoted to owner here, so a row that
		// was present a moment ago and now matches nothing is the owner guard.
		return nil, ErrOwnerProtected
	}
	if err != nil {
		return nil, fmt.Errorf("update user role: %w", err)
	}
	return row, nil
}

// SetUserDisabled disables or re-enables an account and returns the updated row.
//
// Disabling writes disabled_at and then revokes every session and API key of the
// account. Those are separate statements rather than one transaction, so a failure
// in between can leave the flag set while credentials remain on record; none of
// them can authenticate while the account is disabled, because role resolution
// rejects a disabled user. The account is loaded first so an unknown account
// reports ErrUserNotFound while an owner account reports ErrOwnerProtected, whose
// statement refuses to touch it.
func (s *Service) SetUserDisabled(ctx context.Context, userID int64, disabled bool) (*sqlcgen.User, error) {
	if userID <= 0 {
		return nil, ErrInvalidInput
	}
	user, err := s.queries.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user for disable: %w", err)
	}
	if user.Role == sqlcgen.UserRoleOwner {
		return nil, ErrOwnerProtected
	}
	row, err := s.queries.SetUserDisabled(ctx, sqlcgen.SetUserDisabledParams{Disabled: disabled, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		// Like UpdateUserRole, the only row this statement refuses is the owner.
		return nil, ErrOwnerProtected
	}
	if err != nil {
		return nil, fmt.Errorf("set user disabled: %w", err)
	}
	if disabled {
		if _, err := s.queries.RevokeAllSessionsForUser(ctx, userID); err != nil {
			return nil, fmt.Errorf("revoke disabled user sessions: %w", err)
		}
		if _, err := s.queries.RevokeAllAPIKeysForUser(ctx, userID); err != nil {
			return nil, fmt.Errorf("revoke disabled user API keys: %w", err)
		}
	}
	return row, nil
}

// RevokeUserAccess revokes every session and API key of an account without
// disabling it, which is the administrator's "sign out everywhere" operation.
//
// Owner accounts are refused with ErrInvalidInput so a deployment cannot lock
// itself out, and an unknown account reports ErrUserNotFound. The two
// revocations are separate statements, so a failure between them can revoke
// sessions while leaving API keys alive, and the account can sign in again
// immediately afterwards.
func (s *Service) RevokeUserAccess(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return ErrInvalidInput
	}
	user, err := s.queries.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("get user for access revoke: %w", err)
	}
	if user.Role == sqlcgen.UserRoleOwner {
		return ErrInvalidInput
	}
	if _, err := s.queries.RevokeAllSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	if _, err := s.queries.RevokeAllAPIKeysForUser(ctx, userID); err != nil {
		return fmt.Errorf("revoke user API keys: %w", err)
	}
	return nil
}

// Capabilities returns the permission strings granted to a role so clients can
// decide which controls to offer.
//
// Every role gets the file permissions; "admin" and "owner" additionally get the
// system management ones, and only "owner" gets system.owner. The result is
// derived from the role alone and ignores whether the account is disabled.
func Capabilities(role sqlcgen.UserRole) []string {
	capabilities := []string{"files.read", "files.write", "files.share"}
	if role == sqlcgen.UserRoleAdmin || role == sqlcgen.UserRoleOwner {
		capabilities = append(capabilities,
			"system.manageUsers", "system.manageJobs", "system.manageQueues", "system.localImport", "system.maintenance",
		)
	}
	if role == sqlcgen.UserRoleOwner {
		capabilities = append(capabilities, "system.owner")
	}
	return capabilities
}

// rolesForUser resolves the role names a user currently holds: "user" always,
// plus "admin" for admins and owners, plus "owner" for the owner alone.
//
// It runs on every authentication instead of trusting role claims in a token, so a
// role change and a disabled account both take effect on the next request. The
// caller supplies the queries handle, which is what lets completeLogin resolve
// roles through the transaction that just upserted the user. A missing or disabled
// user reports ErrInvalidCredential, and a database failure while loading the user
// is reported the same way instead of being propagated.
func (s *Service) rolesForUser(ctx context.Context, q *sqlcgen.Queries, userID int64) ([]string, error) {
	user, err := q.GetUser(ctx, userID)
	if err != nil || user.DisabledAt.Valid {
		return nil, ErrInvalidCredential
	}
	roles := []string{"user"}
	switch user.Role {
	case sqlcgen.UserRoleOwner:
		roles = append(roles, "admin", "owner")
	case sqlcgen.UserRoleAdmin:
		roles = append(roles, "admin")
	}
	return roles, nil
}

// issueAccessToken mints a signed access token for one session, embedding the
// user's current roles alongside the configured issuer and lifetime.
//
// Roles are read at signing time, so the token carries a snapshot that a later
// role change does not update; revoking the session still invalidates the token,
// because bearer authentication checks the session row. The caller supplies the
// queries handle so a login can sign inside its own transaction and see the user
// row that transaction upserted. A missing or disabled user reports
// ErrInvalidCredential, and a signing failure is returned wrapped.
func (s *Service) issueAccessToken(ctx context.Context, q *sqlcgen.Queries, userID int64, sessionID uuid.UUID) (string, error) {
	roles, err := s.rolesForUser(ctx, q, userID)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	claims := accessClaims{
		SessionID: sessionID, Roles: roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.config.Issuer, Subject: fmt.Sprintf("%d", userID),
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.config.AccessTokenTTL)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.config.SigningKey))
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return token, nil
}

// newOpaqueToken returns a fresh random secret carrying prefix, together with the
// digest to store for it.
//
// The secret is drawn from Service.random, which must be a cryptographically
// secure source in production; it is a plain reader only so tests can pin token
// values. A failing random source is returned wrapped and no token is produced.
func (s *Service) newOpaqueToken(prefix string) (string, []byte, error) {
	buffer := make([]byte, 32)
	if _, err := io.ReadFull(s.random, buffer); err != nil {
		return "", nil, fmt.Errorf("generate opaque token: %w", err)
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(buffer)
	return token, hashToken(token), nil
}

// hashToken returns the fixed-size digest stored in place of a secret, or nil for
// an empty value so callers can reject a blank credential before touching the
// database.
func hashToken(value string) []byte {
	if value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// flowLockID maps a flow UUID to the bigint key of the PostgreSQL advisory lock
// that serializes work on that flow.
//
// The key is derived deterministically from the UUID so every instance sharing the
// database computes the same lock. Distinct flow IDs can collide in principle,
// which would merely serialize two unrelated flows.
func flowLockID(id uuid.UUID) int64 {
	digest := sha256.Sum256(append([]byte("teldrive/login-flow/"), id[:]...))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// parseUserID converts the subject claim of an access token back into a user ID.
// A value that is not a positive integer reports ErrInvalidCredential, so a
// malformed subject is treated like any other bad credential.
func parseUserID(value string) (int64, error) {
	var result int64
	_, err := fmt.Sscan(value, &result)
	if err != nil || result <= 0 {
		return 0, ErrInvalidCredential
	}
	return result, nil
}

// ttlSeconds converts a token lifetime into the whole seconds reported to clients,
// saturating at the maximum int32 instead of overflowing.
func ttlSeconds(ttl time.Duration) int32 {
	seconds := ttl / time.Second
	if seconds > time.Duration(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	return int32(seconds)
}

// normalizeAllowedUsers prepares the configured allowlist for comparison: names
// are trimmed, lowercased, stripped of a leading "@" and deduplicated in their
// original order, while blank entries are dropped.
func normalizeAllowedUsers(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		username := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "@")))
		if username == "" {
			continue
		}
		if _, exists := seen[username]; exists {
			continue
		}
		seen[username] = struct{}{}
		result = append(result, username)
	}
	return result
}

// userAllowed reports whether a Telegram username may complete a login. An empty
// allowlist permits every account; otherwise the username is normalized the same
// way as the configured entries before comparison.
func (s *Service) userAllowed(username string) bool {
	if len(s.config.AllowedUsers) == 0 {
		return true
	}
	candidate := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(username), "@")))
	return slices.Contains(s.config.AllowedUsers, candidate)
}

// nonEmpty returns a trimmed pointer to value, or nil when the trimmed value is
// empty, so optional text columns receive NULL instead of an empty string.
func nonEmpty(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
