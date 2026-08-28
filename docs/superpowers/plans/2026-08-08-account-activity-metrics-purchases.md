# Account Activity Metrics and Purchases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-account activity metrics, one-draw purchase, and daily-pass purchase to the existing workbench while preserving server-side authentication reuse and preventing duplicate charges.

**Architecture:** Extend the existing lottery dashboard decoder with activity, lucky-point, purchase, and spend-tier fields. Add a focused service module that sanitizes dashboard data, persists activity snapshots, and owns two idempotent purchase workflows. Expose three account-scoped web actions and render the approved compact three-metric activity strip plus confirmed purchase buttons in the existing single-page UI.

**Tech Stack:** Go 1.x standard library, embedded HTML/CSS/JavaScript, JSON state store, `net/http`, existing Go test suite.

---

### Task 1: Decode Current Dashboard and Purchase Responses

**Files:**
- Modify: `internal/lottery/client.go`
- Modify: `internal/lottery/client_test.go`

- [ ] **Step 1: Write failing decoder tests**

Add test fixtures covering `eligibility.todaySpend`, `eligibility.spendBonusDraws`, `rules.spendTiers`, `lucky`, `purchase`, `drawLimit.unlockCost`, and nested/enveloped dashboard responses. Assert all values survive `Dashboard()` decoding without exposing the access token.

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/lottery -run 'TestDashboard|TestPurchase' -count=1`

Expected: FAIL because the dashboard structs and purchase methods do not yet expose the new fields.

- [ ] **Step 3: Extend the dashboard model and client methods**

Add typed structures for spend tiers, lucky state, purchase state, and unlock price. Extend the recursive dashboard merge logic for both direct and enveloped responses. Add:

```go
PurchaseDraw(context.Context, string, string) (PurchaseOperation, error)
UnlockDrawLimit(context.Context, string, string) (PurchaseOperation, error)
```

Both methods must require a non-empty Lottery Token and idempotency key, send the same Origin/Referer headers as `Draw`, and decode only the operation status/message needed by the service.

- [ ] **Step 4: Run focused tests**

Run: `go test ./internal/lottery -count=1`

Expected: PASS.

### Task 2: Add Sanitized Activity Reports and Purchase Workflows

**Files:**
- Create: `internal/service/activity.go`
- Modify: `internal/service/runner.go`
- Modify: `internal/service/runner_test.go`
- Modify: `internal/state/store.go`
- Modify: `internal/state/store_test.go`

- [ ] **Step 1: Write failing service and state tests**

Cover zero tiers, partial progress, all tiers reached, `$` conversion, lucky points, dynamic prices, snapshot sanitization, successful purchase reconciliation, already-unlocked pass, insufficient balance, authentication recovery, unknown results, and concurrent duplicate clicks.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/service ./internal/state -run 'Activity|Purchase|Unlock' -count=1`

Expected: FAIL because activity reports and purchase action kinds do not exist.

- [ ] **Step 3: Add action kinds and client interface methods**

Add separate state action kinds for draw purchase and pass unlock. Reuse `GetOrCreateAction`, `UpdateAction`, and `LockAction` so each account/day/operation has a persisted idempotency key and an in-process mutex. Preserve unknown actions across restart so the same side effect cannot be submitted again.

- [ ] **Step 4: Implement activity reporting**

Create `ActivityReport` with only display-safe fields from the approved design. `QueryActivity` must call the existing authenticated `Dashboard`, obtain status conversion settings through the parent token path, convert all quota values to dollars, calculate reached tiers and the next-tier difference, then store an `activity` snapshot.

- [ ] **Step 5: Implement purchase reconciliation**

Create `PurchaseDraw` and `UnlockDailyPass`. Each must lock the account/day/action, load or create the persisted idempotency action, refuse a repeated unknown/completed side effect, execute the upstream call once, and query dashboard afterward. Mark completion only when dashboard proves the purchased count increased or the pass is unlocked. Return a sanitized outcome containing status, message, price, remaining draws, and refreshed activity report.

- [ ] **Step 6: Run focused and package tests**

Run: `go test ./internal/service ./internal/state -count=1`

Expected: PASS.

### Task 3: Expose Account-Scoped Activity APIs

**Files:**
- Modify: `internal/web/server.go`
- Modify: `internal/web/server_test.go`

- [ ] **Step 1: Write failing HTTP tests**

Test `POST /api/accounts/{id}/activity`, `/purchase-draw`, and `/unlock-pass`. Assert account scoping, method validation, sanitized JSON, snapshot inclusion in `GET /api/accounts`, correct status messages, and absence of passwords, cookies, tokens, raw quota, and idempotency keys.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/web -run 'Activity|Purchase|Unlock' -count=1`

Expected: FAIL because routes are not registered.

- [ ] **Step 3: Add handlers and account-list snapshot loading**

Extend `handleAccountAction` with `activity`, `purchase-draw`, and `unlock-pass`. Use bounded request contexts, existing safe upstream error handling, and the service outcomes. Load valid `activity` snapshots into each account item without issuing five new dashboard calls during initial page load.

- [ ] **Step 4: Run web tests**

Run: `go test ./internal/web -count=1`

Expected: PASS.

### Task 4: Implement the Approved A Layout and Confirmation Flow

**Files:**
- Modify: `internal/web/static/index.html`
- Modify: `internal/web/server_test.go`

- [ ] **Step 1: Add failing static UI assertions**

Assert the page contains dedicated activity metrics, dynamic purchase labels, separate `data-activity`, `data-purchase-draw`, and `data-unlock-pass` controls, confirmation copy, mobile stacking rules, and no global statistics or credential controls.

- [ ] **Step 2: Run static test and verify failure**

Run: `go test ./internal/web -run 'Index|Static|Workbench' -count=1`

Expected: FAIL because activity markup and handlers are absent.

- [ ] **Step 3: Render activity state and approved layout**

Add a three-column metric strip for today spend, reached spend tiers, and lucky points. Add the next-tier and bonus-draw note plus a separate two-button purchase row. Render loading, stale snapshot, unavailable conversion, all-tiers-complete, pass-unlocked, pending, unknown, and error states without changing the existing subscription/draw-count components.

- [ ] **Step 4: Add user interactions**

Activity refresh calls the account-scoped activity endpoint. Each purchase click uses `window.confirm` with account, item, dynamic `$` price, and same-day validity. Disable the relevant button during requests and for completed/unknown states. Merge returned activity and draw-count data into the current account, render a concrete result, and restore focus to the account status region.

- [ ] **Step 5: Run syntax and web tests**

Run: `go test ./internal/web -count=1`

Run: `node --check =(perl -0ne 'print $1 if m{<script>\s*(.*?)\s*</script>}s' internal/web/static/index.html)`

Expected: both PASS.

### Task 5: Full Verification and Service Restart

**Files:**
- Verify only: all changed production and test files

- [ ] **Step 1: Run the complete quality gate**

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build -o /tmp/lottery-bot-workbench-activity ./cmd/lottery-bot
```

Expected: all commands exit 0.

- [ ] **Step 2: Review sensitive-data boundaries**

Search API fixtures and responses for passwords, cookies, parent tokens, Lottery Tokens, raw idempotency keys, and raw quota fields. Expected: none appear in browser-facing JSON or durable activity snapshots.

- [ ] **Step 3: Run responsive visual verification**

Use a local fake-upstream QA service and the in-app browser at 1440px and 390px. Verify no horizontal overflow, long usernames wrap, the two purchase buttons stack on mobile, confirmations show the dynamic price, and focus returns to the correct account status region. Do not execute real purchases during QA.

- [ ] **Step 4: Restart the local service and perform read-only smoke tests**

Build and restart only the listener owned by this project on `127.0.0.1:18090`. Verify `GET /api/health`, `GET /api/accounts`, and the new UI marker. Do not call real purchase POST endpoints during smoke testing.
