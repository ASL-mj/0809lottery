package state

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/account"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return store
}

func seedAccountBusinessData(t *testing.T, store *Store, accountID string) {
	t.Helper()
	today := "2026-08-29"
	if _, _, err := store.GetOrCreateAction(accountID, today, ActionCheckin); err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if err := store.PutSnapshot(Snapshot{
		AccountID: accountID,
		Kind:      "subscriptions",
		Data:      json.RawMessage(`{"account_id":"` + accountID + `"}`),
		QueriedAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("PutSnapshot() error = %v", err)
	}
	if _, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{{
		Date:      today,
		AccountID: accountID,
		WindowID:  "morning",
		PlannedAt: time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC),
	}}); err != nil {
		t.Fatalf("EnsureAutoDrawPlans() error = %v", err)
	}
	if _, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt: time.Date(2026, 8, 29, 0, 40, 0, 0, time.UTC),
		AccountID:  accountID,
		WindowID:   "morning",
		Status:     AutoDrawPlanSkipped,
		Message:    "无可用抽奖次数，已跳过",
	}); err != nil {
		t.Fatalf("AppendRuntimeLog() error = %v", err)
	}
}

func TestAccountRegistryCRUD(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	registry := store.AccountRegistry()

	created, err := registry.Create(account.Record{Label: "新账号", MaskedLoginName: "n***@example.test", Status: account.StatusEnabled})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("Create() did not assign an ID and timestamps: %#v", created)
	}
	if created.ID == "account-a" || created.ID == "account-b" {
		t.Fatalf("Create() reused a migrated ID: %q", created.ID)
	}

	fetched, err := registry.Get(created.ID)
	if err != nil || fetched.Label != "新账号" {
		t.Fatalf("Get() = %#v, %v", fetched, err)
	}

	updated, err := registry.Update(account.Record{
		ID:              created.ID,
		Label:           "改名账号",
		MaskedLoginName: "n***@example.test",
		Status:          account.StatusDisabled,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Label != "改名账号" || updated.Status != account.StatusDisabled {
		t.Fatalf("Update() = %#v", updated)
	}
	enabled, err := registry.ListEnabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("ListEnabled() after disable = %#v, %v", enabled, err)
	}
	if _, err := registry.Update(account.Record{ID: updated.ID, Label: "重新启用", MaskedLoginName: "n***@example.test", Status: account.StatusEnabled}); err != nil {
		t.Fatalf("Update(enable) error = %v", err)
	}
	enabled, err = registry.ListEnabled()
	if err != nil || len(enabled) != 1 {
		t.Fatalf("ListEnabled() after enable = %#v, %v", enabled, err)
	}

	if err := registry.Delete(created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := registry.Get(created.ID); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestAccountRegistryRejectsDuplicateRemoteUser(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	registry := store.AccountRegistry()

	first, err := registry.Create(account.Record{Label: "一号", MaskedLoginName: "o***@example.test", Status: account.StatusEnabled})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := registry.Create(account.Record{Label: "二号", MaskedLoginName: "t***@example.test", Status: account.StatusEnabled})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if err := registry.SetRemoteUserID(first.ID, 1524); err != nil {
		t.Fatalf("SetRemoteUserID(first) error = %v", err)
	}
	if err := registry.SetRemoteUserID(second.ID, 1524); !errors.Is(err, account.ErrDuplicateRemoteUser) {
		t.Fatalf("SetRemoteUserID(duplicate) error = %v, want ErrDuplicateRemoteUser", err)
	}
}

func TestAccountRegistryPreservesMigratedIDs(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	registry := store.AccountRegistry()

	created, err := registry.Create(account.Record{ID: "account-a", Label: "账号一", MaskedLoginName: "a***@example.test", Status: account.StatusEnabled})
	if err != nil {
		t.Fatalf("Create(account-a) error = %v", err)
	}
	if created.ID != "account-a" {
		t.Fatalf("Create() changed a provided ID: %q", created.ID)
	}
	if _, err := registry.Create(account.Record{ID: "account-a", Label: "重复", MaskedLoginName: "a***@example.test", Status: account.StatusEnabled}); err == nil {
		t.Fatal("Create() accepted a duplicate ID")
	}
}

func TestRemoveAccountScopedStateOnlyTouchesOneAccount(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	seedAccountBusinessData(t, store, "account-a")
	seedAccountBusinessData(t, store, "account-b")

	if err := store.RemoveAccountScopedState("account-a"); err != nil {
		t.Fatalf("RemoveAccountScopedState() error = %v", err)
	}

	if _, ok := store.Action("account-a", "2026-08-29", ActionCheckin); ok {
		t.Fatal("account-a action survived scoped removal")
	}
	if _, ok := store.Action("account-b", "2026-08-29", ActionCheckin); !ok {
		t.Fatal("account-b action was removed")
	}
	if _, ok := store.Snapshot("account-b", "subscriptions"); !ok {
		t.Fatal("account-b snapshot was removed")
	}
	plans := store.AutoDrawPlans("2026-08-29")
	if len(plans) != 1 || plans[0].AccountID != "account-b" {
		t.Fatalf("plans after scoped removal = %#v", plans)
	}
	logs := store.RuntimeLogs(10)
	if len(logs) != 1 || logs[0].AccountID != "account-b" {
		t.Fatalf("logs after scoped removal = %#v", logs)
	}
}
