package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func TestHTTPRequestLoggerLevelsAndAttributes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		status int
		level  slog.Level
	}{
		{name: "success", status: http.StatusOK, level: slog.LevelInfo},
		{name: "client error", status: http.StatusBadRequest, level: slog.LevelWarn},
		{name: "server error", status: http.StatusInternalServerError, level: slog.LevelError},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordHandler{}
			logger := slog.New(handler)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			})
			wrapped := middleware.RequestID(httpRequestLogger(logger, nil)(next))
			request := httptest.NewRequest(http.MethodGet, "/api/v1/test?q=value", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("User-Agent", "test-agent")
			response := httptest.NewRecorder()

			wrapped.ServeHTTP(response, request)

			if len(handler.records) != 1 {
				t.Fatalf("records = %d, want 1", len(handler.records))
			}
			record := handler.records[0]
			if record.Level != test.level || record.Message != "http.request" {
				t.Fatalf("record = (%s, %q), want (%s, %q)", record.Level, record.Message, test.level, "http.request")
			}
			attrs := map[string]any{}
			record.Attrs(func(attr slog.Attr) bool {
				attrs[attr.Key] = attr.Value.Any()
				return true
			})
			if attrs["status"] != int64(test.status) || attrs["path"] != "/api/v1/test" || attrs["query"] != "q=value" {
				t.Fatalf("attrs = %#v", attrs)
			}
			if attrs["ip"] != "127.0.0.1" {
				t.Fatalf("ip = %#v, want the peer address without its port", attrs["ip"])
			}
			if attrs["request_id"] == "" {
				t.Fatalf("request_id is empty: %#v", attrs)
			}
		})
	}
}

func TestHTTPRequestLoggerSkipsUIRequests(t *testing.T) {
	t.Parallel()

	handler := &recordHandler{}
	logger := slog.New(handler)
	wrapped := httpRequestLogger(logger, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/files", nil))

	if len(handler.records) != 0 {
		t.Fatalf("records = %d, want 0", len(handler.records))
	}
}

// TestHTTPRequestLoggerRedactsCredentialQueryValues pins that a credential passed
// in the query - the event ticket is the one this API accepts there - never
// reaches the access log, while the rest of the query stays readable.
func TestHTTPRequestLoggerRedactsCredentialQueryValues(t *testing.T) {
	t.Parallel()

	handler := &recordHandler{}
	logger := slog.New(handler)
	wrapped := httpRequestLogger(logger, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events?types=file&ticket=live-secret", nil))

	if len(handler.records) != 1 {
		t.Fatalf("records = %d, want 1", len(handler.records))
	}
	attrs := map[string]any{}
	handler.records[0].Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	if attrs["query"] != "ticket=REDACTED&types=file" {
		t.Fatalf("query = %#v, want the ticket value replaced and the rest kept", attrs["query"])
	}
}

// TestRedactQuery pins what the access log records for the query strings the API
// can see: credential values are replaced, everything else survives, and a query
// that does not parse is dropped rather than logged as it came in.
func TestRedactQuery(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "empty query stays empty", query: "", want: ""},
		{name: "ordinary parameters are kept", query: "q=value", want: "q=value"},
		{name: "ticket value is replaced", query: "ticket=live-secret", want: "ticket=REDACTED"},
		{name: "credential names match case-insensitively", query: "Ticket=live-secret&api_key=other", want: "Ticket=REDACTED&api_key=REDACTED"},
		{name: "parameters around a credential survive", query: "types=file&ticket=live-secret&after=5", want: "after=5&ticket=REDACTED&types=file"},
		{name: "repeated credential values are collapsed", query: "token=a&token=b", want: "token=REDACTED"},
		{name: "unparsable query is not logged", query: "ticket=%zz", want: unparsableQueryValue},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := redactQuery(test.query); got != test.want {
				t.Fatalf("redactQuery(%q) = %q, want %q", test.query, got, test.want)
			}
		})
	}
}

// TestClientAddressTrustsForwardedHeaderOnlyFromTrustedProxy pins the rule the log
// follows: the forwarding header is adopted only when the immediate peer is a trusted
// proxy, a direct client cannot choose its own entry, and the recorded address never
// carries a port.
func TestClientAddressTrustsForwardedHeaderOnlyFromTrustedProxy(t *testing.T) {
	t.Parallel()

	security, err := newRequestSecurity([]string{"10.0.0.1", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("newRequestSecurity() error = %v", err)
	}
	for _, test := range []struct {
		name       string
		remoteAddr string
		forwarded  string
		security   *requestSecurity
		want       string
	}{
		{name: "direct peer keeps its address", remoteAddr: "198.51.100.7:5555", forwarded: "203.0.113.9", want: "198.51.100.7"},
		{name: "trusted proxy reports the client", remoteAddr: "10.0.0.1:5555", forwarded: "203.0.113.9", want: "203.0.113.9", security: security},
		{name: "trusted proxy entry with port is trimmed", remoteAddr: "10.0.0.1:5555", forwarded: "203.0.113.9:4444, 10.0.0.1", want: "203.0.113.9", security: security},
		{name: "trusted IPv6 proxy reports the client", remoteAddr: "[2001:db8::1]:5555", forwarded: "2001:db8::9", want: "2001:db8::9", security: security},
		{name: "trusted proxy without a header falls back to its peer", remoteAddr: "10.0.0.1:5555", want: "10.0.0.1", security: security},
		{name: "malformed header from a trusted proxy is ignored", remoteAddr: "10.0.0.1:5555", forwarded: "not-an-address", want: "10.0.0.1", security: security},
		{name: "untrusted peer cannot spoof the header", remoteAddr: "198.51.100.7:5555", forwarded: "203.0.113.9", want: "198.51.100.7", security: security},
		{name: "no security trusts no proxy", remoteAddr: "10.0.0.1:5555", forwarded: "203.0.113.9", want: "10.0.0.1"},
		{name: "nil request has no address", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var request *http.Request
			if test.remoteAddr != "" || test.forwarded != "" {
				request = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
				request.RemoteAddr = test.remoteAddr
				if test.forwarded != "" {
					request.Header.Set("X-Forwarded-For", test.forwarded)
				}
			}
			if got := clientAddress(request, test.security); got != test.want {
				t.Fatalf("clientAddress() = %q, want %q", got, test.want)
			}
		})
	}
}
