//go:build integration

package authn

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestLoginRefreshAPIKeyAndLogoutAgainstRealPostgres(t *testing.T) {
	db := testpostgres.New(t)
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{5}, 32), bytes.NewReader(bytes.Repeat([]byte{9}, 24*20)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &fakeTelegramLogin{}
	service, err := NewService(db.Pool, cipher, gateway, Config{
		SigningKey: "0123456789abcdef0123456789abcdef", Issuer: "test",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour, LoginFlowTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.random = bytes.NewReader(bytes.Join([][]byte{
		bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{8}, 32), bytes.Repeat([]byte{9}, 32),
		bytes.Repeat([]byte{10}, 32), bytes.Repeat([]byte{11}, 32), bytes.Repeat([]byte{12}, 32),
		bytes.Repeat([]byte{13}, 32),
	}, nil))
	testNow := time.Now().UTC()
	service.now = func() time.Time { return testNow }
	ctx := context.Background()

	flow, err := service.StartLogin(ctx, "+15551234567")
	if err != nil {
		t.Fatalf("StartLogin() error = %v", err)
	}
	if _, err := service.VerifyCode(ctx, uuid.New(), "12345"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("unknown flow error = %v", err)
	}
	if flow.ID == uuid.Nil || flow.PasswordRequired {
		t.Fatalf("flow = %#v", flow)
	}
	expiredFlow, err := service.StartLogin(ctx, "+15557654321")
	if err != nil {
		t.Fatalf("StartLogin(expired) error = %v", err)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE telegram_login_flows SET expires_at=now()-interval '1 minute' WHERE id=$1", expiredFlow.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyCode(ctx, expiredFlow.ID, "12345"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("expired flow error = %v", err)
	}
	pending, err := service.VerifyCode(ctx, flow.ID, "12345")
	if err != nil {
		t.Fatalf("VerifyCode() error = %v", err)
	}
	if pending.Flow == nil || !pending.Flow.PasswordRequired || pending.Tokens != nil {
		t.Fatalf("pending result = %#v", pending)
	}
	completed, err := service.VerifyPassword(ctx, flow.ID, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if completed.Tokens == nil || completed.Tokens.AccessToken == "" || completed.Tokens.RefreshToken == "" {
		t.Fatalf("completed result = %#v", completed)
	}

	identity, err := service.AuthenticateBearer(ctx, completed.Tokens.AccessToken)
	if err != nil {
		t.Fatalf("AuthenticateBearer() error = %v", err)
	}
	if identity.UserID != 1001 || identity.SessionID == uuid.Nil || identity.Source != "bearer" || !containsRole(identity.Roles, "owner") {
		t.Fatalf("identity = %#v", identity)
	}
	user, err := service.GetUser(ctx, 1001)
	if err != nil || user.UserID != 1001 || !user.Premium || user.Role != sqlcgen.UserRoleOwner {
		t.Fatalf("GetUser() = %#v, %v", user, err)
	}

	secondSessionID := uuid.New()
	if _, err := service.queries.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID: dbtypes.UUID(secondSessionID), UserID: 1001,
		TelegramSession:  []byte("encrypted-second-session"),
		RefreshTokenHash: bytes.Repeat([]byte{42}, 32),
		ExpiresAt:        dbtypes.Time(testNow.Add(12 * time.Hour)),
	}); err != nil {
		t.Fatalf("create second session: %v", err)
	}
	sessions, err := service.ListSessions(ctx, ListSessionsInput{UserID: 1001, Limit: 10})
	if err != nil || len(sessions) != 2 {
		t.Fatalf("ListSessions() = %#v, %v", sessions, err)
	}
	firstSessionID, _ := dbtypes.GoogleUUID(sessions[0].ID)
	firstSessionTime := sessions[0].CreatedAt.Time
	nextSessions, err := service.ListSessions(ctx, ListSessionsInput{
		UserID: 1001, AfterID: &firstSessionID, AfterCreatedAt: &firstSessionTime, Limit: 1,
	})
	if err != nil || len(nextSessions) != 1 {
		t.Fatalf("ListSessions(next) = %#v, %v", nextSessions, err)
	}
	if err := service.RevokeSession(ctx, 1001, secondSessionID); err != nil {
		t.Fatalf("RevokeSession() error = %v", err)
	}
	if err := service.RevokeSession(ctx, 1001, secondSessionID); err != nil {
		t.Fatalf("idempotent RevokeSession() error = %v", err)
	}
	sessions, err = service.ListSessions(ctx, ListSessionsInput{UserID: 1001, Limit: 10})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions(after revoke) = %#v, %v", sessions, err)
	}
	if err := service.RevokeSession(ctx, 2002, secondSessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-user RevokeSession() error = %v", err)
	}
	renewed, err := service.RenewAccess(ctx, completed.Tokens.RefreshToken)
	if err != nil || renewed.AccessToken == "" || renewed.ExpiresIn <= 0 {
		t.Fatalf("RenewAccess() = %#v, %v", renewed, err)
	}
	renewedIdentity, err := service.AuthenticateBearer(ctx, renewed.AccessToken)
	if err != nil || renewedIdentity.UserID != 1001 || renewedIdentity.SessionID != identity.SessionID {
		t.Fatalf("renewed access identity = %#v, %v", renewedIdentity, err)
	}

	rotated, err := service.Refresh(ctx, completed.Tokens.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if rotated.RefreshToken == completed.Tokens.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if _, err := service.Refresh(ctx, completed.Tokens.RefreshToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old refresh token error = %v", err)
	}

	expires := service.now().Add(time.Hour)
	createdKey, err := service.CreateAPIKey(ctx, 1001, "automation", &expires)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if !strings.HasPrefix(createdKey.Secret, "tdk_") {
		t.Fatalf("API key = %q", createdKey.Secret)
	}
	apiIdentity, err := service.AuthenticateAPIKey(ctx, createdKey.Secret)
	if err != nil || apiIdentity.UserID != 1001 || apiIdentity.Source != "api_key" {
		t.Fatalf("API key identity = %#v, %v", apiIdentity, err)
	}
	secondKey, err := service.CreateAPIKey(ctx, 1001, "second", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey(second) error = %v", err)
	}
	defaultKeys, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001})
	if err != nil || len(defaultKeys) != 2 {
		t.Fatalf("ListAPIKeys(default) = %#v, %v", defaultKeys, err)
	}
	keys, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001, Limit: 10})
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListAPIKeys() = %#v, %v", keys, err)
	}
	page, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001, Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("ListAPIKeys(page) = %#v, %v", page, err)
	}
	pageID, _ := dbtypes.GoogleUUID(page[0].ID)
	pageTime := page[0].CreatedAt.Time
	nextPage, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001, AfterID: &pageID, AfterCreatedAt: &pageTime, Limit: 500})
	if err != nil || len(nextPage) != 1 {
		t.Fatalf("ListAPIKeys(next) = %#v, %v", nextPage, err)
	}
	keyID, _ := dbtypes.GoogleUUID(createdKey.Row.ID)
	if err := service.RevokeAPIKey(ctx, 1001, keyID); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
	if _, err := service.AuthenticateAPIKey(ctx, createdKey.Secret); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("revoked API key error = %v", err)
	}
	secondKeyID, _ := dbtypes.GoogleUUID(secondKey.Row.ID)
	if err := service.RevokeAPIKey(ctx, 1001, secondKeyID); err != nil {
		t.Fatalf("RevokeAPIKey(second) error = %v", err)
	}
	revokedKeys, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001, Limit: 10})
	if err != nil || len(revokedKeys) != 0 {
		t.Fatalf("ListAPIKeys(after revoke) = %#v, %v", revokedKeys, err)
	}
	// A page whose stored rows are all unusable must be refilled from the rows
	// below it, so the caller still sees the one usable key that is ordered
	// after the revoked ones.
	usableKey, err := service.CreateAPIKey(ctx, 1001, "usable", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey(usable) error = %v", err)
	}
	newestKey, err := service.CreateAPIKey(ctx, 1001, "newest", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey(newest) error = %v", err)
	}
	newestKeyID, _ := dbtypes.GoogleUUID(newestKey.Row.ID)
	if err := service.RevokeAPIKey(ctx, 1001, newestKeyID); err != nil {
		t.Fatalf("RevokeAPIKey(newest) error = %v", err)
	}
	refilled, err := service.ListAPIKeys(ctx, ListAPIKeysInput{UserID: 1001, Limit: 1})
	if err != nil || len(refilled) != 1 {
		t.Fatalf("ListAPIKeys(refilled) = %#v, %v", refilled, err)
	}
	refilledID, _ := dbtypes.GoogleUUID(refilled[0].ID)
	usableKeyID, _ := dbtypes.GoogleUUID(usableKey.Row.ID)
	if refilledID != usableKeyID {
		t.Fatalf("ListAPIKeys(refilled) = %v, want the usable key", refilledID)
	}

	if err := service.Logout(ctx, identity); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := service.AuthenticateBearer(ctx, completed.Tokens.AccessToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("logged out bearer error = %v", err)
	}

	var phoneCiphertext, stateCiphertext, sessionCiphertext []byte
	if err := db.Pool.QueryRow(ctx, "SELECT phone_number_ciphertext, telegram_state_ciphertext FROM telegram_login_flows WHERE id=$1", flow.ID).Scan(&phoneCiphertext, &stateCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, "SELECT telegram_session FROM sessions WHERE user_id=1001").Scan(&sessionCiphertext); err != nil {
		t.Fatal(err)
	}
	for name, ciphertext := range map[string][]byte{"phone": phoneCiphertext, "state": stateCiphertext, "session": sessionCiphertext} {
		if bytes.Contains(ciphertext, []byte("+15551234567")) || bytes.Contains(ciphertext, []byte("password-state")) || bytes.Contains(ciphertext, []byte("authorized-session")) {
			t.Fatalf("%s ciphertext leaked plaintext: %q", name, ciphertext)
		}
	}
	if gateway.startCalls != 2 || gateway.codeCalls != 1 || gateway.passwordCalls != 1 || gateway.active != 0 {
		t.Fatalf("gateway calls/active = start %d code %d password %d active %d", gateway.startCalls, gateway.codeCalls, gateway.passwordCalls, gateway.active)
	}

	// User 1001 is the owner of this database, so the administration methods must
	// keep "refused owner" and "unknown account" apart, and they must still
	// update an ordinary account.
	if _, err := service.UpdateUserRole(ctx, 1001, sqlcgen.UserRoleAdmin); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("UpdateUserRole(owner) error = %v", err)
	}
	if _, err := service.SetUserDisabled(ctx, 1001, true); !errors.Is(err, ErrOwnerProtected) {
		t.Fatalf("SetUserDisabled(owner) error = %v", err)
	}
	if _, err := service.GetUser(ctx, 999999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetUser(unknown) error = %v", err)
	}
	if _, err := service.UpdateUserRole(ctx, 999999, sqlcgen.UserRoleAdmin); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UpdateUserRole(unknown) error = %v", err)
	}
	if _, err := service.SetUserDisabled(ctx, 999999, true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SetUserDisabled(unknown) error = %v", err)
	}
	if err := service.RevokeUserAccess(ctx, 999999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("RevokeUserAccess(unknown) error = %v", err)
	}
	if _, err := service.queries.UpsertUser(ctx, sqlcgen.UpsertUserParams{
		UserID: 1002, DisplayName: dbtypes.OptionalText(nonEmpty("Second User")),
		Username: dbtypes.OptionalText(nonEmpty("seconduser")),
	}); err != nil {
		t.Fatalf("create second user: %v", err)
	}
	if promoted, err := service.UpdateUserRole(ctx, 1002, sqlcgen.UserRoleAdmin); err != nil || promoted.Role != sqlcgen.UserRoleAdmin {
		t.Fatalf("UpdateUserRole(second) = %#v, %v", promoted, err)
	}

	// A login that cannot mint its token must roll back as a whole: the flow
	// stays retryable and no orphan session is left behind. Disabling the account
	// makes role resolution reject the completed login.
	var sessionsBefore int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id=$1", int64(1001)).Scan(&sessionsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE users SET disabled_at=now() WHERE user_id=$1", int64(1001)); err != nil {
		t.Fatal(err)
	}
	retryFlow, err := service.StartLogin(ctx, "+15550001111")
	if err != nil {
		t.Fatalf("StartLogin(retry) error = %v", err)
	}
	if _, err := service.VerifyCode(ctx, retryFlow.ID, "12345"); err != nil {
		t.Fatalf("VerifyCode(retry) error = %v", err)
	}
	if _, err := service.VerifyPassword(ctx, retryFlow.ID, "correct horse battery staple"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("VerifyPassword(disabled) error = %v", err)
	}
	var flowPending bool
	if err := db.Pool.QueryRow(ctx, "SELECT completed_at IS NULL FROM telegram_login_flows WHERE id=$1", retryFlow.ID).Scan(&flowPending); err != nil {
		t.Fatal(err)
	}
	if !flowPending {
		t.Fatal("a login whose token could not be minted left its flow completed")
	}
	var sessionsAfter int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id=$1", int64(1001)).Scan(&sessionsAfter); err != nil {
		t.Fatal(err)
	}
	if sessionsAfter != sessionsBefore {
		t.Fatalf("sessions after a rolled back login = %d, want %d", sessionsAfter, sessionsBefore)
	}
}

func TestQRLoginResumesAcrossServiceInstancesAndEnforcesAllowlist(t *testing.T) {
	db := testpostgres.New(t)
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{6}, 32), bytes.NewReader(bytes.Repeat([]byte{8}, 24*20)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &fakeQRLogin{username: "alloweduser"}
	cfg := Config{
		SigningKey: "0123456789abcdef0123456789abcdef", Issuer: "test",
		AllowedUsers: []string{"@AllowedUser"}, AccessTokenTTL: time.Hour,
		RefreshTokenTTL: 24 * time.Hour, LoginFlowTTL: 10 * time.Minute,
	}
	first, err := NewService(db.Pool, cipher, gateway, cfg)
	if err != nil {
		t.Fatal(err)
	}
	first.random = bytes.NewReader(bytes.Repeat([]byte{12}, 64))
	flow, err := first.StartQR(context.Background())
	if err != nil {
		t.Fatalf("StartQR() error = %v", err)
	}
	if flow.ID == uuid.Nil || flow.QRURL != "tg://login?token=first" || flow.PasswordRequired {
		t.Fatalf("StartQR() = %#v", flow)
	}

	// A new service instance proves the flow is resumed from PostgreSQL rather
	// than process-local state.
	second, err := NewService(db.Pool, cipher, gateway, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second.random = bytes.NewReader(bytes.Repeat([]byte{13}, 64))
	pending, err := second.PollQR(context.Background(), flow.ID)
	if err != nil {
		t.Fatalf("PollQR(pending) error = %v", err)
	}
	if pending.QRFlow == nil || pending.QRFlow.QRURL != "tg://login?token=second" || pending.Tokens != nil {
		t.Fatalf("PollQR(pending) = %#v", pending)
	}
	completed, err := second.PollQR(context.Background(), flow.ID)
	if err != nil {
		t.Fatalf("PollQR(completed) error = %v", err)
	}
	if completed.Tokens == nil || completed.Tokens.AccessToken == "" || completed.Tokens.RefreshToken == "" {
		t.Fatalf("PollQR(completed) = %#v", completed)
	}

	var method string
	var phone []byte
	var state []byte
	if err := db.Pool.QueryRow(context.Background(), `
SELECT method::text, phone_number_ciphertext, telegram_state_ciphertext
FROM telegram_login_flows WHERE id=$1`, flow.ID).Scan(&method, &phone, &state); err != nil {
		t.Fatal(err)
	}
	if method != "qr" || phone != nil || bytes.Contains(state, []byte("qr-state")) {
		t.Fatalf("persisted QR flow method=%q phone=%q state=%q", method, phone, state)
	}

	blockedGateway := &fakeQRLogin{username: "blockeduser", completeOnFirstPoll: true}
	blocked, err := NewService(db.Pool, cipher, blockedGateway, cfg)
	if err != nil {
		t.Fatal(err)
	}
	blockedFlow, err := blocked.StartQR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.PollQR(context.Background(), blockedFlow.ID); !errors.Is(err, ErrUserNotAllowed) {
		t.Fatalf("blocked PollQR() error = %v", err)
	}
	var blockedSessions int
	if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM sessions WHERE user_id=$1", int64(2002)).Scan(&blockedSessions); err != nil {
		t.Fatal(err)
	}
	if blockedSessions != 0 {
		t.Fatalf("blocked user sessions = %d", blockedSessions)
	}
}

// Regression test for the two-step QR flow. Once the phone confirmed the QR and
// Telegram asked for the account's two-step password, polls must stop exporting
// login tokens: every export mints a brand new token, so the "no password
// required" answer of a later poll used to reset password_required and overwrite
// the session, which made the password the user was typing at that moment fail
// with an invalid-request response.
func TestQRPollKeepsPasswordRequiredState(t *testing.T) {
	db := testpostgres.New(t)
	cipher, err := secureblob.NewWithKey(bytes.Repeat([]byte{3}, 32), bytes.NewReader(bytes.Repeat([]byte{4}, 24*20)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &fakeQRLogin{username: "alloweduser", passwordOnFirstPoll: true, verifyPasswordUnlocks: true}
	service, err := NewService(db.Pool, cipher, gateway, Config{
		SigningKey: "0123456789abcdef0123456789abcdef", Issuer: "test",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour, LoginFlowTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.random = bytes.NewReader(bytes.Repeat([]byte{5}, 256))
	ctx := context.Background()

	flow, err := service.StartQR(ctx)
	if err != nil {
		t.Fatalf("StartQR() error = %v", err)
	}
	scanned, err := service.PollQR(ctx, flow.ID)
	if err != nil {
		t.Fatalf("PollQR(scanned) error = %v", err)
	}
	if scanned.QRFlow == nil || !scanned.QRFlow.PasswordRequired || scanned.Tokens != nil {
		t.Fatalf("PollQR(scanned) = %#v", scanned)
	}
	if polls := gateway.pollCount(); polls != 1 {
		t.Fatalf("gateway polls = %d, want 1", polls)
	}
	var required bool
	var stateAfterScan []byte
	if err := db.Pool.QueryRow(ctx, `
SELECT password_required, telegram_state_ciphertext
FROM telegram_login_flows WHERE id=$1`, flow.ID).Scan(&required, &stateAfterScan); err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("password_required was not persisted after the QR was confirmed")
	}

	// These are the polls that were already in flight when the password prompt
	// appeared. They must neither export another token nor rewrite the session.
	for attempt := range 3 {
		again, err := service.PollQR(ctx, flow.ID)
		if err != nil {
			t.Fatalf("PollQR(again %d) error = %v", attempt, err)
		}
		if again.QRFlow == nil || !again.QRFlow.PasswordRequired || again.Tokens != nil {
			t.Fatalf("PollQR(again %d) = %#v", attempt, again)
		}
		if again.QRFlow.QRURL != "" {
			t.Fatalf("PollQR(again %d) returned QR URL %q after the password prompt", attempt, again.QRFlow.QRURL)
		}
	}
	if polls := gateway.pollCount(); polls != 1 {
		t.Fatalf("gateway polls after further polls = %d, want 1", polls)
	}
	var requiredAfterPolls bool
	var stateAfterPolls []byte
	if err := db.Pool.QueryRow(ctx, `
SELECT password_required, telegram_state_ciphertext
FROM telegram_login_flows WHERE id=$1`, flow.ID).Scan(&requiredAfterPolls, &stateAfterPolls); err != nil {
		t.Fatal(err)
	}
	if !requiredAfterPolls {
		t.Fatal("password_required was reset by a poll that ran after the password prompt")
	}
	if !bytes.Equal(stateAfterPolls, stateAfterScan) {
		t.Fatal("stored login state was rewritten after the password prompt")
	}

	completed, err := service.VerifyPassword(ctx, flow.ID, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if completed.Tokens == nil || completed.Tokens.AccessToken == "" || completed.Tokens.RefreshToken == "" {
		t.Fatalf("VerifyPassword() = %#v", completed)
	}
}

type fakeTelegramLogin struct {
	mu            sync.Mutex
	startCalls    int
	codeCalls     int
	passwordCalls int
	active        int
}

func (f *fakeTelegramLogin) Start(context.Context, string) (LoginStep, error) {
	f.mu.Lock()
	f.startCalls++
	f.active++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	return LoginStep{State: []byte("code-state")}, nil
}

func (f *fakeTelegramLogin) StartQR(context.Context) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (f *fakeTelegramLogin) PollQR(context.Context, []byte) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}
func (f *fakeTelegramLogin) VerifyCode(context.Context, string, []byte, string) (LoginStep, error) {
	f.mu.Lock()
	f.codeCalls++
	f.active++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	return LoginStep{State: []byte("password-state"), PasswordRequired: true}, nil
}

func (f *fakeTelegramLogin) VerifyPassword(context.Context, []byte, string) (LoginStep, error) {
	f.mu.Lock()
	f.passwordCalls++
	f.active++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	return LoginStep{
		User:    &TelegramUser{ID: 1001, DisplayName: "Test User", Username: "testuser", Premium: true},
		Session: []byte("authorized-session"),
	}, nil
}

type fakeQRLogin struct {
	mu                  sync.Mutex
	polls               int
	username            string
	completeOnFirstPoll bool
	// passwordOnFirstPoll makes the first poll report that Telegram accepted the
	// scanned token but the account still needs its two-step password.
	passwordOnFirstPoll bool
	// verifyPasswordUnlocks lets VerifyPassword complete the login.
	verifyPasswordUnlocks bool
}

func (f *fakeQRLogin) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeQRLogin) Start(context.Context, string) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (f *fakeQRLogin) VerifyCode(context.Context, string, []byte, string) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (f *fakeQRLogin) VerifyPassword(context.Context, []byte, string) (LoginStep, error) {
	if !f.verifyPasswordUnlocks {
		return LoginStep{}, ErrLoginStateInvalid
	}
	userID := int64(1001)
	if f.username == "blockeduser" {
		userID = 2002
	}
	return LoginStep{
		User:    &TelegramUser{ID: userID, DisplayName: "QR User", Username: f.username},
		Session: []byte("qr-authorized-session"),
	}, nil
}

func (f *fakeQRLogin) StartQR(context.Context) (LoginStep, error) {
	return LoginStep{
		State: []byte("qr-state-first"), QRURL: "tg://login?token=first",
		QRExpiresAt: time.Now().UTC().Add(time.Minute),
	}, nil
}

func (f *fakeQRLogin) PollQR(context.Context, []byte) (LoginStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.passwordOnFirstPoll {
		if f.polls == 1 {
			// Mirrors the real gateway: the confirmed token is waiting for the
			// two-step password, so the previously issued QR URL is kept.
			return LoginStep{
				State: []byte("qr-state-password"), PasswordRequired: true,
				QRURL: "tg://login?token=first", QRExpiresAt: time.Now().UTC().Add(time.Minute),
			}, nil
		}
		// Mirrors the real gateway too: exporting again mints a brand new token,
		// so a later poll reports an untouched flow even though the scanned token
		// still waits for the two-step password.
		return LoginStep{
			State: []byte("qr-state-minted"), QRURL: "tg://login?token=minted",
			QRExpiresAt: time.Now().UTC().Add(time.Minute),
		}, nil
	}
	if !f.completeOnFirstPoll && f.polls == 1 {
		return LoginStep{
			State: []byte("qr-state-second"), QRURL: "tg://login?token=second",
			QRExpiresAt: time.Now().UTC().Add(time.Minute),
		}, nil
	}
	userID := int64(1001)
	if f.username == "blockeduser" {
		userID = 2002
	}
	return LoginStep{
		User:    &TelegramUser{ID: userID, DisplayName: "QR User", Username: f.username},
		Session: []byte("qr-authorized-session"),
	}, nil
}

func containsRole(roles []string, target string) bool {
	return slices.Contains(roles, target)
}
