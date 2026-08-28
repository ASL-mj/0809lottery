package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/service"
	"skyeapi/lottery-bot/internal/state"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(config.Config{
		StatePath: filepath.Join(t.TempDir(), "data", "state.json"),
		WebUser:   "admin",
		WebPass:   "secret",
		Accounts: map[string]config.Account{
			"account-a": {ID: "account-a", Label: "账号一", Username: "a@example.com", Password: "do-not-return"},
		},
	})
	server.vaultFactory = func(store *state.Store) (secret.Vault, error) {
		return testStoreVault{store: store}, nil
	}
	t.Cleanup(func() {
		_ = server.Close()
	})
	return server
}

// testStoreVault bridges the vault API onto the legacy persisted auth state
// so existing fixtures keep seeding tokens through store.PutAuth.
type testStoreVault struct {
	store *state.Store
}

func (v testStoreVault) Load(_ context.Context, accountID string) (secret.Bundle, error) {
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
		Cookies:                testCookiesToVault(authState.Cookies),
	}, nil
}

func (v testStoreVault) Save(_ context.Context, accountID string, bundle secret.Bundle) error {
	return v.store.PutAuth(accountID, state.AuthState{
		UserID:                 bundle.UserID,
		ParentAccessToken:      bundle.ParentAccessToken,
		ParentAccessExpiresAt:  bundle.ParentAccessExpiresAt,
		LotteryAccessToken:     bundle.LotteryAccessToken,
		LotteryAccessExpiresAt: bundle.LotteryAccessExpiresAt,
		Cookies:                testCookiesToState(bundle.Cookies),
	})
}

func (v testStoreVault) Delete(context.Context, string) error { return nil }

func testCookiesToVault(values []state.Cookie) []secret.Cookie {
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

func testCookiesToState(values []secret.Cookie) []state.Cookie {
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

func authenticatedRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.SetBasicAuth("admin", "secret")
	return request
}

func authenticatedJSONRequest(method, target, body string) *http.Request {
	request := authenticatedRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func testIntPointer(value int) *int {
	return &value
}

func TestHandlerRequiresBasicAuth(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != `Basic realm="0809 Account Workbench", charset="UTF-8"` {
		t.Fatalf("authentication response = %d %#v", recorder.Code, recorder.Header())
	}
}

func TestHealthIdentifiesAccountWorkbench(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"service":"account-workbench"`) {
		t.Fatalf("health response = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestOnlyAccountWorkbenchRoutesAreExposed(t *testing.T) {
	server := testServer(t)
	for _, target := range []string{"/api/overview", "/api/tasks", "/api/task-executions", "/api/data"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, recorder.Code)
		}
	}
}

func TestRuntimeLogsEndpointReturnsOnlySafeDisplayFields(t *testing.T) {
	server := testServer(t)
	store, err := server.sharedStore()
	if err != nil {
		t.Fatalf("sharedStore() error = %v", err)
	}
	secretPlan, err := store.EnsureAutoDrawPlans([]state.AutoDrawPlan{{
		Date:           "2026-08-08",
		AccountID:      "account-a",
		WindowID:       "morning",
		PlannedAt:      time.Date(2026, time.August, 8, 0, 5, 0, 0, time.UTC),
		IdempotencyKey: "draw:auto:private-idempotency-key",
	}})
	if err != nil || len(secretPlan) != 1 {
		t.Fatalf("EnsureAutoDrawPlans() = %#v, %v", secretPlan, err)
	}
	if _, err := store.AppendRuntimeLog(state.RuntimeLog{
		OccurredAt: time.Date(2026, time.August, 8, 0, 6, 0, 0, time.UTC),
		AccountID:  "account-a",
		WindowID:   "morning",
		Status:     state.AutoDrawPlanCompleted,
		Message:    "自动抽奖成功",
		PrizeLabel: "额度奖励",
		QuotaDeltaUSD: func() *float64 {
			value := 1.25
			return &value
		}(),
	}); err != nil {
		t.Fatalf("AppendRuntimeLog() error = %v", err)
	}
	if _, err := store.AppendRuntimeLog(state.RuntimeLog{
		OccurredAt: time.Date(2026, time.August, 8, 0, 7, 0, 0, time.UTC),
		AccountID:  "account-a",
		WindowID:   "afternoon",
		Status:     state.AutoDrawPlanSkipped,
		Message:    "无可用抽奖次数，已跳过",
	}); err != nil {
		t.Fatalf("AppendRuntimeLog() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/runtime-logs", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("runtime logs status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{`"logs"`, `"account_label":"账号一"`, `"window_id":"afternoon"`, `"status":"skipped"`, `"quota_delta_usd":1.25`} {
		if !strings.Contains(body, required) {
			t.Fatalf("runtime logs response missing %q: %s", required, body)
		}
	}
	if strings.Index(body, `"window_id":"afternoon"`) > strings.Index(body, `"window_id":"morning"`) {
		t.Fatalf("runtime logs are not newest first: %s", body)
	}
	for _, forbidden := range []string{"private-idempotency-key", "idempotency", "password", "cookie", "parent_access_token", "lottery_access_token"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("runtime logs response leaked %q: %s", forbidden, body)
		}
	}

	wrongMethod := httptest.NewRecorder()
	server.Handler().ServeHTTP(wrongMethod, authenticatedRequest(http.MethodPost, "/api/runtime-logs", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("runtime logs POST status = %d, want %d", wrongMethod.Code, http.StatusMethodNotAllowed)
	}
}

func TestPublicRuntimeLogTextRedactsSensitiveHistoricalValues(t *testing.T) {
	for _, value := range []string{
		"Bearer hidden-token",
		"cookie=session-secret",
		"password=hidden",
		"idempotency=private-key",
		"访问令牌：private",
		"密码：private",
	} {
		if got := publicRuntimeLogText(value); got != "已隐藏敏感详情" {
			t.Fatalf("publicRuntimeLogText(%q) = %q, want redacted", value, got)
		}
	}
	if got := publicRuntimeLogText("自动抽奖成功"); got != "自动抽奖成功" {
		t.Fatalf("publicRuntimeLogText() changed safe text: %q", got)
	}
}

func TestAutoDrawStatusEndpointReturnsThreeSafeWindowsPerAccount(t *testing.T) {
	server := testServer(t)
	store, err := server.sharedStore()
	if err != nil {
		t.Fatalf("sharedStore() error = %v", err)
	}
	now := time.Now().In(shanghaiLocation)
	today := now.Format("2006-01-02")
	plans, err := store.EnsureAutoDrawPlans([]state.AutoDrawPlan{
		{Date: today, AccountID: "account-a", WindowID: "morning", PlannedAt: time.Date(now.Year(), now.Month(), now.Day(), 8, 12, 0, 0, shanghaiLocation).UTC(), IdempotencyKey: "draw:auto:hidden-morning"},
		{Date: today, AccountID: "account-a", WindowID: "afternoon", PlannedAt: time.Date(now.Year(), now.Month(), now.Day(), 13, 24, 0, 0, shanghaiLocation).UTC(), IdempotencyKey: "draw:auto:hidden-afternoon"},
		{Date: today, AccountID: "account-a", WindowID: "evening", PlannedAt: time.Date(now.Year(), now.Month(), now.Day(), 18, 36, 0, 0, shanghaiLocation).UTC(), IdempotencyKey: "draw:auto:hidden-evening"},
	})
	if err != nil || len(plans) != 3 {
		t.Fatalf("EnsureAutoDrawPlans() = %#v, %v", plans, err)
	}
	if _, err := store.FinishAutoDrawPlan(plans[0].Key, state.AutoDrawPlanCompleted, "自动抽奖成功", "额度奖励", nil, plans[0].PlannedAt.Add(time.Minute)); err != nil {
		t.Fatalf("FinishAutoDrawPlan(morning) error = %v", err)
	}
	if _, err := store.FinishAutoDrawPlan(plans[2].Key, state.AutoDrawPlanFailed, "Bearer historical-secret", "", nil, plans[2].PlannedAt.Add(time.Minute)); err != nil {
		t.Fatalf("FinishAutoDrawPlan(evening) error = %v", err)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/auto-draw-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("auto draw status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{`"date":"` + today + `"`, `"account_id":"account-a"`, `"window_id":"morning"`, `"window_id":"afternoon"`, `"window_id":"evening"`, `"status":"completed"`, `"status":"pending"`, `"status":"failed"`, `"planned_at"`, `"executed_at"`, `"message":"已隐藏敏感详情"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("auto draw status missing %q: %s", required, body)
		}
	}
	for _, forbidden := range []string{"hidden-morning", "hidden-afternoon", "hidden-evening", "idempotency", "historical-secret", "Bearer"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("auto draw status leaked %q: %s", forbidden, body)
		}
	}

	wrongMethod := httptest.NewRecorder()
	server.Handler().ServeHTTP(wrongMethod, authenticatedRequest(http.MethodPost, "/api/auto-draw-status", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("auto draw status POST = %d, want %d", wrongMethod.Code, http.StatusMethodNotAllowed)
	}
}

func TestRunReleasesStateWhenListenerCannotStart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	server := testServer(t)
	server.cfg.WebAddr = listener.Addr().String()
	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run() error = nil with an occupied listener")
	}

	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("state lock remained held after listener failure: %v", err)
	}
	_ = store.Close()
}

func TestAccountsReturnsOnlyPublicMetadataAndSubscriptionSnapshot(t *testing.T) {
	server := testServer(t)
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	payload := json.RawMessage(`{"account_id":"account-a","subscriptions":[],"query_error":"","queried_at":"2026-08-07T06:00:00Z"}`)
	if err := store.PutSnapshot(state.Snapshot{AccountID: "account-a", Kind: "subscriptions", Data: payload, QueriedAt: time.Date(2026, 8, 7, 6, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("put snapshot = %v", err)
	}
	drawPayload := json.RawMessage(`{"account_id":"account-a","remaining":0,"locked_remaining":1,"daily_used":3,"free_limit":3,"queried_at":"2026-08-07T06:10:00Z"}`)
	if err := store.PutSnapshot(state.Snapshot{AccountID: "account-a", Kind: "draw-count", Data: drawPayload, QueriedAt: time.Date(2026, 8, 7, 6, 10, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("put draw snapshot = %v", err)
	}
	activityPayload := json.RawMessage(`{"account_id":"account-a","today_spend_usd":12.5,"spend_tier_reached":1,"spend_tier_total":3,"next_spend_threshold_usd":20,"next_spend_remaining_usd":7.5,"spend_bonus_draws":1,"lucky_points":10,"lucky_max_points":50,"draw_purchase_cost_usd":3,"purchased_today":1,"purchase_pending":0,"purchase_unknown":0,"pass_unlock_cost_usd":9,"pass_unlocked":false,"day_key":"2026-08-07","queried_at":"2026-08-07T06:20:00Z"}`)
	if err := store.PutSnapshot(state.Snapshot{AccountID: "account-a", Kind: "activity", Data: activityPayload, QueriedAt: time.Date(2026, 8, 7, 6, 20, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("put activity snapshot = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "subscription_snapshot") || !strings.Contains(recorder.Body.String(), "draw_count_snapshot") || !strings.Contains(recorder.Body.String(), "activity_snapshot") || !strings.Contains(recorder.Body.String(), `"claim_status":"pending"`) || strings.Contains(recorder.Body.String(), "do-not-return") || strings.Contains(recorder.Body.String(), "parent_access_token") {
		t.Fatalf("unsafe or incomplete accounts response: %s", recorder.Body.String())
	}
}

func TestAccountsActivitySnapshotRequiresMatchingAccountID(t *testing.T) {
	server := testServer(t)
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	mismatched := json.RawMessage(`{"account_id":"other-account","today_spend_usd":12.5,"queried_at":"2026-08-07T06:20:00Z"}`)
	if err := store.PutSnapshot(state.Snapshot{AccountID: "account-a", Kind: "activity", Data: mismatched, QueriedAt: time.Date(2026, 8, 7, 6, 20, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("put activity snapshot = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "activity_snapshot") {
		t.Fatalf("mismatched activity snapshot must be ignored: %s", recorder.Body.String())
	}
}

func TestAccountsReturnsTodayCheckinStatus(t *testing.T) {
	server := testServer(t)
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	action, _, err := store.GetOrCreateAction("account-a", today, state.ActionCheckin)
	if err != nil {
		t.Fatalf("create check-in action = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.Message = "签到成功"
		nativeReward := 1200.0
		awardUSD := 2.4
		value.CheckinQuotaAwarded = &nativeReward
		value.CheckinQuotaAwardedUSD = &awardUSD
	}); err != nil {
		t.Fatalf("complete check-in action = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"checkin_status":"completed"`) || !strings.Contains(recorder.Body.String(), `"checkin_quota_awarded":2.4`) || strings.Contains(recorder.Body.String(), `"checkin_quota_awarded":1200`) {
		t.Fatalf("accounts check-in status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAccountsUsesUpstreamCheckinAwardWhenAvailable(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/status" {
			_, _ = writer.Write([]byte(`{"success":true,"data":{"quota_per_unit":500}}`))
			return
		}
		if request.URL.Path != "/api/user/checkin" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer parent-token" {
			t.Fatalf("check-in status authorization = %q", request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":true,"records":[{"checkin_date":"` + today + `","quota_awarded":3456}]}}}`))
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"checkin_status":"completed"`) || !strings.Contains(recorder.Body.String(), `"checkin_quota_awarded":6.912`) {
		t.Fatalf("accounts check-in status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAccountsPersistsRealtimeCheckinCompletion(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	serveUpstreamCheckin := true
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/status":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"quota_per_unit":500}}`))
		case "/api/user/checkin":
			if !serveUpstreamCheckin {
				writer.WriteHeader(http.StatusBadGateway)
				_, _ = writer.Write([]byte(`{"success":false}`))
				return
			}
			_, _ = writer.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":true,"records":[{"checkin_date":"` + today + `","quota_awarded":3456}]}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{UserID: 1, ParentAccessToken: "parent-token", ParentAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"checkin_status":"completed"`) || !strings.Contains(first.Body.String(), `"checkin_quota_awarded":6.912`) {
		t.Fatalf("first accounts response = %d: %s", first.Code, first.Body.String())
	}
	sharedStore, err := server.sharedStore()
	if err != nil {
		t.Fatalf("sharedStore() error = %v", err)
	}
	action, ok := sharedStore.Action("account-a", today, state.ActionCheckin)
	if !ok || action.Status != state.ActionCompleted || action.CheckinQuotaAwardedUSD == nil || *action.CheckinQuotaAwardedUSD != 6.912 || !strings.Contains(action.Message, "$6.91") || strings.Contains(action.Message, "3456") {
		t.Fatalf("reconciled action = %#v, %v", action, ok)
	}

	serveUpstreamCheckin = false
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"checkin_status":"completed"`) || !strings.Contains(second.Body.String(), `"checkin_quota_awarded":6.912`) {
		t.Fatalf("second accounts response = %d: %s", second.Code, second.Body.String())
	}
}

func TestAccountActionsValidatePathMethodAndAccount(t *testing.T) {
	server := testServer(t)
	for _, tc := range []struct {
		method string
		target string
		want   int
	}{
		{method: http.MethodGet, target: "/api/accounts/account-a/checkin", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/api/accounts/account-a/claim", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/api/accounts/account-a/draw", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/api/accounts/account-a/activity", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/api/accounts/account-a/purchase-draw", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/api/accounts/account-a/unlock-pass", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, target: "/api/accounts/unknown/checkin", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/unknown/claim", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/unknown/draw", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/unknown/activity", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/unknown/purchase-draw", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/unknown/unlock-pass", want: http.StatusNotFound},
		{method: http.MethodPost, target: "/api/accounts/account-a/unknown", want: http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(tc.method, tc.target, nil))
		if recorder.Code != tc.want {
			t.Fatalf("%s %s status = %d, want %d: %s", tc.method, tc.target, recorder.Code, tc.want, recorder.Body.String())
		}
	}
}

func TestActivityActionReturnsSanitizedReportAndStoresSnapshot(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	var dashboardCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			dashboardCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("dashboard authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":4,"todaySpend":12.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"points":18,"maxPoints":80},"purchase":{"price":3,"pendingCount":1,"unknownCount":0,"purchasedToday":1},"drawLimit":{"remaining":4,"unlockCost":9,"unlocked":false,"dayKey":"` + today + `"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/activity", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("activity status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"account_id":"account-a"`,
		`"today_spend_usd":12.5`,
		`"spend_tier_reached":1`,
		`"spend_tier_total":2`,
		`"next_spend_threshold_usd":20`,
		`"next_spend_remaining_usd":7.5`,
		`"spend_bonus_draws":2`,
		`"lucky_points":18`,
		`"lucky_max_points":80`,
		`"draw_purchase_cost_usd":3`,
		`"pass_unlock_cost_usd":9`,
		`"pass_unlocked":false`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("activity response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"lottery-token", "cookie", "idempotency", `"purchase":{"`, `"rules":{"`, `"drawLimit":{"`, `"lucky":{"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("activity response exposes %q: %s", forbidden, body)
		}
	}
	sharedStore, err := server.sharedStore()
	if err != nil {
		t.Fatalf("sharedStore() error = %v", err)
	}
	snapshot, ok := sharedStore.Snapshot("account-a", "activity")
	if !ok {
		t.Fatal("activity snapshot missing")
	}
	if !bytes.Contains(snapshot.Data, []byte(`"account_id":"account-a"`)) {
		t.Fatalf("activity snapshot = %s", snapshot.Data)
	}
	if dashboardCalls.Load() != 1 {
		t.Fatalf("dashboard calls = %d, want 1", dashboardCalls.Load())
	}
}

func TestPurchaseDrawActionReturnsSanitizedOutcome(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	var dashboardCalls atomic.Int32
	var purchaseCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			call := dashboardCalls.Add(1)
			if call == 1 {
				_, _ = writer.Write([]byte(`{"eligibility":{"remaining":4,"todaySpend":12.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"points":18,"maxPoints":80},"purchase":{"price":3,"pendingCount":0,"unknownCount":0,"purchasedToday":1},"drawLimit":{"remaining":4,"purchasedRemaining":0,"unlockCost":9,"unlocked":false,"dayKey":"` + today + `"}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":5,"todaySpend":15.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"points":18,"maxPoints":80},"purchase":{"price":3,"pendingCount":0,"unknownCount":0,"purchasedToday":2},"drawLimit":{"remaining":5,"purchasedRemaining":1,"unlockCost":9,"unlocked":false,"dayKey":"` + today + `"}}`))
		case "/lottery/api/draw-purchases":
			purchaseCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("purchase authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("Idempotency-Key") == "" {
				t.Fatal("purchase request missing idempotency key")
			}
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/purchase-draw", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("purchase status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"account_id":"account-a"`,
		`"status":"completed"`,
		`"message":"购买抽奖次数成功"`,
		`"price_usd":3`,
		`"remaining":5`,
		`"activity":{"account_id":"account-a"`,
		`"purchased_today":2`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("purchase response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"lottery-token", "cookie", "idempotency", `"action"`, `"key"`, `"result"`, `"purchase":{"`, `"drawLimit":{"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("purchase response exposes %q: %s", forbidden, body)
		}
	}
	if dashboardCalls.Load() != 2 || purchaseCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls: dashboard=%d purchase=%d", dashboardCalls.Load(), purchaseCalls.Load())
	}
}

func TestUnlockPassActionReturnsSanitizedOutcome(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	var dashboardCalls atomic.Int32
	var unlockCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			call := dashboardCalls.Add(1)
			if call == 1 {
				_, _ = writer.Write([]byte(`{"eligibility":{"remaining":4,"todaySpend":12.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"points":18,"maxPoints":80},"purchase":{"price":3,"pendingCount":0,"unknownCount":0,"purchasedToday":1},"drawLimit":{"remaining":4,"unlockCost":9,"unlocked":false,"dayKey":"` + today + `"}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":5,"todaySpend":12.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"points":18,"maxPoints":80},"purchase":{"price":3,"pendingCount":0,"unknownCount":0,"purchasedToday":1},"drawLimit":{"remaining":5,"unlockCost":9,"unlocked":true,"dayKey":"` + today + `"}}`))
		case "/lottery/api/draw-limit/unlock":
			unlockCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("unlock authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("Idempotency-Key") == "" {
				t.Fatal("unlock request missing idempotency key")
			}
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/unlock-pass", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unlock status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"account_id":"account-a"`,
		`"status":"completed"`,
		`"message":"今日通行证解锁成功"`,
		`"price_usd":9`,
		`"remaining":5`,
		`"activity":{"account_id":"account-a"`,
		`"pass_unlocked":true`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("unlock response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"lottery-token", "cookie", "idempotency", `"action"`, `"key"`, `"result"`, `"purchase":{"`, `"drawLimit":{"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unlock response exposes %q: %s", forbidden, body)
		}
	}
	if dashboardCalls.Load() != 2 || unlockCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls: dashboard=%d unlock=%d", dashboardCalls.Load(), unlockCalls.Load())
	}
}

func TestClaimActionReturnsSanitizedOutcome(t *testing.T) {
	var dashboardCalls atomic.Int32
	var claimCalls atomic.Int32
	var drawCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			dashboardCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("dashboard authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":2},"drawLimit":{"remaining":2,"lockedRemaining":0,"dailyUsed":1,"freeLimit":3,"status":"available","dayKey":"2026-08-07"}}`))
		case "/lottery/api/check-ins/claim":
			claimCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("claim authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":4},"drawLimit":{"remaining":4,"lockedRemaining":0,"dailyUsed":1,"freeLimit":3,"status":"available","dayKey":"2026-08-07"}}`))
		case "/lottery/api/draw":
			drawCalls.Add(1)
			t.Fatalf("claim endpoint must not trigger draw")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/claim", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"account_id":"account-a"`,
		`"claim_status":"completed"`,
		`"claim_message":"领取成功"`,
		`"already_completed":false`,
		`"added":2`,
		`"remaining":4`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("claim response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"do-not-return", "lottery-token", "cookie", "idempotency", `"key"`, "claim_before_remaining", "claim_after_remaining", `"action"`, "quota_delta"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("claim response exposes %q: %s", forbidden, body)
		}
	}
	if dashboardCalls.Load() != 1 || claimCalls.Load() != 1 || drawCalls.Load() != 0 {
		t.Fatalf("unexpected upstream calls: dashboard=%d claim=%d draw=%d", dashboardCalls.Load(), claimCalls.Load(), drawCalls.Load())
	}
}

func TestClaimActionCompletedRecordDoesNotCallUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		http.NotFound(writer, request)
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	action, _, err := store.GetOrCreateAction("account-a", today, state.ActionDailyClaim)
	if err != nil {
		t.Fatalf("create claim action = %v", err)
	}
	if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.Message = "今日已领取"
		value.ClaimBeforeRemaining = testIntPointer(2)
		value.ClaimAfterRemaining = testIntPointer(3)
	}); err != nil {
		t.Fatalf("complete claim action = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/claim", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"already_completed":true`) || !strings.Contains(recorder.Body.String(), `"remaining":3`) || !strings.Contains(recorder.Body.String(), `"claim_message":"今日已领取"`) {
		t.Fatalf("claim response = %s", recorder.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("completed claim should not hit upstream, got %d calls", upstreamCalls.Load())
	}
}

func TestClaimActionFailureAndUnknownUsePublicMessages(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/lottery/api/dashboard":
				_, _ = writer.Write([]byte(`{"eligibility":{"remaining":2},"drawLimit":{"remaining":2,"lockedRemaining":0,"dailyUsed":1,"freeLimit":3,"status":"available","dayKey":"2026-08-08"}}`))
			case "/lottery/api/check-ins/claim":
				_, _ = writer.Write([]byte(`{"success":false,"message":"technical-secret bearer-token"}`))
			default:
				http.NotFound(writer, request)
			}
		}))
		defer upstream.Close()

		server := testServer(t)
		server.cfg.BaseURL = upstream.URL
		store, err := state.Open(server.cfg.StatePath)
		if err != nil {
			t.Fatalf("open state = %v", err)
		}
		if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatalf("put auth = %v", err)
		}
		_ = store.Close()

		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/claim", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `"claim_status":"failed"`) || !strings.Contains(body, `"claim_message":"领取失败，请重试"`) {
			t.Fatalf("failure response = %s", body)
		}
		if strings.Contains(body, "technical-secret bearer-token") {
			t.Fatalf("failure response leaked upstream secret: %s", body)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/lottery/api/dashboard":
				_, _ = writer.Write([]byte(`{"eligibility":{"remaining":2},"drawLimit":{"remaining":2,"lockedRemaining":0,"dailyUsed":1,"freeLimit":3,"status":"available","dayKey":"2026-08-08"}}`))
			default:
				http.NotFound(writer, request)
			}
		}))
		defer upstream.Close()

		server := testServer(t)
		server.cfg.BaseURL = upstream.URL
		store, err := state.Open(server.cfg.StatePath)
		if err != nil {
			t.Fatalf("open state = %v", err)
		}
		if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatalf("put auth = %v", err)
		}
		today := time.Now().In(shanghaiLocation).Format("2006-01-02")
		action, _, err := store.GetOrCreateAction("account-a", today, state.ActionDailyClaim)
		if err != nil {
			t.Fatalf("create claim action = %v", err)
		}
		if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
			value.Status = state.ActionUnknown
			value.SideEffectStarted = true
			value.Message = "technical-secret bearer-token"
			value.LastError = value.Message
			value.ClaimBeforeRemaining = testIntPointer(2)
		}); err != nil {
			t.Fatalf("update claim action = %v", err)
		}
		_ = store.Close()

		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/claim", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `"claim_status":"unknown"`) || !strings.Contains(body, `"claim_message":"领取结果待确认"`) {
			t.Fatalf("unknown response = %s", body)
		}
		if strings.Contains(body, "technical-secret bearer-token") {
			t.Fatalf("unknown response leaked action secret: %s", body)
		}
	})
}

func TestDrawActionReturnsSanitizedOutcomeAndServerGeneratedKey(t *testing.T) {
	var dashboardCalls atomic.Int32
	var drawCalls atomic.Int32
	var statusCalls atomic.Int32
	var receivedKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			dashboardCalls.Add(1)
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":1},"drawLimit":{"remaining":1,"lockedRemaining":0,"dailyUsed":2,"freeLimit":3,"status":"available","dayKey":"2026-08-07"}}`))
		case "/lottery/api/draw":
			drawCalls.Add(1)
			receivedKey = request.Header.Get("Idempotency-Key")
			_, _ = writer.Write([]byte(`{"id":"draw-1","prizeId":"prize-1","prize":{"id":"prize-1","label":"额度奖励","shortLabel":"额度奖励"},"status":"done","effect":{"summary":"增加额度","quotaDelta":250}}`))
		case "/api/status":
			statusCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer parent-token" {
				t.Fatalf("status authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"success":true,"data":{"quota_per_unit":500}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  time.Now().Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/draw", `{"idempotency_key":"client-supplied"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("draw status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"account_id":"account-a"`,
		`"skipped":false`,
		`"remaining_before":1`,
		`"message":"手动抽奖成功"`,
		`"prize_label":"额度奖励"`,
		`"quota_delta_usd":0.5`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("draw response missing %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"lottery-token", "parent-token", "client-supplied", "draw:web:", "draw-1", "quotaDelta", `"Result"`, "250", "idempotency", `"key"`, "cookie"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("draw response exposes %q: %s", forbidden, body)
		}
	}
	if receivedKey == "" || !strings.HasPrefix(receivedKey, "draw:web:") || receivedKey == "client-supplied" {
		t.Fatalf("web did not generate a new draw key: %q", receivedKey)
	}
	if dashboardCalls.Load() != 1 || drawCalls.Load() != 1 || statusCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls: dashboard=%d draw=%d status=%d", dashboardCalls.Load(), drawCalls.Load(), statusCalls.Load())
	}
}

func TestDrawActionSkipsWithoutQuota(t *testing.T) {
	var dashboardCalls atomic.Int32
	var drawCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			dashboardCalls.Add(1)
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":0},"drawLimit":{"remaining":0,"lockedRemaining":1,"dailyUsed":3,"freeLimit":3,"status":"locked","dayKey":"2026-08-07"}}`))
		case "/lottery/api/draw":
			drawCalls.Add(1)
			t.Fatalf("skip path must not call draw")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/draw", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("draw status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"skipped":true`) || !strings.Contains(recorder.Body.String(), `"remaining_before":0`) {
		t.Fatalf("draw skip response = %s", recorder.Body.String())
	}
	if dashboardCalls.Load() != 1 || drawCalls.Load() != 0 {
		t.Fatalf("unexpected upstream calls: dashboard=%d draw=%d", dashboardCalls.Load(), drawCalls.Load())
	}
}

func TestAccountsReturnsClaimSummaryWithoutInternalFields(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          state.ActionStatus
		expectedMessage string
		configure       func(*state.Action)
		expectsAdded    bool
	}{
		{
			name:            "completed",
			status:          state.ActionCompleted,
			expectedMessage: "今日已领取",
			configure: func(value *state.Action) {
				value.ClaimBeforeRemaining = testIntPointer(3)
				value.ClaimAfterRemaining = testIntPointer(5)
			},
			expectsAdded: true,
		},
		{
			name:            "failed",
			status:          state.ActionFailed,
			expectedMessage: "领取失败，请重试",
		},
		{
			name:            "unknown",
			status:          state.ActionUnknown,
			expectedMessage: "领取结果待确认",
			configure: func(value *state.Action) {
				value.ClaimBeforeRemaining = testIntPointer(2)
			},
		},
		{
			name:            "pending",
			status:          state.ActionPending,
			expectedMessage: "领取处理中",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			store, err := state.Open(server.cfg.StatePath)
			if err != nil {
				t.Fatalf("open state = %v", err)
			}
			today := time.Now().In(shanghaiLocation).Format("2006-01-02")
			action, _, err := store.GetOrCreateAction("account-a", today, state.ActionDailyClaim)
			if err != nil {
				t.Fatalf("create claim action = %v", err)
			}
			if _, err := store.UpdateAction(action.Key, func(value *state.Action) {
				value.Status = tc.status
				value.Message = "technical-secret bearer-token"
				value.LastError = value.Message
				value.IdempotencyKey = "draw:web:internal"
				if tc.configure != nil {
					tc.configure(value)
				}
			}); err != nil {
				t.Fatalf("complete claim action = %v", err)
			}
			_ = store.Close()

			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("accounts status = %d: %s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			for _, expected := range []string{
				`"claim_status":"` + string(tc.status) + `"`,
				`"claim_message":"` + tc.expectedMessage + `"`,
			} {
				if !strings.Contains(body, expected) {
					t.Fatalf("accounts response missing %q: %s", expected, body)
				}
			}
			if tc.expectsAdded {
				for _, expected := range []string{`"claim_added":2`, `"claim_remaining":5`} {
					if !strings.Contains(body, expected) {
						t.Fatalf("accounts response missing %q: %s", expected, body)
					}
				}
			}
			for _, forbidden := range []string{"technical-secret bearer-token", "claim_before_remaining", "claim_after_remaining", "draw:web:internal", `"idempotency_key"`, `"last_error"`, `"retryable"`, `"side_effect_started"`} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("accounts response exposes %q: %s", forbidden, body)
				}
			}
		})
	}
}

func TestSubscriptionQueryRequiresOneNamedAccount(t *testing.T) {
	server := testServer(t)
	for _, body := range []string{`{}`, `{"account_id":"all"}`, `{"account_id":"unknown"}`} {
		recorder := httptest.NewRecorder()
		request := authenticatedRequest(http.MethodPost, "/api/subscriptions/query", bytes.NewReader([]byte(body)))
		request.Header.Set("Content-Type", "application/json")
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
			t.Fatalf("body %s status = %d: %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDrawCountQueryReturnsSingleAccountPublicFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/lottery/api/dashboard" || request.Header.Get("Authorization") != "Bearer lottery-token" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"eligibility":{"remaining":0,"dailyUsed":3},"drawLimit":{"freeLimit":3,"unlocked":false,"status":"locked","earnedRemaining":1,"purchasedRemaining":0,"remaining":0,"lockedRemaining":1,"dayKey":"2026-08-07"}}`))
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{LotteryAccessToken: "lottery-token", LotteryAccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "/api/draw-count/query", bytes.NewBufferString(`{"account_id":"account-a"}`))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"remaining":0`) || !strings.Contains(recorder.Body.String(), `"locked_remaining":1`) || !strings.Contains(recorder.Body.String(), `"daily_used":3`) || !strings.Contains(recorder.Body.String(), `"free_limit":3`) {
		t.Fatalf("draw count response = %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, forbidden := range []string{"lottery-token", "do-not-return", "parent_access_token", "active_account_count"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("draw count response exposes %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestDrawCountQueryRequiresOneNamedAccount(t *testing.T) {
	server := testServer(t)
	for _, body := range []string{`{}`, `{"account_id":"all"}`, `{"account_id":"unknown"}`} {
		recorder := httptest.NewRecorder()
		request := authenticatedRequest(http.MethodPost, "/api/draw-count/query", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
			t.Fatalf("body %s status = %d: %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestSingleSubscriptionViewDoesNotExposeCrossAccountSummary(t *testing.T) {
	response := singleSubscriptionView("account-a", service.SubscriptionReport{
		ActiveAccountCount:         5,
		ActiveSubscriptionCount:    8,
		UnlimitedSubscriptionCount: 2,
		Accounts: []service.AccountSubscriptionReport{{
			Account:       "a@example.com",
			Subscriptions: []service.SubscriptionItem{{ID: 1, PlanTitle: "订阅"}},
		}},
	})
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal subscription response = %v", err)
	}
	for _, forbidden := range []string{"active_account_count", "active_subscription_count", "unlimited_subscription_count", "finite_total_usd"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("single-account response exposes summary %q: %s", forbidden, encoded)
		}
	}
}

func TestConcurrentRequestsReuseSharedStoreWithout503(t *testing.T) {
	checkinStarted := make(chan struct{})
	releaseCheckin := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/user/checkin":
			close(checkinStarted)
			<-releaseCheckin
			_, _ = writer.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false,"records":[]}}}`))
		case "/api/status":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"quota_per_unit":500}}`))
		case "/lottery/api/dashboard":
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":0,"dailyUsed":3},"drawLimit":{"freeLimit":3,"unlocked":false,"status":"locked","earnedRemaining":1,"purchasedRemaining":0,"remaining":0,"lockedRemaining":1,"dayKey":"2026-08-07"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-token",
		ParentAccessExpiresAt:  time.Now().Add(time.Hour),
		LotteryAccessToken:     "lottery-token",
		LotteryAccessExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("put auth = %v", err)
	}
	_ = store.Close()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
		firstDone <- recorder
	}()

	<-checkinStarted

	second := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "/api/draw-count/query", bytes.NewBufferString(`{"account_id":"account-a"}`))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(second, request)
	if second.Code != http.StatusOK {
		t.Fatalf("concurrent draw count response = %d: %s", second.Code, second.Body.String())
	}

	close(releaseCheckin)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("accounts response = %d: %s", first.Code, first.Body.String())
	}
}

func TestServerCloseReleasesSharedStoreLock(t *testing.T) {
	server := testServer(t)
	if _, err := server.sharedStore(); err != nil {
		t.Fatalf("sharedStore() error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("state.Open() after Close error = %v", err)
	}
	_ = store.Close()
}

func TestIndexContainsOnlyAccountControls(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "刷新订阅") || !strings.Contains(recorder.Body.String(), "刷新次数") || !strings.Contains(recorder.Body.String(), "领取次数") || !strings.Contains(recorder.Body.String(), "手动抽奖") {
		t.Fatalf("index response = %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, expected := range []string{"今日签到奖励", "今日已签到 · 奖励待核对", "renderCheckinReward(account)", ".account__checkin-reward"} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Fatalf("index missing check-in reward display %q", expected)
		}
	}
	for _, expected := range []string{"签到成功：", "签到失败：", "已签到", "领取成功：", "领取失败：", "抽奖成功：", "抽奖失败：", "跳过抽奖：", "今日已领取", "领取结果待确认", "核对结果", "data-checkin", "data-claim", "data-draw", "data-draw-count", "data-refresh", "data-activity", "data-purchase-draw", "data-unlock-pass", "领取中", "抽奖中", "刷新活动", "购买 1 抽 · $", "购买通行证 · $", "今日通行证已购买", "距下一档还需", "今日累计加抽", "全部档位已完成", "window.confirm", "仅北京时间当日有效", "await finishActionAndRefresh(account, 'claiming');", "await finishActionAndRefresh(account, 'drawing');", "await refreshDrawCountOnly(account);", "account.activity_result", "account.activity_error", "pass_unlocked", "purchase_pending", "purchase_unknown", "currentFocusTarget()", "restoreFocus(target)", "390px", ":focus-visible", ".account__identity", ".action--primary", ".metrics", ".metric__value", "<section id=\"accounts\" aria-label=\"账号详情\">", "role=\"status\"", "data-status-account", "额度统一以美元显示", "@media (max-width:1040px)", "@media (max-width:620px)", "系统运行日志", "自动抽奖执行结果，最新优先展示。", "id=\"runtime-logs-refresh\"", "/api/runtime-logs", "运行日志拉取失败：", "最近一次刷新失败：", "暂无运行日志。", "正在读取运行日志...", "morning: '早间 08:00–09:00'", "midday: '午间 13:00–14:00'", "evening: '晚间 18:00–19:00'", "log.account_label || log.account_id", "奖品：", "额度：$", "账号：", "窗口：", "renderRuntimeLogs()", "runtime-log__status", "runtime-state--error", "今日定时抽奖", "已执行 ${handled}/3", ".auto-draw__timeline", ".auto-draw__step--completed", "renderAutoDrawSchedule(account)", "/api/auto-draw-status", "refreshAutoDrawStatus()", "30000", "计划待生成", "执行中", "已跳过"} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Fatalf("index missing check-in feedback %q", expected)
		}
	}
	for _, forbidden := range []string{"刷新全部订阅", "全部签到", "任务管理", "主动抽奖", "登录并保存", "解锁抽奖", "购买抽数", "执行抽奖", "统计控制", "查询抽奖次数", "账号密码", "Cookie", "Access Token", "保存凭证"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("index contains removed control %q", forbidden)
		}
	}
}
