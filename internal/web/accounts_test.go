package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// With no reachable platform the preview fails safely and points at
// reauthentication instead of faking a cleanup result.
func TestSessionPreviewFailsSafelyWithoutPlatform(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/api/accounts/account-a/session-preview", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("session preview status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "重新认证") {
		t.Fatalf("preview failure must point at reauthentication: %s", recorder.Body.String())
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

func TestSessionPreviewAndCleanupEndpoints(t *testing.T) {
	// A platform fake that answers the sessions listing.
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/user/sessions" && request.Method == http.MethodGet:
			_, _ = writer.Write([]byte(`{"success":true,"data":[{"sid":"sid-current","current":true,"login_method":"password","ip":"111.32.43.207","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36","last_active_at":1787954325},{"sid":"sid-phone","current":false,"ip":"111.32.43.208","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 26_4_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/151.0.7922.112 Mobile/15E148 Safari/604.1","last_active_at":1787954300}]}`))
		case request.URL.Path == "/api/user/sessions/sid-phone" && request.Method == http.MethodDelete:
			_, _ = writer.Write([]byte(`{"success":true,"data":{"current":false,"revoked_sid":"sid-phone"}}`))
		case request.URL.Path == "/api/user/sessions/revoke-others" && request.Method == http.MethodPost:
			_, _ = writer.Write([]byte(`{"success":true,"data":{"revoked_count":0}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	server := testServer(t)
	server.cfg.BaseURL = upstream.URL
	store, err := state.Open(server.cfg.StatePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"sid-current"}`))
	if err := store.PutAuth("account-a", state.AuthState{
		UserID:                1,
		ParentAccessToken:     "header." + payload + ".signature",
		ParentAccessExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutAuth() error = %v", err)
	}
	_ = store.Close()

	preview := httptest.NewRecorder()
	server.Handler().ServeHTTP(preview, authenticatedRequest(http.MethodGet, "/api/accounts/account-a/session-preview", nil))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	for _, required := range []string{
		`"capability":"revocable"`, `"sid":"sid-current"`, `"current":true`, `"workbench_owned":false`, `"candidate_count":0`,
		// The owner sees parsed device details for their own sessions.
		`"ip":"111.32.43.207"`, `"device":"Chrome 151 · macOS"`, `"device":"Chrome iOS 151 · iOS"`, `"login_method":"password"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("preview missing %q: %s", required, body)
		}
	}
	// The raw user-agent string must never leave the server.
	for _, forbidden := range []string{"user_agent", "Mozilla/5.0"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("preview exposed %q", forbidden)
		}
	}

	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/sessions/cleanup", `{}`))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("cleanup without confirm status = %d, want 400", missing.Code)
	}

	cleanup := httptest.NewRecorder()
	server.Handler().ServeHTTP(cleanup, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/sessions/cleanup", `{"confirm":true}`))
	if cleanup.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d: %s", cleanup.Code, cleanup.Body.String())
	}
	if !strings.Contains(cleanup.Body.String(), `"revoked":[]`) {
		t.Fatalf("cleanup response = %s", cleanup.Body.String())
	}

	missingSid := httptest.NewRecorder()
	server.Handler().ServeHTTP(missingSid, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/sessions/revoke", `{}`))
	if missingSid.Code != http.StatusBadRequest {
		t.Fatalf("revoke without sid status = %d, want 400", missingSid.Code)
	}

	revoke := httptest.NewRecorder()
	server.Handler().ServeHTTP(revoke, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/sessions/revoke", `{"sid":"sid-phone"}`))
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d: %s", revoke.Code, revoke.Body.String())
	}
	if !strings.Contains(revoke.Body.String(), `"revoked_sid":"sid-phone"`) || !strings.Contains(revoke.Body.String(), `"current_revoked":false`) {
		t.Fatalf("revoke response = %s", revoke.Body.String())
	}

	others := httptest.NewRecorder()
	server.Handler().ServeHTTP(others, authenticatedJSONRequest(http.MethodPost, "/api/accounts/account-a/sessions/revoke-others", `{"confirm":true}`))
	if others.Code != http.StatusOK {
		t.Fatalf("revoke-others status = %d: %s", others.Code, others.Body.String())
	}
	if !strings.Contains(others.Body.String(), `"revoked":`) {
		t.Fatalf("revoke-others response = %s", others.Body.String())
	}
}
