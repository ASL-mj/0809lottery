package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

func testMoney(raw string) quota.Money {
	amount, err := quota.ParseUSD(raw)
	if err != nil {
		panic(err)
	}
	return quota.NewAlreadyUSDPolicy().Convert(amount, quota.Provenance{Source: "test"})
}

func testMoneyPointer(raw string) *quota.Money {
	money := testMoney(raw)
	return &money
}

func TestAutoDrawSchedulerCreatesPlansForUserSchedules(t *testing.T) {
	store := openAutoDrawTestStore(t)
	now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
	offsets := []int{0, 3599, 1800, 12, 34, 56}
	scheduler := newAutoDrawTestScheduler(store, []string{"account-b", "account-a"}, &now, offsets, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		t.Fatal("draw must not run before the first schedule")
		return DrawAvailableOutcome{}, nil
	})

	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	plans := store.AutoDrawPlans(now.Format("2006-01-02"))
	if len(plans) != 6 {
		t.Fatalf("plan count = %d, want 6: %#v", len(plans), plans)
	}
	seen := make(map[string]state.AutoDrawPlan)
	for _, plan := range plans {
		if plan.IdempotencyKey == "" || plan.Status != state.AutoDrawPlanPending {
			t.Fatalf("new plan = %#v", plan)
		}
		seen[plan.AccountID+"/"+plan.WindowID] = plan
	}
	for _, accountID := range []string{"account-a", "account-b"} {
		morning := seen[accountID+"/morning"]
		if local := morning.PlannedAt.In(shanghaiLocation); local.Hour() != 8 || local.Minute() != 0 || local.Second() != 0 {
			t.Fatalf("fixed schedule time = %s, want 08:00:00", local)
		}
		evening := seen[accountID+"/evening"]
		if local := evening.PlannedAt.In(shanghaiLocation); local.Hour() != 18 || local.Minute() != 0 {
			t.Fatalf("fixed schedule time = %s, want 18:00", local)
		}
		afternoon := seen[accountID+"/afternoon"]
		local := afternoon.PlannedAt.In(shanghaiLocation)
		if local.Hour() != 13 || local.Minute() < 0 || local.Minute() >= 60 {
			t.Fatalf("random schedule time outside window: %s", local)
		}
	}

	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if len(store.AutoDrawPlans(now.Format("2006-01-02"))) != 6 {
		t.Fatal("second tick duplicated plans")
	}
}

// Accounts without any user-defined schedule must not receive plans, and a
// fixed schedule whose time already passed today is not created retroactively.
func TestAutoDrawSchedulerWithoutSchedulesCreatesNoPlans(t *testing.T) {
	store := openAutoDrawTestStore(t)
	now := autoDrawTime(2026, time.August, 8, 9, 30, 0)
	scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, nil, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		t.Fatal("nothing may be scheduled without user schedules")
		return DrawAvailableOutcome{}, nil
	})

	if _, err := store.SetDrawSchedules("account-a", nil); err != nil {
		t.Fatalf("clear schedules: %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() without schedules error = %v", err)
	}
	if plans := store.AutoDrawPlans(now.Format("2006-01-02")); len(plans) != 0 {
		t.Fatalf("plans created without schedules: %#v", plans)
	}

	// A missed fixed time is not created retroactively...
	if _, err := store.SetDrawSchedules("account-a", []state.AutoDrawSchedule{
		{ID: "missed", Kind: state.AutoDrawScheduleFixed, Start: "08:30"},
	}); err != nil {
		t.Fatalf("set missed schedule: %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() with missed schedule error = %v", err)
	}
	if _, err := findAutoDrawPlanErr(store, now.Format("2006-01-02"), "account-a", "missed"); err == nil {
		t.Fatal("plan was created for a missed fixed time")
	}

	// ...but an upcoming one is, and fires at the exact minute once the
	// scheduler has ticked between setting it and the fire time.
	if _, err := store.SetDrawSchedules("account-a", []state.AutoDrawSchedule{
		{ID: "upcoming", Kind: state.AutoDrawScheduleFixed, Start: "09:45"},
	}); err != nil {
		t.Fatalf("set upcoming schedule: %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() after setting upcoming schedule error = %v", err)
	}
	now = autoDrawTime(2026, time.August, 8, 9, 45, 1)
	calls := 0
	scheduler.draw = func(context.Context, string, string) (DrawAvailableOutcome, error) {
		calls++
		return DrawAvailableOutcome{Result: &lottery.DrawResult{Prize: lottery.Prize{ShortLabel: "奖品"}}}, nil
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() with upcoming schedule error = %v", err)
	}
	plan := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "upcoming")
	if plan.Status != state.AutoDrawPlanCompleted || calls != 1 {
		t.Fatalf("fixed schedule did not fire once: calls=%d plan=%#v", calls, plan)
	}
}

func findAutoDrawPlanErr(store *state.Store, date, accountID, scheduleID string) (state.AutoDrawPlan, error) {
	for _, plan := range store.AutoDrawPlans(date) {
		if plan.AccountID == accountID && plan.WindowID == scheduleID {
			return plan, nil
		}
	}
	return state.AutoDrawPlan{}, errors.New("plan not found")
}

func TestAutoDrawSchedulerCompletesDuePlanOnlyOnce(t *testing.T) {
	store := openAutoDrawTestStore(t)
	now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
	calls := 0
	scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		calls++
		return DrawAvailableOutcome{
			Result:        &lottery.DrawResult{Prize: lottery.Prize{ShortLabel: "额度奖励"}},
			QuotaDeltaUSD: testMoneyPointer("1.25"),
		}, nil
	})
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("initial Tick() error = %v", err)
	}
	now = autoDrawTime(2026, time.August, 8, 8, 0, 1)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("due Tick() error = %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("repeat due Tick() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("draw calls = %d, want 1", calls)
	}
	morning := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "morning")
	if morning.Status != state.AutoDrawPlanCompleted || morning.PrizeLabel != "额度奖励" || morning.QuotaDeltaUSD == nil || morning.QuotaDeltaUSD.Value != "1.25" || morning.ExecutedAt.IsZero() {
		t.Fatalf("finished plan = %#v", morning)
	}
	logs := store.RuntimeLogs(10)
	if len(logs) != 1 || logs[0].Status != state.AutoDrawPlanCompleted || logs[0].PrizeLabel != "额度奖励" || logs[0].QuotaDeltaUSD == nil || logs[0].QuotaDeltaUSD.Value != "1.25" {
		t.Fatalf("runtime logs = %#v", logs)
	}
}

func TestAutoDrawSchedulerRestartKeepsExistingScheduleAndDoesNotDuplicateTerminalPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
	first := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{33, 44, 55}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		t.Fatal("draw must not run before the window")
		return DrawAvailableOutcome{}, nil
	})
	if err := first.Tick(context.Background()); err != nil {
		t.Fatalf("initial Tick() error = %v", err)
	}
	before := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "morning")
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store, err = state.Open(path)
	if err != nil {
		t.Fatalf("reopen state error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now = autoDrawTime(2026, time.August, 8, 8, 10, 0)
	calls := 0
	restarted := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, nil, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		calls++
		return DrawAvailableOutcome{}, nil
	})
	restarted.randomOffset = func(int) (int, error) { return 0, errors.New("existing plans must not be rescheduled") }
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatalf("restart Tick() error = %v", err)
	}
	after := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "morning")
	if after.PlannedAt != before.PlannedAt || after.IdempotencyKey != before.IdempotencyKey || after.Status != state.AutoDrawPlanCompleted {
		t.Fatalf("restart changed saved plan: before=%#v after=%#v", before, after)
	}
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatalf("repeat restart Tick() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("draw calls after restart = %d, want 1", calls)
	}
}

func TestAutoDrawSchedulerRecordsSkipAndSafeFailureWithoutRetry(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		store := openAutoDrawTestStore(t)
		now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
		calls := 0
		scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
			calls++
			return DrawAvailableOutcome{Skipped: true}, nil
		})
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("initial Tick() error = %v", err)
		}
		now = autoDrawTime(2026, time.August, 8, 8, 1, 0)
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("due Tick() error = %v", err)
		}
		if calls != 1 {
			t.Fatalf("draw calls = %d, want 1", calls)
		}
		logs := store.RuntimeLogs(10)
		if len(logs) != 1 || logs[0].Status != state.AutoDrawPlanSkipped || !strings.Contains(logs[0].Message, "无可用") {
			t.Fatalf("skip logs = %#v", logs)
		}
	})

	t.Run("failure", func(t *testing.T) {
		store := openAutoDrawTestStore(t)
		now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
		calls := 0
		scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
			calls++
			return DrawAvailableOutcome{}, errors.New("Bearer hidden-token cookie=private-password")
		})
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("initial Tick() error = %v", err)
		}
		now = autoDrawTime(2026, time.August, 8, 8, 1, 0)
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("due Tick() error = %v", err)
		}
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("second due Tick() error = %v", err)
		}
		if calls != 1 {
			t.Fatalf("failed plan retried: calls = %d", calls)
		}
		logs := store.RuntimeLogs(10)
		if len(logs) != 1 || logs[0].Status != state.AutoDrawPlanFailed || logs[0].Message != "抽奖服务请求失败" {
			t.Fatalf("failure logs = %#v", logs)
		}
		serialized := logs[0].Message
		for _, secret := range []string{"hidden-token", "private-password", "cookie", "Bearer"} {
			if strings.Contains(serialized, secret) {
				t.Fatalf("runtime log leaked %q: %s", secret, serialized)
			}
		}
	})
}

func TestAutoDrawSchedulerCancellationLeavesPlansRecoverable(t *testing.T) {
	t.Run("canceled startup does not create plans", func(t *testing.T) {
		store := openAutoDrawTestStore(t)
		now := autoDrawTime(2026, time.August, 8, 8, 5, 0)
		scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
			t.Fatal("draw must not run for canceled startup")
			return DrawAvailableOutcome{}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		scheduler.Run(ctx)
		if plans := store.AutoDrawPlans(now.Format("2006-01-02")); len(plans) != 0 {
			t.Fatalf("canceled startup created plans: %#v", plans)
		}
	})

	t.Run("in flight cancellation resumes with same plan", func(t *testing.T) {
		store := openAutoDrawTestStore(t)
		now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
		scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
			t.Fatal("draw must not run while creating the schedule")
			return DrawAvailableOutcome{}, nil
		})
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("initial Tick() error = %v", err)
		}
		now = autoDrawTime(2026, time.August, 8, 8, 1, 0)
		calls := 0
		parent, cancel := context.WithCancel(context.Background())
		scheduler.draw = func(ctx context.Context, _, _ string) (DrawAvailableOutcome, error) {
			calls++
			cancel()
			return DrawAvailableOutcome{}, ctx.Err()
		}
		if err := scheduler.Tick(parent); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Tick() error = %v, want context.Canceled", err)
		}
		plan := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "morning")
		if plan.Status != state.AutoDrawPlanRunning || len(store.RuntimeLogs(10)) != 0 {
			t.Fatalf("canceled plan was finalized: %#v logs=%#v", plan, store.RuntimeLogs(10))
		}

		scheduler.draw = func(context.Context, string, string) (DrawAvailableOutcome, error) {
			calls++
			return DrawAvailableOutcome{}, nil
		}
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("recovery Tick() error = %v", err)
		}
		plan = findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "morning")
		if calls != 2 || plan.Status != state.AutoDrawPlanCompleted || len(store.RuntimeLogs(10)) != 1 {
			t.Fatalf("recovery did not complete exactly once: calls=%d plan=%#v logs=%#v", calls, plan, store.RuntimeLogs(10))
		}
	})
}

func openAutoDrawTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newAutoDrawTestScheduler(store *state.Store, accountIDs []string, now *time.Time, offsets []int, draw AutoDrawExecutor) *AutoDrawScheduler {
	registry := store.AccountRegistry()
	for _, accountID := range accountIDs {
		if _, err := registry.Create(account.Record{ID: accountID, Label: accountID, MaskedLoginName: "t***@example.test", Status: account.StatusEnabled}); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				panic(fmt.Sprintf("create test account %s: %v", accountID, err))
			}
		}
		if _, err := store.SetDrawSchedules(accountID, []state.AutoDrawSchedule{
			{ID: "morning", Kind: state.AutoDrawScheduleFixed, Start: "08:00"},
			{ID: "afternoon", Kind: state.AutoDrawScheduleRandom, Start: "13:00", End: "14:00"},
			{ID: "evening", Kind: state.AutoDrawScheduleFixed, Start: "18:00"},
		}); err != nil {
			panic(fmt.Sprintf("seed draw schedules for %s: %v", accountID, err))
		}
	}
	nextOffset := 0
	return &AutoDrawScheduler{
		store:    store,
		repo:     registry,
		now: func() time.Time {
			return *now
		},
		randomOffset: func(limit int) (int, error) {
			if limit <= 0 {
				return 0, errors.New("invalid random limit")
			}
			if nextOffset >= len(offsets) {
				return 0, errors.New("random offset exhausted")
			}
			value := offsets[nextOffset]
			nextOffset++
			return value, nil
		},
		draw:             draw,
		tickInterval:     time.Hour,
		operationTimeout: time.Second,
	}
}

func autoDrawTime(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, shanghaiLocation)
}

func findAutoDrawPlan(t *testing.T, store *state.Store, date, accountID, windowID string) state.AutoDrawPlan {
	t.Helper()
	for _, plan := range store.AutoDrawPlans(date) {
		if plan.AccountID == accountID && plan.WindowID == windowID {
			return plan
		}
	}
	t.Fatalf("auto draw plan not found: %s/%s/%s", date, accountID, windowID)
	return state.AutoDrawPlan{}
}

// Only enabled registry accounts may receive auto-draw plans.
func TestSchedulerPlansOnlyEnabledAccounts(t *testing.T) {
	store := openAutoDrawTestStore(t)
	now := autoDrawTime(2026, time.August, 8, 7, 0, 0)
	scheduler := newAutoDrawTestScheduler(store, []string{"account-a", "account-b"}, &now, []int{0, 0, 0, 0, 0, 0}, func(context.Context, string, string) (DrawAvailableOutcome, error) {
		t.Fatal("no plan is due yet")
		return DrawAvailableOutcome{}, nil
	})

	if _, err := scheduler.repo.Update(account.Record{ID: "account-b", Label: "account-b", MaskedLoginName: "t***@example.test", Status: account.StatusDisabled}); err != nil {
		t.Fatalf("disable account-b: %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	for _, plan := range store.AutoDrawPlans(now.Format("2006-01-02")) {
		if plan.AccountID == "account-b" {
			t.Fatalf("disabled account received a plan: %#v", plan)
		}
	}
}

// A reauth-required account must produce a persisted skipped plan without any
// login attempt.
func TestSchedulerSkipsReauthRequiredWithoutLogin(t *testing.T) {
	store := openAutoDrawTestStore(t)
	now := autoDrawTime(2026, time.August, 8, 13, 5, 0)
	client := &fakeClient{
		login:       lottery.LoginResult{UserID: 1, AccessToken: "implicit-session", AccessExpiresAt: now.Add(time.Hour).UTC()},
		refreshErrs: []error{&lottery.APIError{StatusCode: 401}},
	}
	broker := auth.NewBroker(store, storeVault{store: store}, func([]state.Cookie) (auth.PlatformClient, error) {
		return client, nil
	}).WithClock(func() time.Time { return now.UTC() })
	runner := NewRunner(config.Config{BaseURL: "https://unit.test"}, store, store.AccountRegistry(), broker)
	scheduler := newAutoDrawTestScheduler(store, []string{"account-a"}, &now, []int{0, 0, 0}, func(ctx context.Context, accountID, key string) (DrawAvailableOutcome, error) {
		return runner.DrawAvailableScheduled(ctx, accountID, key)
	})

	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	plan := findAutoDrawPlan(t, store, now.Format("2006-01-02"), "account-a", "afternoon")
	if plan.Status != state.AutoDrawPlanSkipped {
		t.Fatalf("plan status = %s, want skipped", plan.Status)
	}
	if !strings.Contains(plan.Message, "重新认证") {
		t.Fatalf("skip message must demand reauthentication: %q", plan.Message)
	}
	if client.loginCalls != 0 {
		t.Fatalf("scheduler logged in %d times", client.loginCalls)
	}
}
