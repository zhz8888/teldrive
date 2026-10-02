package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/authn"
)

const (
	// accessCookieName is the cookie carrying the short-lived access token used
	// for cookie-authenticated operations.
	accessCookieName = "teldrive_access"
	// refreshCookieName is the cookie carrying the refresh token exchanged at the
	// refresh endpoint for a new token pair.
	refreshCookieName = "teldrive_refresh"
)

// cookieSecureContextKey is the context key under which WithCookieSecure records
// the Secure attribute decision for the cookies of the current request. The
// unexported empty struct keeps the key private to this package.
type cookieSecureContextKey struct{}

// WithCookieSecure returns a copy of ctx that marks whether session cookies
// issued while handling this request should carry the Secure attribute. The
// composition root sets it per request from the configured public URL, so one
// build serves both plain-HTTP development and TLS deployments.
func WithCookieSecure(ctx context.Context, secure bool) context.Context {
	return context.WithValue(ctx, cookieSecureContextKey{}, secure)
}

// cookieSecure reports the Secure flag recorded by WithCookieSecure, defaulting
// to false when the context was never marked.
func cookieSecure(ctx context.Context) bool {
	secure, _ := ctx.Value(cookieSecureContextKey{}).(bool)
	return secure
}

// CookieTelegramLoginVerifyCode continues a phone login flow with the code
// Telegram sent the user: it either returns the flow again when the two-step
// password is still required, or issues the cookie session. The endpoint is
// unauthenticated, and unusable flows surface as 4xx responses through
// mapServiceError.
func (h *Handler) CookieTelegramLoginVerifyCode(ctx context.Context, req *gen.TelegramCodeVerifyRequest) (gen.CookieTelegramLoginVerifyCodeRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.VerifyCode(ctx, googleUUID(req.FlowId), req.Code)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if result.Flow != nil {
		response := loginFlowResponse(result.Flow)
		return &response, nil
	}
	return h.cookieSessionResponse(ctx, result.Tokens)
}

// CookieTelegramLoginVerifyPassword completes a login flow that required the
// Telegram two-step password and returns the issued cookie session. A wrong
// password maps to 401 telegram_password_invalid through mapServiceError.
func (h *Handler) CookieTelegramLoginVerifyPassword(ctx context.Context, req *gen.TelegramPasswordVerifyRequest) (gen.CookieTelegramLoginVerifyPasswordRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.VerifyPassword(ctx, googleUUID(req.FlowId), req.Password)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return h.cookieSessionResponse(ctx, result.Tokens)
}

// CookieTelegramQRLoginPoll advances a QR login flow: while the QR code has not
// been approved it returns the flow so the client can poll again, and once the
// phone approves it issues the cookie session. The endpoint is unauthenticated.
func (h *Handler) CookieTelegramQRLoginPoll(ctx context.Context, req *gen.TelegramQRLoginPollRequest) (gen.CookieTelegramQRLoginPollRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.PollQR(ctx, googleUUID(req.FlowId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	if result.QRFlow != nil {
		response := qrLoginFlowResponse(result.QRFlow)
		return &response, nil
	}
	return h.cookieSessionResponse(ctx, result.Tokens)
}

// RefreshCookieSession exchanges the refresh cookie for a new token pair and
// returns both cookies with the new access expiry. The refresh token is rotated,
// so the client must discard the value it sent; unknown or revoked tokens map to
// 401.
func (h *Handler) RefreshCookieSession(ctx context.Context, params gen.RefreshCookieSessionParams) (gen.RefreshCookieSessionRes, error) {
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	tokens, err := h.Auth.Refresh(ctx, params.TeldriveRefresh)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return h.cookieSessionResponse(ctx, tokens)
}

// LogoutCookieSession revokes the session of the authenticated caller and clears
// both session cookies by returning already expired values. It needs a credential
// tied to a login session, so an unauthenticated request and a session-less
// credential such as an API key both answer 401, the only failure the operation
// declares.
func (h *Handler) LogoutCookieSession(ctx context.Context) (gen.LogoutCookieSessionRes, error) {
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return nil, mapServiceError(ErrUnauthenticated)
	}
	if identity.SessionID == uuid.Nil {
		return nil, problem(http.StatusUnauthorized, "unauthorized", "a cookie or bearer session is required to log out", nil)
	}
	if err := h.Auth.Logout(ctx, identity); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.LogoutCookieSessionNoContent{SetCookie: h.expiredCookies(ctx)}, nil
}

// cookieSessionResponse builds the Set-Cookie headers and JSON body returned by
// every endpoint that establishes a cookie session. It reports
// ErrOperationUnavailable when the handler is not fully configured or the token
// pair is incomplete, so a partially initialized auth service never hands out an
// unusable session.
func (h *Handler) cookieSessionResponse(ctx context.Context, tokens *authn.TokenPair) (*gen.CookieSessionHeaders, error) {
	if h == nil || h.Auth == nil || tokens == nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn <= 0 {
		return nil, ErrOperationUnavailable
	}
	now := time.Now().UTC()
	accessTTL := time.Duration(tokens.ExpiresIn) * time.Second
	refreshTTL := h.Auth.RefreshTokenTTL()
	return &gen.CookieSessionHeaders{
		SetCookie: []string{
			h.cookie(ctx, accessCookieName, tokens.AccessToken, accessTTL).String(),
			h.cookie(ctx, refreshCookieName, tokens.RefreshToken, refreshTTL).String(),
		},
		Response: gen.CookieSession{
			Authenticated: gen.CookieSessionAuthenticatedTrue,
			ExpiresAt:     now.Add(accessTTL),
		},
	}, nil
}

// cookie builds one session cookie: path "/", HttpOnly and SameSite=Lax, with
// the Secure attribute taken from the request context. The TTL sets both Max-Age
// and Expires so browsers and intermediaries agree on the lifetime.
func (h *Handler) cookie(ctx context.Context, name, value string, ttl time.Duration) *http.Cookie {
	maxAge := int(ttl / time.Second)
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  time.Now().UTC().Add(ttl),
		HttpOnly: true,
		Secure:   cookieSecure(ctx),
		SameSite: http.SameSiteLaxMode,
	}
}

// expiredCookies returns the two Set-Cookie values that delete the session
// cookies, used by logout to clear a session on the client. Max-Age is -1 and
// Expires is the Unix epoch, which removes the cookies regardless of the
// client's clock.
func (h *Handler) expiredCookies(ctx context.Context) []string {
	expires := time.Unix(1, 0).UTC()
	cookies := make([]string, 0, 2)
	for _, name := range []string{accessCookieName, refreshCookieName} {
		cookies = append(cookies, (&http.Cookie{
			Name:     name,
			Path:     "/",
			MaxAge:   -1,
			Expires:  expires,
			HttpOnly: true,
			Secure:   cookieSecure(ctx),
			SameSite: http.SameSiteLaxMode,
		}).String())
	}
	return cookies
}
