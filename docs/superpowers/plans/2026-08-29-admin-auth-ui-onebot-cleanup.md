# 管理员认证 UI、OneBot 清理与浏览器 UA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the browser Basic Auth challenge with an in-workbench administrator login page, remove obsolete OneBot/QQ configuration, and make every platform request use the confirmed macOS Chrome User-Agent.

**Architecture:** Keep the existing Go HTTP service and CSRF middleware. Add a process-local administrator session store backed by cryptographically random opaque cookies, preserve explicitly supplied valid Basic Auth for command-line compatibility without emitting a challenge, and serve a separate embedded login page for browsers. Account credentials, platform tokens, account actions, and scheduler behavior remain unchanged.

**Tech Stack:** Go standard library, embedded HTML/CSS/JavaScript, net/http/httptest, existing Makefile and JSON state store. No new dependency.

---

## File Map

| File | Responsibility |
| --- | --- |
| internal/web/admin_auth.go | Process-local administrator session table, cookie parsing, credential checks, and safe redirect target validation. |
| internal/web/admin_auth_test.go | HTTP tests for login, logout, session expiry, Basic Auth compatibility, and unauthorized responses. |
| internal/web/server.go | Embed /login, add admin login/logout routes, protect the existing mux with the new middleware, and redirect the HTML root. |
| internal/web/static/login.html | Confirmed C-style administrator login page with inline error feedback and no browser alert. |
| internal/web/static/index.html | Add logout control and route API 401 login_required responses to /login. |
| internal/config/config.go | Keep the exact Chrome UA as the single default. |
| config.example.env | Document current workbench settings and the exact UA; keep legacy migration comments without active OneBot values. |
| lottery-bot.env | Remove OneBot/QQ and migrated ACCOUNT_* runtime variables; set the exact UA in the private local environment. |
| README.md | Describe browser login/logout and remove obsolete Basic Auth/OneBot instructions. |
| Makefile | Make health checks use an explicit valid Basic Auth header, avoiding a browser challenge. |

## Task 1: Lock the authentication contract with failing tests

**Files:** Create internal/web/admin_auth_test.go; inspect existing web test helpers.

- [x] **Step 1: Add an isolated HTTP test server helper.**

Use t.TempDir() for state and vault files, the repository's existing test vault key helper, and admin/test-password credentials. Do not print credentials or vault values in test failures.

- [x] **Step 2: Test browser and API unauthenticated behavior.**

Assert that GET / returns 303 with Location /login?next=%2F, GET /api/accounts returns JSON 401 containing login_required, and neither response contains WWW-Authenticate.

- [x] **Step 3: Test the complete local session lifecycle.**

Assert that GET /login is public and sets workbench_csrf; wrong credentials return one generic 401 message with no session cookie; same-origin CSRF-protected correct credentials return 200 and set workbench_session with HttpOnly, SameSite=Lax, and Path=/; the session reaches /api/health; logout clears and invalidates it; and an expired session is rejected.

- [x] **Step 4: Test explicit Basic Auth compatibility.**

Send a correct Authorization: Basic header directly to /api/health and assert it succeeds without a WWW-Authenticate header. This verifies compatibility without restoring the browser challenge.

- [x] **Step 5: Run the focused tests and confirm they fail before implementation.**

Run: go test ./internal/web -run 'TestAdminAuth_' -count=1

Expected: compilation or assertion failures for the missing session/login behavior.

## Task 2: Implement process-local administrator sessions and routes

**Files:** Create internal/web/admin_auth.go; modify internal/web/server.go and internal/web/csrf.go only where route/middleware integration requires it.

- [x] **Step 1: Define the session store and cookie contract.**

Use a mutex-protected map[string]adminSession, 32 random bytes from crypto/rand encoded as an opaque token, a 12-hour absolute expiry, constant-time token comparison, and opportunistic removal of expired entries. Store no session ID on disk or in logs.

- [x] **Step 2: Implement credential and redirect validation.**

Compare configured username/password with subtle.ConstantTimeCompare. Accept Basic Auth only when explicitly present and valid; never emit WWW-Authenticate. Allow next only as an absolute path with no scheme or host, otherwise use /.

- [x] **Step 3: Implement login and logout handlers.**

Decode bounded JSON {username,password}, return 400 for malformed input, return the same generic 401 for every credential failure, create a session only after successful comparison, and set workbench_session with HttpOnly, SameSite=Lax, Path=/, and conditional HTTPS Secure. Logout removes the token and sets an expired matching cookie. Neither handler calls the 0809 client.

- [x] **Step 4: Integrate public routes and middleware.**

Embed static/login.html; register GET /login, POST /api/admin/login, and POST /api/admin/logout; wrap the existing mux with CSRF and the new admin middleware; redirect only the unauthenticated HTML root; and return JSON 401 with login_required for protected APIs.

- [x] **Step 5: Run focused and package tests.**

Run go test ./internal/web -run 'TestAdminAuth_|TestCSRF' -count=1, then go test ./internal/web -count=1; both must pass before page work begins.

## Task 3: Add the confirmed C-style login page and client behavior

**Files:** Create internal/web/static/login.html; modify internal/web/static/index.html.

- [x] **Step 1: Build the login document.**

Use the approved single-column panel, existing light-gray background, thin border, black primary button, green local-status marker, labeled controls, password visibility toggle, responsive width, and visible focus styles. Render failures in an element with role=alert and aria-live=polite; never call alert(. Submit with the CSRF cookie echoed in X-CSRF-Token, disable while waiting, show 登录中, and follow only the server-safe next value.

- [x] **Step 2: Add logout and 401 handling to the workbench.**

Add a compact header logout button. Centralize fetch response handling so HTTP 401 or JSON login_required navigates to /login?next= plus the current pathname. Logout posts with the existing CSRF header and then navigates to /login?next=%2F; request errors remain inline.

- [x] **Step 3: Run static page checks.**

Run: rg -n 'alert\\(' internal/web/static/login.html internal/web/static/index.html; expected no matches. Verify role=alert, form labels, and responsive overflow rules are present.

## Task 4: Remove OneBot remnants and pin the UA

**Files:** Modify lottery-bot.env, config.example.env, internal/config/config.go only if needed, README.md, and Makefile.

- [x] **Step 1: Validate the migrated vault before changing the private environment.**

Start a temporary loopback instance using only this checkout's lottery-bot.env, query /api/health with explicit Basic Auth, and confirm the vault opens. Do not echo environment values.

- [x] **Step 2: Remove obsolete private variables and set the exact UA.**

Delete ONEBOT_URL, ONEBOT_TOKEN, TARGET_QQ, and migrated ACCOUNT_* variables from lottery-bot.env. Set USER_AGENT exactly to:

~~~text
Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36
~~~

Preserve the vault key/path, state path, administrator credentials, and platform base URL.

- [x] **Step 3: Keep tracked examples and documentation current.**

Remove active OneBot/QQ names from tracked files; leave only a commented one-time migration note for legacy ACCOUNT_* names. Document /login browser use and explicit Basic Auth for scripts. Make health checks send explicit Basic Auth instead of depending on a challenge.

- [x] **Step 4: Add exact-UA coverage.**

Keep code default, example, and private environment values identical; add or update config and lottery-client tests to assert the exact outbound User-Agent.

## Task 5: Full verification and handoff

**Files:** No further source changes unless a verification failure identifies a concrete regression.

- [x] **Step 1: Search for forbidden remnants.**

Run: rg -n -i 'onebot|target_qq|qq机器人|qq业务|qq_' --glob '!data/**' --glob '!*.sum' .

Expected: no active project code, docs, deployment, or environment references.

- [x] **Step 2: Run complete Go verification.**

Run each command and read its output:

~~~bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
git diff --check
~~~

Every command must exit 0.

- [x] **Step 3: Smoke-test the service from this checkout.**

Use a temporary loopback port and this project's environment. Verify unauthenticated redirect, embedded login page, failed login without platform traffic, successful login to /api/accounts, and logout invalidation. Do not stop a user-owned foreground process or print secret material.

- [x] **Step 4: Report evidence and residual boundaries.**

List changed files and exact verification commands. If real-browser screenshots are unavailable, state that static HTML and HTTP behavior were verified without claiming visual browser validation.
