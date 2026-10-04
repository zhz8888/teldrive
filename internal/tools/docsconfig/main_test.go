package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTitleToleratesEmptySegment(t *testing.T) {
	t.Parallel()
	if got := title("foo--bar"); got != "Foo  Bar" {
		t.Fatalf("title(\"foo--bar\") = %q", got)
	}
	if got := title(""); got != "" {
		t.Fatalf("title(\"\") = %q", got)
	}
}

func TestRenderReferenceWritesOneHeadingPerSection(t *testing.T) {
	t.Parallel()
	rows := []row{
		{section: "http.extra", configKey: "http.extra.flag"},
		{section: "http", configKey: "http.timeout"},
		{section: "http", configKey: "http.address"},
	}
	slices.SortFunc(rows, compareRows)
	out := renderReference(rows, locales[0])
	for _, heading := range []string{"## HTTP\n", "## Http.extra\n"} {
		if count := strings.Count(out, heading); count != 1 {
			t.Fatalf("heading %q appears %d times in:\n%s", heading, count, out)
		}
	}
	address := strings.Index(out, "`http.address`")
	timeout := strings.Index(out, "`http.timeout`")
	extra := strings.Index(out, "`http.extra.flag`")
	if address < 0 || timeout < 0 || extra < 0 || address > timeout || timeout > extra {
		t.Fatalf("row order is wrong: address=%d timeout=%d extra=%d", address, timeout, extra)
	}
}

// A locale must translate what it has and fall back to the struct tag for what
// it does not, so a partial translation degrades to English instead of rendering
// an empty cell that reads as "no description".
func TestRenderReferenceFallsBackToStructTag(t *testing.T) {
	t.Parallel()
	zh := locales[1]
	if zh.code != "zh" {
		t.Fatalf("expected the second locale to be zh, got %q", zh.code)
	}
	row := row{section: "http", configKey: "http.not-yet-translated", description: "English text"}
	if got := zh.describe(row); got != "English text" {
		t.Fatalf("untranslated key rendered as %q", got)
	}
	row.configKey = "http.write-timeout"
	if got := zh.describe(row); !strings.Contains(got, "响应") {
		t.Fatalf("translated key rendered as %q", got)
	}
}

// The generated columns must be locale-independent, so the Chinese page can
// never drift from the names the loader accepts.
func TestRenderReferenceKeepsNamesIdenticalAcrossLocales(t *testing.T) {
	t.Parallel()
	rows := []row{
		{section: "http", configKey: "http.address", flag: "--http-address", env: "TELDRIVE_HTTP_ADDRESS", defaultVal: "127.0.0.1:8080"},
		{section: "http", configKey: "http.write-timeout", flag: "--http-write-timeout", env: "TELDRIVE_HTTP_WRITE_TIMEOUT", defaultVal: "1h0m0s"},
	}
	slices.SortFunc(rows, compareRows)

	column := func(out, key string) string {
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(line, "| `"+key+"`") {
				cells := strings.Split(line, "|")
				// Drop the empty cells either side of the row.
				return strings.Join(cells[2:6], "|")
			}
		}
		t.Fatalf("row %q missing from:\n%s", key, out)
		return ""
	}

	for _, key := range []string{"http.address", "http.write-timeout"} {
		en := column(renderReference(rows, locales[0]), key)
		zh := column(renderReference(rows, locales[1]), key)
		if en != zh {
			t.Fatalf("row %q differs across locales:\nen: %s\nzh: %s", key, en, zh)
		}
	}
}

func TestCollectSkipsIgnoredFields(t *testing.T) {
	t.Parallel()
	type sample struct {
		Visible string `koanf:"visible" description:"Shown"`
		Hidden  string `koanf:"-"`
	}
	rows := collect(reflect.ValueOf(sample{Visible: "v", Hidden: "h"}), reflect.TypeFor[sample](), "", "")
	if len(rows) != 1 || rows[0].configKey != "visible" || rows[0].section != "visible" {
		t.Fatalf("collect() = %#v", rows)
	}
}
