package app

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// redactedQueryValue replaces the value of every credential-bearing query
// parameter in the access log, keeping the parameter name so an operator can still
// see that the credential was presented.
const redactedQueryValue = "REDACTED"

// unparsableQueryValue is logged instead of a query string that cannot be parsed.
// A malformed credential cannot be recognised reliably, so nothing of such a query
// is recorded.
const unparsableQueryValue = "unparsable"

// credentialQueryKeys names the query parameters whose values are credentials and
// must never be written to the access log. "ticket" is the one this API accepts in
// a query (typespec declares EventTicketAuth as a query API key), and it stays
// valid until it expires, so logging it verbatim would persist a live
// bearer-equivalent secret. The other names cover the same kind of secret under the
// spellings other clients use.
var credentialQueryKeys = map[string]struct{}{
	"ticket":       {},
	"token":        {},
	"access_token": {},
	"api_key":      {},
}

// redactQuery returns rawQuery in the form the access log may record: every
// credential parameter keeps its name and loses its value, and the remaining
// parameters are re-encoded, which sorts them by name because that is what
// url.Values.Encode does. An empty query stays empty, and a query that does not
// parse is replaced by unparsableQueryValue so a malformed credential is never
// logged verbatim.
func redactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return unparsableQueryValue
	}
	for key := range values {
		if _, ok := credentialQueryKeys[strings.ToLower(key)]; ok {
			values[key] = []string{redactedQueryValue}
		}
	}
	return values.Encode()
}

// httpRequestLogger returns middleware that emits one "http.request" record per
// handled request, with status, latency, request ID and client details. Static UI
// traffic is skipped so asset fetches cannot drown the API log; only paths under
// /v1/, /api/ and /health/ are recorded. A nil logger falls back to slog.Default.
// The record is written from a deferred call, and its level tracks the response:
// error for 5xx, warn for other 4xx, info otherwise.
//
// The client address comes from security, which is the same trusted-proxy decision
// the rest of the chain uses: the forwarding header is believed only when the
// immediate peer is a configured trusted proxy, so a client that reaches the server
// directly cannot choose what the log says. A nil security trusts no proxy.
//
// The query string is logged through redactQuery, because this API accepts the
// event-stream ticket as a query parameter and that ticket is a bearer-equivalent
// credential until it expires: its value is replaced, everything else in the query
// is kept.
func httpRequestLogger(logger *slog.Logger, security *requestSecurity) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/v1/") && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/health/") {
				next.ServeHTTP(w, r)
				return
			}
			started := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				status := ww.Status()
				level := slog.LevelInfo
				if status >= http.StatusInternalServerError {
					level = slog.LevelError
				} else if status >= http.StatusBadRequest {
					level = slog.LevelWarn
				}

				logger.LogAttrs(r.Context(), level, "http.request",
					slog.Int("status", status),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("query", redactQuery(r.URL.RawQuery)),
					slog.String("ip", clientAddress(r, security)),
					slog.String("user_agent", r.UserAgent()),
					slog.Duration("latency", time.Since(started)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

// clientAddress returns the client address recorded in the request log, without the
// port. When the immediate peer is a trusted proxy it is the first X-Forwarded-For
// entry, which is the address the original client used according to the proxy chain;
// for every other peer, including a direct connection, the header is ignored and the
// connection peer is reported instead. That is the same stance requestSecurity takes
// for the scheme: forwarding headers are only ever believed from a trusted proxy, so
// a client cannot choose its own log entry. Only a well-formed IP is taken from the
// header, so a malformed value cannot inject arbitrary text into the log, and an
// absent or unparsable peer address is reported exactly as written.
func clientAddress(r *http.Request, security *requestSecurity) string {
	if r == nil {
		return ""
	}
	peer := hostWithoutPort(r.RemoteAddr)
	if security == nil || !security.isTrustedProxy(r) {
		return peer
	}
	forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	if address, err := netip.ParseAddr(hostWithoutPort(forwarded)); err == nil {
		return address.String()
	}
	return peer
}

// hostWithoutPort strips the port from an address, returning the address as written
// when it carries none. Both a bare IP and a bracketed IPv6 host are accepted, so the
// same helper serves the peer address and a forwarded-for entry.
func hostWithoutPort(address string) string {
	address = strings.TrimSpace(address)
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}
