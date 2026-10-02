package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// ANSI SGR sequences used to colorize terminal output; the color is dropped
// when the handler is built without color support.
const (
	// ansiReset clears all attributes set by a preceding color.
	ansiReset = "\x1b[0m"
	// ansiGray dims attribute keys so values stand out.
	ansiGray = "\x1b[90m"
	// ansiMagenta colors the DEBUG level.
	ansiMagenta = "\x1b[35m"
	// ansiGreen colors the INFO level.
	ansiGreen = "\x1b[32m"
	// ansiYellow colors the WARN level.
	ansiYellow = "\x1b[33m"
	// ansiRed colors the ERROR level.
	ansiRed = "\x1b[31m"
	// ansiWhite colors any level not covered by the cases above.
	ansiWhite = "\x1b[37m"
)

// prettyHandler is the slog.Handler behind the "text" log format: one line per
// record with a timestamp, level icon, message, and key=value attributes.
// Methods that derive a handler clone shared state, so Enabled, Handle, and
// the With* methods may be called from multiple goroutines; Handle assembles a
// record into one string and holds mu around the single write, so records cannot
// interleave on a shared out.
type prettyHandler struct {
	out    io.Writer
	level  slog.Leveler
	color  bool
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

// newPrettyHandler returns a handler that writes records at or above level to
// out and emits ANSI colors only when color is true. The mutex is allocated
// here and shared by every clone returned from WithAttrs and WithGroup.
func newPrettyHandler(out io.Writer, level slog.Leveler, color bool) slog.Handler {
	return &prettyHandler{out: out, level: level, color: color, mu: &sync.Mutex{}}
}

// supportsColor reports whether out is a terminal that understands ANSI
// escapes. Writers that expose no file descriptor, such as buffers and pipes
// used by tests, report false so their output stays free of escape sequences.
func supportsColor(out io.Writer) bool {
	fd, ok := out.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return isatty.IsTerminal(fd.Fd()) || isatty.IsCygwinTerminal(fd.Fd())
}

// Enabled reports whether a record at level passes the handler's level filter.
func (h *prettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle formats record as a single line and writes it to the handler's output.
// Attributes attached with WithAttrs come first, followed by the record's own
// attributes; groups are joined with their children using dots. It returns the
// write error, if any, and never mutates the record.
func (h *prettyHandler) Handle(_ context.Context, record slog.Record) error {
	icon, levelText, color := prettyLevel(record.Level)
	if !h.color {
		color = ""
	}

	var line strings.Builder
	line.WriteString(record.Time.Format("2006-01-02 15:04:05"))
	line.WriteString("  ")
	line.WriteString(icon)
	line.WriteString(" ")
	if color != "" {
		line.WriteString(color)
	}
	line.WriteString(fmt.Sprintf("%-5s", levelText))
	if color != "" {
		line.WriteString(ansiReset)
	}
	line.WriteString("  ")
	line.WriteString(record.Message)

	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	for _, attr := range attrs {
		h.appendAttr(&line, h.groups, attr)
	}
	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, line.String())
	return err
}

// WithAttrs returns a handler that prints attrs on every subsequent record.
// The receiver is left untouched: the clone copies the existing attribute
// slice and the mutex pointer it shares with the original handler.
func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

// WithGroup returns a handler that prefixes the keys of subsequent attributes
// with name and a dot. An empty name returns the receiver itself, matching the
// slog.Handler contract.
func (h *prettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

// appendAttr renders one attribute as "  key=value" on line. It resolves
// LogValuer values first and drops an attribute only when it equals the zero
// attribute, which requires both an empty key and a zero value; a keyed value
// such as a nil error is still rendered. Group attributes recurse with the group
// key pushed onto groups; a group with no key passes its own name down instead of
// leaving an empty key segment.
func (h *prettyHandler) appendAttr(line *strings.Builder, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		nested := append(append([]string(nil), groups...), attr.Key)
		for _, child := range attr.Value.Group() {
			h.appendAttr(line, nested, child)
		}
		return
	}

	keyParts := append([]string(nil), groups...)
	if attr.Key != "" {
		keyParts = append(keyParts, attr.Key)
	}
	key := strings.Join(keyParts, ".")
	if key == "" {
		return
	}

	line.WriteString("  ")
	if h.color {
		line.WriteString(ansiGray)
	}
	line.WriteString(key)
	line.WriteString("=")
	if h.color {
		line.WriteString(ansiReset)
	}
	line.WriteString(formatSlogValue(attr.Value))
}

// prettyLevel maps a level to the icon, five-character label, and ANSI color
// used for it. Levels below DEBUG and at or above ERROR saturate to the nearest
// known level, and the final branch is unreachable because slog levels are
// integers.
func prettyLevel(level slog.Level) (icon, text, color string) {
	switch {
	case level <= slog.LevelDebug:
		return "🐛", "DEBUG", ansiMagenta
	case level < slog.LevelWarn:
		return "✓", "INFO", ansiGreen
	case level < slog.LevelError:
		return "⚠", "WARN", ansiYellow
	case level >= slog.LevelError:
		return "✗", "ERROR", ansiRed
	default:
		return "·", strings.ToUpper(level.String()), ansiWhite
	}
}

// formatSlogValue renders a slog value for the key=value tail of a log line
// without quoting it: strings and errors verbatim, numbers in decimal except
// floats which use %g, durations in Go duration form, and times in RFC 3339.
// Unknown kinds fall back to their slog string representation.
func formatSlogValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindBool:
		return fmt.Sprintf("%t", value.Bool())
	case slog.KindInt64:
		return fmt.Sprintf("%d", value.Int64())
	case slog.KindUint64:
		return fmt.Sprintf("%d", value.Uint64())
	case slog.KindFloat64:
		return fmt.Sprintf("%g", value.Float64())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339)
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(value.Any())
	default:
		return value.String()
	}
}
