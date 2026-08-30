package state

import (
	"testing"

	"skyeapi/lottery-bot/internal/account"
)


func TestNormalizeDrawScheduleAcceptsCheckinTask(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	registry := store.AccountRegistry()
	if _, err := registry.Create(account.Record{ID: "account-a", Label: "账号一", MaskedLoginName: "a***@example.test", Status: account.StatusEnabled}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	saved, err := store.SetDrawSchedules("account-a", []AutoDrawSchedule{
		{ID: "checkin", Kind: "fixed", Start: "10:00", TaskType: AutoTaskCheckin, Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetDrawSchedules(checkin) error = %v", err)
	}
	if saved[0].TaskType != AutoTaskCheckin {
		t.Fatalf("task type = %q, want checkin", saved[0].TaskType)
	}

	if _, err := store.SetDrawSchedules("account-a", []AutoDrawSchedule{
		{ID: "bad", Kind: "fixed", Start: "10:00", TaskType: "subscribe", Enabled: true},
	}); err == nil {
		t.Fatal("unknown task type accepted")
	}
}
