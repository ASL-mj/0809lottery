package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultBaseURL   = "https://www.0809.one"
	defaultStatePath = "/root/projects/lottery-bot/data/state.json"
	defaultUserAgent = "SkyeLotteryBot/1.0"
	defaultWebAddr   = "127.0.0.1:18090"

	defaultSessionLimit        = 50
	defaultSessionSafetyMargin = 5
	defaultDurableSessionLimit = 2

	maxDurableSessionLimit = 3
	vaultKeySize           = 32
)

type Account struct {
	ID       string
	Label    string
	Username string
	Password string
}

type Config struct {
	BaseURL   string
	StatePath string
	VaultPath string
	VaultKey  []byte
	UserAgent string
	WebAddr   string
	WebUser   string
	WebPass   string

	SessionLimit        int
	SessionSafetyMargin int
	DurableSessionLimit int

	// LegacyAccounts carries environment-variable credentials for the one-time
	// v3 migration only. Normal serve mode never reads it; dynamic accounts
	// live in the account registry and the encrypted vault.
	LegacyAccounts map[string]Account

}

func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

func LoadFrom(getenv func(string) string) (Config, error) {
	baseURL := strings.TrimRight(valueOrDefault(getenv("LOTTERY_BASE_URL"), defaultBaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Config{}, fmt.Errorf("LOTTERY_BASE_URL must be an https URL")
	}

	statePath := valueOrDefault(getenv("STATE_PATH"), defaultStatePath)
	cfg := Config{
		BaseURL:   baseURL,
		StatePath: statePath,
		VaultPath: valueOrDefault(getenv("LOTTERY_VAULT_PATH"), filepath.Join(filepath.Dir(statePath), "vault.json")),
		UserAgent: valueOrDefault(getenv("USER_AGENT"), defaultUserAgent),
		WebAddr:   valueOrDefault(getenv("WEB_ADDR"), defaultWebAddr),
		WebUser:   strings.TrimSpace(getenv("WEB_USERNAME")),
		WebPass:   getenv("WEB_PASSWORD"),
	}
	if cfg.VaultKey, err = loadVaultKey(getenv("LOTTERY_VAULT_KEY")); err != nil {
		return Config{}, err
	}
	if cfg.SessionLimit, cfg.SessionSafetyMargin, cfg.DurableSessionLimit, err = loadSessionSettings(getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadLegacyAccounts reads the five fixed environment-variable accounts. It is
// only callable by the one-time migration; serve mode no longer accepts
// credentials through the environment.
func LoadLegacyAccounts(getenv func(string) string) (map[string]Account, error) {
	accounts := make(map[string]Account, 5)
	for _, item := range []struct {
		ID     string
		Prefix string
	}{
		{ID: "account-a", Prefix: "ACCOUNT_A"},
		{ID: "account-b", Prefix: "ACCOUNT_B"},
		{ID: "account-c", Prefix: "ACCOUNT_C"},
		{ID: "account-d", Prefix: "ACCOUNT_D"},
		{ID: "account-e", Prefix: "ACCOUNT_E"},
	} {
		username := strings.TrimSpace(getenv(item.Prefix + "_USERNAME"))
		if username == "" {
			return nil, fmt.Errorf("%s_USERNAME is required", item.Prefix)
		}
		password := strings.TrimSpace(getenv(item.Prefix + "_PASSWORD"))
		if password == "" {
			return nil, fmt.Errorf("%s_PASSWORD is required", item.Prefix)
		}
		label := strings.TrimSpace(getenv(item.Prefix + "_LABEL"))
		if label == "" {
			label = item.ID
		}
		accounts[item.ID] = Account{ID: item.ID, Label: label, Username: username, Password: password}
	}
	return accounts, nil
}

func (c Config) ValidateServe() error {
	if strings.TrimSpace(c.WebAddr) == "" {
		return fmt.Errorf("WEB_ADDR is required for serve mode")
	}
	if strings.TrimSpace(c.WebUser) == "" || c.WebPass == "" {
		return fmt.Errorf("WEB_USERNAME and WEB_PASSWORD are required for serve mode")
	}
	if strings.TrimSpace(c.VaultPath) == "" {
		return fmt.Errorf("LOTTERY_VAULT_PATH is required for serve mode")
	}
	if len(c.VaultKey) != vaultKeySize {
		return fmt.Errorf("LOTTERY_VAULT_KEY must decode to exactly %d bytes for serve mode", vaultKeySize)
	}
	return c.validateSessionSettings()
}

func (c Config) ValidateMigrate() error {
	if len(c.VaultKey) != vaultKeySize {
		return fmt.Errorf("LOTTERY_VAULT_KEY must decode to exactly %d bytes for migrate mode", vaultKeySize)
	}
	if len(c.LegacyAccounts) == 0 {
		return fmt.Errorf("migrate mode requires the legacy ACCOUNT_* credentials")
	}
	return c.validateSessionSettings()
}

func (c Config) validateSessionSettings() error {
	if c.SessionLimit <= 0 || c.SessionSafetyMargin <= 0 || c.DurableSessionLimit <= 0 {
		return fmt.Errorf("session limits must be positive")
	}
	if c.SessionLimit-c.SessionSafetyMargin < 1 {
		return fmt.Errorf("session safety margin leaves no usable capacity")
	}
	if c.DurableSessionLimit > maxDurableSessionLimit {
		return fmt.Errorf("durable session limit must be between 1 and %d", maxDurableSessionLimit)
	}
	return nil
}

func loadVaultKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("LOTTERY_VAULT_KEY must be a base64-encoded 32-byte key")
	}
	if len(key) != vaultKeySize {
		return nil, fmt.Errorf("LOTTERY_VAULT_KEY must decode to exactly %d bytes", vaultKeySize)
	}
	return key, nil
}

func loadSessionSettings(getenv func(string) string) (int, int, int, error) {
	sessionLimit, err := positiveIntEnv(getenv("LOTTERY_SESSION_LIMIT"), defaultSessionLimit, "LOTTERY_SESSION_LIMIT")
	if err != nil {
		return 0, 0, 0, err
	}
	safetyMargin, err := positiveIntEnv(getenv("LOTTERY_SESSION_SAFETY_MARGIN"), defaultSessionSafetyMargin, "LOTTERY_SESSION_SAFETY_MARGIN")
	if err != nil {
		return 0, 0, 0, err
	}
	durableLimit, err := positiveIntEnv(getenv("LOTTERY_DURABLE_SESSIONS"), defaultDurableSessionLimit, "LOTTERY_DURABLE_SESSIONS")
	if err != nil {
		return 0, 0, 0, err
	}
	return sessionLimit, safetyMargin, durableLimit, nil
}

func positiveIntEnv(value string, fallback int, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed := 0
	for _, digit := range []byte(value) {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		parsed = parsed*10 + int(digit-'0')
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
