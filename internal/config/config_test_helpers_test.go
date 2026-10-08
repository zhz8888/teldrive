package config

// validTestConfig returns a Default config with the settings validation requires,
// so a test can mutate one field and blame that field for the failure.
func validTestConfig() Config {
	cfg := Default()
	cfg.Database.URL = "postgres://example/teldrive"
	cfg.Telegram.AppID = 12345
	cfg.Telegram.AppHash = "telegram-app-hash"
	cfg.Security.SigningKey = "0123456789abcdef0123456789abcdef"
	cfg.Security.DataKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	return cfg
}
