package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testVaultKey() string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
}

func baseValues() map[string]string {
	return map[string]string{
		"LOTTERY_VAULT_KEY": testVaultKey(),
		"WEB_USERNAME":      "admin",
		"WEB_PASSWORD":      "secret",
	}
}

func TestLoadFromUsesDefaultsWithoutLegacyCredentials(t *testing.T) {
	cfg, err := LoadFrom(func(key string) string { return baseValues()[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if cfg.BaseURL != defaultBaseURL {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	const wantUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	if cfg.UserAgent != wantUserAgent {
		t.Fatalf("default user agent = %q, want the confirmed macOS Chrome value", cfg.UserAgent)
	}
	if cfg.SessionLimit != defaultSessionLimit || cfg.SessionSafetyMargin != defaultSessionSafetyMargin || cfg.DurableSessionLimit != defaultDurableSessionLimit {
		t.Fatalf("session settings = %d/%d/%d", cfg.SessionLimit, cfg.SessionSafetyMargin, cfg.DurableSessionLimit)
	}
	if len(cfg.LegacyAccounts) != 0 {
		t.Fatalf("serve mode must not load legacy accounts: %#v", cfg.LegacyAccounts)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("ValidateServe() error = %v", err)
	}
}

func TestLoadFromDecodesVaultKey(t *testing.T) {
	values := baseValues()
	values["LOTTERY_VAULT_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 31)))
	if _, err := LoadFrom(func(key string) string { return values[key] }); err == nil {
		t.Fatal("LoadFrom() accepted a 31-byte vault key")
	}

	values["LOTTERY_VAULT_KEY"] = "not-base64!!!"
	if _, err := LoadFrom(func(key string) string { return values[key] }); err == nil {
		t.Fatal("LoadFrom() accepted a non-base64 vault key")
	}
}

func TestLoadFromRejectsInvalidSessionSettings(t *testing.T) {
	values := baseValues()
	values["LOTTERY_SESSION_LIMIT"] = "5"
	values["LOTTERY_SESSION_SAFETY_MARGIN"] = "10"
	cfg, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if err := cfg.ValidateServe(); err == nil {
		t.Fatal("ValidateServe() accepted a margin larger than the limit")
	}

	values = baseValues()
	values["LOTTERY_DURABLE_SESSIONS"] = "4"
	cfg, err = LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if err := cfg.ValidateServe(); err == nil {
		t.Fatal("ValidateServe() accepted a durable session limit above 3")
	}

	values = baseValues()
	values["LOTTERY_SESSION_LIMIT"] = "zero"
	if _, err := LoadFrom(func(key string) string { return values[key] }); err == nil {
		t.Fatal("LoadFrom() accepted a non-numeric session limit")
	}
}

func TestValidateServeRequiresVaultKey(t *testing.T) {
	values := baseValues()
	values["LOTTERY_VAULT_KEY"] = ""
	cfg, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if err := cfg.ValidateServe(); err == nil {
		t.Fatal("ValidateServe() accepted a missing vault key")
	}
}

func TestLoadLegacyAccounts(t *testing.T) {
	values := map[string]string{
		"ACCOUNT_A_USERNAME": "a-user",
		"ACCOUNT_A_PASSWORD": "a-password",
		"ACCOUNT_B_USERNAME": "b-user",
		"ACCOUNT_B_PASSWORD": "b-password",
		"ACCOUNT_C_USERNAME": "c-user",
		"ACCOUNT_C_PASSWORD": "c-password",
		"ACCOUNT_D_USERNAME": "d-user",
		"ACCOUNT_D_PASSWORD": "d-password",
		"ACCOUNT_E_USERNAME": "e-user",
		"ACCOUNT_E_PASSWORD": "e-password",
	}
	accounts, err := LoadLegacyAccounts(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadLegacyAccounts() error = %v", err)
	}
	for _, id := range []string{"account-a", "account-b", "account-c", "account-d", "account-e"} {
		if accounts[id].Username == "" || accounts[id].Password == "" {
			t.Fatalf("legacy account %s was not loaded: %#v", id, accounts[id])
		}
	}
}

func TestLoadLegacyAccountsRejectsMissingCredentials(t *testing.T) {
	if _, err := LoadLegacyAccounts(func(string) string { return "" }); err == nil {
		t.Fatal("LoadLegacyAccounts() error = nil, want missing credentials error")
	}
	partial := map[string]string{
		"ACCOUNT_A_USERNAME": "a-user", "ACCOUNT_B_USERNAME": "b-user", "ACCOUNT_C_USERNAME": "c-user",
		"ACCOUNT_D_USERNAME": "d-user", "ACCOUNT_E_USERNAME": "e-user",
	}
	if _, err := LoadLegacyAccounts(func(key string) string { return partial[key] }); err == nil {
		t.Fatal("LoadLegacyAccounts() error = nil, want missing password error")
	}
}

func TestValidateMigrateRequiresLegacyAccountsAndKey(t *testing.T) {
	values := baseValues()
	cfg, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if err := cfg.ValidateMigrate(); err == nil {
		t.Fatal("ValidateMigrate() accepted missing legacy accounts")
	}
	legacy, err := LoadLegacyAccounts(legacyValues)
	if err != nil {
		t.Fatalf("LoadLegacyAccounts() error = %v", err)
	}
	cfg.LegacyAccounts = legacy
	if err := cfg.ValidateMigrate(); err != nil {
		t.Fatalf("ValidateMigrate() error = %v", err)
	}

	values["LOTTERY_VAULT_KEY"] = ""
	cfgNoKey, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	cfgNoKey.LegacyAccounts = legacy
	if err := cfgNoKey.ValidateMigrate(); err == nil {
		t.Fatal("ValidateMigrate() accepted a missing vault key")
	}
}

func legacyValues(key string) string {
	return map[string]string{
		"ACCOUNT_A_USERNAME": "a-user", "ACCOUNT_A_PASSWORD": "a-password",
		"ACCOUNT_B_USERNAME": "b-user", "ACCOUNT_B_PASSWORD": "b-password",
		"ACCOUNT_C_USERNAME": "c-user", "ACCOUNT_C_PASSWORD": "c-password",
		"ACCOUNT_D_USERNAME": "d-user", "ACCOUNT_D_PASSWORD": "d-password",
		"ACCOUNT_E_USERNAME": "e-user", "ACCOUNT_E_PASSWORD": "e-password",
	}[key]
}
