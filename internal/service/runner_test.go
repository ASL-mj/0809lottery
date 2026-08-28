package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/state"
)

type fakeClient struct {
	mu                      sync.Mutex
	loginCalls              int
	refreshCalls            int
	bridgeCalls             int
	drawCalls               int
	purchaseCalls           int
	unlockCalls             int
	dashboardCalls          int
	claimCalls              int
	checkinCalls            int
	checkinStatusCalls      int
	checkinEligibilityCalls int
	statusCalls             int
	userSelfCalls           int
	drawTokens              []string
	drawKeys                []string
	purchaseTokens          []string
	purchaseKeys            []string
	unlockTokens            []string
	unlockKeys              []string
	claimTokens             []string
	login                   lottery.LoginResult
	refresh                 lottery.LoginResult
	refreshErrs             []error
	refreshEntered          chan struct{}
	refreshRelease          <-chan struct{}
	bridge                  lottery.BridgeResult
	bridgeErrs              []error
	drawResults             []lottery.DrawResult
	drawErrs                []error
	purchaseResults         []lottery.OperationResult
	purchaseErrs            []error
	unlockResults           []lottery.OperationResult
	unlockErrs              []error
	dashboards              []lottery.Dashboard
	dashboardErrs           []error
	claimResults            []lottery.ClaimResult
	claimErrs               []error
	checkinResults          []lottery.CheckinResult
	checkinErrs             []error
	checkinStatuses         []lottery.CheckinStatus
	checkinStatusErrs       []error
	checkinEligibilities    []lottery.CheckinEligibility
	checkinEligibilityErrs  []error
	statusErrs              []error
	userSelfTokens          []string
	userSelfErrs            []error
	userUsage               lottery.UserUsage
	subscriptionPlans       map[int]string
	subscriptionSelf        lottery.SubscriptionSelf
	status                  lottery.StatusSettings
	subscriptionErr         error
	drawEntered             chan struct{}
	drawRelease             <-chan struct{}
	purchaseEntered         chan struct{}
	purchaseRelease         <-chan struct{}
	unlockEntered           chan struct{}
	unlockRelease           <-chan struct{}
	claimEntered            chan struct{}
	claimRelease            <-chan struct{}
}

func (f *fakeClient) Login(_ context.Context, _ lottery.Credentials) (lottery.LoginResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls++
	return f.login, nil
}

func (f *fakeClient) Refresh(_ context.Context) (lottery.LoginResult, error) {
	f.mu.Lock()
	f.refreshCalls++
	index := f.refreshCalls - 1
	entered := f.refreshEntered
	release := f.refreshRelease
	var err error
	var result lottery.LoginResult
	if index < len(f.refreshErrs) && f.refreshErrs[index] != nil {
		err = f.refreshErrs[index]
	} else if f.refresh.UserID <= 0 || f.refresh.AccessToken == "" {
		err = &lottery.APIError{StatusCode: http.StatusUnauthorized}
	} else {
		result = f.refresh
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.LoginResult{}, err
	}
	return result, nil
}

func (f *fakeClient) Bridge(_ context.Context, _ string, _ int64) (lottery.BridgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridgeCalls++
	if len(f.bridgeErrs) > 0 {
		err := f.bridgeErrs[0]
		f.bridgeErrs = f.bridgeErrs[1:]
		if err != nil {
			return lottery.BridgeResult{}, err
		}
	}
	return f.bridge, nil
}

func (f *fakeClient) Draw(_ context.Context, token, key string) (lottery.DrawResult, error) {
	f.mu.Lock()
	f.drawCalls++
	f.drawTokens = append(f.drawTokens, token)
	f.drawKeys = append(f.drawKeys, key)
	index := f.drawCalls - 1
	entered := f.drawEntered
	release := f.drawRelease
	var err error
	var result lottery.DrawResult
	switch {
	case index < len(f.drawErrs) && f.drawErrs[index] != nil:
		err = f.drawErrs[index]
	case index < len(f.drawResults):
		result = f.drawResults[index]
	default:
		err = errors.New("missing fake draw result")
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.DrawResult{}, err
	}
	return result, nil
}

func (f *fakeClient) PurchaseDraw(_ context.Context, token, key string) (lottery.OperationResult, error) {
	f.mu.Lock()
	f.purchaseCalls++
	f.purchaseTokens = append(f.purchaseTokens, token)
	f.purchaseKeys = append(f.purchaseKeys, key)
	index := f.purchaseCalls - 1
	entered := f.purchaseEntered
	release := f.purchaseRelease
	var err error
	var result lottery.OperationResult
	switch {
	case index < len(f.purchaseErrs) && f.purchaseErrs[index] != nil:
		err = f.purchaseErrs[index]
	case index < len(f.purchaseResults):
		result = f.purchaseResults[index]
	default:
		result = lottery.OperationResult{Status: "ok"}
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.OperationResult{}, err
	}
	return result, nil
}

func (f *fakeClient) UnlockDrawLimit(_ context.Context, token, key string) (lottery.OperationResult, error) {
	f.mu.Lock()
	f.unlockCalls++
	f.unlockTokens = append(f.unlockTokens, token)
	f.unlockKeys = append(f.unlockKeys, key)
	index := f.unlockCalls - 1
	entered := f.unlockEntered
	release := f.unlockRelease
	var err error
	var result lottery.OperationResult
	switch {
	case index < len(f.unlockErrs) && f.unlockErrs[index] != nil:
		err = f.unlockErrs[index]
	case index < len(f.unlockResults):
		result = f.unlockResults[index]
	default:
		result = lottery.OperationResult{Status: "ok"}
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.OperationResult{}, err
	}
	return result, nil
}

func (f *fakeClient) Dashboard(_ context.Context, _ string) (lottery.Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dashboardCalls++
	index := f.dashboardCalls - 1
	if index < len(f.dashboardErrs) && f.dashboardErrs[index] != nil {
		return lottery.Dashboard{}, f.dashboardErrs[index]
	}
	if index < len(f.dashboards) {
		return f.dashboards[index], nil
	}
	return lottery.Dashboard{}, errors.New("missing fake dashboard result")
}

func (f *fakeClient) ClaimDaily(_ context.Context, token string) (lottery.ClaimResult, error) {
	f.mu.Lock()
	f.claimCalls++
	f.claimTokens = append(f.claimTokens, token)
	index := f.claimCalls - 1
	entered := f.claimEntered
	release := f.claimRelease
	var err error
	var result lottery.ClaimResult
	switch {
	case index < len(f.claimErrs) && f.claimErrs[index] != nil:
		err = f.claimErrs[index]
	case index < len(f.claimResults):
		result = f.claimResults[index]
	default:
		err = errors.New("missing fake claim result")
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.ClaimResult{}, err
	}
	return result, nil
}

func (f *fakeClient) Checkin(_ context.Context, _ string) (lottery.CheckinResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkinCalls++
	index := f.checkinCalls - 1
	if index < len(f.checkinErrs) && f.checkinErrs[index] != nil {
		return lottery.CheckinResult{}, f.checkinErrs[index]
	}
	if index < len(f.checkinResults) {
		return f.checkinResults[index], nil
	}
	return lottery.CheckinResult{}, errors.New("missing fake check-in result")
}

func (f *fakeClient) CheckinStatus(_ context.Context, _ string) (lottery.CheckinStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkinStatusCalls++
	index := f.checkinStatusCalls - 1
	if index < len(f.checkinStatusErrs) && f.checkinStatusErrs[index] != nil {
		return lottery.CheckinStatus{}, f.checkinStatusErrs[index]
	}
	if index < len(f.checkinStatuses) {
		return f.checkinStatuses[index], nil
	}
	return lottery.CheckinStatus{}, errors.New("missing fake check-in status")
}

func (f *fakeClient) CheckinEligibility(_ context.Context, _ string, _ int64) (lottery.CheckinEligibility, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkinEligibilityCalls++
	index := f.checkinEligibilityCalls - 1
	if index < len(f.checkinEligibilityErrs) && f.checkinEligibilityErrs[index] != nil {
		return lottery.CheckinEligibility{}, f.checkinEligibilityErrs[index]
	}
	if index < len(f.checkinEligibilities) {
		return f.checkinEligibilities[index], nil
	}
	return lottery.CheckinEligibility{}, errors.New("missing fake check-in eligibility")
}

func (f *fakeClient) UserSelf(_ context.Context, token string) (lottery.UserUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userSelfCalls++
	f.userSelfTokens = append(f.userSelfTokens, token)
	index := f.userSelfCalls - 1
	if index < len(f.userSelfErrs) && f.userSelfErrs[index] != nil {
		return lottery.UserUsage{}, f.userSelfErrs[index]
	}
	if f.subscriptionErr != nil {
		return lottery.UserUsage{}, f.subscriptionErr
	}
	return f.userUsage, nil
}

func (f *fakeClient) SubscriptionPlans(_ context.Context, _ string) (map[int]string, error) {
	if f.subscriptionErr != nil {
		return nil, f.subscriptionErr
	}
	return f.subscriptionPlans, nil
}

func (f *fakeClient) SubscriptionSelf(_ context.Context, _ string) (lottery.SubscriptionSelf, error) {
	if f.subscriptionErr != nil {
		return lottery.SubscriptionSelf{}, f.subscriptionErr
	}
	return f.subscriptionSelf, nil
}

func (f *fakeClient) Status(_ context.Context, _ string) (lottery.StatusSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	index := f.statusCalls - 1
	if index < len(f.statusErrs) && f.statusErrs[index] != nil {
		return lottery.StatusSettings{}, f.statusErrs[index]
	}
	if f.subscriptionErr != nil {
		return lottery.StatusSettings{}, f.subscriptionErr
	}
	return f.status, nil
}

func (f *fakeClient) Cookies() []state.Cookie {
	return []state.Cookie{{Name: "new_api_refresh", Value: "refresh"}}
}

func TestRunnerDashboardReusesLotteryToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 4, 8, 50, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		LotteryAccessToken:     "cached-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithRemaining(2)}}
	runner := testRunner(t, store, client, now)

	dashboard, err := runner.Dashboard(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	if remaining, ok := dashboard.Remaining(); !ok || remaining != 2 || client.loginCalls != 0 || client.bridgeCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected dashboard=%#v client=%#v", dashboard, client)
	}
}

func TestRunnerDashboardRecoversUnauthorizedLotteryToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 4, 8, 50, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "expired-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		bridge:        lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
		dashboardErrs: []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		dashboards:    []lottery.Dashboard{{}, dashboardWithRemaining(1)},
	}
	runner := testRunner(t, store, client, now)

	dashboard, err := runner.Dashboard(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	if remaining, ok := dashboard.Remaining(); !ok || remaining != 1 || client.bridgeCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected recovered dashboard=%#v client=%#v", dashboard, client)
	}
	auth := store.Auth("account-a")
	if auth.LotteryAccessToken != "fresh-token" {
		t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
	}
}

func TestQuerySubscriptionsFiltersAndAggregatesActiveSubscriptions(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 10, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "parent-token",
		ParentAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		subscriptionPlans: map[int]string{7: "高级订阅", 8: "不限额订阅"},
		subscriptionSelf: lottery.SubscriptionSelf{Subscriptions: []lottery.SubscriptionSummary{
			{Subscription: &lottery.UserSubscription{ID: 12, PlanID: 7, AmountTotal: 1000, AmountUsed: 250, EndTime: now.Add(24 * time.Hour).Unix(), Status: "active"}},
			{Subscription: &lottery.UserSubscription{ID: 13, PlanID: 8, AmountTotal: 0, AmountUsed: 0, EndTime: now.Add(48 * time.Hour).Unix(), Status: "active"}},
			{Subscription: &lottery.UserSubscription{ID: 14, PlanID: 7, AmountTotal: 900, AmountUsed: 100, EndTime: now.Add(-time.Hour).Unix(), Status: "active"}},
			{Subscription: &lottery.UserSubscription{ID: 15, PlanID: 7, AmountTotal: 900, AmountUsed: 100, EndTime: now.Add(72 * time.Hour).Unix(), Status: "cancelled"}},
		}},
		status: lottery.StatusSettings{QuotaPerUnit: 500, QuotaDisplayType: "USD"},
	}
	runner := testRunner(t, store, client, now)

	report, err := runner.QuerySubscriptions(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QuerySubscriptions() error = %v", err)
	}
	if len(report.Accounts) != 1 || len(report.Accounts[0].Subscriptions) != 2 {
		t.Fatalf("active subscriptions = %#v", report.Accounts)
	}
	account := report.Accounts[0]
	if !account.HasUnlimited || account.RemainingTotal != 750 {
		t.Fatalf("account summary = %#v", account)
	}
	if report.ActiveAccountCount != 1 || report.ActiveSubscriptionCount != 2 || report.FiniteTotalAmount != 1000 || report.FiniteRemainingAmount != 750 || report.UnlimitedSubscriptionCount != 1 {
		t.Fatalf("global summary = %#v", report)
	}
}

func TestQuerySubscriptionsRequiresReauthWhenRefreshRejected(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 10, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "expired-parent-token",
		ParentAccessExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		userSelfErrs:      []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		subscriptionPlans: map[int]string{7: "高级订阅"},
		subscriptionSelf: lottery.SubscriptionSelf{Subscriptions: []lottery.SubscriptionSummary{
			{Subscription: &lottery.UserSubscription{ID: 12, PlanID: 7, AmountTotal: 1000, AmountUsed: 250, EndTime: now.Add(time.Hour).Unix(), Status: "active"}},
		}},
	}
	runner := testRunner(t, store, client, now)

	report, err := runner.QuerySubscriptions(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QuerySubscriptions() error = %v", err)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].QueryError == "" {
		t.Fatalf("QuerySubscriptions() = %#v, want a reauthentication query error", report)
	}
	if !strings.Contains(report.Accounts[0].QueryError, "explicit reauthentication") {
		t.Fatalf("query error must demand explicit reauthentication: %q", report.Accounts[0].QueryError)
	}
	if client.refreshCalls != 1 || client.loginCalls != 0 {
		t.Fatalf("subscription query refresh/login calls = %d/%d, want 1/0", client.refreshCalls, client.loginCalls)
	}
}

func TestRunnerCheckinUsesParentToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		checkinResults: []lottery.CheckinResult{{Success: true, Message: "签到成功", QuotaAwarded: 1200}},
		status:         lottery.StatusSettings{QuotaPerUnit: 500},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || client.checkinCalls != 1 || client.loginCalls != 0 {
		t.Fatalf("unexpected check-in outcome=%#v client=%#v", outcome, client)
	}
	if outcome.Action.CheckinQuotaAwarded == nil || *outcome.Action.CheckinQuotaAwarded != 1200 {
		t.Fatalf("check-in reward was not recorded: %#v", outcome.Action)
	}
}

func TestRunnerCheckinStopsBeforePostWhenDailyActivityIsInsufficient(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		checkinEligibilities: []lottery.CheckinEligibility{{CanCheckin: false, Remaining: 1000000, Required: 1000000}},
		status:               lottery.StatusSettings{QuotaPerUnit: 500000},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionFailed || !outcome.Action.Retryable || outcome.Action.SideEffectStarted || client.checkinCalls != 0 || !strings.Contains(outcome.Action.Message, "$2.00") {
		t.Fatalf("unexpected precheck outcome=%#v client=%#v", outcome, client)
	}
}

func TestRunnerCheckinMakesExplicitFailureRetryable(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		checkinEligibilities: []lottery.CheckinEligibility{{CanCheckin: true}},
		checkinResults:       []lottery.CheckinResult{{Success: false, Message: "签到失败，请稍后重试"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionFailed || !outcome.Action.Retryable || outcome.Action.SideEffectStarted || client.checkinCalls != 1 {
		t.Fatalf("unexpected explicit failure outcome=%#v client=%#v", outcome, client)
	}
}

func TestRunnerCheckinReconcilesFailedLocalActionWithUpstreamSuccess(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionCheckin)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.SideEffectStarted = true
		value.Retryable = false
		value.Message = "签到失败，请稍后重试"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	reward := 491727.0
	client := &fakeClient{
		checkinStatuses: []lottery.CheckinStatus{{CheckedInToday: true, TodayQuotaAwarded: &reward}},
		status:          lottery.StatusSettings{QuotaPerUnit: 500000},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || !outcome.AlreadyRecorded || client.checkinCalls != 0 || outcome.Action.LastError != "" || outcome.Action.CheckinQuotaAwardedUSD == nil || *outcome.Action.CheckinQuotaAwardedUSD != 0.983454 {
		t.Fatalf("unexpected reconciled outcome=%#v client=%#v", outcome, client)
	}
}

func TestRunnerCheckinReopensFailedLocalActionWhenUpstreamIsNotCheckedIn(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionCheckin)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.SideEffectStarted = true
		value.Retryable = false
		value.Message = "签到失败，请稍后重试"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{
		checkinStatuses:      []lottery.CheckinStatus{{CheckedInToday: false}},
		checkinEligibilities: []lottery.CheckinEligibility{{CanCheckin: true}},
		checkinResults:       []lottery.CheckinResult{{Success: true, Message: "签到成功", QuotaAwarded: 1200}},
		status:               lottery.StatusSettings{QuotaPerUnit: 500},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || outcome.AlreadyRecorded || client.checkinStatusCalls != 1 || client.checkinEligibilityCalls != 1 || client.checkinCalls != 1 {
		t.Fatalf("unexpected reopened outcome=%#v client=%#v", outcome, client)
	}
}

func TestRunnerCheckinEligibilityRecoversForbiddenParentToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 1, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "stale-parent",
		ParentAccessExpiresAt: now.Add(time.Hour),
		Cookies:               []state.Cookie{{Name: "new_api_refresh", Value: "refresh"}},
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		refresh:                lottery.LoginResult{UserID: 1, AccessToken: "refreshed-parent", AccessExpiresAt: now.Add(2 * time.Hour)},
		checkinEligibilityErrs: []error{&lottery.APIError{StatusCode: http.StatusForbidden}, nil},
		checkinEligibilities:   []lottery.CheckinEligibility{{}, {CanCheckin: true}},
		checkinResults:         []lottery.CheckinResult{{Success: true, Message: "签到成功"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.Checkin(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Checkin() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || client.loginCalls != 0 || client.refreshCalls != 1 || client.checkinEligibilityCalls != 2 || client.checkinCalls != 1 {
		t.Fatalf("unexpected recovered outcome=%#v client=%#v", outcome, client)
	}
	if auth := store.Auth("account-a"); auth.ParentAccessToken != "refreshed-parent" {
		t.Fatalf("refreshed parent token was not persisted: %#v", auth)
	}
}

func TestRunnerCheckinStatusReusesAndRecoversParentToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "stale-parent",
		ParentAccessExpiresAt: now.Add(time.Hour),
		Cookies:               []state.Cookie{{Name: "new_api_refresh", Value: "refresh"}},
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	reward := 1200.0
	client := &fakeClient{
		refresh:           lottery.LoginResult{UserID: 1, AccessToken: "refreshed-parent", AccessExpiresAt: now.Add(2 * time.Hour)},
		checkinStatusErrs: []error{&lottery.APIError{StatusCode: 403}},
		checkinStatuses: []lottery.CheckinStatus{
			{},
			{CheckedInToday: true, TodayQuotaAwarded: &reward},
		},
		status: lottery.StatusSettings{QuotaPerUnit: 500},
	}
	runner := testRunner(t, store, client, now)

	status, err := runner.CheckinStatus(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("CheckinStatus() error = %v", err)
	}
	if !status.CheckedInToday || status.TodayQuotaAwardedUSD == nil || *status.TodayQuotaAwardedUSD != 2.4 || client.checkinStatusCalls != 2 || client.loginCalls != 0 || client.refreshCalls != 1 {
		t.Fatalf("unexpected check-in status=%#v client=%#v", status, client)
	}
	if auth := store.Auth("account-a"); auth.ParentAccessToken != "refreshed-parent" {
		t.Fatalf("refreshed parent token was not persisted: %#v", auth)
	}
}

func TestRunnerQueryDrawCountPreservesLockedOpportunity(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 21, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	remaining, locked, earned, purchased, dailyUsed, freeLimit := 0, 1, 1, 0, 3, 3
	unlocked := false
	client := &fakeClient{dashboards: []lottery.Dashboard{{
		Eligibility: lottery.DashboardEligibility{DrawLimit: lottery.DrawLimit{
			Remaining: &remaining, LockedRemaining: &locked, EarnedRemaining: &earned,
			PurchasedRemaining: &purchased, DailyUsed: &dailyUsed, FreeLimit: &freeLimit,
			Unlocked: &unlocked, Status: "locked", DayKey: "2026-08-07",
		}},
	}}}
	runner := testRunner(t, store, client, now)

	report, err := runner.QueryDrawCount(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QueryDrawCount() error = %v", err)
	}
	if report.Remaining != 0 || report.LockedRemaining != 1 || report.DailyUsed != 3 || report.FreeLimit != 3 || report.Unlocked || report.Status != "locked" || client.dashboardCalls != 1 || client.loginCalls != 0 || client.bridgeCalls != 0 {
		t.Fatalf("unexpected draw count report=%#v client=%#v", report, client)
	}
}

func TestRunnerQueryUsageStoresSanitizedSnapshot(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "parent-token",
		ParentAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		userUsage: lottery.UserUsage{Quota: 1000, UsedQuota: 250, RequestCount: 7},
		status:    lottery.StatusSettings{QuotaPerUnit: 500},
	}
	runner := testRunner(t, store, client, now)

	usage, err := runner.QueryUsage(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QueryUsage() error = %v", err)
	}
	if usage.Quota != 1000 || usage.UsedQuota != 250 || usage.RequestCount != 7 || usage.QuotaUSD == nil || *usage.QuotaUSD != 2 || usage.UsedQuotaUSD == nil || *usage.UsedQuotaUSD != 0.5 {
		t.Fatalf("usage = %#v", usage)
	}
	snapshot, ok := store.Snapshot("account-a", "usage")
	if !ok || !strings.Contains(string(snapshot.Data), "\"used_quota_usd\"") || strings.Contains(string(snapshot.Data), "\"used_quota\"") || strings.Contains(string(snapshot.Data), "parent-token") {
		t.Fatalf("usage snapshot = %#v, %v", snapshot, ok)
	}
}

func TestRunnerQueryActivityComputesSpendTierProgress(t *testing.T) {
	for _, tc := range []struct {
		name          string
		todaySpend    float64
		wantReached   int
		wantTotal     int
		wantThreshold *float64
		wantRemaining *float64
	}{
		{name: "zero tiers reached", todaySpend: 1, wantReached: 0, wantTotal: 3, wantThreshold: float64Ptr(5), wantRemaining: float64Ptr(4)},
		{name: "partial tiers reached", todaySpend: 8, wantReached: 1, wantTotal: 3, wantThreshold: float64Ptr(10), wantRemaining: float64Ptr(2)},
		{name: "all tiers reached", todaySpend: 15, wantReached: 3, wantTotal: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			defer store.Close()
			now := time.Date(2026, time.August, 7, 21, 10, 0, 0, shanghaiLocation)
			if err := store.PutAuth("account-a", state.AuthState{
				LotteryAccessToken:     "lottery-token",
				LotteryAccessExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("PutAuth() error = %v", err)
			}
			todaySpend := tc.todaySpend
			remaining := 4
			client := &fakeClient{dashboards: []lottery.Dashboard{{
				Eligibility: lottery.DashboardEligibility{
					TodaySpend: &todaySpend,
					Remaining:  &remaining,
				},
				Rules: lottery.DashboardRules{SpendTiers: []map[string]any{
					{"amount": 5.0, "draws": 1},
					{"amount": 10.0, "draws": 2},
					{"amount": 15.0, "draws": 3},
				}},
			}}}
			runner := testRunner(t, store, client, now)

			report, err := runner.QueryActivity(context.Background(), "account-a")
			if err != nil {
				t.Fatalf("QueryActivity() error = %v", err)
			}
			if report.SpendTierReached != tc.wantReached || report.SpendTierTotal != tc.wantTotal {
				t.Fatalf("tier progress = reached %d total %d, want %d/%d", report.SpendTierReached, report.SpendTierTotal, tc.wantReached, tc.wantTotal)
			}
			if !equalFloatPtr(report.NextSpendThresholdUSD, tc.wantThreshold) {
				t.Fatalf("NextSpendThresholdUSD = %#v, want %#v", report.NextSpendThresholdUSD, tc.wantThreshold)
			}
			if !equalFloatPtr(report.NextSpendRemainingUSD, tc.wantRemaining) {
				t.Fatalf("NextSpendRemainingUSD = %#v, want %#v", report.NextSpendRemainingUSD, tc.wantRemaining)
			}
		})
	}
}

func TestRunnerQueryActivitySupportsAliasesAndStoresSanitizedSnapshot(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 21, 15, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	todaySpend := 12.5
	bonusDraws := 2
	remaining := 6
	unlocked := true
	unlockCost := 3
	client := &fakeClient{dashboards: []lottery.Dashboard{{
		Eligibility: lottery.DashboardEligibility{
			TodaySpend:      &todaySpend,
			SpendBonusDraws: &bonusDraws,
			DrawLimit: lottery.DrawLimit{
				Remaining:  &remaining,
				Unlocked:   &unlocked,
				UnlockCost: &unlockCost,
				DayKey:     "2026-08-07",
			},
		},
		Rules: lottery.DashboardRules{SpendTiers: []map[string]any{
			{"threshold": 5.0, "bonusDraws": 1},
			{"amount": 10.0, "draws": 2},
		}},
		Lucky: map[string]any{
			"currentPoints": 11,
			"max":           20,
		},
		Purchase: map[string]any{
			"unitCost":       1.25,
			"todayPurchased": 3,
			"pendingCount":   1,
			"unknownCount":   2,
			"idempotency":    "should-not-leak",
		},
	}}}
	runner := testRunner(t, store, client, now)

	report, err := runner.QueryActivity(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QueryActivity() error = %v", err)
	}
	if report.TodaySpendUSD != 12.5 || report.SpendBonusDraws != 2 || report.LuckyPoints != 11 || report.LuckyMaxPoints != 20 {
		t.Fatalf("unexpected alias fields in report = %#v", report)
	}
	if report.DrawPurchaseCostUSD == nil || *report.DrawPurchaseCostUSD != 1.25 || report.PurchasedToday != 3 || report.PurchasePending != 1 || report.PurchaseUnknown != 2 {
		t.Fatalf("unexpected purchase fields in report = %#v", report)
	}
	if report.SpendTierTotal != 2 || report.PassUnlockCostUSD == nil || *report.PassUnlockCostUSD != 3 || !report.PassUnlocked || report.DayKey != "2026-08-07" {
		t.Fatalf("unexpected draw-limit fields in report = %#v", report)
	}
	if report.SpendTierReached != 2 || report.NextSpendThresholdUSD != nil || report.NextSpendRemainingUSD != nil {
		t.Fatalf("unexpected tier completion fields in report = %#v", report)
	}
	snapshot, ok := store.Snapshot("account-a", "activity")
	if !ok {
		t.Fatal("Snapshot(activity) missing")
	}
	var payload map[string]any
	if err := json.Unmarshal(snapshot.Data, &payload); err != nil {
		t.Fatalf("json.Unmarshal(snapshot.Data) error = %v", err)
	}
	gotKeys := make([]string, 0, len(payload))
	for key := range payload {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"account_id",
		"day_key",
		"draw_purchase_cost_usd",
		"lucky_max_points",
		"lucky_points",
		"pass_unlock_cost_usd",
		"pass_unlocked",
		"purchase_pending",
		"purchase_unknown",
		"purchased_today",
		"queried_at",
		"spend_bonus_draws",
		"spend_tier_reached",
		"spend_tier_total",
		"today_spend_usd",
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("activity snapshot keys = %#v, want %#v; payload=%s", gotKeys, wantKeys, string(snapshot.Data))
	}
	for _, forbidden := range []string{"lucky", "purchase", "total", "remaining", "idempotency", "cookie", "token"} {
		if _, exists := payload[forbidden]; exists {
			t.Fatalf("activity snapshot leaked forbidden key %q: %s", forbidden, string(snapshot.Data))
		}
	}
}

func TestRunnerQueryActivitySupportsPurchasePriceAlias(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 21, 20, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{{
		Purchase: map[string]any{
			"price": 2.5,
		},
	}}}
	runner := testRunner(t, store, client, now)

	report, err := runner.QueryActivity(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("QueryActivity() error = %v", err)
	}
	if report.DrawPurchaseCostUSD == nil || *report.DrawPurchaseCostUSD != 2.5 {
		t.Fatalf("DrawPurchaseCostUSD = %#v, want 2.5", report.DrawPurchaseCostUSD)
	}
}

func TestRunnerDrawAvailableRejectsEmptyIdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "blank", key: "   \t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			defer store.Close()
			now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
			if err := store.PutAuth("account-a", state.AuthState{
				UserID:                 1,
				ParentAccessToken:      "parent-token",
				ParentAccessExpiresAt:  now.Add(time.Hour),
				LotteryAccessToken:     "lottery-token",
				LotteryAccessExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("PutAuth() error = %v", err)
			}
			client := &fakeClient{}
			runner := testRunner(t, store, client, now)
			createClientCalls := 0
			runner.newClient = func([]state.Cookie) (WebsiteClient, error) {
				createClientCalls++
				return client, nil
			}

			_, err := runner.DrawAvailable(context.Background(), "account-a", tc.key)
			if err == nil || !strings.Contains(err.Error(), "幂等键不能为空") {
				t.Fatalf("DrawAvailable() error = %v, want empty-key rejection", err)
			}
			if createClientCalls != 0 || client.dashboardCalls != 0 || client.drawCalls != 0 || client.loginCalls != 0 || client.bridgeCalls != 0 {
				t.Fatalf("unexpected network calls: createClient=%d client=%#v", createClientCalls, client)
			}
		})
	}
}

func TestRunnerDrawAvailableDrawsWhenQuotaExists(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards:  []lottery.Dashboard{dashboardWithRemaining(2)},
		drawResults: []lottery.DrawResult{{ID: "draw-1", Prize: lottery.Prize{ShortLabel: "高额额度甲"}}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
	if err != nil {
		t.Fatalf("DrawAvailable() error = %v", err)
	}
	if outcome.Skipped || outcome.RemainingBefore != 2 || outcome.Result == nil || outcome.Result.ID != "draw-1" || outcome.Message != "手动抽奖成功" {
		t.Fatalf("unexpected draw outcome = %#v", outcome)
	}
	if client.drawCalls != 1 || len(client.drawKeys) != 1 || client.drawKeys[0] != "draw:web:test" {
		t.Fatalf("draw request = calls=%d keys=%#v", client.drawCalls, client.drawKeys)
	}
}

func TestRunnerDrawAvailableSkipsWithoutQuota(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithRemaining(0)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
	if err != nil {
		t.Fatalf("DrawAvailable() error = %v", err)
	}
	if !outcome.Skipped || outcome.RemainingBefore != 0 || outcome.Result != nil || client.drawCalls != 0 || outcome.Message != "当前没有可用抽奖次数，已跳过手动抽奖" {
		t.Fatalf("unexpected skip outcome = %#v, draw calls=%d", outcome, client.drawCalls)
	}
}

func TestRunnerDrawAvailableRecoversDashboardAuthErrorBeforeDraw(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			store := testStore(t)
			defer store.Close()
			now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
			if err := store.PutAuth("account-a", state.AuthState{
				UserID:                 1,
				ParentAccessToken:      "parent-token",
				ParentAccessExpiresAt:  now.Add(time.Hour),
				LotteryAccessToken:     "stale-token",
				LotteryAccessExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("PutAuth() error = %v", err)
			}
			client := &fakeClient{
				bridge:        lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
				dashboardErrs: []error{&lottery.APIError{StatusCode: statusCode}},
				dashboards: []lottery.Dashboard{
					{},
					dashboardWithRemaining(1),
				},
				drawResults: []lottery.DrawResult{{ID: "draw-1"}},
			}
			runner := testRunner(t, store, client, now)

			outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
			if err != nil {
				t.Fatalf("DrawAvailable() error = %v", err)
			}
			if outcome.Skipped || outcome.RemainingBefore != 1 || outcome.Result == nil || outcome.Result.ID != "draw-1" {
				t.Fatalf("unexpected outcome = %#v", outcome)
			}
			if client.dashboardCalls != 2 || client.bridgeCalls != 1 || client.drawCalls != 1 {
				t.Fatalf("unexpected client calls = %#v", client)
			}
			auth := store.Auth("account-a")
			if auth.LotteryAccessToken != "fresh-token" {
				t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
			}
		})
	}
}

func TestRunnerDrawAvailableRecoversAuthErrorAndReusesIdempotencyKey(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			store := testStore(t)
			defer store.Close()
			now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
			if err := store.PutAuth("account-a", state.AuthState{
				UserID:                 1,
				ParentAccessToken:      "parent-token",
				ParentAccessExpiresAt:  now.Add(time.Hour),
				LotteryAccessToken:     "stale-token",
				LotteryAccessExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("PutAuth() error = %v", err)
			}
			client := &fakeClient{
				bridge:     lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
				dashboards: []lottery.Dashboard{dashboardWithRemaining(1)},
				drawErrs:   []error{&lottery.APIError{StatusCode: statusCode}},
				drawResults: []lottery.DrawResult{
					{},
					{ID: "draw-1"},
				},
			}
			runner := testRunner(t, store, client, now)

			outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
			if err != nil {
				t.Fatalf("DrawAvailable() error = %v", err)
			}
			if outcome.Skipped || outcome.Result == nil || outcome.Result.ID != "draw-1" {
				t.Fatalf("unexpected outcome = %#v", outcome)
			}
			if client.drawCalls != 2 || len(client.drawKeys) != 2 || client.drawKeys[0] != "draw:web:test" || client.drawKeys[1] != "draw:web:test" {
				t.Fatalf("unexpected draw calls = %#v", client)
			}
			auth := store.Auth("account-a")
			if auth.LotteryAccessToken != "fresh-token" {
				t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
			}
		})
	}
}

func TestRunnerDrawAvailableRetriesTransientErrorWithSameIdempotencyKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{dashboardWithRemaining(1)},
		drawErrs:   []error{errors.New("temporary draw failure")},
		drawResults: []lottery.DrawResult{
			{},
			{ID: "draw-1"},
		},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
	if err != nil {
		t.Fatalf("DrawAvailable() error = %v", err)
	}
	if outcome.Skipped || outcome.Result == nil || outcome.Result.ID != "draw-1" {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.drawCalls != 2 || len(client.drawKeys) != 2 || client.drawKeys[0] != "draw:web:test" || client.drawKeys[1] != "draw:web:test" {
		t.Fatalf("unexpected draw retry = %#v", client)
	}
}

func TestRunnerDrawAvailableSerializesSameAccountCalls(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	drawRelease := make(chan struct{})
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithRemaining(1),
			dashboardWithRemaining(0),
		},
		drawResults: []lottery.DrawResult{{ID: "draw-1"}},
		drawEntered: make(chan struct{}, 1),
		drawRelease: drawRelease,
	}
	runner := testRunner(t, store, client, now)

	type drawResult struct {
		outcome DrawAvailableOutcome
		err     error
	}
	firstDone := make(chan drawResult, 1)
	go func() {
		outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:first")
		firstDone <- drawResult{outcome: outcome, err: err}
	}()

	select {
	case <-client.drawEntered:
	case <-time.After(time.Second):
		t.Fatal("first draw did not reach Draw()")
	}

	secondDone := make(chan drawResult, 1)
	go func() {
		outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:second")
		secondDone <- drawResult{outcome: outcome, err: err}
	}()

	select {
	case result := <-secondDone:
		t.Fatalf("second DrawAvailable returned before first release: %#v", result)
	case <-time.After(50 * time.Millisecond):
	}

	client.mu.Lock()
	if client.dashboardCalls != 1 || client.drawCalls != 1 {
		client.mu.Unlock()
		t.Fatalf("same-account call entered I/O before release: %#v", client)
	}
	client.mu.Unlock()

	close(drawRelease)

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first DrawAvailable() error = %v", first.err)
	}
	second := <-secondDone
	if second.err != nil {
		t.Fatalf("second DrawAvailable() error = %v", second.err)
	}
	if first.outcome.Skipped || first.outcome.Result == nil || first.outcome.Result.ID != "draw-1" {
		t.Fatalf("unexpected first outcome = %#v", first.outcome)
	}
	if !second.outcome.Skipped || second.outcome.Result != nil || second.outcome.RemainingBefore != 0 {
		t.Fatalf("unexpected second outcome = %#v", second.outcome)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.dashboardCalls != 2 || client.drawCalls != 1 {
		t.Fatalf("unexpected final client calls = %#v", client)
	}
}

func TestRunnerDrawAvailableComputesQuotaDeltaUSDAndRedactsJSON(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{dashboardWithRemaining(1)},
		drawResults: []lottery.DrawResult{{
			ID:    "draw-1",
			Prize: lottery.Prize{ShortLabel: "高额额度甲"},
			Effect: lottery.Effect{
				QuotaDelta: 250,
			},
		}},
		status: lottery.StatusSettings{QuotaPerUnit: 500},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
	if err != nil {
		t.Fatalf("DrawAvailable() error = %v", err)
	}
	if outcome.QuotaDeltaUSD == nil || *outcome.QuotaDeltaUSD != 0.5 {
		t.Fatalf("unexpected quota delta = %#v", outcome)
	}
	payload, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	jsonText := string(payload)
	if strings.Contains(jsonText, "quotaDelta") || strings.Contains(jsonText, "\"Result\"") || strings.Contains(jsonText, "250") || strings.Contains(jsonText, "draw:web:test") {
		t.Fatalf("manual draw outcome leaked internal fields: %s", jsonText)
	}
}

func TestRunnerDrawAvailableIgnoresQuotaConversionFailure(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{dashboardWithRemaining(1)},
		drawResults: []lottery.DrawResult{{
			ID:     "draw-1",
			Effect: lottery.Effect{QuotaDelta: 250},
		}},
		statusErrs: []error{errors.New("status unavailable")},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.DrawAvailable(context.Background(), "account-a", "draw:web:test")
	if err != nil {
		t.Fatalf("DrawAvailable() error = %v", err)
	}
	if outcome.Result == nil || outcome.Result.ID != "draw-1" || outcome.QuotaDeltaUSD != nil {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
}

func TestRunnerClaimDailySuccessUsesResponseDashboard(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	afterDashboard := dashboardWithRemaining(3)
	client := &fakeClient{
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimResults: []lottery.ClaimResult{{Success: true, Message: "领取成功", Dashboard: &afterDashboard}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if outcome.AlreadyRecorded || outcome.Added != 1 || outcome.Remaining == nil || *outcome.Remaining != 3 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.Status != state.ActionCompleted || outcome.Action.ClaimBeforeRemaining == nil || *outcome.Action.ClaimBeforeRemaining != 2 || outcome.Action.ClaimAfterRemaining == nil || *outcome.Action.ClaimAfterRemaining != 3 {
		t.Fatalf("unexpected action = %#v", outcome.Action)
	}
	if client.dashboardCalls != 1 || client.claimCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyCompletedActionDoesNotCallNetwork(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 5, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.ClaimBeforeRemaining = intPointer(2)
		value.ClaimAfterRemaining = intPointer(3)
		value.Message = "今日已领取"
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if !outcome.AlreadyRecorded || outcome.Added != 1 || outcome.Remaining == nil || *outcome.Remaining != 3 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.dashboardCalls != 0 || client.claimCalls != 0 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyPendingActionResumesAndSucceeds(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 7, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = false
		value.Message = "旧 pending"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	afterDashboard := dashboardWithRemaining(3)
	client := &fakeClient{
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimResults: []lottery.ClaimResult{{Success: true, Message: "领取成功", Dashboard: &afterDashboard}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if outcome.AlreadyRecorded || outcome.Action.Status != state.ActionCompleted || outcome.Added != 1 || outcome.Remaining == nil || *outcome.Remaining != 3 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.claimCalls != 1 || client.dashboardCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyExplicitFailureIsRetryable(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 10, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimResults: []lottery.ClaimResult{{Success: false, Message: "今日领取失败"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if outcome.Action.Status != state.ActionFailed || !outcome.Action.Retryable || outcome.Action.SideEffectStarted || outcome.Added != 0 || outcome.Remaining == nil || *outcome.Remaining != 2 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.dashboardCalls != 1 || client.claimCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyUnknownActionReconcilesToCompletedWithoutClaim(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 15, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.SideEffectStarted = true
		value.ClaimBeforeRemaining = intPointer(2)
		value.Message = "领取结果未知"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithRemaining(3)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if !outcome.AlreadyRecorded || outcome.Action.Status != state.ActionCompleted || outcome.Action.Message != "今日领取已对账完成" || outcome.Added != 1 || outcome.Remaining == nil || *outcome.Remaining != 3 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.claimCalls != 0 || client.dashboardCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyUnknownActionStaysUnknownWhenDashboardCannotProveSuccess(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 20, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.SideEffectStarted = true
		value.ClaimBeforeRemaining = intPointer(2)
		value.Message = "领取结果未知"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithRemaining(2)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if !outcome.AlreadyRecorded || outcome.Action.Status != state.ActionUnknown || outcome.Added != 0 || outcome.Remaining == nil || *outcome.Remaining != 2 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.ClaimAfterRemaining != nil {
		t.Fatalf("unexpected action after remaining = %#v", outcome.Action)
	}
	if client.claimCalls != 0 || client.dashboardCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyRetryableFailureResetsBeforeRetry(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 25, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.Retryable = true
		value.SideEffectStarted = false
		value.ClaimBeforeRemaining = intPointer(9)
		value.ClaimAfterRemaining = intPointer(10)
		value.Message = "旧失败记录"
		value.LastError = value.Message
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	afterDashboard := dashboardWithRemaining(4)
	client := &fakeClient{
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimResults: []lottery.ClaimResult{{Success: true, Message: "领取成功", Dashboard: &afterDashboard}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || outcome.Added != 2 || outcome.Remaining == nil || *outcome.Remaining != 4 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.ClaimBeforeRemaining == nil || *outcome.Action.ClaimBeforeRemaining != 2 || outcome.Action.ClaimAfterRemaining == nil || *outcome.Action.ClaimAfterRemaining != 4 || outcome.Action.LastError != "" {
		t.Fatalf("retry state was not refreshed: %#v", outcome.Action)
	}
	if client.dashboardCalls != 1 || client.claimCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyTransientErrorDoesNotRepeatClaim(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 30, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithRemaining(2),
			dashboardWithRemaining(2),
			dashboardWithRemaining(3),
		},
		claimErrs: []error{errors.New("temporary network failure")},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first ClaimDaily() error = %v", err)
	}
	if first.Action.Status != state.ActionUnknown || first.Added != 0 || first.Remaining == nil || *first.Remaining != 2 {
		t.Fatalf("unexpected first outcome = %#v", first)
	}
	second, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second ClaimDaily() error = %v", err)
	}
	if !second.AlreadyRecorded || second.Action.Status != state.ActionCompleted || second.Added != 1 || second.Remaining == nil || *second.Remaining != 3 {
		t.Fatalf("unexpected second outcome = %#v", second)
	}
	if client.claimCalls != 1 || client.dashboardCalls != 3 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyConcurrentCallsSerializeToSingleClaim(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 35, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	afterDashboard := dashboardWithRemaining(3)
	claimEntered := make(chan struct{}, 1)
	claimRelease := make(chan struct{})
	client := &fakeClient{
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimResults: []lottery.ClaimResult{{Success: true, Message: "领取成功", Dashboard: &afterDashboard}},
		claimEntered: claimEntered,
		claimRelease: claimRelease,
	}
	runner := testRunner(t, store, client, now)

	type result struct {
		outcome DailyClaimOutcome
		err     error
	}
	firstDone := make(chan result, 1)
	secondDone := make(chan result, 1)
	go func() {
		outcome, err := runner.ClaimDaily(context.Background(), "account-a")
		firstDone <- result{outcome: outcome, err: err}
	}()
	<-claimEntered
	go func() {
		outcome, err := runner.ClaimDaily(context.Background(), "account-a")
		secondDone <- result{outcome: outcome, err: err}
	}()

	select {
	case <-secondDone:
		t.Fatal("second ClaimDaily returned before first claim completed")
	case <-time.After(50 * time.Millisecond):
	}
	client.mu.Lock()
	claimCalls := client.claimCalls
	client.mu.Unlock()
	if claimCalls != 1 {
		t.Fatalf("claim calls before release = %d, want 1", claimCalls)
	}

	close(claimRelease)
	first := <-firstDone
	second := <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("ClaimDaily() errors = %#v %#v", first.err, second.err)
	}
	if first.outcome.AlreadyRecorded || first.outcome.Action.Status != state.ActionCompleted {
		t.Fatalf("unexpected first outcome = %#v", first.outcome)
	}
	if !second.outcome.AlreadyRecorded || second.outcome.Action.Status != state.ActionCompleted {
		t.Fatalf("unexpected second outcome = %#v", second.outcome)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.claimCalls != 1 || client.dashboardCalls != 1 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerClaimDailyRecoversUnauthorizedLotteryToken(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 14, 40, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "stale-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	afterDashboard := dashboardWithRemaining(3)
	client := &fakeClient{
		bridge:       lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
		dashboards:   []lottery.Dashboard{dashboardWithRemaining(2)},
		claimErrs:    []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		claimResults: []lottery.ClaimResult{{}, {Success: true, Message: "领取成功", Dashboard: &afterDashboard}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.ClaimDaily(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if outcome.Action.Status != state.ActionCompleted || outcome.Added != 1 || outcome.Remaining == nil || *outcome.Remaining != 3 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.claimCalls != 2 || client.bridgeCalls != 1 || client.loginCalls != 0 || client.drawCalls != 0 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
	if len(client.claimTokens) != 2 || client.claimTokens[0] != "stale-token" || client.claimTokens[1] != "fresh-token" {
		t.Fatalf("claim tokens = %#v", client.claimTokens)
	}
	auth := store.Auth("account-a")
	if auth.LotteryAccessToken != "fresh-token" {
		t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
	}
}

func TestRunnerPurchaseDrawSuccess(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 0, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(2, 1, 2, 1.25),
		},
		purchaseResults: []lottery.OperationResult{{Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("PurchaseDraw() error = %v", err)
	}
	if outcome.AlreadyRecorded || outcome.Status != string(state.ActionCompleted) || outcome.PriceUSD == nil || *outcome.PriceUSD != 1.25 || outcome.Remaining == nil || *outcome.Remaining != 2 {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Activity == nil || outcome.Activity.PurchasedToday != 2 {
		t.Fatalf("activity = %#v, want purchased today 2", outcome.Activity)
	}
	if outcome.Action.Status != state.ActionCompleted || outcome.Action.PurchaseBeforeToday == nil || *outcome.Action.PurchaseBeforeToday != 1 || outcome.Action.PurchaseBeforeRemaining == nil || *outcome.Action.PurchaseBeforeRemaining != 0 || outcome.Action.PriceUSD == nil || *outcome.Action.PriceUSD != 1.25 || outcome.Action.SideEffectStarted {
		t.Fatalf("unexpected action = %#v", outcome.Action)
	}
	snapshot, ok := store.Snapshot("account-a", "activity")
	if !ok || len(snapshot.Data) == 0 {
		t.Fatalf("Snapshot(activity) = %#v, %v", snapshot, ok)
	}
	if client.purchaseCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerPurchaseDrawCompletedActionRotatesToNewIdempotencyKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 5, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(0, 0, 1, 1.25),
			dashboardWithPurchase(1, 1, 2, 1.25),
			dashboardWithPurchase(1, 1, 2, 1.25),
			dashboardWithPurchase(2, 2, 3, 1.25),
		},
		purchaseResults: []lottery.OperationResult{{Status: "ok"}, {Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first PurchaseDraw() error = %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second PurchaseDraw() error = %v", err)
	}
	if first.Action.Key != second.Action.Key {
		t.Fatalf("action keys differ: %q vs %q", first.Action.Key, second.Action.Key)
	}
	if len(client.purchaseKeys) != 2 || client.purchaseKeys[0] == "" || client.purchaseKeys[1] == "" || client.purchaseKeys[0] == client.purchaseKeys[1] {
		t.Fatalf("purchase keys = %#v, want two distinct keys", client.purchaseKeys)
	}
	if client.purchaseCalls != 2 || client.dashboardCalls != 4 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerPurchaseDrawConcurrentCallsSerializeToSinglePost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 10, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	purchaseEntered := make(chan struct{}, 1)
	purchaseRelease := make(chan struct{})
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(2, 1, 2, 1.25),
		},
		purchaseResults: []lottery.OperationResult{{Status: "ok"}},
		purchaseEntered: purchaseEntered,
		purchaseRelease: purchaseRelease,
	}
	runner := testRunner(t, store, client, now)

	type result struct {
		outcome PurchaseOutcome
		err     error
	}
	firstDone := make(chan result, 1)
	secondDone := make(chan result, 1)
	go func() {
		outcome, err := runner.PurchaseDraw(context.Background(), "account-a")
		firstDone <- result{outcome: outcome, err: err}
	}()
	<-purchaseEntered
	go func() {
		outcome, err := runner.PurchaseDraw(context.Background(), "account-a")
		secondDone <- result{outcome: outcome, err: err}
	}()

	select {
	case <-secondDone:
		t.Fatal("second PurchaseDraw returned before first purchase completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(purchaseRelease)
	first := <-firstDone
	second := <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("PurchaseDraw() errors = %#v %#v", first.err, second.err)
	}
	if first.outcome.Status != string(state.ActionCompleted) || second.outcome.Status != string(state.ActionCompleted) || !second.outcome.AlreadyRecorded {
		t.Fatalf("unexpected outcomes = %#v %#v", first.outcome, second.outcome)
	}
	if client.purchaseCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerPurchaseDrawUnknownActionDoesNotRepost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 15, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(1, 0, 1, 1.25),
		},
		purchaseErrs: []error{errors.New("temporary network failure")},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first PurchaseDraw() error = %v", err)
	}
	second, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second PurchaseDraw() error = %v", err)
	}
	if first.Status != string(state.ActionUnknown) || second.Status != string(state.ActionUnknown) || !second.AlreadyRecorded {
		t.Fatalf("unexpected outcomes = %#v %#v", first, second)
	}
	if client.purchaseCalls != 1 || client.dashboardCalls != 3 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerPurchaseDrawRecoversUnauthorizedLotteryTokenWithSameKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 20, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "stale-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		bridge: lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(2, 1, 2, 1.25),
		},
		purchaseErrs:    []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		purchaseResults: []lottery.OperationResult{{Status: "ignored"}, {Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("PurchaseDraw() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.purchaseCalls != 2 || client.bridgeCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
	if len(client.purchaseTokens) != 2 || client.purchaseTokens[0] != "stale-token" || client.purchaseTokens[1] != "fresh-token" {
		t.Fatalf("purchase tokens = %#v", client.purchaseTokens)
	}
	if len(client.purchaseKeys) != 2 || client.purchaseKeys[0] == "" || client.purchaseKeys[0] != client.purchaseKeys[1] {
		t.Fatalf("purchase keys = %#v, want same key reused", client.purchaseKeys)
	}
	auth := store.Auth("account-a")
	if auth.LotteryAccessToken != "fresh-token" {
		t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
	}
}

func TestRunnerPurchaseDrawPaymentRequiredIsRetryableWithNewKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 25, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(1, 0, 1, 1.25),
			dashboardWithPurchase(2, 1, 2, 1.25),
		},
		purchaseErrs:    []error{&lottery.APIError{StatusCode: http.StatusPaymentRequired, Message: "insufficient balance"}},
		purchaseResults: []lottery.OperationResult{{Status: "ignored"}, {Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first PurchaseDraw() error = %v", err)
	}
	if first.Status != string(state.ActionFailed) || !first.Action.Retryable || first.Action.SideEffectStarted {
		t.Fatalf("unexpected first outcome = %#v", first)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second PurchaseDraw() error = %v", err)
	}
	if second.Status != string(state.ActionCompleted) {
		t.Fatalf("unexpected second outcome = %#v", second)
	}
	if len(client.purchaseKeys) != 2 || client.purchaseKeys[0] == client.purchaseKeys[1] {
		t.Fatalf("purchase keys = %#v, want new key on retry", client.purchaseKeys)
	}
}

func TestRunnerPurchaseDrawPendingSideEffectStartedReconcilesWithoutPost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 27, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionDrawPurchase)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = true
		value.PurchaseBeforeToday = intPointer(1)
		value.PurchaseBeforeRemaining = intPointer(0)
		value.Message = "购买处理中"
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithPurchase(2, 1, 2, 1.25)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.PurchaseDraw(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("PurchaseDraw() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.purchaseCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassAlreadyUnlockedSkipsPost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 30, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithPass(true, 2, 3)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) || !strings.Contains(outcome.Message, "已解锁") || outcome.Activity == nil || !outcome.Activity.PassUnlocked {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	snapshot, ok := store.Snapshot("account-a", "activity")
	if !ok {
		t.Fatal("Snapshot(activity) missing")
	}
	var report ActivityReport
	if err := json.Unmarshal(snapshot.Data, &report); err != nil {
		t.Fatalf("json.Unmarshal(activity snapshot) error = %v", err)
	}
	if !report.PassUnlocked {
		t.Fatalf("activity snapshot = %#v, want pass unlocked", report)
	}
	if client.unlockCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassStaleUnlockedDayKeyDoesNotSkipAndUsesTodayAfterProof(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 33, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPassDayKey(true, 2, 3, "2026-08-06"),
			dashboardWithPassDayKey(true, 2, 3, "2026-08-07"),
		},
		unlockResults: []lottery.OperationResult{{Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) || outcome.Activity == nil || !outcome.Activity.PassUnlocked {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.PassBeforeUnlocked == nil || *outcome.Action.PassBeforeUnlocked {
		t.Fatalf("unexpected action = %#v", outcome.Action)
	}
	if client.unlockCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassEmptyDayKeyDoesNotSkipAndUsesTodayAfterProof(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 34, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPassDayKey(true, 2, 3, ""),
			dashboardWithPassDayKey(true, 2, 3, "2026-08-07"),
		},
		unlockResults: []lottery.OperationResult{{Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) || outcome.Activity == nil || !outcome.Activity.PassUnlocked {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.PassBeforeUnlocked == nil || *outcome.Action.PassBeforeUnlocked {
		t.Fatalf("unexpected action = %#v", outcome.Action)
	}
	if client.unlockCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassSuccess(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 35, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(true, 2, 3),
		},
		unlockResults: []lottery.OperationResult{{Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) || outcome.PriceUSD == nil || *outcome.PriceUSD != 3 || outcome.Activity == nil || !outcome.Activity.PassUnlocked {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if outcome.Action.PassBeforeUnlocked == nil || *outcome.Action.PassBeforeUnlocked {
		t.Fatalf("unexpected action = %#v", outcome.Action)
	}
	if client.unlockCalls != 1 || client.dashboardCalls != 2 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassUnknownActionDoesNotRepost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 40, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(false, 2, 3),
		},
		unlockErrs: []error{errors.New("temporary network failure")},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first UnlockDailyPass() error = %v", err)
	}
	second, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second UnlockDailyPass() error = %v", err)
	}
	if first.Status != string(state.ActionUnknown) || second.Status != string(state.ActionUnknown) || !second.AlreadyRecorded {
		t.Fatalf("unexpected outcomes = %#v %#v", first, second)
	}
	if client.unlockCalls != 1 || client.dashboardCalls != 3 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassRecoversUnauthorizedLotteryTokenWithSameKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 45, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  now.Add(time.Hour),
		LotteryAccessToken:     "stale-token",
		LotteryAccessExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		bridge: lottery.BridgeResult{AccessToken: "fresh-token", ExpiresAt: now.Add(2 * time.Hour)},
		dashboards: []lottery.Dashboard{
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(true, 2, 3),
		},
		unlockErrs:    []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		unlockResults: []lottery.OperationResult{{Status: "ignored"}, {Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionCompleted) {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.unlockCalls != 2 || client.bridgeCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
	if len(client.unlockTokens) != 2 || client.unlockTokens[0] != "stale-token" || client.unlockTokens[1] != "fresh-token" {
		t.Fatalf("unlock tokens = %#v", client.unlockTokens)
	}
	if len(client.unlockKeys) != 2 || client.unlockKeys[0] == "" || client.unlockKeys[0] != client.unlockKeys[1] {
		t.Fatalf("unlock keys = %#v, want same key reused", client.unlockKeys)
	}
	auth := store.Auth("account-a")
	if auth.LotteryAccessToken != "fresh-token" {
		t.Fatalf("refreshed lottery token was not persisted: %#v", auth)
	}
}

func TestRunnerUnlockDailyPassPaymentRequiredIsRetryableWithNewKey(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 50, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	client := &fakeClient{
		dashboards: []lottery.Dashboard{
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(false, 2, 3),
			dashboardWithPass(true, 2, 3),
		},
		unlockErrs:    []error{&lottery.APIError{StatusCode: http.StatusPaymentRequired, Message: "insufficient balance"}},
		unlockResults: []lottery.OperationResult{{Status: "ignored"}, {Status: "ok"}},
	}
	runner := testRunner(t, store, client, now)

	first, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("first UnlockDailyPass() error = %v", err)
	}
	if first.Status != string(state.ActionFailed) || !first.Action.Retryable || first.Action.SideEffectStarted {
		t.Fatalf("unexpected first outcome = %#v", first)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("second UnlockDailyPass() error = %v", err)
	}
	if second.Status != string(state.ActionCompleted) {
		t.Fatalf("unexpected second outcome = %#v", second)
	}
	if len(client.unlockKeys) != 2 || client.unlockKeys[0] == client.unlockKeys[1] {
		t.Fatalf("unlock keys = %#v, want new key on retry", client.unlockKeys)
	}
}

func TestRunnerUnlockDailyPassSideEffectStartedReconcilesWithoutPost(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 55, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionPassUnlock)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = true
		value.PassBeforeUnlocked = boolPointerValue(false)
		value.Message = "通行证解锁处理中"
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithPass(false, 2, 3)}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionPending) || !outcome.AlreadyRecorded {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.unlockCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassStaleUnlockedDayKeyDoesNotMisreconcile(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 56, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionPassUnlock)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = true
		value.PassBeforeUnlocked = boolPointerValue(false)
		value.Message = "通行证解锁处理中"
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithPassDayKey(true, 2, 3, "2026-08-06")}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionPending) || !outcome.AlreadyRecorded {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.unlockCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestRunnerUnlockDailyPassEmptyDayKeyDoesNotMisreconcile(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 7, 15, 57, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	action, _, err := store.GetOrCreateAction("account-a", now.Format("2006-01-02"), state.ActionPassUnlock)
	if err != nil {
		t.Fatalf("GetOrCreateAction() error = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = true
		value.PassBeforeUnlocked = boolPointerValue(false)
		value.Message = "通行证解锁处理中"
	}); err != nil {
		t.Fatalf("UpdateAction() error = %v", err)
	}
	client := &fakeClient{dashboards: []lottery.Dashboard{dashboardWithPassDayKey(true, 2, 3, "")}}
	runner := testRunner(t, store, client, now)

	outcome, err := runner.UnlockDailyPass(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("UnlockDailyPass() error = %v", err)
	}
	if outcome.Status != string(state.ActionPending) || !outcome.AlreadyRecorded {
		t.Fatalf("unexpected outcome = %#v", outcome)
	}
	if client.unlockCalls != 0 || client.dashboardCalls != 1 {
		t.Fatalf("unexpected client calls = %#v", client)
	}
}

func TestIsExplicitInsufficient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "402", err: &lottery.APIError{StatusCode: http.StatusPaymentRequired}, want: true},
		{name: "insufficient balance", err: errors.New("insufficient balance for purchase"), want: true},
		{name: "insufficient quota", err: errors.New("insufficient quota"), want: true},
		{name: "chinese balance", err: errors.New("余额不足，请充值"), want: true},
		{name: "permission", err: errors.New("insufficient permission"), want: false},
		{name: "scope", err: errors.New("insufficient scope"), want: false},
		{name: "generic", err: errors.New("something else"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExplicitInsufficient(tc.err); got != tc.want {
				t.Fatalf("isExplicitInsufficient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestPurchaseOutcomeMarshalJSONOmitsSensitiveFields(t *testing.T) {
	payload, err := json.Marshal(PurchaseOutcome{
		AccountID: "account-a",
		Status:    string(state.ActionCompleted),
		Message:   "购买成功",
		PriceUSD:  float64Ptr(1.25),
		Remaining: intPointer(2),
		Activity:  &ActivityReport{AccountID: "account-a", PurchasedToday: 2},
		Action: state.Action{
			AccountID:      "account-hidden",
			IdempotencyKey: "secret-key",
			Message:        "hidden",
			LastError:      "hidden-error",
		},
		AlreadyRecorded: true,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	text := string(payload)
	for _, forbidden := range []string{"secret-key", "idempotency", "account-hidden", "hidden-error", "already_recorded", "\"action\""} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("json payload leaked %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"account_id":"account-a"`, `"status":"completed"`, `"price_usd":1.25`, `"remaining":2`, `"activity"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("json payload missing %q: %s", required, text)
		}
	}
}

func dashboardWithRemaining(value int) lottery.Dashboard {
	return lottery.Dashboard{Eligibility: lottery.DashboardEligibility{Remaining: &value}}
}

func dashboardWithPurchase(purchasedToday, purchasedRemaining, remaining int, price float64) lottery.Dashboard {
	return lottery.Dashboard{
		Purchase: map[string]any{
			"cost":            price,
			"purchasedToday":  purchasedToday,
			"pendingCount":    0,
			"unknownCount":    0,
			"todayPurchased":  purchasedToday,
			"purchased_count": purchasedToday,
		},
		DrawLimit: lottery.DrawLimit{
			PurchasedRemaining: intPointer(purchasedRemaining),
			Remaining:          intPointer(remaining),
		},
	}
}

func dashboardWithPass(unlocked bool, remaining, unlockCost int) lottery.Dashboard {
	return dashboardWithPassDayKey(unlocked, remaining, unlockCost, "2026-08-07")
}

func dashboardWithPassDayKey(unlocked bool, remaining, unlockCost int, dayKey string) lottery.Dashboard {
	return lottery.Dashboard{
		DrawLimit: lottery.DrawLimit{
			Unlocked:   &unlocked,
			Remaining:  intPointer(remaining),
			UnlockCost: intPointer(unlockCost),
			DayKey:     dayKey,
		},
	}
}

func float64Ptr(value float64) *float64 {
	return &value
}

func equalFloatPtr(got, want *float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return store
}

// storeVault bridges the vault API onto the legacy persisted auth state so
// existing fixtures keep seeding tokens through store.PutAuth.
type storeVault struct {
	store *state.Store
}

func (v storeVault) Load(_ context.Context, accountID string) (secret.Bundle, error) {
	authState := v.store.Auth(accountID)
	if authState.UserID == 0 && authState.ParentAccessToken == "" && authState.LotteryAccessToken == "" && len(authState.Cookies) == 0 {
		return secret.Bundle{}, secret.ErrNotFound
	}
	return secret.Bundle{
		UserID:                 authState.UserID,
		ParentAccessToken:      authState.ParentAccessToken,
		ParentAccessExpiresAt:  authState.ParentAccessExpiresAt,
		LotteryAccessToken:     authState.LotteryAccessToken,
		LotteryAccessExpiresAt: authState.LotteryAccessExpiresAt,
		Cookies:                legacyCookiesToVault(authState.Cookies),
	}, nil
}

func (v storeVault) Save(_ context.Context, accountID string, bundle secret.Bundle) error {
	return v.store.PutAuth(accountID, state.AuthState{
		UserID:                 bundle.UserID,
		ParentAccessToken:      bundle.ParentAccessToken,
		ParentAccessExpiresAt:  bundle.ParentAccessExpiresAt,
		LotteryAccessToken:     bundle.LotteryAccessToken,
		LotteryAccessExpiresAt: bundle.LotteryAccessExpiresAt,
		Cookies:                vaultCookiesToLegacy(bundle.Cookies),
	})
}

func (v storeVault) Delete(context.Context, string) error { return nil }

func legacyCookiesToVault(values []state.Cookie) []secret.Cookie {
	if len(values) == 0 {
		return nil
	}
	cookies := make([]secret.Cookie, 0, len(values))
	for _, value := range values {
		cookies = append(cookies, secret.Cookie{
			Name: value.Name, Value: value.Value, Path: value.Path,
			Domain: value.Domain, Expires: value.Expires, Secure: value.Secure, HTTPOnly: value.HTTPOnly,
		})
	}
	return cookies
}

func vaultCookiesToLegacy(values []secret.Cookie) []state.Cookie {
	if len(values) == 0 {
		return nil
	}
	cookies := make([]state.Cookie, 0, len(values))
	for _, value := range values {
		cookies = append(cookies, state.Cookie{
			Name: value.Name, Value: value.Value, Path: value.Path,
			Domain: value.Domain, Expires: value.Expires, Secure: value.Secure, HTTPOnly: value.HTTPOnly,
		})
	}
	return cookies
}

func testRunner(t *testing.T, store *state.Store, client *fakeClient, now time.Time) *Runner {
	t.Helper()
	cfg := config.Config{
		Accounts: map[string]config.Account{
			"account-a": {ID: "account-a", Label: "账号 A", Username: "a", Password: "password"},
			"account-b": {ID: "account-b", Label: "账号 B", Username: "b", Password: "password"},
			"account-c": {ID: "account-c", Label: "账号 C", Username: "c", Password: "password"},
			"account-d": {ID: "account-d", Label: "账号 D", Username: "d", Password: "password"},
		},
	}
	broker := auth.NewBroker(store, storeVault{store: store}, func([]state.Cookie) (auth.PlatformClient, error) {
		return client, nil
	}).WithClock(func() time.Time { return now })
	runner := NewRunnerWithFactory(cfg, store, broker, func([]state.Cookie) (WebsiteClient, error) { return client, nil })
	runner.now = func() time.Time { return now }
	runner.wait = func(context.Context, time.Duration) error { return nil }
	return runner
}
