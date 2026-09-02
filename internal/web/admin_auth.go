package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	adminSessionCookie = "workbench_session"
	adminSessionTTL    = 7 * 24 * time.Hour
	adminSessionBytes  = 32
)

type adminSessionStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func newAdminSessionStore() *adminSessionStore {
	return &adminSessionStore{entries: make(map[string]time.Time)}
}

func (store *adminSessionStore) create(now time.Time) (string, time.Time, error) {
	tokenBytes := make([]byte, adminSessionBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	expiresAt := now.Add(adminSessionTTL)
	store.mu.Lock()
	store.entries[token] = expiresAt
	store.mu.Unlock()
	return token, expiresAt, nil
}

func (store *adminSessionStore) valid(token string, now time.Time) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	valid := false
	for entry, expiresAt := range store.entries {
		if !expiresAt.After(now) {
			delete(store.entries, entry)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(entry), []byte(token)) == 1 {
			valid = true
		}
	}
	return valid
}

func (store *adminSessionStore) renew(token string, now time.Time) (time.Time, bool) {
	if strings.TrimSpace(token) == "" {
		return time.Time{}, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for entry, expiresAt := range store.entries {
		if !expiresAt.After(now) {
			delete(store.entries, entry)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(entry), []byte(token)) == 1 {
			renewed := now.Add(adminSessionTTL)
			store.entries[entry] = renewed
			return renewed, true
		}
	}
	return time.Time{}, false
}

func (store *adminSessionStore) revoke(token string) {
	if token == "" {
		return
	}
	store.mu.Lock()
	delete(store.entries, token)
	store.mu.Unlock()
}

type adminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func setAdminSessionCookie(writer http.ResponseWriter, request *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(writer, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(adminSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   requestIsHTTPS(request),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) ensureAdminSessions() *adminSessionStore {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if s.adminSessions == nil {
		s.adminSessions = newAdminSessionStore()
	}
	return s.adminSessions
}

func (s *Server) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	s.setCSRFCookie(writer)
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write(loginHTML)
}

func (s *Server) handleAdminLogin(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	var input adminLoginRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if !s.validAdminCredentials(input.Username, input.Password) {
		writeError(writer, http.StatusUnauthorized, "管理员账号或密码不正确")
		return
	}
	token, expiresAt, err := s.ensureAdminSessions().create(time.Now().UTC())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法建立工作台会话")
		return
	}
	setAdminSessionCookie(writer, request, token, expiresAt)
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"redirect": safeLoginNext(request.URL.Query().Get("next")),
	})
}

func (s *Server) handleAdminLogout(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	if cookie, err := request.Cookie(adminSessionCookie); err == nil {
		s.ensureAdminSessions().revoke(cookie.Value)
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(request),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) validAdminCredentials(username, password string) bool {
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.WebUser)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.WebPass)) == 1
	return usernameOK && passwordOK
}

func (s *Server) validAdminRequest(request *http.Request) bool {
	if cookie, err := request.Cookie(adminSessionCookie); err == nil {
		if s.ensureAdminSessions().valid(cookie.Value, time.Now().UTC()) {
			return true
		}
	}
	username, password, ok := request.BasicAuth()
	return ok && s.validAdminCredentials(username, password)
}

func (s *Server) withAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isPublicAdminRoute(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if request.URL.Path == "/api/admin/logout" && request.Method == http.MethodPost {
			if s.validAdminRequest(request) {
				next.ServeHTTP(writer, request)
				return
			}
		} else if cookie, err := request.Cookie(adminSessionCookie); err == nil {
			if expiresAt, ok := s.ensureAdminSessions().renew(cookie.Value, time.Now().UTC()); ok {
				setAdminSessionCookie(writer, request, cookie.Value, expiresAt)
				next.ServeHTTP(writer, request)
				return
			}
		}
		if username, password, ok := request.BasicAuth(); ok && s.validAdminCredentials(username, password) {
			next.ServeHTTP(writer, request)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{
				"error":   "login_required",
				"message": "需要工作台管理员认证",
			})
			return
		}
		target := request.URL.RequestURI()
		if target == "" {
			target = "/"
		}
		redirect := "/login?next=" + url.QueryEscape(safeLoginNext(target))
		http.Redirect(writer, request, redirect, http.StatusSeeOther)
	})
}

func isPublicAdminRoute(request *http.Request) bool {
	return (request.URL.Path == "/login" && request.Method == http.MethodGet) ||
		(request.URL.Path == "/api/admin/login" && request.Method == http.MethodPost)
}

func safeLoginNext(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return parsed.RequestURI()
}

func requestIsHTTPS(request *http.Request) bool {
	return request.TLS != nil || strings.EqualFold(strings.TrimSpace(request.Header.Get("X-Forwarded-Proto")), "https")
}
