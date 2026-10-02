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
	out := renderReference(rows)
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
