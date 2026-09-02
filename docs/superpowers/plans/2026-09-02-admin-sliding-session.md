# 管理员滑动会话 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将星额台管理员工作台会话改为滑动 7 天有效期，并让服务端会话与浏览器 Cookie 在每次 Cookie 认证请求后同步续期。

**Architecture:** 保留进程内随机会话 Token 和 `adminSessionStore`，把只读 `valid` 检查扩展为锁内原子 `renew` 操作。管理员认证中间件拿到新的过期时间后重新下发同名 HttpOnly Cookie；Basic Auth 仍只做兼容认证，不创建会话。

**Tech Stack:** Go `net/http`、`time`、`httptest`，现有 Makefile、Go 测试和静态分析工具。

---

### Task 1: Lock the sliding-session contract with tests

**Files:**
- Modify: `internal/web/admin_auth_test.go`
- Test: `internal/web/admin_auth_test.go`

- [ ] **Step 1: Add the 7-day cookie assertion to the login lifecycle test**

Extend `TestAdminAuthLoginCookieAndLogoutLifecycle` after the existing positive `MaxAge` assertion:

```go
wantMaxAge := int((7 * 24 * time.Hour) / time.Second)
if session.MaxAge != wantMaxAge {
    t.Fatalf("session cookie max age = %d, want %d", session.MaxAge, wantMaxAge)
}
if delta := session.Expires.Sub(time.Now().UTC()); delta < 6*24*time.Hour || delta > 8*24*time.Hour {
    t.Fatalf("session cookie expires in %v, want about 7 days", delta)
}
```

- [ ] **Step 2: Add a middleware renewal test**

Add `TestAdminAuthValidCookieRequestRenewsSession` that creates a session at `base := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)`, sends a request with the session Cookie, and verifies:

```go
renewed, ok := server.ensureAdminSessions().renew(token, base.Add(2*time.Hour))
if !ok || !renewed.Equal(base.Add(2*time.Hour + 7*24*time.Hour)) {
    t.Fatalf("renewed expiry = %v, %v", renewed, ok)
}
```

The HTTP portion must assert the protected request returns `200`, emits a `workbench_session` Cookie, and its `MaxAge` is exactly `7*24*60*60`.

- [ ] **Step 3: Add a stale-session boundary test**

Add `TestAdminAuthSlidingSessionExpiresAfterLastAccess` that creates a session at `base`, renews it at `base + 6 days`, asserts it is valid at `base + 6 days + 7 days - 1 second`, and asserts it is invalid at `base + 13 days + 1 second`.

- [ ] **Step 4: Run the focused tests and confirm they fail before implementation**

Run:

```bash
go test ./internal/web -run 'TestAdminAuth(LoginCookieAndLogoutLifecycle|ValidCookieRequestRenewsSession|SlidingSessionExpiresAfterLastAccess)$' -count=1
```

Expected: the existing login test fails on the old 12-hour MaxAge and the new renewal tests fail because `renew` and middleware renewal are not implemented.

### Task 2: Implement atomic 7-day server-side renewal

**Files:**
- Modify: `internal/web/admin_auth.go:13-68`

- [ ] **Step 1: Change the shared TTL constant**

Replace:

```go
adminSessionTTL = 12 * time.Hour
```

with:

```go
adminSessionTTL = 7 * 24 * time.Hour
```

- [ ] **Step 2: Add the locked renewal operation**

Add this method beside `valid`:

```go
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
```

The operation must validate, remove expired entries, compare in constant time, and update the matching record under one mutex acquisition. Existing `valid` remains available for tests and any read-only callers.

### Task 3: Renew the browser Cookie in the authentication middleware

**Files:**
- Modify: `internal/web/admin_auth.go:90-125,150-178`
- Test: `internal/web/admin_auth_test.go`

- [ ] **Step 1: Add a Cookie writer helper**

Add:

```go
func setAdminSessionCookie(writer http.ResponseWriter, request *http.Request, token string, expiresAt time.Time) {
    http.SetCookie(writer, &http.Cookie{
        Name: adminSessionCookie, Value: token, Path: "/",
        Expires: expiresAt, MaxAge: int(adminSessionTTL / time.Second),
        HttpOnly: true, Secure: requestIsHTTPS(request), SameSite: http.SameSiteLaxMode,
    })
}
```

Use this helper for successful login and for renewed Cookie sessions so attributes cannot diverge.

- [ ] **Step 2: Change Cookie authentication to return renewal state**

In `withAdminAuth`, read the Cookie once. When present, call `renew(cookie.Value, now)`. On success, set the renewed Cookie and call the next handler. If it fails, continue to the existing Basic Auth fallback. Do not renew or set a Cookie for Basic Auth.

The control flow must preserve public routes, API JSON 401 responses, HTML redirects, logout behavior, and the existing `validAdminRequest` behavior for callers that only need a boolean.

- [ ] **Step 3: Ensure revoked sessions cannot be renewed**

Keep `revoke` deleting the entry under the same mutex. The renewal test must call `revoke(token)` before a request and assert the request remains unauthenticated.

### Task 4: Documentation and complete verification

**Files:**
- Modify: `README.md:3,21,63-67`
- Test: `internal/web/admin_auth_test.go`

- [ ] **Step 1: Update administrator session documentation**

Replace the phrase “本地短期会话 Cookie” with “本地滑动 7 天会话 Cookie”，and document that any authenticated Cookie request extends the session to seven days from that request. State that Basic Auth does not create or extend a Cookie session and process restart still clears in-memory sessions.

- [ ] **Step 2: Run focused and full verification**

Run:

```bash
gofmt -w internal/web/admin_auth.go internal/web/admin_auth_test.go
go test ./internal/web -run 'TestAdminAuth|TestSafeLoginNext' -count=1
go test ./... -count=1
go vet ./...
make build
git diff --check
```

Expected: all commands exit `0`; the focused tests cover login Cookie TTL, sliding renewal, stale expiry, Basic Auth compatibility, logout, and public-route behavior.

- [ ] **Step 3: Review the final diff and commit implementation**

Run:

```bash
git diff --stat
git status --short --branch
git diff -- internal/web/admin_auth.go internal/web/admin_auth_test.go README.md
```

Commit only the implementation files and documentation with:

```bash
git add internal/web/admin_auth.go internal/web/admin_auth_test.go README.md
git commit -m "feat: extend admin sessions with sliding seven day expiry"
```
