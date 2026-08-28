package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

const csrfCookieName = "workbench_csrf"

// csrfToken lazily generates the per-process token used as the CSRF cookie
// value. Browsers echo it back in the X-CSRF-Token header.
func (s *Server) csrfToken() string {
	s.csrfMu.Lock()
	defer s.csrfMu.Unlock()
	if s.csrf != "" {
		return s.csrf
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		// A missing token must fail closed: without a cookie no unsafe request
		// passes the middleware.
		return ""
	}
	s.csrf = hex.EncodeToString(buffer)
	return s.csrf
}

func (s *Server) setCSRFCookie(writer http.ResponseWriter) {
	token := s.csrfToken()
	if token == "" {
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		HttpOnly: false,
	})
}

var csrfUnsafeMethods = map[string]bool{
	http.MethodPost:  true,
	http.MethodPatch: true,
	http.MethodPut:   true,
	http.MethodDelete: true,
}

// withCSRF rejects unsafe methods that do not echo the CSRF cookie in the
// X-CSRF-Token header and do not originate from the workbench's own origin.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !csrfUnsafeMethods[request.Method] {
			next.ServeHTTP(writer, request)
			return
		}
		if !s.sameOrigin(request) {
			writeError(writer, http.StatusForbidden, "请求来源不受信任")
			return
		}
		cookie, err := request.Cookie(csrfCookieName)
		if err != nil || cookie.Value == "" {
			writeError(writer, http.StatusForbidden, "缺少 CSRF 令牌")
			return
		}
		header := request.Header.Get("X-CSRF-Token")
		if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
			writeError(writer, http.StatusForbidden, "CSRF 令牌不匹配")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// sameOrigin verifies the Origin header, falling back to Referer. Requests
// without either header are rejected.
func (s *Server) sameOrigin(request *http.Request) bool {
	source := request.Header.Get("Origin")
	if source == "" {
		source = request.Header.Get("Referer")
	}
	if strings.TrimSpace(source) == "" {
		return false
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return false
	}
	expected := request.Host
	return subtle.ConstantTimeCompare([]byte(parsed.Host), []byte(expected)) == 1
}
