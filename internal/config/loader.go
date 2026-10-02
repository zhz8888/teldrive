package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
	stdmaps "maps"

	"github.com/tgdrive/teldrive/v2/internal/size"
)

// defaultConfigPath is the location advertised in the --config flag help text.
// It is never resolved as written: resolveConfigPath discovers the file through
// the user's home directory instead.
const defaultConfigPath = "$HOME/.teldrive/config.toml"

// ignoredKey is the `koanf` tag value that opts a field out of configuration.
// mapstructure skips such fields while decoding, so the loader must not expose
// them as flags, environment variables, or defaults either.
const ignoredKey = "-"

var (
	// matchFirstCap matches the boundary in front of a capitalized word, so
	// "ReadTimeout" becomes "Read-Timeout".
	matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
	// matchAllCap matches the boundary between a lower-case letter or digit and
	// an upper-case letter, so "readTimeout" becomes "read-Timeout".
	matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")
)

// Loader resolves the configuration from four sources of increasing precedence:
// built-in defaults, the config file, TELDRIVE_ environment variables, and the
// pflags registered by RegisterFlags. The lookup and homeDir fields are seams
// that keep resolution testable without touching the real process environment,
// so callers should build a Loader with NewLoader rather than directly.
type Loader struct {
	// lookup reads an environment variable and reports whether it was set;
	// production uses os.LookupEnv and tests inject a stub.
	lookup func(string) (string, bool)
	// homeDir returns the directory searched for .teldrive/config.toml;
	// production uses os.UserHomeDir.
	homeDir func() (string, error)
	// flagMap maps a registered pflag name such as "http-address" to the dotted
	// koanf path it feeds, such as "http.address".
	flagMap map[string]string
	// envMap maps a TELDRIVE_-relative variable name such as "HTTP_ADDRESS" to
	// the dotted koanf path it feeds.
	envMap map[string]string
}

// NewLoader returns a Loader backed by the real process environment and the
// current user's home directory. It reads nothing until RegisterFlags or Load is
// called.
func NewLoader() *Loader {
	return newLoader(os.LookupEnv, os.UserHomeDir)
}

// newLoader builds a Loader from explicit environment and home-directory
// accessors so tests can supply deterministic sources. Both accessors must be
// non-nil, because Load rejects a Loader that has either one unset.
func newLoader(lookup func(string) (string, bool), homeDir func() (string, error)) *Loader {
	return &Loader{
		lookup:  lookup,
		homeDir: homeDir,
		flagMap: make(map[string]string),
		envMap:  make(map[string]string),
	}
}

// RegisterFlags exposes every configuration leaf as a kebab-case pflag. The
// same path maps to a TOML/YAML key and TELDRIVE_ environment variable.
func (l *Loader) RegisterFlags(flags *pflag.FlagSet) {
	if flags == nil {
		return
	}
	flags.StringP("config", "c", "", "Config file path (default "+defaultConfigPath+")")
	defaults := Default()
	l.registerStruct(flags, "", reflect.ValueOf(defaults), reflect.TypeFor[Config]())
}

// Load applies sources in the same precedence order as the original TelDrive:
// defaults < config file < environment variables < explicitly changed flags.
func (l *Loader) Load(flags *pflag.FlagSet) (Config, error) {
	if l == nil || l.lookup == nil || l.homeDir == nil {
		return Config{}, fmt.Errorf("%w: configuration loader is not initialized", ErrInvalid)
	}
	if flags == nil {
		return Config{}, fmt.Errorf("%w: flag set is required", ErrInvalid)
	}

	k := koanf.New(".")
	if err := k.Load(staticProvider{values: defaultsMap(Default())}, nil); err != nil {
		return Config{}, fmt.Errorf("load configuration defaults: %w", err)
	}

	configPath, err := l.resolveConfigPath(flags)
	if err != nil {
		return Config{}, err
	}
	if configPath != "" {
		parser, err := parserForPath(configPath)
		if err != nil {
			return Config{}, err
		}
		if err := k.Load(file.Provider(configPath), parser); err != nil {
			return Config{}, fmt.Errorf("read config file %q: %w", configPath, err)
		}
	}

	l.envMap = make(map[string]string)
	l.generateEnvMap(reflect.TypeFor[Config](), "", "")
	if err := k.Load(staticProvider{values: l.environmentValues()}, nil); err != nil {
		return Config{}, fmt.Errorf("load environment configuration: %w", err)
	}
	if err := k.Load(&flagProvider{flags: flags, flagMap: l.flagMap, onlyChanged: true}, nil); err != nil {
		return Config{}, fmt.Errorf("load command-line configuration: %w", err)
	}

	var cfg Config
	unmarshal := koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			MatchName: func(mapKey, fieldName string) bool {
				return normalizeKey(mapKey) == normalizeKey(fieldName)
			},
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
				decodeSize,
				decodeEncryptionKeys,
			),
			Result:           &cfg,
			WeaklyTypedInput: true,
		},
	}
	if err := k.UnmarshalWithConf("", &cfg, unmarshal); err != nil {
		return Config{}, fmt.Errorf("%w: decode configuration: %v", ErrInvalid, err)
	}
	if cfg.Encryption.Keys == nil {
		cfg.Encryption.Keys = map[int32]string{}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Load is retained for library callers that only need defaults, auto-discovered
// config files, and environment variables. The executable uses Loader directly
// so explicit pflags retain highest precedence.
func Load() (Config, error) {
	loader := NewLoader()
	flags := pflag.NewFlagSet("teldrive", pflag.ContinueOnError)
	loader.RegisterFlags(flags)
	return loader.Load(flags)
}

// LoadFrom resolves the configuration using only the supplied environment
// lookup, ignoring the real process environment and the user's home directory;
// config files are still discovered in the working directory. It returns an
// error wrapping ErrInvalid when lookup is nil or when the resolved
// configuration fails validation.
func LoadFrom(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("%w: environment lookup is nil", ErrInvalid)
	}
	loader := newLoader(lookup, func() (string, error) { return "", nil })
	flags := pflag.NewFlagSet("teldrive", pflag.ContinueOnError)
	loader.RegisterFlags(flags)
	return loader.Load(flags)
}

// resolveConfigPath returns the config file to read. An explicit --config value
// wins and must exist, otherwise the first existing candidate among
// ~/.teldrive/config.{toml,yaml,yml} and config.{toml,yaml,yml} in the working
// directory is used. It returns an empty path, and no error, when no config file
// exists at all.
func (l *Loader) resolveConfigPath(flags *pflag.FlagSet) (string, error) {
	if flag := flags.Lookup("config"); flag != nil && flag.Value.String() != "" {
		path := flag.Value.String()
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("stat config file %q: %w", path, err)
		}
		return path, nil
	}
	home, err := l.homeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	candidates := []string{}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".teldrive", "config.toml"),
			filepath.Join(home, ".teldrive", "config.yaml"),
			filepath.Join(home, ".teldrive", "config.yml"),
		)
	}
	candidates = append(candidates, "config.toml", "config.yaml", "config.yml")
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", nil
}

// parserForPath selects the koanf parser from the file extension. It returns an
// error wrapping ErrInvalid for any extension other than .toml, .yaml, or .yml.
func parserForPath(path string) (koanf.Parser, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".toml":
		return toml.Parser(), nil
	case ".yaml", ".yml":
		return yaml.Parser(), nil
	default:
		return nil, fmt.Errorf("%w: config file must use .toml, .yaml, or .yml", ErrInvalid)
	}
}

// environmentValues reads every mapped TELDRIVE_ variable that is set and
// returns the values as a nested map keyed by dotted koanf path. The known keys
// are consulted in sorted order, which keeps the calls into lookup
// deterministic, and unset variables are skipped so that lower-precedence
// sources keep their values.
func (l *Loader) environmentValues() map[string]any {
	flat := make(map[string]any)
	keys := slices.Sorted(stdmaps.Keys(l.envMap))
	for _, envKey := range keys {
		if value, ok := l.lookup(envPrefix + envKey); ok {
			flat[l.envMap[envKey]] = value
		}
	}
	return maps.Unflatten(flat, ".")
}

// generateEnvMap walks the configuration type and records, for every leaf
// field, the mapping from its TELDRIVE_ variable name without the prefix to its
// dotted koanf path. Nested structs extend both sides, so Telegram.AppID yields
// the entry TELEGRAM_APP_ID for telegram.app-id. Fields tagged `koanf:"-"` are
// skipped because mapstructure never decodes them.
func (l *Loader) generateEnvMap(t reflect.Type, path, envPath string) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for field := range t.Fields() {
		key := FieldKey(field)
		if key == ignoredKey {
			continue
		}
		childPath := joinPath(path, key)
		childEnv := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if envPath != "" {
			childEnv = envPath + "_" + childEnv
		}
		if IsNestedStruct(field.Type) {
			l.generateEnvMap(field.Type, childPath, childEnv)
			continue
		}
		l.envMap[childEnv] = childPath
	}
}

// registerStruct registers one pflag per configuration leaf, named by replacing
// the dots of the leaf's config path with dashes and described by the field's
// `description` tag, falling back to "Set <path>" when that tag is absent.
// Defaults come from the fully defaulted value, and only the types that occur in
// the configuration tree are handled: duration, size, encryption keys, string
// slices, and the string, bool, int, int32, and int64 kinds. Fields of any other
// type are skipped, so they can only be set from a file or the environment, and
// fields tagged `koanf:"-"` are skipped because mapstructure never decodes them.
func (l *Loader) registerStruct(flags *pflag.FlagSet, path string, value reflect.Value, t reflect.Type) {
	for i := range t.NumField() {
		field := t.Field(i)
		fieldValue := value.Field(i)
		key := FieldKey(field)
		if key == ignoredKey {
			continue
		}
		key = joinPath(path, key)
		if IsNestedStruct(field.Type) {
			l.registerStruct(flags, key, fieldValue, field.Type)
			continue
		}
		name := strings.ReplaceAll(key, ".", "-")
		l.flagMap[name] = key
		description := field.Tag.Get("description")
		if description == "" {
			description = "Set " + key
		}
		switch {
		case field.Type == reflect.TypeFor[time.Duration]():
			flags.Duration(name, time.Duration(fieldValue.Int()), description)
		case field.Type == reflect.TypeFor[size.Size]():
			flags.String(name, fieldValue.Interface().(size.Size).String(), description)
		case field.Type == reflect.TypeFor[map[int32]string]():
			flags.String(name, formatEncryptionKeys(fieldValue.Interface().(map[int32]string)), description)
		case field.Type == reflect.TypeFor[[]string]():
			flags.StringSlice(name, fieldValue.Interface().([]string), description)
		case field.Type.Kind() == reflect.String:
			flags.String(name, fieldValue.String(), description)
		case field.Type.Kind() == reflect.Bool:
			flags.Bool(name, fieldValue.Bool(), description)
		case field.Type.Kind() == reflect.Int:
			flags.Int(name, int(fieldValue.Int()), description)
		case field.Type.Kind() == reflect.Int32:
			flags.Int32(name, int32(fieldValue.Int()), description)
		case field.Type.Kind() == reflect.Int64:
			flags.Int64(name, fieldValue.Int(), description)
		}
	}
}

// defaultsMap converts a fully defaulted Config into the nested string map that
// seeds the koanf store before any file, environment, or flag source is applied.
func defaultsMap(cfg Config) map[string]any {
	return structMap(reflect.ValueOf(cfg), reflect.TypeFor[Config]())
}

// structMap converts a configuration value into a nested map keyed by koanf
// path. Durations, sizes, and encryption key maps are rendered as their string
// forms so koanf holds them in the same representation as the file and flag
// sources; every other leaf keeps its native value. Fields tagged `koanf:"-"` are
// omitted because mapstructure never decodes them.
func structMap(value reflect.Value, t reflect.Type) map[string]any {
	result := make(map[string]any)
	for i := range t.NumField() {
		field := t.Field(i)
		fieldValue := value.Field(i)
		key := FieldKey(field)
		if key == ignoredKey {
			continue
		}
		switch {
		case IsNestedStruct(field.Type):
			result[key] = structMap(fieldValue, field.Type)
		case field.Type == reflect.TypeFor[time.Duration]():
			result[key] = time.Duration(fieldValue.Int()).String()
		case field.Type == reflect.TypeFor[size.Size]():
			result[key] = fieldValue.Interface().(size.Size).String()
		case field.Type == reflect.TypeFor[map[int32]string]():
			result[key] = formatEncryptionKeys(fieldValue.Interface().(map[int32]string))
		default:
			result[key] = fieldValue.Interface()
		}
	}
	return result
}

// decodeSize is a mapstructure decode hook that converts a string, int, int64,
// or float64 source value into a size.Size. Values destined for another type,
// and source values of another kind, are returned unchanged so the remaining
// hooks can handle them; an unparsable size string is returned as an error.
func decodeSize(_ reflect.Type, to reflect.Type, data any) (any, error) {
	if to != reflect.TypeFor[size.Size]() {
		return data, nil
	}
	switch value := data.(type) {
	case string:
		return size.Parse(value)
	case int:
		return size.Size(value), nil
	case int64:
		return size.Size(value), nil
	case float64:
		return size.Size(value), nil
	default:
		return data, nil
	}
}

// decodeEncryptionKeys is a mapstructure decode hook that builds the
// map[int32]string encryption key set. A string source is parsed as version:key
// entries by parseEncryptionKeys, and an object source is converted from its
// string version keys into the same map. Values destined for another type pass
// through untouched, while a non-positive or unparsable version and an empty key
// yield an error wrapping ErrInvalid.
func decodeEncryptionKeys(_ reflect.Type, to reflect.Type, data any) (any, error) {
	if to != reflect.TypeFor[map[int32]string]() {
		return data, nil
	}
	switch value := data.(type) {
	case string:
		return parseEncryptionKeys(value)
	case map[string]any:
		keys := make(map[int32]string, len(value))
		for rawVersion, rawKey := range value {
			version, err := strconv.ParseInt(rawVersion, 10, 32)
			key, ok := rawKey.(string)
			if err != nil || !ok || version <= 0 || strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("%w: an encryption key entry is invalid", ErrInvalid)
			}
			keys[int32(version)] = key
		}
		return keys, nil
	default:
		return data, nil
	}
}

// formatEncryptionKeys renders an encryption key map as the canonical
// comma-separated version:key list sorted by ascending version. It is the
// inverse of parseEncryptionKeys, and an empty map formats as an empty string.
func formatEncryptionKeys(keys map[int32]string) string {
	versions := make([]int, 0, len(keys))
	for version := range keys {
		versions = append(versions, int(version))
	}
	slices.Sort(versions)
	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, fmt.Sprintf("%d:%s", version, keys[int32(version)]))
	}
	return strings.Join(parts, ",")
}

// FieldKey returns the config key of a field: its `koanf` tag when one is set, or
// the kebab-case form of the field name otherwise. A field tagged `koanf:"-"`
// yields "-", which callers must treat as "not configurable". It is exported so
// internal/tools/docsconfig documents the very names the loader accepts.
func FieldKey(field reflect.StructField) string {
	if tag := field.Tag.Get("koanf"); tag != "" {
		return tag
	}
	return toKebab(field.Name)
}

// toKebab converts an exported Go field name such as "ReadTimeout" into the
// kebab-case key "read-timeout", applying matchFirstCap and then matchAllCap to
// insert the word boundaries.
func toKebab(value string) string {
	kebab := matchFirstCap.ReplaceAllString(value, "${1}-${2}")
	kebab = matchAllCap.ReplaceAllString(kebab, "${1}-${2}")
	return strings.ToLower(kebab)
}

// normalizeKey lowercases a key and strips dashes and underscores so that config
// keys and Go field names compare equal while decoding: "read-timeout",
// "read_timeout", and "ReadTimeout" all normalize to "readtimeout".
func normalizeKey(value string) string {
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, "_", "")
	return strings.ToLower(value)
}

// joinPath appends key to a dotted prefix and returns key alone when the prefix
// is empty, so a top-level path never carries a leading dot.
func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// IsNestedStruct reports whether t is a configuration group rather than a leaf.
// time.Duration and size.Size are struct types but are treated as scalars, so
// they are excluded. It is exported so internal/tools/docsconfig classifies
// fields exactly as the loader does.
func IsNestedStruct(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t != reflect.TypeFor[time.Duration]() && t != reflect.TypeFor[size.Size]()
}

// staticProvider is a koanf provider over an in-memory nested map, used for the
// defaults and environment sources. It is immutable once constructed.
type staticProvider struct{ values map[string]any }

// Read returns the provider's configuration map unchanged; it never fails.
func (p staticProvider) Read() (map[string]any, error) { return p.values, nil }

// ReadBytes always returns nil, because koanf decodes this provider from the map
// returned by Read rather than from raw bytes.
func (p staticProvider) ReadBytes() ([]byte, error) { return nil, nil }

// flagProvider is a koanf provider over a pflag.FlagSet. It exposes only the
// flags that map to a configuration path, so the built-in --config flag and any
// flags added by other libraries are ignored.
type flagProvider struct {
	// flags is the set to read, normally the one RegisterFlags populated.
	flags *pflag.FlagSet
	// flagMap maps a flag name such as "http-address" to its dotted config path.
	flagMap map[string]string
	// onlyChanged restricts the provider to flags the user set explicitly, which
	// is what gives those flags the highest precedence.
	onlyChanged bool
}

// Read returns the values of the mapped flags as a nested map keyed by config
// path, with every value rendered as a string so the mapstructure hooks can
// convert it. The built-in --config flag, flags with no configuration mapping,
// and, when onlyChanged is set, flags left at their default are skipped. It
// never fails.
func (p *flagProvider) Read() (map[string]any, error) {
	flat := make(map[string]any)
	p.flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Name == "config" || (p.onlyChanged && !flag.Changed) {
			return
		}
		key, ok := p.flagMap[flag.Name]
		if !ok {
			return
		}
		flat[key] = flag.Value.String()
	})
	return maps.Unflatten(flat, "."), nil
}

// ReadBytes always returns nil, because koanf decodes this provider from the map
// returned by Read rather than from raw bytes.
func (p *flagProvider) ReadBytes() ([]byte, error) { return nil, nil }
