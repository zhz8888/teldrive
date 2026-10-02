package app

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// httpRequestLogger returns middleware that emits one "http.request" record per
// handled request, with status, latency, request ID and client details. Static UI
// traffic is skipped so asset fetches cannot drown the API log; only paths under
// /v1/, /api/ and /health/ are recorded. A nil logger falls back to slog.Default.
// The record is written from a deferred call, and its level tracks the response:
// error for 5xx, warn for other 4xx, info otherwise.
func httpRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
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
					slog.String("query", r.URL.RawQuery),
					slog.String("ip", r.RemoteAddr),
					slog.String("user_agent", r.UserAgent()),
					slog.Duration("latency", time.Since(started)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
