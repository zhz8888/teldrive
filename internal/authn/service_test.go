package authn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/principal"
)

func TestServiceValidationAndHelpers(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, nil, nil, Config{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService() error = %v", err)
	}
	s := &Service{
		config: Config{SigningKey: strings.Repeat("k", 32), Issuer: "test", AccessTokenTTL: time.Minute},
		now:    time.Now,
	}
	ctx := context.Background()
	if _, err := s.StartLogin(ctx, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("StartLogin() error = %v", err)
	}
	if _, err := s.VerifyCode(ctx, uuid.Nil, "code"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyCode(nil) error = %v", err)
	}
	if _, err := s.VerifyCode(ctx, uuid.New(), " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyCode(empty) error = %v", err)
	}
	if _, err := s.VerifyPassword(ctx, uuid.Nil, "password"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyPassword(nil) error = %v", err)
	}
	if _, err := s.VerifyPassword(ctx, uuid.New(), ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyPassword(empty) error = %v", err)
	}
	if _, err := s.Refresh(ctx, " "); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Refresh(empty) error = %v", err)
	}
	if _, err := s.RenewAccess(ctx, " "); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("RenewAccess(empty) error = %v", err)
	}
	if _, err := s.AuthenticateBearer(ctx, "not-a-jwt"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("AuthenticateBearer() error = %v", err)
	}
	if _, err := s.AuthenticateAPIKey(ctx, " "); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("AuthenticateAPIKey() error = %v", err)
	}
	if _, err := s.GetUser(ctx, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("GetUser() error = %v", err)
	}
	if err := s.Logout(ctx, principal.Identity{UserID: 1, Source: "api_key"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Logout(api key) error = %v", err)
	}
	past := time.Now().Add(-time.Minute)
	for _, input := range []struct {
		userID int64
		name   string
		expiry *time.Time
	}{{0, "key", nil}, {1, " ", nil}, {1, "key", &past}} {
		if _, err := s.CreateAPIKey(ctx, input.userID, input.name, input.expiry); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("CreateAPIKey(%#v) error = %v", input, err)
		}
	}
	if _, err := s.ListAPIKeys(ctx, ListAPIKeysInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ListAPIKeys() error = %v", err)
	}
	if err := s.RevokeAPIKey(ctx, 0, uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
	if hashToken("") != nil || len(hashToken("value")) != 32 {
		t.Fatal("hashToken validation failed")
	}
	if _, err := parseUserID("bad"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("parseUserID() error = %v", err)
	}
	if value, err := parseUserID("42"); err != nil || value != 42 {
		t.Fatalf("parseUserID(42) = %d, %v", value, err)
	}
	if nonEmpty(" ") != nil || nonEmpty(" value ") == nil {
		t.Fatal("nonEmpty validation failed")
	}
	if ttlSeconds(time.Minute) != 60 {
		t.Fatalf("ttlSeconds() = %d", ttlSeconds(time.Minute))
	}
}

// TestSlotLimiterCapsConcurrentLoginFlows checks that the login-flow semaphore is
// shared and bounded, and that a caller which cannot take a slot returns without
// ever touching the pool: the service in this test has a nil pool, so acquiring a
// connection would panic instead of failing the assertion.
func TestSlotLimiterCapsConcurrentLoginFlows(t *testing.T) {
	t.Parallel()
	s := &Service{now: time.Now}
	slots := s.slotLimiter()
	if cap(slots) != maxConcurrentLoginFlows {
		t.Fatalf("cap(slots) = %d, want %d", cap(slots), maxConcurrentLoginFlows)
	}
	if s.slotLimiter() != slots {
		t.Fatal("slotLimiter() returned a different channel on the second call")
	}
	for i := 0; i < maxConcurrentLoginFlows; i++ {
		slots <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentLoginFlows; i++ {
			<-slots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.withFlowLock(ctx, uuid.New(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("withFlowLock() error = %v, want context.Canceled", err)
	}
}

// TestStartLoginRejectsStepWithoutResumableState checks the guard that keeps a
// gateway step from being stored as a flow the client could never continue. The
// gateway reports success with an empty state, which is what the gotd gateway
// returns when gotd treats a cancelled callback as a clean shutdown, and the
// service must reject it instead of sealing and storing the empty value.
func TestStartLoginRejectsStepWithoutResumableState(t *testing.T) {
	t.Parallel()
	s := &Service{
		config: Config{SigningKey: strings.Repeat("k", 32), Issuer: "test", AccessTokenTTL: time.Minute},
		login:  &emptyStateLogin{}, now: time.Now,
	}
	if _, err := s.StartLogin(context.Background(), "+10000000001"); !errors.Is(err, ErrLoginStateInvalid) {
		t.Fatalf("StartLogin() error = %v, want ErrLoginStateInvalid", err)
	}
}

// TestLoginLimitsThrottleSendsAndAttempts checks that the code-send budget is
// per phone number and that only wrong codes or passwords consume the per-flow
// attempt budget.
func TestLoginLimitsThrottleSendsAndAttempts(t *testing.T) {
	t.Parallel()
	s := &Service{now: time.Now}
	phone := "+10000000000"
	for send := 0; send < loginCodeSends; send++ {
		if _, ok := s.sendLimiter().Allow(phone); !ok {
			t.Fatalf("send %d refused before the budget ran out", send)
		}
		s.sendLimiter().Fail(phone)
	}
	if _, ok := s.sendLimiter().Allow(phone); ok {
		t.Fatal("send allowed after the budget ran out")
	}
	if _, ok := s.sendLimiter().Allow("+19999999999"); !ok {
		t.Fatal("an unrelated phone number was throttled")
	}
	// Succeed is the mechanism a completed login uses to hand the budget of its
	// phone number back: without it the refusals would only ever accumulate, and an
	// owner who signs in repeatedly would end up refused for good.
	s.sendLimiter().Succeed(phone)
	if _, ok := s.sendLimiter().Allow(phone); !ok {
		t.Fatal("Succeed did not return the send budget")
	}
	flowID := uuid.New()
	for attempt := 0; attempt < loginAttemptFailures; attempt++ {
		s.recordAttemptFailure(flowID, ErrCodeInvalid)
	}
	if _, ok := s.attemptLimiter().Allow(flowID.String()); ok {
		t.Fatal("attempt allowed after the failure budget ran out")
	}
	other := uuid.New()
	s.recordAttemptFailure(other, errors.New("network"))
	if _, ok := s.attemptLimiter().Allow(other.String()); !ok {
		t.Fatal("an unrelated error consumed the attempt budget")
	}
}

// emptyStateLogin is a gateway whose Start reports success without any state to
// resume from, which is the shape StartLogin must refuse.
type emptyStateLogin struct{}

func (emptyStateLogin) Start(context.Context, string) (LoginStep, error) {
	return LoginStep{}, nil
}

func (emptyStateLogin) StartQR(context.Context) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (emptyStateLogin) PollQR(context.Context, []byte) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (emptyStateLogin) VerifyCode(context.Context, string, []byte, string) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}

func (emptyStateLogin) VerifyPassword(context.Context, []byte, string) (LoginStep, error) {
	return LoginStep{}, ErrLoginStateInvalid
}
