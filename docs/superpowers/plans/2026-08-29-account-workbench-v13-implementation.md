# Account Workbench v13 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将已确认的 v13 账号卡片原型同步到真实工作台，保持现有账号管理、认证、订阅、签到、抽奖、购买和自动抽奖接口行为不变。

**Architecture:** 仅调整单文件前端的 HTML 片段生成函数与 CSS。每个指标组继续复用现有状态对象和 API 事件委托；账号操作统一进入一排主操作栏，购买操作独立成一排，自动抽奖计划与有效订阅在卡片底部左右分栏。后端接口与状态字段不新增。

**Tech Stack:** Go 服务、内嵌单文件 HTML/CSS/Vanilla JavaScript、现有 `go test` 测试套件。

---

### Task 1: Lock current behavior and inspect render contracts

**Files:**
- Test: `internal/web/server_test.go`
- Modify: `internal/web/static/index.html`

- [ ] **Step 1: Run the current verification baseline**

Run `go test ./...`, `go vet ./...`, and `git diff --check`. Record failures before editing; do not alter unrelated dirty files.

- [ ] **Step 2: Confirm front-end contracts**

Keep the existing `data-*` attributes and handlers for refresh, check-in, claim, draw, draw-count, activity, token usage, purchase, account management, schedule add/remove, and subscription refresh. The implementation must continue calling the current endpoints.

### Task 2: Implement the v13 account card layout

**Files:**
- Modify: `internal/web/static/index.html` (CSS and render functions around `renderBalance`, `renderTokenUsage`, `renderDrawCount`, `renderActivity`, `renderAccount`)

- [ ] **Step 1: Replace metric internals with compact two-row pairs**

Each of the four `.metric-group` blocks must use a title/refresh header, a two-value `.metric-pair`, and one `.metric-subline`. Balance shows used/remaining plus request count; today's usage shows tokens/consumed USD plus call count; draw count shows available/locked plus today's used; activity shows today's spend/tier plus lucky points.

- [ ] **Step 2: Make the four metric groups equal height**

Use a four-column grid with `align-items: stretch` and make each group a flex column. Preserve responsive two-column and one-column breakpoints.

- [ ] **Step 3: Add independent metric refresh icon buttons**

Use the existing refresh icon and current event attributes (`data-balance`, `data-token-usage`, `data-draw-count`, `data-activity`) in each metric header. Buttons remain keyboard accessible with `aria-label` and `title`.

- [ ] **Step 4: Consolidate account controls**

Render manual draw, check-in, claim, edit, auth check, re-auth, session management, and delete in one icon-bearing `.account__actions` row. Keep purchase draw/pass buttons in a separate purchase row below it, preserving existing purchase confirmation and pending-state behavior.

- [ ] **Step 5: Move check-in reward to a dedicated status line**

Keep `checkin_quota_awarded` out of the metric groups. Render the confirmed USD reward as an independent green status marker and retain the full result message in the account footer.

- [ ] **Step 6: Split plans and subscriptions at the bottom**

Render configurable schedules in the left column with a visible “设置自动抽奖” control. Render the subscription list in the right column with its refresh action in the section header using the existing `data-refresh` handler. Do not reintroduce fixed three-node timeline assumptions.

### Task 3: Verify rendering and regressions

**Files:**
- Modify: `internal/web/static/index.html` only if verification exposes a defect.

- [ ] **Step 1: Run static checks**

Run `git diff --check`; extract the inline script and run `node --check` against it when available.

- [ ] **Step 2: Run Go checks**

Run `go test ./...` and `go vet ./...` from `/Users/Zhuanz1/Desktop/privateProjects/make-money/lottery-bot`.

- [ ] **Step 3: Smoke-test the real page**

Start or reuse the local Go service using the repository's Makefile/environment conventions, open the real workbench in the in-app browser, and verify the four equal-height metric cards, action row icons, purchase row, status reward, schedule/subscription split, and refresh controls. Report any browser validation boundary explicitly.
