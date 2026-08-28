package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"skyeapi/lottery-bot/internal/state"
)

func TestCreateAccountNeverReturnsCredentials(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPost, "/api/accounts",
		`{"label":"测试账号","login_name":"test@example.test","password":"test-password"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create account status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "test-password") || strings.Contains(body, "test@example.test") {
		t.Fatal("credential leak")
	}
	for _, required := range []string{`"masked_login_name":"t***@example.test"`, `"status":"enabled"`, `"id":"account-`} {
		if !strings.Contains(body, required) {
			t.Fatalf("create response missing %q: %s", required, body)
		}
	}
}

func TestCreateAccountRequiresCompleteFields(t *testing.T) {
	server := testServer(t)
	for _, body := range []string{
		`{"label":"账号","login_name":"user@example.test"}`,
		`{"label":"账号","password":"secret"}`,
		`{"label":"","login_name":"user@example.test","password":"secret"}`,
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPost, "/api/accounts", body))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("create with body %s status = %d, want 400", body, recorder.Code)
		}
	}
}

func TestUpdateAccountCanDisableAndEnable(t *testing.T) {
	server := testServer(t)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPatch, "/api/accounts/account-a",
		`{"label":"改名账号","status":"disabled"}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"disabled"`) || !strings.Contains(recorder.Body.String(), `"label":"改名账号"`) {
		t.Fatalf("update response = %d: %s", recorder.Code, recorder.Body.String())
	}

	action := httptest.NewRecorder()
	server.Handler().ServeHTTP(action, authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil))
	if action.Code != http.StatusConflict {
		t.Fatalf("check-in on disabled account status = %d, want 409: %s", action.Code, action.Body.String())
	}

	enable := httptest.NewRecorder()
	server.Handler().ServeHTTP(enable, authenticatedJSONRequest(http.MethodPatch, "/api/accounts/account-a", `{"status":"enabled"}`))
	if enable.Code != http.StatusOK || !strings.Contains(enable.Body.String(), `"status":"enabled"`) {
		t.Fatalf("enable response = %d: %s", enable.Code, enable.Body.String())
	}
}

func TestUpdateAccountRefreshesMaskedLoginName(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPatch, "/api/accounts/account-a",
		`{"login_name":"new-login@example.test","password":"new-password"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"masked_login_name":"n***@example.test"`) || strings.Contains(recorder.Body.String(), "new-password") || strings.Contains(recorder.Body.String(), "new-login@example.test") {
		t.Fatalf("update response leaked or missed masking: %s", recorder.Body.String())
	}
}

func TestDeleteAccountRequiresConfirmationAndRemovesScopedState(t *testing.T) {
	server := testServer(t)
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := store.GetOrCreateAction("account-a", "2026-08-29", state.ActionCheckin); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if err := store.PutAuth("account-a", state.AuthState{UserID: 5, ParentAccessToken: "seed-token"}); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	_ = store.Close()

	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, authenticatedJSONRequest(http.MethodDelete, "/api/accounts/account-a", `{}`))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("delete without confirmation status = %d, want 400", missing.Code)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodDelete, "/api/accounts/account-a", `{"confirmation":"DELETE"}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"deleted":true`) {
		t.Fatalf("delete response = %d: %s", recorder.Code, recorder.Body.String())
	}

	store, err = server.sharedStore()
	if err != nil {
		t.Fatalf("shared store: %v", err)
	}
	if _, ok := store.Action("account-a", "2026-08-29", state.ActionCheckin); ok {
		t.Fatal("scoped action survived account deletion")
	}
	if _, err := store.AccountRegistry().Get("account-a"); err == nil {
		t.Fatal("registry entry survived account deletion")
	}
	if store.Auth("account-a").UserID != 0 {
		t.Fatal("legacy auth survived account deletion")
	}

	after := httptest.NewRecorder()
	server.Handler().ServeHTTP(after, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	if strings.Contains(after.Body.String(), `"account-a"`) {
		t.Fatalf("deleted account still listed: %s", after.Body.String())
	}
}

func TestValidateAccountReportsHealthWithoutLogin(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/validate", `{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("validate status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"account_id":"account-a"`) || !strings.Contains(body, `"auth_health"`) {
		t.Fatalf("validate response incomplete: %s", body)
	}
	if strings.Contains(body, "password") || strings.Contains(body, "cookie") || strings.Contains(body, "access_token") {
		t.Fatalf("validate response leaked auth material: %s", body)
	}
}

func TestReauthenticateRequiresExplicitConfirm(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/reauthenticate", `{}`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("reauthenticate without confirm status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionPreviewReportsUnsupportedCapability(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts/account-a/session-preview", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("session preview status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"capability":"unsupported"`) || !strings.Contains(body, `"candidate_count":0`) {
		t.Fatalf("session preview must report unsupported with zero candidates: %s", body)
	}
	if !strings.Contains(body, "不可用") {
		t.Fatalf("session preview must explain that cleanup is unavailable: %s", body)
	}
}

func TestAccountViewsNeverExposeRawLogins(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "a@example.com") {
		t.Fatal("account list leaked the raw login name")
	}
	if !strings.Contains(body, `"masked_login_name":"a***@example.com"`) {
		t.Fatalf("account list missing masked login name: %s", body)
	}
}
