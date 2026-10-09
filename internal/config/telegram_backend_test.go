package config

import "testing"

// TestValidateAcceptsMixedCaseBackendAndLogLevel pins the casing tolerance the two
// field comments promise: both tags use oneofci, so "Remote" and "DEBUG" are
// configuration the daemon starts with rather than a validation error.
func TestValidateAcceptsMixedCaseBackendAndLogLevel(t *testing.T) {
	t.Parallel()
	cfg := validTestConfig()
	cfg.Telegram.Backend = "Remote"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("mixed-case backend rejected: %v", err)
	}
	cfg.Logging.LogLevel = "DEBUG"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("mixed-case log level rejected: %v", err)
	}
}

// TestLoadFromLowercasesBackendAndLogLevel is the guard for the loader
// normalization. Without it the mixed-case value reaches the required_if rules that
// compare Backend against "remote" and "filesystem" unchanged, so a differently
// spelled backend would skip the credential checks that belong to it.
func TestLoadFromLowercasesBackendAndLogLevel(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"TELDRIVE_DATABASE_URL":         "postgres://example/teldrive",
		"TELDRIVE_SECURITY_SIGNING_KEY": "0123456789abcdef0123456789abcdef",
		"TELDRIVE_SECURITY_DATA_KEY":    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"TELDRIVE_TELEGRAM_BACKEND":     "  Filesystem  ",
		"TELDRIVE_LOGGING_LOG_LEVEL":    "WARN",
	}
	cfg, err := LoadFrom(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if cfg.Telegram.Backend != "filesystem" {
		t.Fatalf("backend = %q, want filesystem", cfg.Telegram.Backend)
	}
	if cfg.Logging.LogLevel != "warn" {
		t.Fatalf("log level = %q, want warn", cfg.Logging.LogLevel)
	}
}

func TestValidateFilesystemTelegramBackendWithoutCredentials(t *testing.T) {
	t.Parallel()
	cfg := validTestConfig()
	cfg.Telegram.Backend = "filesystem"
	cfg.Telegram.LocalRoot = t.TempDir()
	cfg.Telegram.AppID = 0
	cfg.Telegram.AppHash = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("filesystem backend rejected: %v", err)
	}
}

func TestValidateTelegramBackendSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "unknown", mutate: func(cfg *Config) { cfg.Telegram.Backend = "mock" }},
		{name: "filesystem missing root", mutate: func(cfg *Config) {
			cfg.Telegram.Backend = "filesystem"
			cfg.Telegram.LocalRoot = ""
		}},
		{name: "remote missing credentials", mutate: func(cfg *Config) {
			cfg.Telegram.Backend = "remote"
			cfg.Telegram.AppID = 0
			cfg.Telegram.AppHash = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := validTestConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid Telegram backend configuration was accepted")
			}
		})
	}
}
