// Package logging builds the application's slog logger for the JSON and text
// output formats selected at startup.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds a slog logger writing to output at the minimum level named
// by levelText, using the handler selected by format. An empty format and
// "json" both select the JSON handler; "text" selects the human-readable
// handler, which colors its output only when output is a terminal. Both
// levelText and format are trimmed and matched case-insensitively. It returns
// an error if output is nil, levelText is not a recognized slog level, or
// format is neither JSON nor text; the returned logger is safe for concurrent
// use by multiple goroutines.
func NewLogger(output io.Writer, levelText, format string) (*slog.Logger, error) {
	if output == nil {
		return nil, fmt.Errorf("logger output is required")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(levelText))); err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		handler = slog.NewJSONHandler(output, opts)
	case "text":
		handler = newPrettyHandler(output, level, supportsColor(output))
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
	return slog.New(handler), nil
}
