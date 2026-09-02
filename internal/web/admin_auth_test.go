package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminAuthRootRedirectsToLoginWithoutChallenge(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "http://workbench.test/", nil)
	request.Host = "workbench.test"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("root status = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	if got := recorder.Header().Get("Location"); got != "/login?next=%2F" {
		t.Fatalf("root redirect = %q", got)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("root sent Basic Auth challenge: %q", got)
	}
}

func TestAdminAuthAPIUsesJSONUnauthorizedWithoutChallenge(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/accounts", nil)
	request.Host = "workbench.test"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "login_required") {
		t.Fatalf("API unauthorized response = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("API sent Basic Auth challenge: %q", got)
	}
}

func TestAdminAuthLoginCookieAndLogoutLifecycle(t *testing.T) {
	server := testServer(t)

	loginPage := httptest.NewRecorder()
	server.Handler().ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "http://workbench.test/login", nil))
	if loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "登录 0809 账号工作台") {
		t.Fatalf("login page = %d: %s", loginPage.Code, loginPage.Body.String())
	}
	csrf := cookieNamed(loginPage.Result().Cookies(), csrfCookieName)
	if csrf == nil || csrf.Value == "" {
		t.Fatal("login page did not set CSRF cookie")
	}

	wrong := adminLoginRequestForTest(t, server, "wrong", "wrong", csrf)
	if wrong.Code != http.StatusUnauthorized || !strings.Contains(wrong.Body.String(), "管理员账号或密码不正确") {
		t.Fatalf("wrong login = %d: %s", wrong.Code, wrong.Body.String())
	}
	if session := cookieNamed(wrong.Result().Cookies(), adminSessionCookie); session != nil {
		t.Fatal("wrong login issued a session cookie")
	}

	success := adminLoginRequestForTest(t, server, "admin", "secret", csrf)
	if success.Code != http.StatusOK {
		t.Fatalf("successful login = %d: %s", success.Code, success.Body.String())
	}
	session := cookieNamed(success.Result().Cookies(), adminSessionCookie)
	if session == nil || session.Value == "" {
		t.Fatal("successful login did not issue a session cookie")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Fatalf("session cookie attributes = %#v", session)
	}
	if session.MaxAge <= 0 {
		t.Fatalf("session cookie max age = %d", session.MaxAge)
	}
	wantMaxAge := int((7 * 24 * time.Hour) / time.Second)
	if session.MaxAge != wantMaxAge {
		t.Fatalf("session cookie max age = %d, want %d", session.MaxAge, wantMaxAge)
	}
	if delta := session.Expires.Sub(time.Now().UTC()); delta < 6*24*time.Hour || delta > 8*24*time.Hour {
		t.Fatalf("session cookie expires in %v, want about 7 days", delta)
	}

	health := httptest.NewRecorder()
	healthRequest := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/health", nil)
	healthRequest.Host = "workbench.test"
	healthRequest.AddCookie(session)
	server.Handler().ServeHTTP(health, healthRequest)
	if health.Code != http.StatusOK {
		t.Fatalf("session health = %d: %s", health.Code, health.Body.String())
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodPost, "http://workbench.test/api/admin/logout", nil)
	logoutRequest.Host = "workbench.test"
	logoutRequest.Header.Set("Origin", "http://workbench.test")
	logoutRequest.Header.Set("X-CSRF-Token", csrf.Value)
	logoutRequest.AddCookie(csrf)
	logoutRequest.AddCookie(session)
	server.Handler().ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", logout.Code, logout.Body.String())
	}
	cleared := cookieNamed(logout.Result().Cookies(), adminSessionCookie)
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("logout cookie = %#v", cleared)
	}

	afterLogout := httptest.NewRecorder()
	afterLogoutRequest := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/health", nil)
	afterLogoutRequest.Host = "workbench.test"
	afterLogoutRequest.AddCookie(session)
	server.Handler().ServeHTTP(afterLogout, afterLogoutRequest)
	if afterLogout.Code != http.StatusUnauthorized {
		t.Fatalf("health after logout = %d: %s", afterLogout.Code, afterLogout.Body.String())
	}
}

func TestAdminAuthExpiredSessionIsRejected(t *testing.T) {
	server := testServer(t)
	token, _, err := server.ensureAdminSessions().create(time.Now().UTC().Add(-adminSessionTTL - time.Minute))
	if err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/health", nil)
	request.Host = "workbench.test"
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired session = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminAuthValidCookieRequestRenewsSession(t *testing.T) {
	server := testServer(t)
	base := time.Date(2026, time.September, 2, 1, 0, 0, 0, time.UTC)
	token, expiresAt, err := server.ensureAdminSessions().create(base)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !expiresAt.Equal(base.Add(adminSessionTTL)) {
		t.Fatalf("initial expiry = %v, want %v", expiresAt, base.Add(adminSessionTTL))
	}

	renewed, ok := server.ensureAdminSessions().renew(token, base.Add(2*time.Hour))
	if !ok || !renewed.Equal(base.Add(2*time.Hour+adminSessionTTL)) {
		t.Fatalf("renewed expiry = %v, %v", renewed, ok)
	}
	if !server.ensureAdminSessions().valid(token, base.Add(2*time.Hour+adminSessionTTL-time.Second)) {
		t.Fatal("renewed session should remain valid until its new expiry")
	}

	request := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/health", nil)
	request.Host = "workbench.test"
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("renewed session health = %d: %s", recorder.Code, recorder.Body.String())
	}
	renewedCookie := cookieNamed(recorder.Result().Cookies(), adminSessionCookie)
	if renewedCookie == nil || renewedCookie.MaxAge != int(adminSessionTTL/time.Second) {
		t.Fatalf("renewed response cookie = %#v", renewedCookie)
	}
}

func TestAdminAuthSlidingSessionExpiresAfterLastAccess(t *testing.T) {
	server := testServer(t)
	base := time.Date(2026, time.September, 2, 1, 0, 0, 0, time.UTC)
	token, _, err := server.ensureAdminSessions().create(base)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	lastAccess := base.Add(6 * 24 * time.Hour)
	if _, ok := server.ensureAdminSessions().renew(token, lastAccess); !ok {
		t.Fatal("session renewal at last access failed")
	}
	if !server.ensureAdminSessions().valid(token, lastAccess.Add(adminSessionTTL-time.Second)) {
		t.Fatal("session should be valid before seven days after last access")
	}
	if server.ensureAdminSessions().valid(token, lastAccess.Add(adminSessionTTL)) {
		t.Fatal("session should expire seven days after last access")
	}
}

func TestAdminAuthRevokedSessionCannotBeRenewed(t *testing.T) {
	server := testServer(t)
	base := time.Date(2026, time.September, 2, 1, 0, 0, 0, time.UTC)
	token, _, err := server.ensureAdminSessions().create(base)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	server.ensureAdminSessions().revoke(token)
	if _, ok := server.ensureAdminSessions().renew(token, base.Add(time.Hour)); ok {
		t.Fatal("revoked session was renewed")
	}
}

func TestAdminAuthExplicitBasicAuthRemainsCompatible(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "http://workbench.test/api/health", nil)
	request.Host = "workbench.test"
	request.SetBasicAuth("admin", "secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Basic Auth health = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("Basic Auth response sent challenge: %q", got)
	}
	if cookie := cookieNamed(recorder.Result().Cookies(), adminSessionCookie); cookie != nil {
		t.Fatalf("Basic Auth response created a session cookie: %#v", cookie)
	}
}

func TestSafeLoginNextRejectsOpenRedirects(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"", "/"},
		{"https://evil.example/", "/"},
		{"//evil.example/", "/"},
		{"/\\evil.example/", "/"},
		{"/accounts?tab=active", "/accounts?tab=active"},
	}
	for _, test := range tests {
		if got := safeLoginNext(test.input); got != test.want {
			t.Errorf("safeLoginNext(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func adminLoginRequestForTest(t *testing.T, server *Server, username, password string, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(adminLoginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("marshal login request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://workbench.test/api/admin/login?next=%2F", bytes.NewReader(body))
	request.Host = "workbench.test"
	request.Header.Set("Origin", "http://workbench.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf.Value)
	request.AddCookie(csrf)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
