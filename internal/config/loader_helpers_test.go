package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/tgdrive/teldrive/v2/internal/size"
)

func TestDecodeSizeVariants(t *testing.T) {
	t.Parallel()
	target := reflect.TypeFor[size.Size]()
	other := reflect.TypeFor[string]()

	if got, err := decodeSize(nil, other, "1MiB"); err != nil || got != "1MiB" {
		t.Fatalf("non-size decode = %#v, %v", got, err)
	}
	tests := []struct {
		input any
		want  size.Size
	}{
		{input: "1MiB", want: size.Size(1 << 20)},
		{input: int(12), want: 12},
		{input: int64(13), want: 13},
		{input: float64(14), want: 14},
	}
	for _, test := range tests {
		got, err := decodeSize(nil, target, test.input)
		if err != nil || got != test.want {
			t.Fatalf("decodeSize(%#v) = %#v, %v, want %v", test.input, got, err, test.want)
		}
	}
	marker := struct{}{}
	if got, err := decodeSize(nil, target, marker); err != nil || got != marker {
		t.Fatalf("default decode = %#v, %v", got, err)
	}
	if _, err := decodeSize(nil, target, "not-a-size"); err == nil {
		t.Fatal("invalid size was accepted")
	}
}

func TestDecodeEncryptionKeysVariants(t *testing.T) {
	t.Parallel()
	target := reflect.TypeFor[map[int32]string]()
	other := reflect.TypeFor[string]()
	if got, err := decodeEncryptionKeys(nil, other, "1:key"); err != nil || got != "1:key" {
		t.Fatalf("non-key decode = %#v, %v", got, err)
	}
	got, err := decodeEncryptionKeys(nil, target, "2:beta,1:alpha")
	if err != nil {
		t.Fatal(err)
	}
	keys := got.(map[int32]string)
	if keys[1] != "alpha" || keys[2] != "beta" {
		t.Fatalf("string keys = %#v", keys)
	}
	got, err = decodeEncryptionKeys(nil, target, map[string]any{"3": "gamma"})
	if err != nil || got.(map[int32]string)[3] != "gamma" {
		t.Fatalf("map keys = %#v, %v", got, err)
	}
	for _, invalid := range []map[string]any{
		{"bad": "key"}, {"0": "key"}, {"1": ""}, {"1": 123},
	} {
		if _, err := decodeEncryptionKeys(nil, target, invalid); err == nil {
			t.Fatalf("invalid key map accepted: %#v", invalid)
		}
	}
	marker := 17
	if got, err := decodeEncryptionKeys(nil, target, marker); err != nil || got != marker {
		t.Fatalf("default key decode = %#v, %v", got, err)
	}
	if formatted := formatEncryptionKeys(map[int32]string{2: "beta", 1: "alpha"}); formatted != "1:alpha,2:beta" {
		t.Fatalf("formatEncryptionKeys() = %q", formatted)
	}
}

func TestLoaderProviderAndKeyHelpers(t *testing.T) {
	t.Parallel()
	provider := staticProvider{values: map[string]any{"x": 1}}
	if values, err := provider.Read(); err != nil || values["x"] != 1 {
		t.Fatalf("static Read() = %#v, %v", values, err)
	}
	if data, err := provider.ReadBytes(); err != nil || data != nil {
		t.Fatalf("static ReadBytes() = %#v, %v", data, err)
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("name", "value", "")
	flagSource := &flagProvider{flags: flags, flagMap: map[string]string{"name": "section.name"}}
	values, err := flagSource.Read()
	if err != nil || values["section"].(map[string]any)["name"] != "value" {
		t.Fatalf("flag Read() = %#v, %v", values, err)
	}
	if data, err := flagSource.ReadBytes(); err != nil || data != nil {
		t.Fatalf("flag ReadBytes() = %#v, %v", data, err)
	}

	if got := toKebab("HTTPServerURL"); got != "http-server-url" {
		t.Fatalf("toKebab() = %q", got)
	}
	if got := normalizeKey("Some-Key_Name"); got != "somekeyname" {
		t.Fatalf("normalizeKey() = %q", got)
	}
	if got := joinPath("", "key"); got != "key" {
		t.Fatalf("joinPath root = %q", got)
	}
	if got := joinPath("parent", "key"); got != "parent.key" {
		t.Fatalf("joinPath nested = %q", got)
	}
	if !IsNestedStruct(reflect.TypeFor[struct{ Value string }]()) || IsNestedStruct(reflect.TypeFor[time.Duration]()) || IsNestedStruct(reflect.TypeFor[size.Size]()) {
		t.Fatal("IsNestedStruct() classification is incorrect")
	}
}

func TestKoanfDashTagIsIgnored(t *testing.T) {
	t.Parallel()
	type sample struct {
		Visible string `koanf:"visible" description:"Visible setting"`
		Hidden  string `koanf:"-"`
	}
	value := reflect.ValueOf(sample{Visible: "v", Hidden: "h"})
	fieldType := reflect.TypeFor[sample]()

	if key := FieldKey(fieldType.Field(1)); key != "-" {
		t.Fatalf("FieldKey() = %q, want -", key)
	}
	if got := structMap(value, fieldType); len(got) != 1 || got["visible"] != "v" {
		t.Fatalf("structMap() = %#v", got)
	}

	loader := newLoader(func(string) (string, bool) { return "", false }, func() (string, error) { return "", nil })
	loader.generateEnvMap(fieldType, "", "")
	if len(loader.envMap) != 1 || loader.envMap["VISIBLE"] != "visible" {
		t.Fatalf("generateEnvMap() = %#v", loader.envMap)
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	loader.registerStruct(flags, "", value, fieldType)
	if flags.Lookup("") != nil || len(loader.flagMap) != 1 || loader.flagMap["visible"] != "visible" {
		t.Fatalf("registerStruct() flags = %#v, flagMap = %#v", flags.Lookup(""), loader.flagMap)
	}
}

func TestIgnoredFieldIsAbsentFromFlagsAndEnvironment(t *testing.T) {
	t.Parallel()
	loader := NewLoader()
	flags := pflag.NewFlagSet("teldrive", pflag.ContinueOnError)
	loader.RegisterFlags(flags)
	flags.VisitAll(func(flag *pflag.Flag) {
		if strings.Contains(flag.Name, "--") {
			t.Errorf("registered flag %q for a field tagged koanf:\"-\"", flag.Name)
		}
	})
	for name, path := range loader.flagMap {
		if strings.Contains(name, "--") || strings.Contains(path, ".-") {
			t.Errorf("flag map entry %q -> %q for a field tagged koanf:\"-\"", name, path)
		}
	}
	loader.generateEnvMap(reflect.TypeFor[Config](), "", "")
	for name := range loader.envMap {
		if strings.HasSuffix(name, "_") {
			t.Errorf("generated environment variable TELDRIVE_%s for a field tagged koanf:\"-\"", name)
		}
	}
	database, ok := structMap(reflect.ValueOf(Default()), reflect.TypeFor[Config]())["database"].(map[string]any)
	if !ok {
		t.Fatal("structMap() has no database section")
	}
	if _, exists := database["-"]; exists {
		t.Error("structMap() kept the database default of a field tagged koanf:\"-\"")
	}
}
