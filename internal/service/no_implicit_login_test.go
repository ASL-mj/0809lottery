package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

// The auto-draw scheduler executes draws through Runner.DrawAvailable outside
// any user interaction. When the persisted session is unusable the background
// action must fail safely instead of silently creating a new password login.
func TestBackgroundDrawActionNoImplicitLogin(t *testing.T) {
	store := testStore(t)
	defer store.Close()
	now := time.Date(2026, time.August, 29, 19, 5, 0, 0, shanghaiLocation)
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                 1,
		ParentAccessToken:      "expired-parent",
		ParentAccessExpiresAt:  now.Add(-time.Hour),
		LotteryAccessToken:     "expired-lottery",
		LotteryAccessExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}

	client := &fakeClient{
		// A successful login result proves the probe fails because login was
		// attempted, not because the fallback itself is broken.
		login:       lottery.LoginResult{UserID: 1, AccessToken: "implicit-session", AccessExpiresAt: now.Add(time.Hour)},
		userSelfErrs: []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
		refreshErrs:  []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}},
	}
	runner := testRunner(t, store, client, now)

	_, err := runner.DrawAvailable(context.Background(), "account-a", "draw:probe:no-implicit-login")
	if err == nil {
		t.Fatal("background draw with unusable auth returned nil error")
	}
	if client.loginCalls != 0 {
		t.Fatalf("background action logged in %d times", client.loginCalls)
	}
	if client.drawCalls != 0 {
		t.Fatalf("background action drew %d times without usable auth", client.drawCalls)
	}
}
