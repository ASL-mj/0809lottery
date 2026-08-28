package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/state"
)

// The accounts endpoint is the browser-facing snapshot surface. Even when the
// state file holds live credentials the response must only contain public
// business fields.
func TestAccountsPublicResponseNoSecretLeak(t *testing.T) {
	server := testServer(t)
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open state = %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "parent-access-secret",
		ParentAccessExpiresAt:  time.Now().Add(time.Hour),
		LotteryAccessToken:     "lottery-access-secret",
		LotteryAccessExpiresAt: time.Now().Add(time.Hour),
		Cookies: []state.Cookie{{
			Name:  "new_api_refresh",
			Value: "refresh-cookie-secret",
		}},
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	_ = store.Close()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{
		"parent_access_token",
		"lottery_access_token",
		"cookie",
		"Bearer ",
		"parent-access-secret",
		"lottery-access-secret",
		"refresh-cookie-secret",
		"do-not-return",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public response leaked authentication data %q: %s", forbidden, body)
		}
	}
}
