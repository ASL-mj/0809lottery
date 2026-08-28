package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/secret"
)

const v3Fixture = `{
  "version": 3,
  "accounts": {
    "account-a": {
      "user_id": 11,
      "parent_access_token": "v3-parent-token-a",
      "parent_access_expires_at": "2026-09-01T00:00:00Z",
      "lottery_access_token": "v3-lottery-token-a",
      "lottery_access_expires_at": "2026-09-01T01:00:00Z",
      "cookies": [{"name": "new_api_refresh", "value": "v3-cookie-a", "path": "/api/user/auth"}]
    },
    "account-b": {"user_id": 22}
  },
  "actions": {
    "2026-08-28:checkin:account-a": {
      "key": "2026-08-28:checkin:account-a",
      "account_id": "account-a",
      "date": "2026-08-28",
      "kind": "checkin",
      "status": "completed",
      "created_at": "2026-08-28T00:00:00Z",
      "updated_at": "2026-08-28T00:05:00Z"
    }
  },
  "snapshots": {
    "account-a:subscriptions": {
      "account_id": "account-a",
      "kind": "subscriptions",
      "data": {"account_id": "account-a", "subscriptions": []},
      "queried_at": "2026-08-28T06:00:00Z"
    }
  },
  "plans": {
    "2026-08-28:morning:account-a": {
      "key": "2026-08-28:morning:account-a",
      "date": "2026-08-28",
      "account_id": "account-a",
      "window_id": "morning",
      "planned_at": "2026-08-28T00:30:00Z",
      "idempotency_key": "draw:auto:v3-hidden",
      "status": "skipped",
      "created_at": "2026-08-28T00:00:00Z",
      "updated_at": "2026-08-28T00:31:00Z"
    }
  },
  "logs": [
    {
      "id": "runtime-v3-1",
      "occurred_at": "2026-08-28T00:31:00Z",
      "account_id": "account-a",
      "window_id": "morning",
      "status": "skipped",
      "message": "无可用抽奖次数，已跳过"
    }
  ]
}`

func writeV3Fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(v3Fixture), 0600); err != nil {
		t.Fatalf("write v3 fixture: %v", err)
	}
	return path
}

func legacyAccounts() []LegacyAccount {
	return []LegacyAccount{
		{ID: "account-a", Label: "账号一", LoginName: "a@example.test", Password: "legacy-password-a"},
		{ID: "account-b", Label: "账号二", LoginName: "b@example.test", Password: "legacy-password-b"},
	}
}

func newTestFileVault(t *testing.T) *secret.FileVault {
	t.Helper()
	vault, err := secret.NewFileVault(filepath.Join(t.TempDir(), "vault", "secrets.json"), bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("NewFileVault() error = %v", err)
	}
	return vault
}

func TestMigrateV3ImportsLegacyAccountsSecretsAndBusinessData(t *testing.T) {
	path := writeV3Fixture(t)
	vault := newTestFileVault(t)
	ctx := context.Background()

	result, err := MigrateV3(ctx, path, vault, legacyAccounts())
	if err != nil {
		t.Fatalf("MigrateV3() error = %v", err)
	}
	if result.Migrated != 2 {
		t.Fatalf("MigrateV3() migrated = %d, want 2", result.Migrated)
	}

	bundle, err := vault.Load(ctx, "account-a")
	if err != nil {
		t.Fatalf("vault.Load(account-a) error = %v", err)
	}
	if bundle.LoginName != "a@example.test" || bundle.Password != "legacy-password-a" {
		t.Fatalf("vault credentials = %#v", bundle)
	}
	if bundle.UserID != 11 || bundle.ParentAccessToken != "v3-parent-token-a" || bundle.LotteryAccessToken != "v3-lottery-token-a" {
		t.Fatalf("vault tokens = %#v", bundle)
	}
	if len(bundle.Cookies) != 1 || bundle.Cookies[0].Value != "v3-cookie-a" {
		t.Fatalf("vault cookies = %#v", bundle.Cookies)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(migrated state) error = %v", err)
	}
	defer store.Close()
	if store.data.Version != 4 {
		t.Fatalf("migrated version = %d, want 4", store.data.Version)
	}
	registry := store.AccountRegistry()
	first, err := registry.Get("account-a")
	if err != nil {
		t.Fatalf("Get(account-a) error = %v", err)
	}
	if first.Label != "账号一" || first.Status != account.StatusEnabled || first.RemoteUserID != 11 {
		t.Fatalf("migrated record = %#v", first)
	}
	if first.MaskedLoginName != "a***@example.test" {
		t.Fatalf("masked login = %q", first.MaskedLoginName)
	}
	records, err := registry.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("List() = %#v, %v", records, err)
	}
	if action, ok := store.Action("account-a", "2026-08-28", ActionCheckin); !ok || action.Status != ActionCompleted {
		t.Fatalf("migrated action = %#v, %v", action, ok)
	}
	if _, ok := store.Snapshot("account-a", "subscriptions"); !ok {
		t.Fatal("migrated snapshot missing")
	}
	if plan, ok := store.AutoDrawPlan("2026-08-28:morning:account-a"); !ok || plan.Status != AutoDrawPlanSkipped {
		t.Fatalf("migrated plan = %#v, %v", plan, ok)
	}
	if logs := store.RuntimeLogs(10); len(logs) != 1 {
		t.Fatalf("migrated logs = %#v", logs)
	}

	// Secrets must not remain in the ordinary state file.
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated state: %v", err)
	}
	for _, leaked := range []string{"v3-parent-token-a", "v3-lottery-token-a", "v3-cookie-a", "legacy-password-a", "a@example.test", "legacy_auth"} {
		if strings.Contains(string(payload), leaked) {
			t.Fatalf("migrated state file contains %q", leaked)
		}
	}
}

func TestMigrateV3DoesNotMutateStateWhenVaultWriteFails(t *testing.T) {
	path := writeV3Fixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := MigrateV3(context.Background(), path, failingVault{}, legacyAccounts()); err == nil {
		t.Fatal("MigrateV3() error = nil")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after failed migration: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("migration changed state after Vault failure")
	}
}

func TestMigrateV3IsIdempotentOnVersion4(t *testing.T) {
	path := writeV3Fixture(t)
	vault := newTestFileVault(t)
	if _, err := MigrateV3(context.Background(), path, vault, legacyAccounts()); err != nil {
		t.Fatalf("first MigrateV3() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated state: %v", err)
	}
	result, err := MigrateV3(context.Background(), path, vault, legacyAccounts())
	if err != nil {
		t.Fatalf("second MigrateV3() error = %v", err)
	}
	if result.Migrated != 0 || result.Message != "no migration needed" {
		t.Fatalf("second MigrateV3() = %#v, want no migration needed", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after second migration: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("second migration modified a version-4 state file")
	}
}

func TestMigrateV3RefusesMissingLegacySecret(t *testing.T) {
	path := writeV3Fixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	legacy := legacyAccounts()
	legacy[1].Password = ""
	if _, err := MigrateV3(context.Background(), path, newTestFileVault(t), legacy); err == nil {
		t.Fatal("MigrateV3() accepted a legacy account without a password")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after refused migration: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused migration modified the state file")
	}
}

func TestStoreReadsVersion3StateWithoutMigrating(t *testing.T) {
	path := writeV3Fixture(t)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(v3) error = %v", err)
	}
	if store.data.Version != 3 {
		t.Fatalf("loaded version = %d, want 3", store.data.Version)
	}
	auth := store.Auth("account-a")
	if auth.ParentAccessToken != "v3-parent-token-a" || auth.UserID != 11 {
		t.Fatalf("v3 auth state = %#v", auth)
	}
	if err := store.PutAuth("account-b", AuthState{UserID: 22, ParentAccessToken: "still-v3"}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close v3 store: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v3 state error = %v", err)
	}
	defer reopened.Close()
	if reopened.data.Version != 3 {
		t.Fatalf("reopen version = %d, want 3", reopened.data.Version)
	}
	if reopened.Auth("account-b").ParentAccessToken != "still-v3" {
		t.Fatalf("v3 auth write lost: %#v", reopened.Auth("account-b"))
	}
}

type failingVault struct{}

func (failingVault) Load(context.Context, string) (secret.Bundle, error) {
	return secret.Bundle{}, errors.New("vault unavailable")
}

func (failingVault) Save(context.Context, string, secret.Bundle) error {
	return errors.New("vault unavailable")
}

func (failingVault) Delete(context.Context, string) error {
	return errors.New("vault unavailable")
}
