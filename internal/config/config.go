package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	defaultBaseURL   = "https://www.0809.one"
	defaultStatePath = "/root/projects/lottery-bot/data/state.json"
	defaultUserAgent = "SkyeLotteryBot/1.0"
	defaultWebAddr   = "127.0.0.1:18090"
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
	UserAgent string
	WebAddr   string
	WebUser   string
	WebPass   string
	Accounts  map[string]Account
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
			return Config{}, fmt.Errorf("%s_USERNAME is required", item.Prefix)
		}
		password := strings.TrimSpace(getenv(item.Prefix + "_PASSWORD"))
		if password == "" {
			return Config{}, fmt.Errorf("%s_PASSWORD is required", item.Prefix)
		}
		label := strings.TrimSpace(getenv(item.Prefix + "_LABEL"))
		if label == "" {
			label = item.ID
		}
		accounts[item.ID] = Account{ID: item.ID, Label: label, Username: username, Password: password}
	}

	return Config{
		BaseURL:   baseURL,
		StatePath: valueOrDefault(getenv("STATE_PATH"), defaultStatePath),
		UserAgent: valueOrDefault(getenv("USER_AGENT"), defaultUserAgent),
		WebAddr:   valueOrDefault(getenv("WEB_ADDR"), defaultWebAddr),
		WebUser:   strings.TrimSpace(getenv("WEB_USERNAME")),
		WebPass:   getenv("WEB_PASSWORD"),
		Accounts:  accounts,
	}, nil
}

func (c Config) ValidateWeb() error {
	if strings.TrimSpace(c.WebAddr) == "" {
		return fmt.Errorf("WEB_ADDR is required for serve mode")
	}
	if strings.TrimSpace(c.WebUser) == "" || c.WebPass == "" {
		return fmt.Errorf("WEB_USERNAME and WEB_PASSWORD are required for serve mode")
	}
	return nil
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
