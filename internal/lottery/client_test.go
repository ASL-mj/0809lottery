package lottery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/state"
)

func TestLoginBridgeAndDraw(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/user/login":
			if request.Method != http.MethodPost {
				t.Fatalf("login method = %s", request.Method)
			}
			http.SetCookie(writer, &http.Cookie{Name: "new_api_refresh", Value: "refresh-cookie", Path: "/"})
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"access_token":      "parent-token",
					"access_expires_at": time.Now().Add(15 * time.Minute).Unix(),
					"user":              map[string]any{"id": 42},
				},
			})
		case "/lottery/api/bridge/session":
			if request.Header.Get("Authorization") != "Bearer parent-token" {
				t.Fatalf("bridge authorization = %q", request.Header.Get("Authorization"))
			}
			var input map[string]string
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input["uid"] != "42" {
				t.Fatalf("bridge body = %#v, %v", input, err)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"accessToken":    "lottery-token",
				"tokenExpiresAt": time.Now().Add(time.Hour).UnixMilli(),
			})
		case "/lottery/api/draw":
			if request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("draw authorization = %q", request.Header.Get("Authorization"))
			}
			if !strings.HasPrefix(request.Header.Get("Idempotency-Key"), "draw:") {
				t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id":                 "draw-1",
				"prizeId":            "paid-c-three-hours",
				"prize":              map[string]any{"label": "高额额度丙 · 3 小时 20 额度", "shortLabel": "高额额度丙"},
				"status":             "fulfilled",
				"fulfillmentStatus":  "active",
				"fulfillmentMessage": "限时额度与订阅已生效",
				"effect":             map[string]any{"summary": "+20 限时额度", "quotaDelta": 20},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	login, err := client.Login(context.Background(), Credentials{Username: "user", Password: "password"})
	if err != nil || login.UserID != 42 || login.AccessToken != "parent-token" {
		t.Fatalf("Login() = %#v, %v", login, err)
	}
	if len(client.Cookies()) != 1 || client.Cookies()[0].Name != "new_api_refresh" {
		t.Fatalf("Cookies() = %#v", client.Cookies())
	}
	bridge, err := client.Bridge(context.Background(), login.AccessToken, login.UserID)
	if err != nil || bridge.AccessToken != "lottery-token" {
		t.Fatalf("Bridge() = %#v, %v", bridge, err)
	}
	draw, err := client.Draw(context.Background(), bridge.AccessToken, "draw:test")
	if err != nil || draw.Prize.ShortLabel != "高额额度丙" || draw.Effect.QuotaDelta != 20 {
		t.Fatalf("Draw() = %#v, %v", draw, err)
	}
}

func TestRefreshSendsAndPersistsScopedCookie(t *testing.T) {
	var receivedRefreshCookie bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/user/auth/refresh" || request.Method != http.MethodPost {
			http.NotFound(writer, request)
			return
		}
		cookie, err := request.Cookie("new_api_refresh")
		if err == nil && cookie.Value == "seed-refresh" {
			receivedRefreshCookie = true
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     "new_api_refresh",
			Value:    "renewed-refresh",
			Path:     "/api/user/auth",
			HttpOnly: true,
		})
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"access_token":      "refreshed-parent-token",
				"access_expires_at": time.Now().Add(time.Hour).Unix(),
				"user":              map[string]any{"id": 42},
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", []state.Cookie{{
		Name:     "new_api_refresh",
		Value:    "seed-refresh",
		Path:     "/api/user/auth",
		HTTPOnly: true,
	}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	result, err := client.Refresh(context.Background())
	if err != nil || result.UserID != 42 || result.AccessToken != "refreshed-parent-token" {
		t.Fatalf("Refresh() = %#v, %v", result, err)
	}
	if !receivedRefreshCookie {
		t.Fatal("Refresh() did not send the scoped refresh cookie")
	}
	cookies := client.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "new_api_refresh" || cookies[0].Value != "renewed-refresh" || cookies[0].Path != "/api/user/auth" || !cookies[0].HTTPOnly {
		t.Fatalf("Cookies() = %#v, want one refreshed scoped cookie", cookies)
	}
}

func TestDrawReturnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"message":"token expired"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Draw(context.Background(), "stale-token", "draw:test")
	if !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("Draw() error = %v, want 401 APIError", err)
	}
}

func TestDashboardClaimAndCheckin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/dashboard":
			if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("dashboard request = method=%s authorization=%q", request.Method, request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"eligibility":{"remaining":2,"spendBonusDraws":1}}`))
		case "/lottery/api/check-ins/claim":
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer lottery-token" {
				t.Fatalf("claim request = method=%s authorization=%q", request.Method, request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"success":true,"message":"领取成功"}`))
		case "/api/user/checkin":
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer parent-token" {
				t.Fatalf("check-in request = method=%s authorization=%q", request.Method, request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":1000,"checkin_date":"2026-08-05"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	dashboard, err := client.Dashboard(context.Background(), "lottery-token")
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	if remaining, ok := dashboard.Remaining(); !ok || remaining != 2 {
		t.Fatalf("Dashboard().Remaining() = %d, %v", remaining, ok)
	}
	if bonus, ok := dashboard.SpendBonusDraws(); !ok || bonus != 1 {
		t.Fatalf("Dashboard().SpendBonusDraws() = %d, %v", bonus, ok)
	}
	claim, err := client.ClaimDaily(context.Background(), "lottery-token")
	if err != nil || !claim.Success || claim.Message != "领取成功" {
		t.Fatalf("ClaimDaily() = %#v, %v", claim, err)
	}
	checkin, err := client.Checkin(context.Background(), "parent-token")
	if err != nil || !checkin.Success || checkin.QuotaAwarded != 1000 || checkin.CheckinDate != "2026-08-05" {
		t.Fatalf("Checkin() = %#v, %v", checkin, err)
	}
}

func TestDashboardDecodesNewDrawLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/lottery/api/dashboard" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"eligibility":{"remaining":0,"dailyUsed":3,"drawLimit":{"earnedRemaining":1}},"drawLimit":{"freeLimit":3,"unlocked":false,"status":"locked","remaining":0,"lockedRemaining":1,"freeUsed":3,"dayKey":"2026-08-07"}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	dashboard, err := client.Dashboard(context.Background(), "lottery-token")
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	limit, ok := dashboard.EffectiveDrawLimit()
	if !ok || limit.Remaining == nil || *limit.Remaining != 0 || limit.LockedRemaining == nil || *limit.LockedRemaining != 1 || limit.EarnedRemaining == nil || *limit.EarnedRemaining != 1 || limit.DailyUsed == nil || *limit.DailyUsed != 3 || limit.FreeLimit == nil || *limit.FreeLimit != 3 || limit.Unlocked == nil || *limit.Unlocked {
		t.Fatalf("EffectiveDrawLimit() = %#v, %v", limit, ok)
	}
}

func TestDashboardUsesNestedDrawLimitWhenRootIsAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"eligibility":{"drawLimit":{"freeLimit":3,"unlocked":true,"status":"unlocked","remaining":2,"lockedRemaining":0,"earnedRemaining":1,"purchasedRemaining":1,"dailyUsed":4,"dayKey":"2026-08-07"}}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	dashboard, err := client.Dashboard(context.Background(), "lottery-token")
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	limit, ok := dashboard.EffectiveDrawLimit()
	if !ok || limit.Remaining == nil || *limit.Remaining != 2 || limit.DailyUsed == nil || *limit.DailyUsed != 4 || limit.Unlocked == nil || !*limit.Unlocked || limit.Status != "unlocked" {
		t.Fatalf("EffectiveDrawLimit() = %#v, %v", limit, ok)
	}
}

func TestDashboardDecodesExtendedFieldsFromDirectAndWrappedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "direct",
			body: `{"eligibility":{"remaining":4,"todaySpend":12.5},"spendBonusDraws":2,"rules":{"spendTiers":[{"amount":10,"draws":1},{"amount":20,"draws":2}]},"lucky":{"enabled":true,"remaining":1},"purchase":{"enabled":true,"price":3},"drawLimit":{"remaining":4,"unlockCost":9}}`,
		},
		{
			name: "wrapped",
			body: `{"success":true,"data":{"eligibility":{"remaining":5,"spendBonusDraws":1,"todaySpend":8},"rules":{"spendTiers":[{"amount":30,"draws":3}]},"lucky":{"enabled":false},"purchase":{"enabled":false},"drawLimit":{"remaining":5,"unlockCost":12}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/lottery/api/dashboard" || request.Method != http.MethodGet {
					http.NotFound(writer, request)
					return
				}
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "test-agent", nil)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			dashboard, err := client.Dashboard(context.Background(), "lottery-token")
			if err != nil {
				t.Fatalf("Dashboard() error = %v", err)
			}

			if dashboard.Eligibility.TodaySpend == nil {
				t.Fatalf("Dashboard().Eligibility.TodaySpend = nil")
			}
			if test.name == "direct" && *dashboard.Eligibility.TodaySpend != 12.5 {
				t.Fatalf("Dashboard().Eligibility.TodaySpend = %v, want 12.5", *dashboard.Eligibility.TodaySpend)
			}
			if test.name == "wrapped" && *dashboard.Eligibility.TodaySpend != 8 {
				t.Fatalf("Dashboard().Eligibility.TodaySpend = %v, want 8", *dashboard.Eligibility.TodaySpend)
			}

			wantBonus := 2
			if test.name == "wrapped" {
				wantBonus = 1
			}
			if bonus, ok := dashboard.SpendBonusDraws(); !ok || bonus != wantBonus {
				t.Fatalf("Dashboard().SpendBonusDraws() = %d, %v, want %d", bonus, ok, wantBonus)
			}

			if len(dashboard.Rules.SpendTiers) == 0 {
				t.Fatalf("Dashboard().Rules.SpendTiers = %#v, want non-empty", dashboard.Rules.SpendTiers)
			}
			firstTier := dashboard.Rules.SpendTiers[0]
			if amount, ok := firstTier["amount"].(float64); !ok || amount <= 0 {
				t.Fatalf("Dashboard().Rules.SpendTiers[0][amount] = %#v", firstTier["amount"])
			}
			if draws, ok := firstTier["draws"].(float64); !ok || draws <= 0 {
				t.Fatalf("Dashboard().Rules.SpendTiers[0][draws] = %#v", firstTier["draws"])
			}

			if enabled, ok := dashboard.Lucky["enabled"].(bool); !ok {
				t.Fatalf("Dashboard().Lucky[enabled] = %#v", dashboard.Lucky["enabled"])
			} else if test.name == "direct" && !enabled {
				t.Fatalf("Dashboard().Lucky[enabled] = %v, want true", enabled)
			}
			if enabled, ok := dashboard.Purchase["enabled"].(bool); !ok {
				t.Fatalf("Dashboard().Purchase[enabled] = %#v", dashboard.Purchase["enabled"])
			} else if test.name == "wrapped" && enabled {
				t.Fatalf("Dashboard().Purchase[enabled] = %v, want false", enabled)
			}

			if dashboard.DrawLimit.UnlockCost == nil {
				t.Fatalf("Dashboard().DrawLimit.UnlockCost = nil")
			}
			wantUnlockCost := 9
			if test.name == "wrapped" {
				wantUnlockCost = 12
			}
			if *dashboard.DrawLimit.UnlockCost != wantUnlockCost {
				t.Fatalf("Dashboard().DrawLimit.UnlockCost = %d, want %d", *dashboard.DrawLimit.UnlockCost, wantUnlockCost)
			}
		})
	}
}

func TestCheckinStatusReturnsTodayAward(t *testing.T) {
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/user/checkin" || request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer parent-token" {
			t.Fatalf("check-in status request = path=%s method=%s authorization=%q", request.URL.Path, request.Method, request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":true,"records":[{"checkin_date":"` + today + `","quota_awarded":1200},{"checkin_date":"2026-01-01","quota_awarded":1}]}}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	status, err := client.CheckinStatus(context.Background(), "parent-token")
	if err != nil || !status.CheckedInToday || status.TodayQuotaAwarded == nil || *status.TodayQuotaAwarded != 1200 {
		t.Fatalf("CheckinStatus() = %#v, %v", status, err)
	}
}

func TestCheckinStatusRequiresSuccessfulEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"success":false,"message":"not enabled"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.CheckinStatus(context.Background(), "parent-token"); !IsStatus(err, http.StatusOK) {
		t.Fatalf("CheckinStatus() error = %v, want APIError", err)
	}
}

func TestCheckinEligibilityUsesDailyTokenGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/custom/daily_tokens" || request.URL.Query().Get("user_id") != "42" || request.Header.Get("Authorization") != "Bearer parent-token" {
			t.Fatalf("eligibility request = path=%s query=%s authorization=%q", request.URL.Path, request.URL.RawQuery, request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"success":true,"data":{"can_checkin":false,"remaining":1000000,"required":1000000}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	eligibility, err := client.CheckinEligibility(context.Background(), "parent-token", 42)
	if err != nil || eligibility.CanCheckin || eligibility.Remaining != 1000000 || eligibility.Required != 1000000 {
		t.Fatalf("CheckinEligibility() = %#v, %v", eligibility, err)
	}
}

func TestCheckinEligibilityRequiresExplicitSuccessfulEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing success", body: `{"data":{"can_checkin":true}}`},
		{name: "failed envelope", body: `{"success":false,"message":"not allowed"}`},
		{name: "missing can_checkin", body: `{"success":true,"data":{"remaining":1,"required":2}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "test-agent", nil)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			if _, err := client.CheckinEligibility(context.Background(), "parent-token", 42); err == nil {
				t.Fatal("CheckinEligibility() error = nil, want envelope validation error")
			}
		})
	}
}

func TestClaimRequiresExplicitSuccessState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"message":"unrecognised response"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.ClaimDaily(context.Background(), "lottery-token"); err == nil {
		t.Fatal("ClaimDaily() error = nil, want explicit success validation error")
	}
}

func TestClaimAcceptsDashboardResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"eligibility":{"remaining":3,"spendBonusDraws":1}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	claim, err := client.ClaimDaily(context.Background(), "lottery-token")
	if err != nil {
		t.Fatalf("ClaimDaily() error = %v", err)
	}
	if !claim.Success || claim.Dashboard == nil {
		t.Fatalf("ClaimDaily() = %#v, want successful dashboard response", claim)
	}
	if remaining, ok := claim.Dashboard.Remaining(); !ok || remaining != 3 {
		t.Fatalf("ClaimDaily().Dashboard.Remaining() = %d, %v", remaining, ok)
	}
}

func TestSubscriptionEndpointsDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer parent-token" {
			t.Fatalf("subscription authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/api/subscription/plans":
			_, _ = writer.Write([]byte(`{"success":true,"data":[{"plan":{"id":7,"title":"高级订阅"}}]}`))
		case "/api/subscription/self":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"subscriptions":[{"subscription":{"id":12,"plan_id":7,"amount_total":1000000,"amount_used":250000,"end_time":1893456000,"status":"active"}}]}}`))
		case "/api/status":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"USD","usd_exchange_rate":7.2}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	plans, err := client.SubscriptionPlans(context.Background(), "parent-token")
	if err != nil || plans[7] != "高级订阅" {
		t.Fatalf("SubscriptionPlans() = %#v, %v", plans, err)
	}
	self, err := client.SubscriptionSelf(context.Background(), "parent-token")
	if err != nil || len(self.Subscriptions) != 1 || self.Subscriptions[0].Subscription.ID != 12 {
		t.Fatalf("SubscriptionSelf() = %#v, %v", self, err)
	}
	settings, err := client.Status(context.Background(), "parent-token")
	if err != nil || settings.QuotaPerUnit != 500000 || settings.USDExchangeRate != 7.2 {
		t.Fatalf("Status() = %#v, %v", settings, err)
	}
}

func TestUserSelfDecodesUsageEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/user/self" || request.Header.Get("Authorization") != "Bearer parent-token" {
			t.Fatalf("user self request = path=%s authorization=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"success":true,"data":{"user":{"quota":1000,"used_quota":250,"request_count":7}}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	usage, err := client.UserSelf(context.Background(), "parent-token")
	if err != nil || usage.Quota != 1000 || usage.UsedQuota != 250 || usage.RequestCount != 7 {
		t.Fatalf("UserSelf() = %#v, %v", usage, err)
	}
}

func TestPurchaseDrawAndUnlockDrawLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedBaseURL := "http://" + request.Host
		if request.Header.Get("Authorization") != "Bearer lottery-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("Idempotency-Key") == "" {
			t.Fatal("idempotency key header is empty")
		}
		if request.Header.Get("Origin") != expectedBaseURL {
			t.Fatalf("origin = %q, want %q", request.Header.Get("Origin"), expectedBaseURL)
		}
		if request.Header.Get("Referer") != expectedBaseURL+"/lottery/" {
			t.Fatalf("referer = %q, want %q", request.Header.Get("Referer"), expectedBaseURL+"/lottery/")
		}
		if request.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", request.Method)
		}

		switch request.URL.Path {
		case "/lottery/api/draw-purchases":
			_, _ = writer.Write([]byte(`{"operation":{"status":"completed"}}`))
		case "/lottery/api/draw-limit/unlock":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"operation":{"status":"queued"}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	purchase, err := client.PurchaseDraw(context.Background(), "lottery-token", "purchase:key")
	if err != nil || purchase.Status != "completed" {
		t.Fatalf("PurchaseDraw() = %#v, %v", purchase, err)
	}

	unlock, err := client.UnlockDrawLimit(context.Background(), "lottery-token", "unlock:key")
	if err != nil || unlock.Status != "queued" {
		t.Fatalf("UnlockDrawLimit() = %#v, %v", unlock, err)
	}
}

func TestPurchaseDrawAndUnlockDrawLimitValidateInputsAndStatus(t *testing.T) {
	client, err := NewClient("https://example.com", "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.PurchaseDraw(context.Background(), "", "purchase:key"); err == nil {
		t.Fatal("PurchaseDraw() error = nil, want token validation error")
	}
	if _, err := client.PurchaseDraw(context.Background(), "lottery-token", ""); err == nil {
		t.Fatal("PurchaseDraw() error = nil, want idempotency key validation error")
	}
	if _, err := client.UnlockDrawLimit(context.Background(), "", "unlock:key"); err == nil {
		t.Fatal("UnlockDrawLimit() error = nil, want token validation error")
	}
	if _, err := client.UnlockDrawLimit(context.Background(), "lottery-token", ""); err == nil {
		t.Fatal("UnlockDrawLimit() error = nil, want idempotency key validation error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/lottery/api/draw-purchases":
			_, _ = writer.Write([]byte(`{"success":true,"data":{"operation":{}}}`))
		case "/lottery/api/draw-limit/unlock":
			_, _ = writer.Write([]byte(`{"success":false,"message":"blocked"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err = NewClient(server.URL, "test-agent", nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.PurchaseDraw(context.Background(), "lottery-token", "purchase:key"); err == nil {
		t.Fatal("PurchaseDraw() error = nil, want missing status error")
	}
	if _, err := client.UnlockDrawLimit(context.Background(), "lottery-token", "unlock:key"); !IsStatus(err, http.StatusOK) {
		t.Fatalf("UnlockDrawLimit() error = %v, want APIError", err)
	}
}
