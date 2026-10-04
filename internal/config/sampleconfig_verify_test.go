package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/tgdrive/teldrive/v2/internal/config"
)

// knownConfigKeys returns every leaf configuration path, rendered the way a
// sample file spells it: "uploads.default-part-size".
func knownConfigKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	var walk func(typ reflect.Type, prefix string)
	walk = func(typ reflect.Type, prefix string) {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			key := config.FieldKey(field)
			if key == "-" {
				continue
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if config.IsNestedStruct(field.Type) {
				walk(field.Type, path)
				continue
			}
			keys[path] = true
		}
	}
	walk(reflect.TypeFor[config.Config](), "")
	return keys
}

// sampleKeys reads a shipped sample and returns its leaf paths.
func sampleKeys(t *testing.T, name string) []string {
	t.Helper()
	k := koanf.New(".")
	var parser koanf.Parser = yaml.Parser()
	if strings.HasSuffix(name, ".toml") {
		parser = toml.Parser()
	}
	if err := k.Load(file.Provider(filepath.Join("..", "..", name)), parser); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	flat := k.All()
	out := make([]string, 0, len(flat))
	for path := range flat {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// TestShippedSamplesOnlyUseKnownKeys fails when a sample config sets a key the
// server does not read. The loader does not reject unknown keys, so a stale key
// in a shipped sample is silently dropped and the operator believes a setting is
// in effect when it is not.
func TestShippedSamplesOnlyUseKnownKeys(t *testing.T) {
	known := knownConfigKeys(t)
	for _, name := range []string{"config.sample.yaml", "config.sample.toml"} {
		t.Run(name, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join("..", "..", name)); err != nil {
				t.Skipf("sample not present: %v", err)
			}
			for _, key := range sampleKeys(t, name) {
				if !known[key] {
					t.Errorf("%s sets %q, which is not a configuration key the server reads", name, key)
				}
			}
		})
	}
}

// TestShippedSamplesDecode checks that the shipped samples still load with the
// real loader once the required secrets are supplied.
func TestShippedSamplesDecode(t *testing.T) {
	for _, name := range []string{"config.sample.yaml", "config.sample.toml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", name))
			if err != nil {
				t.Skipf("sample not present: %v", err)
			}
			// resolveConfigPath only discovers config.{toml,yaml,yml}, so the
			// sample is copied under the name it would have in a deployment.
			dir := t.TempDir()
			target := filepath.Join(dir, "config"+filepath.Ext(name))
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatalf("write sample: %v", err)
			}
			t.Chdir(dir)
			t.Setenv("TELDRIVE_DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")
			t.Setenv("TELDRIVE_SECURITY_SIGNING_KEY", "0123456789abcdef0123456789abcdef")
			t.Setenv("TELDRIVE_SECURITY_DATA_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")

			if _, err := config.Load(); err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
		})
	}
}
