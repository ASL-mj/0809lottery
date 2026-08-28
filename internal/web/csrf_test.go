package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnsafeMethodsRequireMatchingCSRFToken(t *testing.T) {
	server := testServer(t)

	// Without the token header the request is rejected even with valid auth.
	request := authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil)
	request.Header.Del("X-CSRF-Token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF token status = %d, want 403", recorder.Code)
	}

	// A mismatching token is rejected too.
	mismatch := authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil)
	mismatch.Header.Set("X-CSRF-Token", "wrong-token")
	mismatchRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(mismatchRecorder, mismatch)
	if mismatchRecorder.Code != http.StatusForbidden {
		t.Fatalf("mismatched CSRF token status = %d, want 403", mismatchRecorder.Code)
	}
}

func TestCrossOriginRequestsAreRejected(t *testing.T) {
	server := testServer(t)
	request := authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil)
	request.Header.Set("Origin", "http://evil.example")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", recorder.Code)
	}

	originless := authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil)
	originless.Header.Del("Origin")
	originless.Header.Del("Referer")
	originlessRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(originlessRecorder, originless)
	if originlessRecorder.Code != http.StatusForbidden {
		t.Fatalf("originless status = %d, want 403", originlessRecorder.Code)
	}

	refererOK := authenticatedRequest(http.MethodPost, "/api/accounts/account-a/checkin", nil)
	refererOK.Header.Del("Origin")
	refererOK.Header.Set("Referer", "http://workbench.test/profile")
	refererRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(refererRecorder, refererOK)
	if refererRecorder.Code == http.StatusForbidden {
		t.Fatalf("same-origin referer rejected: %s", refererRecorder.Body.String())
	}
}

func TestIndexSetsCSRFCookie(t *testing.T) {
	server := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("index status = %d", recorder.Code)
	}
	cookies := recorder.Result().Cookies()
	found := false
	for _, cookie := range cookies {
		if cookie.Name == csrfCookieName && cookie.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("index did not set the CSRF cookie")
	}
}
