// Package main generates docs/content/docs/configuration/reference.mdx from the
// server configuration structs. It is a build-time tool invoked by
// `just docs-generate` (part of `just generate`); it renders the Koanf, Validate,
// and Description struct tags of the same structs the config loader reads, and it
// derives the config key, flag, and environment variable names through the
// exported helpers of internal/config, so the page cannot drift from the names
// the loader accepts.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/size"
)

// row is one leaf configuration setting as rendered in a table row. Rows are
// sorted by section and config key before output.
type row struct {
	// section is the heading this setting is listed under, named after the
	// top-level configuration group.
	section string
	// configKey is the dotted key as it appears in the configuration file.
	configKey string
	// flag is the equivalent command-line flag, including the leading "--".
	flag string
	// env is the equivalent environment variable, including the "TELDRIVE_"
	// prefix.
	env string
	// defaultVal is the formatted runtime default, empty when there is none.
	defaultVal string
	// description is the value of the field's `description` struct tag.
	description string
	// validation is the value of the field's `validate` struct tag.
	validation string
}

// main walks the default configuration with reflection, writes the generated
// reference page, and panics if the output path cannot be written. It must run
// from the repository root because the output path is relative, which the
// justfile guarantees.
func main() {
	cfg := config.Default()
	rows := collect(reflect.ValueOf(cfg), reflect.TypeFor[config.Config](), "", "")
	slices.SortFunc(rows, compareRows)

	out := filepath.Join("docs", "content", "docs", "configuration", "reference.mdx")
	if err := os.WriteFile(out, []byte(renderReference(rows)), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("generated %s (%d settings)\n", out, len(rows))
}

// compareRows orders rows by section and then by config key. Sorting on the
// section first keeps the rows of one section contiguous even when a top-level
// config key contains a dot, so renderReference only has to compare a row with
// the previous section before writing a heading.
func compareRows(a, b row) int {
	if order := strings.Compare(a.section, b.section); order != 0 {
		return order
	}
	return strings.Compare(a.configKey, b.configKey)
}

// renderReference renders the whole reference page: the front matter and
// preamble, then one heading and table per section. A heading is written the
// first time a section appears, which is enough because compareRows makes every
// section contiguous.
func renderReference(rows []row) string {
	var b strings.Builder
	b.WriteString("---\ntitle: \"CLI, environment & config reference\"\ndescription: Complete generated mapping of Teldrive config keys to command-line flags and TELDRIVE_ environment variables.\n---\n\n")
	b.WriteString("Generated from the server configuration structs. Precedence: **defaults < config file < environment < explicit CLI flags**.\n\n")
	b.WriteString("Name mapping example: `http.address` → `TELDRIVE_HTTP_ADDRESS` → `--http-address`. Slices use comma-separated values; encryption maps use `version:key` entries.\n\n")
	b.WriteString("The **Default** column is the runtime default, not a production recommendation.\n\n")

	lastSection := ""
	for _, r := range rows {
		if r.section != lastSection {
			lastSection = r.section
			b.WriteString("## " + title(lastSection) + "\n\n")
			b.WriteString("| Config key | CLI flag | Environment variable | Default | Validation | Description |\n")
			b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s | %s |\n",
			escape(r.configKey), escape(r.flag), escape(r.env), codeOrDash(r.defaultVal), codeOrDash(r.validation), escape(r.description))
	}
	return b.String()
}

// collect recursively walks t and v and returns one row per leaf setting.
// Fields tagged `koanf:"-"` are skipped, exactly as the loader skips them, and
// nested structs are descended into instead of being emitted. path accumulates
// the dotted config key, and the first segment of that key becomes the section
// heading.
func collect(v reflect.Value, t reflect.Type, path, section string) []row {
	var rows []row
	for i := range t.NumField() {
		f := t.Field(i)
		key := config.FieldKey(f)
		if key == "-" {
			continue
		}
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		childSection := section
		if childSection == "" {
			childSection = key
		}
		fv := v.Field(i)
		if config.IsNestedStruct(f.Type) {
			rows = append(rows, collect(fv, f.Type, childPath, childSection)...)
			continue
		}
		rows = append(rows, row{
			section:     childSection,
			configKey:   childPath,
			flag:        "--" + strings.ReplaceAll(childPath, ".", "-"),
			env:         "TELDRIVE_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(childPath)),
			defaultVal:  formatValue(fv),
			description: f.Tag.Get("description"),
			validation:  f.Tag.Get("validate"),
		})
	}
	return rows
}

// formatValue renders the default value of a leaf setting for the Default
// column. Duration and size values use their human-readable String forms,
// slices are joined with commas, and empty slices and maps yield an empty
// string, which codeOrDash turns into an em dash.
func formatValue(v reflect.Value) string {
	if v.Type() == reflect.TypeFor[time.Duration]() {
		return time.Duration(v.Int()).String()
	}
	if v.Type() == reflect.TypeFor[size.Size]() {
		return v.Interface().(size.Size).String()
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return fmt.Sprint(v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		return fmt.Sprint(v.Int())
	case reflect.Slice:
		if v.Len() == 0 {
			return ""
		}
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = fmt.Sprint(v.Index(i).Interface())
		}
		return strings.Join(parts, ",")
	case reflect.Map:
		if v.Len() == 0 {
			return ""
		}
		return fmt.Sprint(v.Interface())
	default:
		return fmt.Sprint(v.Interface())
	}
}

// title converts a kebab-case section key into a markdown heading, capitalizing
// each dash-separated word and spelling "http" as the acronym. An empty segment,
// which a key can contain when it repeats a dash, is left as it is instead of
// being indexed, so no key can panic the generator.
func title(s string) string {
	parts := strings.Split(s, "-")
	for i := range parts {
		if parts[i] == "" {
			continue
		}
		if parts[i] == "http" {
			parts[i] = "HTTP"
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, " ")
}

// codeOrDash renders s as an inline code span, or as an em dash when s is empty
// so that blank cells stay visible in the generated table.
func codeOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return "`" + escape(s) + "`"
}

// escape makes s safe for a markdown table cell by escaping pipes and folding
// newlines into spaces.
func escape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
