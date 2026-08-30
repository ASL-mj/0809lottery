# 星额台自动任务与性能优化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 0809 多账号工作台更新为“星额台”，把自动抽奖计划扩展为可独立启停的自动任务，并降低首屏请求量、明确会话撤销错误分类。

**Architecture:** 在现有 Go 状态仓库和账号级锁之上扩展任务计划模型，使用统一调度器分发 `draw` 与 `claim`；新增只读本地 bootstrap 接口，前端使用 TTL/in-flight 去重并按需查询远端数据；保留旧计划和旧路由兼容。撤销链路透传上游错误类别，不泄露秘密。

**Tech Stack:** Go 标准库、现有 `internal/state`/`internal/service`/`internal/auth`/`internal/web`、内嵌 HTML/CSS/JavaScript、Go 测试、Makefile。

---

### Task 1: 自动任务状态模型与兼容层

**Files:**
- Modify: `internal/state/schedules.go`
- Modify: `internal/state/store.go`
- Modify: `internal/state/store_test.go`
- Modify: `internal/service/auto_draw.go`
- Create: `internal/service/auto_task.go`
- Test: `internal/service/auto_draw_test.go`, `internal/service/auto_task_test.go`

- [ ] **Step 1: Write failing state tests**

增加测试覆盖：旧计划读取为 `task_type=draw, enabled=true`；新计划可保存 `draw`/`claim` 与 `enabled=false`；随机时间仍拒绝跨零点；同一账号同一天的 `claim` 计划只能生成一个实例。

- [ ] **Step 2: Run focused state tests and verify failure**

Run: `go test ./internal/state ./internal/service -run 'Auto(Task|Draw)|Schedule' -count=1`

Expected: FAIL because `TaskType`/`Enabled` and claim dispatch are not implemented.

- [ ] **Step 3: Implement normalized task fields**

在 `AutoDrawSchedule` 增加 `TaskType` 和 `Enabled`，规范化空值为 `draw`/`true`，保留旧 JSON 字段兼容读取；为计划实例增加可区分任务类型的字段，旧实例默认 `draw`。

- [ ] **Step 4: Run focused tests and verify pass**

Run: `go test ./internal/state ./internal/service -run 'Auto(Task|Draw)|Schedule' -count=1`

Expected: PASS。

- [ ] **Step 5: Commit the state-model slice**

```bash
git add internal/state/schedules.go internal/state/store.go internal/state/store_test.go internal/service/auto_draw.go internal/service/auto_draw_test.go internal/service/auto_task.go internal/service/auto_task_test.go
git commit -m "feat: add typed auto task schedules"
```

### Task 2: 自动任务调度与每日领取

**Files:**
- Modify: `internal/service/auto_draw.go`
- Modify: `internal/service/runner.go`
- Modify: `internal/service/daily_claim.go`
- Modify: `internal/state/store.go`
- Test: `internal/service/auto_task_test.go`, `internal/service/runner_test.go`

- [ ] **Step 1: Write failing dispatch tests**

构造 fake client，验证 `draw` 调用 `DrawAvailableScheduled`，`claim` 调用 `ClaimDaily`；停用任务不生成今日计划；claim 成功后写入 `ActionDailyClaim` 和运行日志，重复 Tick 不重复调用上游。

- [ ] **Step 2: Run dispatch tests and verify failure**

Run: `go test ./internal/service -run 'AutoTask|ClaimDaily' -count=1`

Expected: FAIL because scheduler only invokes draw and has no task-type dispatch.

- [ ] **Step 3: Implement task executor dispatch**

新增任务执行函数和统一结果记录：按计划 `TaskType` 选择抽奖或每日领取；使用现有账号锁、动作幂等和运行日志；`claim` 每日最多一次，认证恢复沿用现有 Broker。

- [ ] **Step 4: Run dispatch and regression tests**

Run: `go test ./internal/service -run 'AutoTask|AutoDraw|ClaimDaily' -count=1`

Expected: PASS。

- [ ] **Step 5: Commit scheduler changes**

```bash
git add internal/service/auto_draw.go internal/service/runner.go internal/service/daily_claim.go internal/state/store.go internal/service/auto_task_test.go internal/service/runner_test.go
git commit -m "feat: schedule daily claim and draw tasks"
```

### Task 3: Web API、bootstrap 与请求观测

**Files:**
- Modify: `internal/web/server.go`
- Modify: `internal/web/accounts.go`
- Create: `internal/web/bootstrap.go`
- Test: `internal/web/server_test.go`, `internal/web/accounts_test.go`, `internal/web/bootstrap_test.go`

- [ ] **Step 1: Write failing bootstrap/API tests**

验证 `GET /api/bootstrap` 只读本地状态、返回任务/快照/有限日志且不调用 fake upstream；验证 `GET|PUT /api/accounts/{id}/auto-tasks` 和 `POST .../toggle`；验证旧 `draw-schedule` 路由仍可读写并映射任务类型。

- [ ] **Step 2: Run focused web tests and verify failure**

Run: `go test ./internal/web -run 'Bootstrap|AutoTask|DrawSchedule' -count=1`

Expected: FAIL because routes and bootstrap handler do not exist.

- [ ] **Step 3: Implement bootstrap and task routes**

增加只读 bootstrap DTO；从 Store 组装脱敏账号、缓存快照、任务计划和最多 20 条日志；任务路由复用现有校验、CSRF、账号状态和错误处理；加入阶段耗时日志字段，禁止写入秘密。

- [ ] **Step 4: Run web regression tests**

Run: `go test ./internal/web -run 'Bootstrap|AutoTask|DrawSchedule|Secret' -count=1`

Expected: PASS。

- [ ] **Step 5: Commit web API slice**

```bash
git add internal/web/server.go internal/web/accounts.go internal/web/bootstrap.go internal/web/server_test.go internal/web/accounts_test.go internal/web/bootstrap_test.go
git commit -m "feat: add local bootstrap and auto task APIs"
```

### Task 4: 会话撤销错误分类

**Files:**
- Modify: `internal/lottery/client.go`
- Modify: `internal/auth/remote_sessions.go`
- Modify: `internal/web/accounts.go`
- Test: `internal/lottery/client_test.go`, `internal/auth/remote_sessions_test.go`, `internal/web/accounts_test.go`

- [ ] **Step 1: Write failing error-mapping tests**

用 httptest upstream 返回 401、403、502、503、504、超时和非法 JSON；验证 Client 保留状态/错误类别，Web 层分别返回 401、502、504 和对应中文提示。

- [ ] **Step 2: Run focused session tests and verify failure**

Run: `go test ./internal/lottery ./internal/auth ./internal/web -run 'Revoke|Session|Gateway|Timeout' -count=1`

Expected: FAIL because `revokeSessions` currently maps every error to the same 502 message.

- [ ] **Step 3: Implement typed upstream error handling**

为 `APIError` 和网络错误提供安全分类函数；`revokeSessions` 根据 401/403、502/503/504、超时、其他状态和协议错误写入不同 HTTP 状态/提示；撤销成功但刷新列表失败时返回结果待确认，不清除未确认账本。

- [ ] **Step 4: Run session regression tests**

Run: `go test ./internal/lottery ./internal/auth ./internal/web -run 'Revoke|Session|Gateway|Timeout|Secret' -count=1`

Expected: PASS。

- [ ] **Step 5: Commit error handling slice**

```bash
git add internal/lottery/client.go internal/lottery/client_test.go internal/auth/remote_sessions.go internal/auth/remote_sessions_test.go internal/web/accounts.go internal/web/accounts_test.go
git commit -m "fix: classify session revoke failures"
```

### Task 5: 品牌和前端性能/UI

**Files:**
- Modify: `internal/web/static/index.html`
- Modify: `internal/web/static/login.html`
- Modify: `README.md`
- Modify: `Makefile`
- Modify: `internal/web/server_test.go`

- [ ] **Step 1: Write static contract assertions**

增加测试断言页面包含“星额台”、SVG Logo、“自动任务”、Switch、`/api/bootstrap`、按需刷新标记，并不再把“自动抽奖计划”作为唯一标题。

- [ ] **Step 2: Implement brand and task UI**

替换页面标题和头部 Logo；沿用原计划列表 CSS/HTML，标题改为“自动任务”；每行增加 `input[type=checkbox]` Switch；设置弹窗新增任务类型、启用状态和时间字段；新增签到领取任务的渲染和开关事件。

- [ ] **Step 3: Implement bootstrap-first loading and dedupe**

页面启动请求 `/api/bootstrap`，先渲染当前账号；为按需接口添加 30 秒 TTL 和 in-flight Promise Map；切换账号只并行请求当前账号数据；可见性恢复只刷新过期项；移除首屏 `loadDrawHistories()` 和重复日志/计划串行链。

- [ ] **Step 4: Run frontend static checks**

Run: `node --check <(awk '/<script>/{inside=1; next} /<\\/script>/{inside=0} inside' internal/web/static/index.html)` and the same command for `login.html`.

Expected: both exit 0。

- [ ] **Step 5: Commit UI/performance slice**

```bash
git add internal/web/static/index.html internal/web/static/login.html README.md Makefile internal/web/server_test.go
git commit -m "feat: rebrand workbench and optimize task loading"
```

### Task 6: 集成验证与本地验收

**Files:**
- Modify: `docs/superpowers/specs/2026-08-30-star-amount-auto-tasks-performance-design.md` only if implementation clarifies a contract
- Modify: `README.md` if local startup wording changes

- [ ] **Step 1: Run complete Go verification**

Run: `gofmt -l $(rg --files -g '*.go')`, `go test ./... -count=1`, `go vet ./...`, `make build`, `git diff --check`.

Expected: no format output; all tests pass; vet/build exit 0; diff check clean。

- [ ] **Step 2: Start local foreground service**

Run from the project root using only the project environment: `make run`.

Expected: service listens on the configured loopback address and remains in the foreground. Do not use another checkout or environment file.

- [ ] **Step 3: Capture local request waterfall**

In the in-app browser, record first open, browser refresh and account switch. Verify first response is `/api/bootstrap`, no per-account draw-history requests happen before an account is selected, and record request count/elapsed times.

- [ ] **Step 4: Exercise UI acceptance**

Verify “星额台” branding, original plan-row layout, Switch toggles, task-type dialog, subscriptions refresh, session management, and distinct revoke errors using local test data or safe mocked responses.

- [ ] **Step 5: Stop at user acceptance**

Leave the local service available for the user. Do not run `git push`, production deployment, remote session cleanup, or external configuration changes until explicit acceptance.

