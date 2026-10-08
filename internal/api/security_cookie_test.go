package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/zhz8888/teldrive/v2/internal/api/gen"
)

// cookieTestAuthenticator is an Authenticator that records the credential it was
// called with and replays a canned identity, so the cookie tests can tell which
// credential path the security layer took.
type cookieTestAuthenticator struct {
	// bearerToken receives the token passed to AuthenticateBearer.
	bearerToken string
	// apiKey receives the key passed to AuthenticateAPIKey; cookie auth must never
	// reach it.
	apiKey string
	// identity is the identity both methods return.
	identity Identity
	// err, when set, makes both methods fail with it.
	err error
}

// AuthenticateBearer records the presented token and returns the canned identity.
func (a *cookieTestAuthenticator) AuthenticateBearer(_ context.Context, token string) (Identity, error) {
	a.bearerToken = token
	return a.identity, a.err
}

// AuthenticateAPIKey records the presented key and returns the canned identity.
func (a *cookieTestAuthenticator) AuthenticateAPIKey(_ context.Context, key string) (Identity, error) {
	a.apiKey = key
	return a.identity, a.err
}

// TestHandleCookieAuthUsesBearerAuthentication pins that the session cookie is
// validated through the bearer-token path and that the API-key path stays
// untouched, which is what lets one session lookup serve both credential forms.
func TestHandleCookieAuthUsesBearerAuthentication(t *testing.T) {
	t.Parallel()
	sessionID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	authenticator := &cookieTestAuthenticator{identity: Identity{
		UserID:    1001,
		SessionID: sessionID,
		Roles:     []string{"user"},
		Source:    "bearer",
	}}
	security := NewSecurity(authenticator)
	ctx, err := security.HandleCookieAuth(context.Background(), gen.GetCurrentUserOperation, gen.CookieAuth{APIKey: "cookie-access-token"})
	if err != nil {
		t.Fatalf("HandleCookieAuth() error = %v", err)
	}
	if authenticator.bearerToken != "cookie-access-token" {
		t.Fatalf("bearer token = %q", authenticator.bearerToken)
	}
	if authenticator.apiKey != "" {
		t.Fatalf("API-key authenticator was called with %q", authenticator.apiKey)
	}
	identity, ok := IdentityFromContext(ctx)
	if !ok || identity.UserID != 1001 || identity.SessionID != sessionID || identity.Source != "bearer" {
		t.Fatalf("identity = %#v, %v", identity, ok)
	}
}

func TestHandleCookieAuthRejectsInvalidCredentials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		authenticator *cookieTestAuthenticator
		token         string
		want          error
	}{
		{name: "missing token", authenticator: &cookieTestAuthenticator{}, want: ErrUnauthenticated},
		{name: "authentication failure", authenticator: &cookieTestAuthenticator{err: errors.New("bad token")}, token: "bad", want: ErrUnauthenticated},
		{name: "invalid identity", authenticator: &cookieTestAuthenticator{identity: Identity{}}, token: "token", want: ErrInvalidIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			security := NewSecurity(test.authenticator)
			_, err := security.HandleCookieAuth(context.Background(), gen.GetCurrentUserOperation, gen.CookieAuth{APIKey: test.token})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}
