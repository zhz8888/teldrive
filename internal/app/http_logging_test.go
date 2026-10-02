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
