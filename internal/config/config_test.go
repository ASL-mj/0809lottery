package config

import "testing"

func TestLoadFrom(t *testing.T) {
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
	config, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if config.BaseURL != defaultBaseURL {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if config.Accounts["account-a"].Username != "a-user" || config.Accounts["account-b"].Username != "b-user" || config.Accounts["account-c"].Username != "c-user" || config.Accounts["account-d"].Username != "d-user" || config.Accounts["account-e"].Username != "e-user" {
		t.Fatalf("accounts were not loaded: %#v", config.Accounts)
	}
}

func TestLoadFromRejectsMissingAccountCredentials(t *testing.T) {
	_, err := LoadFrom(func(string) string { return "" })
	if err == nil {
		t.Fatal("LoadFrom() error = nil, want missing account credentials error")
	}
}

func TestLoadFromRejectsMissingAccountPassword(t *testing.T) {
	values := map[string]string{
		"ACCOUNT_A_USERNAME": "a-user", "ACCOUNT_B_USERNAME": "b-user", "ACCOUNT_C_USERNAME": "c-user",
		"ACCOUNT_D_USERNAME": "d-user", "ACCOUNT_E_USERNAME": "e-user",
	}
	if _, err := LoadFrom(func(key string) string { return values[key] }); err == nil {
		t.Fatal("LoadFrom() error = nil, want missing account password error")
	}
}

func TestWebValidation(t *testing.T) {
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
		"WEB_USERNAME":       "admin",
		"WEB_PASSWORD":       "secret",
	}
	cfg, err := LoadFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if err := cfg.ValidateWeb(); err != nil {
		t.Fatalf("ValidateWeb() error = %v", err)
	}
}
