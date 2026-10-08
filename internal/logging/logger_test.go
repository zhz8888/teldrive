package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestPrettyHandlerLineFormat pins the text format: a bracketed timestamp with
// an underscore between date and time, a bracketed level, the message, and the
// attributes after it. Changing any of those parts has to be deliberate.
func TestPrettyHandlerLineFormat(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	handler := newPrettyHandler(&out, slog.LevelDebug, false)
	record := slog.NewRecord(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), slog.LevelWarn, "request failed", 0)
	record.Add("status", 401)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	want := "[2026-08-27_10:00:00] [WARN] request failed  status=401\n"
	if got := out.String(); got != want {
		t.Fatalf("pretty output = %q, want %q", got, want)
	}
}

// TestPrettyHandlerLabelsEveryLevel checks that each level renders as its own
// bracketed name rather than a symbol, and that the timestamp keeps the
// date_time shape the format promises.
func TestPrettyHandlerLabelsEveryLevel(t *testing.T) {
	t.Parallel()

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		var out bytes.Buffer
		handler := newPrettyHandler(&out, slog.LevelDebug, false)
		record := slog.NewRecord(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), level, "event", 0)
		if err := handler.Handle(context.Background(), record); err != nil {
			t.Fatalf("Handle() error = %v", err)
		}

		want := "[2026-08-27_10:00:00] [" + strings.ToUpper(level.String()) + "] event\n"
		if got := out.String(); got != want {
			t.Fatalf("level %v output = %q, want %q", level, got, want)
		}
	}
}

func TestPrettyHandlerColorsLevels(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	handler := newPrettyHandler(&out, slog.LevelDebug, true)
	logger := slog.New(handler)
	logger.Log(context.Background(), slog.LevelWarn, "request failed", "status", 401)

	got := out.String()
	if !strings.Contains(got, ansiYellow+"WARN"+ansiReset) {
		t.Fatalf("colored WARN level missing from %q", got)
	}
	if !strings.Contains(got, ansiGray+"status="+ansiReset+"401") {
		t.Fatalf("colored attribute key missing from %q", got)
	}
}

func TestPrettyHandlerWithoutColor(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	handler := newPrettyHandler(&out, slog.LevelInfo, false)
	record := slog.NewRecord(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), slog.LevelInfo, "started", 0)
	record.Add("version", "1.2.3")
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("unexpected ANSI color in %q", got)
	}
	if !strings.Contains(got, "[INFO] started") || !strings.Contains(got, "version=1.2.3") {
		t.Fatalf("pretty output = %q", got)
	}
}

func TestJSONLoggerRemainsUncolored(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	logger, err := NewLogger(&out, "info", "json")
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	logger.Info("started", "status", 200)

	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("JSON output contains ANSI color: %q", out.String())
	}
}
