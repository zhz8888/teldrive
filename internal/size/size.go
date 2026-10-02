// Package size parses and renders byte counts written with human-readable
// binary units.
//
// Configuration exposes cache and upload limits as strings such as "5MB" or
// "1GiB". This package turns those strings into an int64 byte count and renders
// byte counts back into the same notation for generated documentation and
// command-line defaults.
//
// All units are binary: every spelling of a unit (K, KB, KiB) means 1024, never
// 1000.
package size

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Size represents a byte count and accepts human-readable binary units.
type Size int64

// suffixes maps a normalised, upper-case unit spelling to its multiplier in
// bytes. Parse upper-cases the unit before the lookup, so the accepted spellings
// are case-insensitive.
var suffixes = map[string]float64{
	"":    1,
	"B":   1,
	"K":   1 << 10,
	"KB":  1 << 10,
	"KIB": 1 << 10,
	"M":   1 << 20,
	"MB":  1 << 20,
	"MIB": 1 << 20,
	"G":   1 << 30,
	"GB":  1 << 30,
	"GIB": 1 << 30,
	"T":   1 << 40,
	"TB":  1 << 40,
	"TIB": 1 << 40,
}

// Parse converts a value such as "5MB", "1.5 GiB" or "2048" into a byte count.
// The unit is case-insensitive and optional; a missing unit means bytes.
//
// Fractional values are accepted but truncated towards zero, so "1.5B" yields 1.
// An empty string, a value without a leading number, an unknown unit or a value
// that would overflow int64 is rejected with an error prefixed by "size: ", while
// a digit run that strconv.ParseFloat rejects is returned as its bare error.
func Parse(value string) (Size, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errorsf("empty size")
	}

	numberEnd := 0
	for numberEnd < len(value) {
		c := value[numberEnd]
		if (c >= '0' && c <= '9') || c == '.' {
			numberEnd++
			continue
		}
		break
	}
	if numberEnd == 0 {
		return 0, errorsf("invalid size %q", value)
	}

	number, err := strconv.ParseFloat(value[:numberEnd], 64)
	if err != nil {
		return 0, err
	}
	if number < 0 {
		return 0, errorsf("size must be non-negative")
	}

	suffix := strings.ToUpper(strings.TrimSpace(value[numberEnd:]))
	multiplier, ok := suffixes[suffix]
	if !ok {
		return 0, errorsf("unknown size suffix %q", suffix)
	}
	bytes := number * multiplier
	if bytes > math.MaxInt64 {
		return 0, errorsf("size overflows int64")
	}
	return Size(bytes), nil
}

// errorsf builds a Parse error, prefixing the message with "size: " so callers
// can tell configuration unit errors apart from other failures.
func errorsf(format string, args ...any) error {
	return fmt.Errorf("size: "+format, args...)
}

// Set parses value and assigns it to the receiver, which makes Size usable as a
// flag.Value. The receiver is left untouched when parsing fails.
func (s *Size) Set(value string) error {
	parsed, err := Parse(value)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// String renders the byte count using the largest unit it is an exact multiple
// of, falling back to plain bytes. Rendering is lossless but never fractional:
// 1536 becomes "1536B" rather than "1.5KB", and 5<<20 becomes "5MB".
func (s Size) String() string {
	bytes := int64(s)
	units := []struct {
		suffix string
		value  int64
	}{
		{"TB", 1 << 40},
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
	}
	for _, unit := range units {
		if bytes >= unit.value && bytes%unit.value == 0 {
			return strconv.FormatInt(bytes/unit.value, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10) + "B"
}

// UnmarshalText implements encoding.TextUnmarshaler, which lets configuration
// decoders assign a Size field directly from a TOML, YAML or JSON string. It
// behaves exactly like Set.
func (s *Size) UnmarshalText(text []byte) error {
	return s.Set(string(text))
}

// Type reports the value type name for Size when it is used as a pflag.Value.
func (s Size) Type() string { return "Size" }
