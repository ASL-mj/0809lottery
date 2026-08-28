package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/quota"
)

func TestStorePersistsAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	auth := AuthState{UserID: 42, ParentAccessToken: "parent-token", Cookies: []Cookie{{Name: "session", Value: "cookie"}}}
	if err := store.PutAuth("account-a", auth); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	storedAuth := store.Auth("account-a")
	if storedAuth.UserID != 42 || len(storedAuth.Cookies) != 1 {
		t.Fatalf("Auth() = %#v", storedAuth)
	}
}

func TestStoreSerializesConcurrentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	defer first.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("second Open() error = nil, want state lock conflict")
	}
}

func TestStoreCreatesActionsIdempotently(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	first, created, err := store.GetOrCreateAction("account-a", "2026-08-05", ActionDailyClaim)
	if err != nil || !created {
		t.Fatalf("first GetOrCreateAction() = %#v, %v, %v", first, created, err)
	}
	second, created, err := store.GetOrCreateAction("account-a", "2026-08-05", ActionDailyClaim)
	if err != nil || created {
		t.Fatalf("second GetOrCreateAction() = %#v, %v, %v", second, created, err)
	}
	if second.Key != first.Key || second.IdempotencyKey != first.IdempotencyKey || second.Status != ActionPending {
		t.Fatalf("idempotent action changed: first=%#v second=%#v", first, second)
	}
	loaded, ok := store.Action("account-a", "2026-08-05", ActionDailyClaim)
	if !ok || loaded.Key != first.Key {
		t.Fatalf("Action() = %#v, %v", loaded, ok)
	}

	updated, err := store.UpdateAction(first.Key, func(action *Action) {
		action.Status = ActionFailed
		action.Retryable = true
	})
	if err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	if updated.Status != ActionFailed || !updated.Retryable {
		t.Fatalf("updated action = %#v", updated)
	}
	reset, err := store.ResetRetryableAction(first.Key)
	if err != nil || reset.Status != ActionPending || reset.Retryable {
		t.Fatalf("ResetRetryableAction() = %#v, %v", reset, err)
	}
	_, err = store.UpdateAction(first.Key, func(action *Action) { action.SideEffectStarted = true })
	if err != nil {
		t.Fatalf("UpdateAction(side effect) error = %v", err)
	}
	if _, err := store.ResetRetryableAction(first.Key); err == nil {
		t.Fatal("ResetRetryableAction() error = nil after side effect started")
	}
}

func TestStoreRotateRepeatableActionRotatesCompletedAndRetryableFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status ActionStatus
	}{
		{name: "completed", status: ActionCompleted},
		{name: "retryable failed", status: ActionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer store.Close()

			action, _, err := store.GetOrCreateAction("account-a", "2026-08-07", ActionDrawPurchase)
			if err != nil {
				t.Fatalf("GetOrCreateAction() error = %v", err)
			}
			originalKey := action.IdempotencyKey
			if _, err := store.UpdateAction(action.Key, func(value *Action) {
				value.Status = tc.status
				value.Retryable = tc.status == ActionFailed
				value.SideEffectStarted = false
				value.Attempts = 3
				value.PriceUSD = testMoneyPtr("1.25")
				value.PurchaseBeforeToday = intPointer(1)
				value.PurchaseBeforeRemaining = intPointer(0)
				value.PassBeforeUnlocked = boolPointer(false)
				value.Message = "old message"
				value.LastError = "old error"
				value.Result = &DrawSummary{DrawID: "draw-1"}
			}); err != nil {
				t.Fatalf("UpdateAction() error = %v", err)
			}

			rotated, err := store.RotateRepeatableAction(action.Key)
			if err != nil {
				t.Fatalf("RotateRepeatableAction() error = %v", err)
			}
			if rotated.Key != action.Key {
				t.Fatalf("RotateRepeatableAction() key = %q, want %q", rotated.Key, action.Key)
			}
			if rotated.IdempotencyKey == "" || rotated.IdempotencyKey == originalKey {
				t.Fatalf("RotateRepeatableAction() idempotency key = %q, want new value distinct from %q", rotated.IdempotencyKey, originalKey)
			}
			if rotated.Status != ActionPending || rotated.Retryable || rotated.SideEffectStarted || rotated.Attempts != 0 || rotated.PriceUSD != nil || rotated.PurchaseBeforeToday != nil || rotated.PurchaseBeforeRemaining != nil || rotated.PassBeforeUnlocked != nil || rotated.Message != "" || rotated.LastError != "" || rotated.Result != nil {
				t.Fatalf("RotateRepeatableAction() did not reset action: %#v", rotated)
			}
		})
	}
}

func TestStoreRotateRepeatableActionRejectsUnknownAndPendingSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            ActionStatus
		retryable         bool
		sideEffectStarted bool
	}{
		{name: "unknown side effect started", status: ActionUnknown, sideEffectStarted: true},
		{name: "pending side effect started", status: ActionPending, sideEffectStarted: true},
		{name: "completed side effect started", status: ActionCompleted, sideEffectStarted: true},
		{name: "failed not retryable", status: ActionFailed, retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer store.Close()

			action, _, err := store.GetOrCreateAction("account-a", "2026-08-07", ActionPassUnlock)
			if err != nil {
				t.Fatalf("GetOrCreateAction() error = %v", err)
			}
			originalKey := action.IdempotencyKey
			if _, err := store.UpdateAction(action.Key, func(value *Action) {
				value.Status = tc.status
				value.Retryable = tc.retryable
				value.SideEffectStarted = tc.sideEffectStarted
				value.Message = "keep me"
			}); err != nil {
				t.Fatalf("UpdateAction() error = %v", err)
			}

			if _, err := store.RotateRepeatableAction(action.Key); err == nil {
				t.Fatal("RotateRepeatableAction() error = nil, want rejection")
			}
			loaded, ok := store.Action("account-a", "2026-08-07", ActionPassUnlock)
			if !ok {
				t.Fatal("Action() missing after rejected rotation")
			}
			if loaded.IdempotencyKey != originalKey || loaded.Message != "keep me" {
				t.Fatalf("rejected rotation mutated action: %#v", loaded)
			}
		})
	}
}

func TestStoreLockActionSerializesSameKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	releaseFirst := store.LockAction("account-a", "2026-08-07", ActionDailyClaim)
	acquiredSecond := make(chan struct{}, 1)
	releaseSecond := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		unlock := store.LockAction("account-a", "2026-08-07", ActionDailyClaim)
		acquiredSecond <- struct{}{}
		<-releaseSecond
		unlock()
	}()

	select {
	case <-acquiredSecond:
		t.Fatal("second LockAction acquired before the first release")
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()

	select {
	case <-acquiredSecond:
	case <-time.After(time.Second):
		t.Fatal("second LockAction did not acquire after release")
	}

	close(releaseSecond)
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second LockAction goroutine did not finish")
	}
}

func TestStoreActionCopiesPurchaseEvidencePointers(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	action, _, err := store.GetOrCreateAction("account-a", "2026-08-07", ActionDrawPurchase)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *Action) {
		value.PriceUSD = testMoneyPtr("1.25")
		value.PurchaseBeforeToday = intPointer(2)
		value.PurchaseBeforeRemaining = intPointer(1)
		value.PassBeforeUnlocked = boolPointer(false)
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}

	loaded, ok := store.Action("account-a", "2026-08-07", ActionDrawPurchase)
	if !ok {
		t.Fatal("Action() missing")
	}
	loaded.PriceUSD.Value = "9.9"
	*loaded.PurchaseBeforeToday = 9
	*loaded.PurchaseBeforeRemaining = 9
	*loaded.PassBeforeUnlocked = true

	reloaded, ok := store.Action("account-a", "2026-08-07", ActionDrawPurchase)
	if !ok {
		t.Fatal("Action() missing on reload")
	}
	if reloaded.PriceUSD == nil || reloaded.PriceUSD.Value != "1.25" || reloaded.PurchaseBeforeToday == nil || *reloaded.PurchaseBeforeToday != 2 || reloaded.PurchaseBeforeRemaining == nil || *reloaded.PurchaseBeforeRemaining != 1 || reloaded.PassBeforeUnlocked == nil || *reloaded.PassBeforeUnlocked {
		t.Fatalf("Action() returned aliased purchase evidence pointers: %#v", reloaded)
	}
}

func TestStoreEnsureAutoDrawPlansPersistsThreeWindowsUniquely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	plannedAt := time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)
	inputs := []AutoDrawPlan{
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-1", PlannedAt: plannedAt},
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-2", PlannedAt: plannedAt.Add(10 * time.Minute)},
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-3", PlannedAt: plannedAt.Add(20 * time.Minute)},
	}
	created, err := store.EnsureAutoDrawPlans(inputs)
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() error = %v", err)
	}
	if len(created) != 3 {
		t.Fatalf("EnsureAutoDrawPlans() len = %d, want 3", len(created))
	}
	for i, plan := range created {
		if plan.Key == "" || plan.IdempotencyKey == "" {
			t.Fatalf("created plan missing key/idempotency: %#v", plan)
		}
		if want := "draw:auto:"; plan.IdempotencyKey[:len(want)] != want {
			t.Fatalf("created plan idempotency prefix = %q, want draw:auto:*", plan.IdempotencyKey)
		}
		if plan.PlannedAt != inputs[i].PlannedAt {
			t.Fatalf("created plan planned_at = %v, want %v", plan.PlannedAt, inputs[i].PlannedAt)
		}
	}

	duplicate, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-2", PlannedAt: plannedAt.Add(2 * time.Hour)},
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-3", PlannedAt: plannedAt.Add(3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() duplicate error = %v", err)
	}
	if duplicate[0].Key != created[1].Key || duplicate[0].PlannedAt != created[1].PlannedAt || duplicate[0].IdempotencyKey != created[1].IdempotencyKey {
		t.Fatalf("duplicate plan should preserve existing values: got %#v want %#v", duplicate[0], created[1])
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen Open() error = %v", err)
	}
	defer reopened.Close()

	plans := reopened.AutoDrawPlans("2026-08-07")
	if len(plans) != 3 {
		t.Fatalf("AutoDrawPlans() len = %d, want 3", len(plans))
	}
	loaded, ok := reopened.AutoDrawPlan(created[0].Key)
	if !ok || loaded.IdempotencyKey != created[0].IdempotencyKey || loaded.PlannedAt != created[0].PlannedAt {
		t.Fatalf("AutoDrawPlan() = %#v, %v", loaded, ok)
	}
}

func TestStoreBeginAutoDrawPlanHonorsTerminalAndRecoversRunning(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	created, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-pending", PlannedAt: time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)},
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-running", PlannedAt: time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC), Status: AutoDrawPlanRunning},
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-complete", PlannedAt: time.Date(2026, time.August, 7, 11, 0, 0, 0, time.UTC), Status: AutoDrawPlanCompleted, ExecutedAt: time.Date(2026, time.August, 7, 11, 5, 0, 0, time.UTC), Message: "done", PrizeLabel: "quota", QuotaDeltaUSD: testMoneyPtr("0.5")},
	})
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() error = %v", err)
	}

	pendingStarted, shouldExecute, err := store.BeginAutoDrawPlan(created[0].Key)
	if err != nil || !shouldExecute {
		t.Fatalf("BeginAutoDrawPlan(pending) = %#v, %v, %v", pendingStarted, shouldExecute, err)
	}
	if pendingStarted.Status != AutoDrawPlanRunning || !pendingStarted.ExecutedAt.IsZero() {
		t.Fatalf("BeginAutoDrawPlan(pending) plan = %#v", pendingStarted)
	}

	finished, err := store.FinishAutoDrawPlan(created[0].Key, AutoDrawPlanCompleted, "safe complete", "quota +1", testMoneyPtr("1.25"), time.Date(2026, time.August, 7, 9, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FinishAutoDrawPlan() error = %v", err)
	}
	if finished.Status != AutoDrawPlanCompleted || finished.Message != "safe complete" || finished.PrizeLabel != "quota +1" || finished.QuotaDeltaUSD == nil || finished.QuotaDeltaUSD.Value != "1.25" {
		t.Fatalf("FinishAutoDrawPlan() = %#v", finished)
	}

	terminal, shouldExecute, err := store.BeginAutoDrawPlan(created[0].Key)
	if err != nil || shouldExecute {
		t.Fatalf("BeginAutoDrawPlan(terminal) = %#v, %v, %v", terminal, shouldExecute, err)
	}
	if terminal.Status != AutoDrawPlanCompleted {
		t.Fatalf("terminal BeginAutoDrawPlan() = %#v", terminal)
	}

	runningBefore, ok := store.AutoDrawPlan(created[1].Key)
	if !ok {
		t.Fatalf("AutoDrawPlan() missing running plan")
	}
	runningBeforeUpdatedAt := runningBefore.UpdatedAt
	recovered, shouldExecute, err := store.BeginAutoDrawPlan(created[1].Key)
	if err != nil || !shouldExecute {
		t.Fatalf("BeginAutoDrawPlan(running) = %#v, %v, %v", recovered, shouldExecute, err)
	}
	if recovered.Status != AutoDrawPlanRunning || !recovered.UpdatedAt.After(runningBeforeUpdatedAt) {
		t.Fatalf("recovered running plan = %#v, previous updated_at = %v", recovered, runningBeforeUpdatedAt)
	}
}

func TestStoreRuntimeLogsNewestFirstAndCopiesQuotaPointers(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	first, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt:    time.Date(2026, time.August, 7, 8, 0, 0, 0, time.UTC),
		AccountID:     "account-a",
		WindowID:      "window-1",
		Status:        AutoDrawPlanRunning,
		Message:       "safe start",
		QuotaDeltaUSD: testMoneyPtr("0.25"),
	})
	if err != nil {
		t.Fatalf("AppendRuntimeLog(first) error = %v", err)
	}
	second, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt:    time.Date(2026, time.August, 7, 8, 5, 0, 0, time.UTC),
		AccountID:     "account-a",
		WindowID:      "window-1",
		Status:        AutoDrawPlanCompleted,
		Message:       "safe finish",
		PrizeLabel:    "quota +1",
		QuotaDeltaUSD: testMoneyPtr("1"),
	})
	if err != nil {
		t.Fatalf("AppendRuntimeLog(second) error = %v", err)
	}

	logs := store.RuntimeLogs(10)
	if len(logs) != 2 || logs[0].ID != second.ID || logs[1].ID != first.ID {
		t.Fatalf("RuntimeLogs() = %#v", logs)
	}
	logs[0].QuotaDeltaUSD.Value = "9.9"

	reloaded := store.RuntimeLogs(1)
	if len(reloaded) != 1 || reloaded[0].QuotaDeltaUSD == nil || reloaded[0].QuotaDeltaUSD.Value != "1" {
		t.Fatalf("RuntimeLogs() returned aliased quota pointer: %#v", reloaded)
	}
}

func TestStorePruneAutoDrawDataRemovesEntriesOlderThanSevenDays(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	created, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{
		{Date: "2026-07-30", AccountID: "account-a", WindowID: "old-window", PlannedAt: time.Date(2026, time.July, 30, 9, 0, 0, 0, time.UTC)},
		{Date: "2026-08-03", AccountID: "account-a", WindowID: "keep-window", PlannedAt: time.Date(2026, time.August, 3, 9, 0, 0, 0, time.UTC)},
		{Date: "2026-08-01", AccountID: "account-a", WindowID: "cutoff-old-window", PlannedAt: time.Date(2026, time.July, 31, 23, 59, 0, 0, time.UTC)},
		{Date: "2026-08-01", AccountID: "account-a", WindowID: "cutoff-keep-window", PlannedAt: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() error = %v", err)
	}
	if _, err := store.FinishAutoDrawPlan(created[0].Key, AutoDrawPlanFailed, "safe old failure", "", nil, time.Date(2026, time.July, 30, 9, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("FinishAutoDrawPlan(old) error = %v", err)
	}
	if _, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt: time.Date(2026, time.July, 30, 9, 5, 0, 0, time.UTC),
		AccountID:  "account-a",
		WindowID:   "old-window",
		Status:     AutoDrawPlanFailed,
		Message:    "safe old failure",
	}); err != nil {
		t.Fatalf("AppendRuntimeLog(old) error = %v", err)
	}
	if _, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt: time.Date(2026, time.August, 3, 9, 5, 0, 0, time.UTC),
		AccountID:  "account-a",
		WindowID:   "keep-window",
		Status:     AutoDrawPlanCompleted,
		Message:    "safe keep",
	}); err != nil {
		t.Fatalf("AppendRuntimeLog(keep) error = %v", err)
	}

	if err := store.PruneAutoDrawData("2026-08-01", time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("PruneAutoDrawData() error = %v", err)
	}
	remaining := store.AutoDrawPlans("")
	if len(remaining) != 2 || remaining[0].WindowID != "cutoff-keep-window" || remaining[1].WindowID != "keep-window" {
		t.Fatalf("remaining plans = %#v", remaining)
	}
	logs := store.RuntimeLogs(10)
	if len(logs) != 1 || logs[0].WindowID != "keep-window" {
		t.Fatalf("remaining logs = %#v", logs)
	}
}

func TestStoreSanitizesSensitiveAutoDrawDisplayText(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	plans, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{{
		Date:      "2026-08-08",
		AccountID: "account-a",
		WindowID:  "morning",
		PlannedAt: time.Date(2026, time.August, 8, 0, 0, 0, 0, time.UTC),
	}})
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() error = %v", err)
	}
	finished, err := store.FinishAutoDrawPlan(plans[0].Key, AutoDrawPlanFailed, "Bearer hidden-token", "password: hidden", nil, time.Time{})
	if err != nil {
		t.Fatalf("FinishAutoDrawPlan() error = %v", err)
	}
	if finished.Message != "已隐藏敏感详情" || finished.PrizeLabel != "已隐藏敏感详情" {
		t.Fatalf("finished plan leaked sensitive display text: %#v", finished)
	}
	log, err := store.AppendRuntimeLog(RuntimeLog{
		AccountID:  "account-a",
		WindowID:   "morning",
		Status:     AutoDrawPlanFailed,
		Message:    "cookie=hidden",
		PrizeLabel: "idempotency-key",
	})
	if err != nil {
		t.Fatalf("AppendRuntimeLog() error = %v", err)
	}
	if log.Message != "已隐藏敏感详情" || log.PrizeLabel != "已隐藏敏感详情" {
		t.Fatalf("runtime log leaked sensitive display text: %#v", log)
	}
}

func TestStoreLoadsCompatibleStateAndIgnoresUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := map[string]interface{}{
		"version": 1,
		"accounts": map[string]interface{}{
			"account-a": map[string]interface{}{"user_id": 42},
		},
		"actions": map[string]interface{}{
			"2026-08-05:checkin:account-a": map[string]interface{}{
				"key":            "2026-08-05:checkin:account-a",
				"account_id":     "account-a",
				"date":           "2026-08-05",
				"kind":           "checkin",
				"status":         "completed",
				"obsolete_field": "ignored",
			},
		},
		"obsolete_section": map[string]interface{}{"status": "completed"},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() legacy state error = %v", err)
	}
	defer store.Close()

	if auth := store.Auth("account-a"); auth.UserID != 42 {
		t.Fatalf("migrated auth = %#v", auth)
	}
	action, ok := store.Action("account-a", "2026-08-05", ActionCheckin)
	if !ok || action.Status != ActionCompleted {
		t.Fatalf("migrated action = %#v, %v", action, ok)
	}
	if plans := store.AutoDrawPlans("2026-08-05"); len(plans) != 0 {
		t.Fatalf("legacy auto draw plans = %#v, want empty", plans)
	}
	if logs := store.RuntimeLogs(10); len(logs) != 0 {
		t.Fatalf("legacy runtime logs = %#v, want empty", logs)
	}

	if err := store.PutSnapshot(Snapshot{
		AccountID: "account-a",
		Kind:      "dashboard",
		Data:      json.RawMessage(`{"eligibility":{"remaining":2}}`),
	}); err != nil {
		t.Fatalf("PutSnapshot() error = %v", err)
	}
	snapshot, ok := store.Snapshot("account-a", "dashboard")
	if !ok || string(snapshot.Data) != `{"eligibility":{"remaining":2}}` {
		t.Fatalf("Snapshot() = %#v, %v", snapshot, ok)
	}
}

func TestStoreRollsBackInMemoryStateWhenPersistFails(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	if err := store.PutAuth("account-a", AuthState{UserID: 1, ParentAccessToken: "parent-old"}); err != nil {
		t.Fatalf("PutAuth() seed error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", "2026-08-07", ActionCheckin)
	if err != nil {
		t.Fatalf("GetOrCreateAction() seed error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *Action) {
		value.Status = ActionFailed
		value.Retryable = true
		value.Message = "old message"
	}); err != nil {
		t.Fatalf("UpdateAction() seed error = %v", err)
	}
	if err := store.PutSnapshot(Snapshot{
		AccountID: "account-a",
		Kind:      "dashboard",
		Data:      json.RawMessage(`{"remaining":1}`),
	}); err != nil {
		t.Fatalf("PutSnapshot() seed error = %v", err)
	}

	brokenPath := filepath.Join(t.TempDir(), "missing", "state.json")
	restorePath := store.path

	store.path = brokenPath
	if err := store.PutAuth("account-a", AuthState{UserID: 2, ParentAccessToken: "parent-new"}); err == nil {
		t.Fatal("PutAuth() error = nil, want persist failure")
	}
	store.path = restorePath
	auth := store.Auth("account-a")
	if auth.UserID != 1 || auth.ParentAccessToken != "parent-old" {
		t.Fatalf("PutAuth() rollback failed: %#v", auth)
	}

	store.path = brokenPath
	if _, err := store.UpdateAction(action.Key, func(value *Action) {
		value.Status = ActionCompleted
		value.Message = "new message"
		value.Retryable = false
	}); err == nil {
		t.Fatal("UpdateAction() error = nil, want persist failure")
	}
	store.path = restorePath
	rolledBackAction, ok := store.Action("account-a", "2026-08-07", ActionCheckin)
	if !ok || rolledBackAction.Status != ActionFailed || !rolledBackAction.Retryable || rolledBackAction.Message != "old message" {
		t.Fatalf("UpdateAction() rollback failed: %#v, %v", rolledBackAction, ok)
	}

	store.path = brokenPath
	if _, err := store.ResetRetryableAction(action.Key); err == nil {
		t.Fatal("ResetRetryableAction() error = nil, want persist failure")
	}
	store.path = restorePath
	rolledBackAction, ok = store.Action("account-a", "2026-08-07", ActionCheckin)
	if !ok || rolledBackAction.Status != ActionFailed || !rolledBackAction.Retryable || rolledBackAction.Message != "old message" {
		t.Fatalf("ResetRetryableAction() rollback failed: %#v, %v", rolledBackAction, ok)
	}

	store.path = brokenPath
	if _, _, err := store.GetOrCreateAction("account-a", "2026-08-08", ActionDailyClaim); err == nil {
		t.Fatal("GetOrCreateAction() error = nil, want persist failure")
	}
	store.path = restorePath
	if _, ok := store.Action("account-a", "2026-08-08", ActionDailyClaim); ok {
		t.Fatal("GetOrCreateAction() left behind action after persist failure")
	}

	store.path = brokenPath
	if err := store.PutSnapshot(Snapshot{
		AccountID: "account-a",
		Kind:      "dashboard",
		Data:      json.RawMessage(`{"remaining":2}`),
	}); err == nil {
		t.Fatal("PutSnapshot() error = nil, want persist failure")
	}
	store.path = restorePath
	snapshot, ok := store.Snapshot("account-a", "dashboard")
	if !ok || string(snapshot.Data) != `{"remaining":1}` {
		t.Fatalf("PutSnapshot() rollback failed: %#v, %v", snapshot, ok)
	}

	autoPlans, err := store.EnsureAutoDrawPlans([]AutoDrawPlan{
		{Date: "2026-08-07", AccountID: "account-a", WindowID: "window-1", PlannedAt: time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("EnsureAutoDrawPlans() seed error = %v", err)
	}
	store.path = brokenPath
	if _, err := store.FinishAutoDrawPlan(autoPlans[0].Key, AutoDrawPlanFailed, "new message", "", nil, time.Date(2026, time.August, 7, 9, 5, 0, 0, time.UTC)); err == nil {
		t.Fatal("FinishAutoDrawPlan() error = nil, want persist failure")
	}
	store.path = restorePath
	rolledBackPlan, ok := store.AutoDrawPlan(autoPlans[0].Key)
	if !ok || rolledBackPlan.Status != AutoDrawPlanPending || rolledBackPlan.Message != "" {
		t.Fatalf("FinishAutoDrawPlan() rollback failed: %#v, %v", rolledBackPlan, ok)
	}

	store.path = brokenPath
	if _, err := store.AppendRuntimeLog(RuntimeLog{
		OccurredAt: time.Date(2026, time.August, 7, 9, 10, 0, 0, time.UTC),
		AccountID:  "account-a",
		WindowID:   "window-1",
		Status:     AutoDrawPlanCompleted,
		Message:    "safe message",
	}); err == nil {
		t.Fatal("AppendRuntimeLog() error = nil, want persist failure")
	}
	store.path = restorePath
	if logs := store.RuntimeLogs(10); len(logs) != 0 {
		t.Fatalf("AppendRuntimeLog() rollback failed: %#v", logs)
	}
}

func intPointer(value int) *int {
	return &value
}

func float64Ptr(value float64) *float64 {
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}


func testMoneyPtr(raw string) *quota.Money {
	amount, err := quota.ParseUSD(raw)
	if err != nil {
		panic(err)
	}
	money := quota.NewAlreadyUSDPolicy().Convert(amount, quota.Provenance{Source: "test"})
	return &money
}
