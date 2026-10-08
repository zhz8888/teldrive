package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/api"
	"github.com/zhz8888/teldrive/v2/internal/authn"
)

const (
	// accessCookieName is the cookie carrying the browser session's access token,
	// which is the credential a cookie-authenticated request is validated with.
	accessCookieName = "teldrive_access"
	// refreshCookieName is the cookie carrying the long-lived refresh token that
	// the renewal middleware exchanges for a replacement access token.
	refreshCookieName = "teldrive_refresh"
)

const (
	// accessRefreshSkew is how far before its expiry an access cookie is treated as
	// needing renewal, which absorbs clock differences between servers and the
	// round trip to the token endpoint.
	accessRefreshSkew = 10 * time.Second
	// refreshTimeout bounds one refresh-token exchange. Without it a slow or
	// unreachable authentication backend would hold the request open until the
	// client gave up.
	refreshTimeout = 5 * time.Second
)

// sessionRefresher is the slice of the authentication service the renewal
// middleware depends on. It is declared here rather than taking *authn.Service so
// tests can substitute a stub and so the middleware cannot reach anything else.
type sessionRefresher interface {
	// RenewAccess exchanges refreshToken for a replacement access token without
	// rotating the refresh token itself. It reports authn.ErrInvalidCredential for a
	// blank or unknown token and authn.ErrSessionNotFound for a session row whose ID
	// cannot be decoded; the middleware treats exactly those two as "the session is
	// gone" and clears the cookies, while any other error is reported to the client
	// as a temporary failure. Implementations are shared across requests and must be
	// safe for concurrent use.
	RenewAccess(context.Context, string) (*authn.AccessRenewal, error)
}

// sessionRenewer carries the dependencies of the renewal middleware. It is created
// per middleware installation and holds no per-request state.
type sessionRenewer struct {
	// auth performs the refresh exchange. It is never nil while the middleware is
	// running, because sessionRenewalMiddleware returns early when it is nil.
	auth sessionRefresher
	// now returns the current time and is read once per request that carries a
	// refresh cookie. It is injected so tests can control the expiry decision.
	now func() time.Time
}

// sessionRenewalMiddleware wraps next so a browser request whose access cookie has
// expired, or is about to, is served with a freshly renewed one. A nil auth
// disables the wrapper completely and returns next unchanged, which is how the
// behaviour is turned off when no authentication service is wired.
func sessionRenewalMiddleware(auth sessionRefresher, next http.Handler) http.Handler {
	if auth == nil {
		return next
	}
	return (&sessionRenewer{auth: auth, now: time.Now}).middleware(next)
}

// middleware runs the renewal pass. The chain is skipped when the path is exempt,
// when there is no non-blank refresh cookie, or when the access cookie is still
// valid, so the common case costs one cookie read and no network call. A renewal
// failure that means the session is gone clears both cookies and continues
// unauthenticated, letting the downstream layer answer 401; any other failure
// answers 503 with code session_refresh_unavailable rather than silently
// downgrading an authenticated caller to anonymous. On success the replacement
// access cookie is both sent to the client and spliced into the inbound request,
// so the handler sees the fresh token without a second round trip.
func (m *sessionRenewer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionRenewalSkipped(r) {
			next.ServeHTTP(w, r)
			return
		}
		refreshCookie, err := r.Cookie(refreshCookieName)
		if err != nil || strings.TrimSpace(refreshCookie.Value) == "" {
			next.ServeHTTP(w, r)
			return
		}
		accessCookie, accessErr := r.Cookie(accessCookieName)
		if accessErr == nil && !accessCookieNeedsRefresh(accessCookie.Value, m.now().UTC()) {
			next.ServeHTTP(w, r)
			return
		}

		renewal, err := m.renew(r.Context(), refreshCookie.Value)
		if err != nil {
			if errors.Is(err, authn.ErrInvalidCredential) || errors.Is(err, authn.ErrSessionNotFound) {
				clearSessionCookies(w, r)
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "session_refresh_unavailable",
					"message": "The session could not be renewed.",
				},
			})
			return
		}

		accessTTL := time.Duration(renewal.ExpiresIn) * time.Second
		access := sessionCookie(r, accessCookieName, renewal.AccessToken, accessTTL)
		http.SetCookie(w, access)
		replaceRequestCookie(r, access)
		next.ServeHTTP(w, r)
	})
}

// sessionRenewalSkipped reports whether the renewal pass must not run for r. It
// covers requests outside the /v1/ API, requests that authenticate with an explicit
// header instead of a cookie, and the login endpoints under /v1/auth/ except the two
// logout paths, because signing in must not be preceded by a renewal attempt with a
// stale cookie. Only logout is exempted from that carve-out so a dead session is
// still cleaned up. A nil request is skipped.
func sessionRenewalSkipped(r *http.Request) bool {
	if r == nil || hasExplicitAPICredential(r) || !strings.HasPrefix(r.URL.Path, "/v1/") {
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/auth/") {
		return false
	}
	return r.URL.Path != "/v1/auth/logout" && r.URL.Path != "/v1/auth/cookie/logout"
}

// renew exchanges refreshToken under its own timeout so a hung authentication
// backend cannot hold the request open past refreshTimeout. The parent context is
// still honoured, and the returned error is the authenticator's own, unwrapped, so
// the caller can classify it with errors.Is.
func (m *sessionRenewer) renew(ctx context.Context, refreshToken string) (*authn.AccessRenewal, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	return m.auth.RenewAccess(refreshCtx, refreshToken)
}

// accessCookieNeedsRefresh reports whether an access cookie should be replaced
// before the request is handled. It decodes the JWT payload without verifying the
// signature, because the token is validated downstream and this only decides whether
// a renewal is worth attempting; any cookie it cannot parse into a positive exp
// claim counts as stale, so malformed input triggers a renewal instead of an
// unauthenticated request. now is compared against exp plus accessRefreshSkew.
func accessCookieNeedsRefresh(raw string, now time.Time) bool {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return true
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		return true
	}
	return !time.Unix(claims.ExpiresAt, 0).After(now.Add(accessRefreshSkew))
}

// sessionCookie builds the browser cookie that carries a session token. It is
// HttpOnly so scripts cannot read it and SameSite=Lax so cross-site form posts do
// not send it; Secure follows requestIsSecure, so an HTTPS deployment behind a
// trusted proxy still gets a Secure cookie while plain-HTTP development does not
// lose it. The path is "/" so both the API and the UI send it, and ttl drives both
// MaxAge and Expires.
func sessionCookie(r *http.Request, name, value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl / time.Second),
		Expires:  time.Now().UTC().Add(ttl),
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
	}
}

// clearSessionCookies expires both session cookies on the client and is used once a
// refresh token is known to be dead, so the browser stops replaying it on every
// request. Name, path, Secure and SameSite must match sessionCookie, otherwise the
// browser treats the deletion as a different cookie and keeps the stale one.
func clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{accessCookieName, refreshCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(1, 0).UTC(),
			HttpOnly: true,
			Secure:   requestIsSecure(r),
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// replaceRequestCookie rewrites the inbound Cookie header so r carries replacement
// instead of the value the client sent. It rebuilds the header from the parsed
// cookies, preserving unrelated cookies, collapsing repeated occurrences of the
// replaced name into a single updated entry, and appending the replacement when the
// request did not carry it at all. The request must not be read concurrently while
// it is rewritten, which is why this runs before the handler is invoked.
func replaceRequestCookie(r *http.Request, replacement *http.Cookie) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	replaced := false
	for _, cookie := range cookies {
		if cookie.Name == replacement.Name {
			if replaced {
				continue
			}
			cookie.Value = replacement.Value
			replaced = true
		}
		r.AddCookie(cookie)
	}
	if !replaced {
		r.AddCookie(&http.Cookie{Name: replacement.Name, Value: replacement.Value})
	}
}

// requestSecurity decides whether a request arrived over a secure connection, which
// the cookie builders use to set the Secure flag. Instances are read-only after
// construction and safe for concurrent use.
type requestSecurity struct {
	// trustedProxies lists the peers whose forwarding headers are believed. A bare
	// IP address is stored as a full-length prefix so addresses and CIDR ranges
	// share one lookup; an empty slice means no proxy is trusted and only TLS
	// terminated on the connection itself counts as secure.
	trustedProxies []netip.Prefix
}

// newRequestSecurity compiles the configured trusted proxy list. Each value is
// trimmed and parsed first as a CIDR prefix and then as a single IP address, which
// is widened to a full-length prefix. A value that is neither is rejected with the
// prefix parse error, so a typo fails startup instead of quietly trusting nothing
// and marking every cookie non-Secure.
func newRequestSecurity(values []string) (*requestSecurity, error) {
	security := &requestSecurity{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil {
				return nil, err
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		security.trustedProxies = append(security.trustedProxies, prefix)
	}
	return security, nil
}

// middleware resolves the connection's security once per request and stores it on
// the context, where requestIsSecure and api.WithCookieSecure read it. The verdict
// is true for a direct TLS connection or for a plain connection whose immediate peer
// is a trusted proxy reporting X-Forwarded-Proto: https; the header is ignored for
// every other peer, so a client cannot claim its own connection was secure.
func (s *requestSecurity) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure := r.TLS != nil || (s.isTrustedProxy(r) && forwardedProto(r) == "https")
		ctx := context.WithValue(r.Context(), requestSecureContextKey{}, secure)
		ctx = api.WithCookieSecure(ctx, secure)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestSecureContextKey is the unexported context key under which the middleware
// stores the per-request security verdict. The empty struct type keeps it distinct
// from context keys defined in other packages.
type requestSecureContextKey struct{}

// requestIsSecure reports the verdict requestSecurity.middleware stored for r,
// reading it back as a plain bool. A request that never passed through that
// middleware, or one carrying an unexpected value, reports false, so cookies default
// to non-Secure rather than being marked Secure on a connection that was not.
func requestIsSecure(r *http.Request) bool {
	secure, _ := r.Context().Value(requestSecureContextKey{}).(bool)
	return secure
}

// isTrustedProxy reports whether the immediate peer of r is one of the configured
// trusted proxies. Only r.RemoteAddr is examined and never a forwarded-for header,
// which is what makes the check unspoofable; an address that cannot be parsed is not
// trusted. A RemoteAddr without a port is accepted as-is so in-process listeners and
// tests still resolve.
func (s *requestSecurity) isTrustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	address, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// forwardedProto returns the first X-Forwarded-Proto entry, lowercased and trimmed,
// or "" when the header is absent. Only the first entry is considered: a proxy chain
// appends its own hops, and the leftmost value describes the scheme the original
// client used, which is the one that decides whether the cookie may be marked
// Secure.
func forwardedProto(r *http.Request) string {
	value, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.ToLower(strings.TrimSpace(value))
}

// browserCSRFMiddleware rejects state-changing requests that would be authorized by
// an ambient session cookie rather than by an explicit credential. Safe methods,
// requests without a session cookie, requests carrying Authorization or X-API-Key,
// and same-origin browser requests pass through; everything else gets 403 with code
// csrf_rejected. Requiring something a cross-site HTML form cannot produce is what
// makes the check effective, since SameSite=Lax alone does not cover every client.
func browserCSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) || !hasSessionCookie(r) || hasExplicitAPICredential(r) || isSameOriginBrowserRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "csrf_rejected",
				"message": "The browser request did not originate from this Teldrive server.",
			},
		})
	})
}

// isSafeMethod reports whether method is defined as safe, meaning it is not expected
// to change state. GET, HEAD, OPTIONS and TRACE pass the CSRF check; every other
// method, including ones this server does not implement, must prove its origin.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// hasSessionCookie reports whether r carries a non-blank access or refresh cookie,
// which is what makes the request eligible for cookie authentication and therefore
// subject to the CSRF check. Whitespace-only values do not count, so an expired
// cookie cleared to an empty value cannot force the check on every request. A nil
// request has no cookies.
func hasSessionCookie(r *http.Request) bool {
	if r == nil {
		return false
	}
	if cookie, err := r.Cookie(accessCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return true
	}
	if cookie, err := r.Cookie(refreshCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return true
	}
	return false
}

// hasExplicitAPICredential reports whether r authenticates with a header that a
// browser must be told to set explicitly: Authorization or X-API-Key. Such requests
// are not reachable from a cross-site form, so both the CSRF check and the renewal
// pass leave them alone. A nil request has none.
func hasExplicitAPICredential(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get("Authorization")) != "" || strings.TrimSpace(r.Header.Get("X-API-Key")) != ""
}

// isSameOriginBrowserRequest reports whether r can be attributed to the UI served by
// this server. It first consults the browser-set Sec-Fetch-Site header, where
// same-origin and none count as same-origin (none is a direct navigation or a
// non-browser agent) while cross-site does not. same-site does not count on its own:
// it only says the request came from the same registrable domain, so a sibling
// subdomain of the deployment would pass while still carrying the session cookie,
// which SameSite=Lax permits for same-site requests. Those requests fall through to
// the Origin comparison below. When the header is absent, as in older browsers, the
// Origin header must have a host equal to r.Host and a scheme matching the connection
// security; a missing, unparsable or scheme-mismatched Origin is therefore not
// same-origin. A nil request is not.
func isSameOriginBrowserRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin", "none":
		return true
	case "cross-site":
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
		return false
	}
	expectedScheme := "http"
	if requestIsSecure(r) {
		expectedScheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, expectedScheme)
}
