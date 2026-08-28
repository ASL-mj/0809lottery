# Account Workbench Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace fixed environment-variable accounts and implicit password-login recovery with a user-managed account registry, encrypted SecretVault, session-safe authentication, and traceable dollar-amount calculations while preserving every existing per-account action.

**Architecture:** Keep the existing Go HTTP service, JSON business-state store, embedded page, idempotent actions and scheduler. Add focused `account`, `secret`, `auth`, and `quota` packages; refactor services to acquire tokens through `SessionBroker` instead of reading `config.Account` and `state.AuthState`. Remote session cleanup is capability-gated: this plan ships the safe `unsupported` implementation and does not guess a destructive 0809 endpoint.

**Tech Stack:** Go 1.23 standard library (`crypto/aes`, `cipher`, `math/big`, `net/http`), existing JSON state store, embedded HTML/CSS/JavaScript, `net/http/httptest`, Go race detector. No third-party dependency is required.

---

## Preconditions

1. Work from `/Users/Zhuanz1/Desktop/privateProjects/make-money/lottery-bot`. This checkout has no `.git`; retain command evidence and changed-file lists instead of creating commits here.
2. Before migration, provide a protected base64-encoded 32-byte `LOTTERY_VAULT_KEY`. Do not put account passwords in the new environment file.
3. A public 0809 asset scan did not verify a session-list, session-revoke, or logout API. A live revoke adapter is outside this plan until a reviewed contract records the exact endpoint, stable session ID, current-session semantics, failure modes and idempotency rules.
4. Migration may not modify version-3 state until every SecretVault write has been reread successfully.

## File Map

| File | Responsibility |
| --- | --- |
| `internal/account/record.go` | Account metadata, validation, masking and public auth-health model. |
| `internal/account/repository.go` | Repository interface used by services and scheduler. |
| `internal/secret/vault.go` | Secret bundle and vault contract. |
| `internal/secret/file_vault.go` | AES-256-GCM encrypted Vault, atomic write and permissions. |
| `internal/auth/broker.go` | Reuse/validate/refresh/explicit-login state machine. |
| `internal/auth/remote_sessions.go` | Capability, preview and capacity-protection policy. |
| `internal/quota/money.go` | Exact money DTO based on `math/big.Rat`. |
| `internal/quota/policy.go` | Versioned raw-quota-to-USD conversion rules. |
| `internal/state/accounts.go` | Version-4 account persistence and account-scoped cleanup. |
| `internal/state/migration.go` | Safe version-3 import. |
| `internal/web/accounts.go` | Account/auth/session HTTP handlers. |
| `internal/web/csrf.go` | CSRF and same-origin middleware. |

Existing files to modify: `internal/config/config.go`, `cmd/lottery-bot/main.go`, `internal/lottery/client.go`, all relevant `internal/service/*.go`, `internal/state/store.go`, `internal/web/server.go`, `internal/web/static/index.html`, tests, `README.md`, and `config.example.env`.

### Task 1: Freeze Existing Behavior with Regression Probes

**Files:**
- Modify: `internal/service/runner_test.go`
- Modify: `internal/web/server_test.go`
- Modify: `internal/state/store_test.go`
- Create: `internal/auth/broker_test.go`

- [x] **Step 1: Run the current baseline**

Run:

```bash
go test ./internal/config ./internal/lottery ./internal/service ./internal/state ./internal/web -count=1
```

Expected: pass, or report the known environment listener restriction rather than a product regression.

- [x] **Step 2: Add failing tests for the non-negotiable boundaries**

Extend the existing fake client with `loginCalls`, `refreshCalls`, and `userSelfCalls`. Add a background action test and a public-response test:

```go
if client.loginCalls != 0 {
    t.Fatalf("background action logged in %d times", client.loginCalls)
}
if strings.Contains(body, "parent_access_token") || strings.Contains(body, "cookie") {
    t.Fatalf("public response leaked authentication data: %s", body)
}
```

- [x] **Step 3: Verify the new tests are red**

Run:

```bash
go test ./internal/auth ./internal/service ./internal/web -run 'NoImplicitLogin|NoSecretLeak' -count=1
```

Expected: fail because `SessionBroker` does not exist and Runner can still fall back to password login.

### Task 2: Introduce SecretVault and Vault-Aware Configuration

**Files:**
- Create: `internal/secret/vault.go`
- Create: `internal/secret/file_vault.go`
- Create: `internal/secret/file_vault_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.env`
- Modify: `cmd/lottery-bot/main.go`

- [x] **Step 1: Write failing Vault tests**

Cover round trip, wrong key, invalid key, missing key, file mode and cleartext absence:

```go
func TestFileVaultDoesNotPersistCleartext(t *testing.T) {
    vault := newTestVault(t)
    bundle := secret.Bundle{LoginName: "user@example.test", Password: "test-password"}
    requireNoError(t, vault.Save(context.Background(), "account-a", bundle))
    data := mustReadFile(t, vault.Path())
    if bytes.Contains(data, []byte(bundle.LoginName)) || bytes.Contains(data, []byte(bundle.Password)) {
        t.Fatal("vault persisted cleartext secret")
    }
}
```

- [x] **Step 2: Run the red tests**

Run:

```bash
go test ./internal/secret ./internal/config -count=1
```

Expected: fail because the package and Vault configuration are absent.

- [x] **Step 3: Implement the Vault contract and encrypted file adapter**

Define the only secret-bearing model:

```go
type Bundle struct {
    LoginName string
    Password string
    UserID int64
    ParentAccessToken string
    ParentAccessExpiresAt time.Time
    LotteryAccessToken string
    LotteryAccessExpiresAt time.Time
    Cookies []Cookie
    ManagedSessions []ManagedSession
}

// Cookie and ManagedSession belong to package secret so the encrypted bundle
// does not import state or auth and create a package cycle.
type Cookie struct {
    Name string
    Value string
    Path string
    Domain string
    Expires time.Time
    Secure bool
    HTTPOnly bool
}

type SessionOrigin string

const SessionOriginWorkbench SessionOrigin = "workbench"

type ManagedSession struct {
    RemoteID string
    Origin SessionOrigin
    Pinned bool
    LastSeenAt time.Time
}

type Vault interface {
    Load(context.Context, string) (Bundle, error)
    Save(context.Context, string, Bundle) error
    Delete(context.Context, string) error
}
```

Use a fresh random nonce for every AES-256-GCM write, atomic temp-file rename, directory mode `0700`, file mode `0600`, and an exact 32-byte decoded master key. Never log a Bundle, key or encryption failure detail containing a value.

- [x] **Step 4: Refactor runtime configuration**

Replace five account fields with these runtime values:

```go
type Config struct {
    BaseURL string
    StatePath string
    VaultPath string
    VaultKey []byte
    UserAgent string
    WebAddr string
    WebUser string
    WebPass string
    SessionLimit int
    SessionSafetyMargin int
    DurableSessionLimit int
}
```

Default session values are `50`, `5`, `2`; reject invalid combinations. Keep `LoadLegacyAccounts(getenv)` separate and callable only by migration.

- [x] **Step 5: Add explicit CLI modes and update the example**

Support exactly `lottery-bot serve` and `lottery-bot migrate`. `serve` requires a Vault key; `migrate` requires that key plus legacy credentials. Replace `ACCOUNT_A` through `ACCOUNT_E` in `config.example.env` with secure Vault-key setup instructions and no credential samples.

- [x] **Step 6: Verify Vault and config behavior**

Run:

```bash
go test ./internal/secret ./internal/config ./cmd/lottery-bot -count=1
```

Expected: pass.

### Task 3: Add Dynamic Accounts and Transactional Version-3 Migration

**Files:**
- Create: `internal/account/record.go`
- Create: `internal/account/repository.go`
- Create: `internal/account/record_test.go`
- Create: `internal/state/accounts.go`
- Create: `internal/state/migration.go`
- Create: `internal/state/migration_test.go`
- Modify: `internal/state/store.go`
- Modify: `internal/state/store_test.go`
- Modify: `cmd/lottery-bot/main.go`

- [x] **Step 1: Write failing registry and migration tests**

Cover create, update, enable/disable, delete, account masking, duplicate remote user ID, account-scoped cleanup and migration rollback:

```go
func TestMigrateV3DoesNotMutateStateWhenVaultWriteFails(t *testing.T) {
    path := writeV3Fixture(t)
    before := mustReadFile(t, path)
    err := state.MigrateV3(context.Background(), path, failingVault{}, legacyAccounts())
    if err == nil { t.Fatal("MigrateV3() error = nil") }
    if after := mustReadFile(t, path); !bytes.Equal(before, after) {
        t.Fatal("migration changed state after Vault failure")
    }
}
```

- [x] **Step 2: Run the red tests**

Run:

```bash
go test ./internal/account ./internal/state -run 'Account|Migrate' -count=1
```

Expected: fail because account metadata and migration APIs do not exist.

- [x] **Step 3: Implement account records and repository operations**

Implement `List`, `Get`, `Create`, `Update`, `SetRemoteUserID`, `Delete`, and `ListEnabled`. Use `crypto/rand` IDs for new accounts; preserve `account-a` through `account-e` only in migration. Persist `masked_login_name`, never raw login name.

- [x] **Step 4: Move state to version 4 without secrets**

Replace persisted `Accounts map[string]AuthState` with account records and public auth health. Preserve actions, snapshots, plans and logs. Add an account-scoped removal method that removes only the selected account's actions, snapshots, plans and logs after disabling it.

- [x] **Step 5: Implement `lottery-bot migrate`**

Acquire the existing state-file lock, decode v3 in memory, import legacy login/password from environment plus existing tokens/Cookie/user ID into Vault, reread every entry, then atomically write v4 state. Refuse to proceed if any legacy secret is missing; do not force a new login. A v4 state returns an idempotent “no migration needed” result.

- [x] **Step 6: Verify registry and migration**

Run:

```bash
go test ./internal/account ./internal/state -count=1
```

Expected: pass.

### Task 4: Build SessionBroker and Eliminate Implicit Login

**Files:**
- Create: `internal/auth/broker.go`
- Create: `internal/auth/broker_test.go`
- Modify: `internal/lottery/client.go`
- Modify: `internal/lottery/client_test.go`
- Modify: `internal/service/runner.go`
- Modify: `internal/service/runner_test.go`
- Modify: `internal/state/store.go`

- [x] **Step 1: Write the auth state-machine tests**

Create these exact cases with fake Vault and platform client:

```go
func TestAcquireReadOnlyDoesNotLoginAfterRefreshForbidden(t *testing.T) {}
func TestAcquireExplicitReauthenticateLogsInOnce(t *testing.T) {}
func TestAcquireDoesNotLoginAfterTimeoutOrServerError(t *testing.T) {}
func TestAcquireSerializesConcurrentRefreshes(t *testing.T) {}
func TestRenewAfterLotteryUnauthorizedRetriesOnce(t *testing.T) {}
```

- [x] **Step 2: Verify the tests fail before implementation**

Run:

```bash
go test ./internal/auth -count=1
```

Expected: fail because Broker does not exist.

- [x] **Step 3: Decouple platform login from `config.Account`**

Replace `Login(ctx, config.Account)` in `internal/lottery/client.go` with:

```go
type Credentials struct {
    Username string
    Password string
}
```

Keep existing HTTP request creation, response decoding and cookie tracking. Test with synthetic credentials only.

- [x] **Step 4: Implement SessionBroker**

Define `ReadOnly`, `SideEffect`, `ScheduledAutomation`, and `ExplicitReauthenticate` intents. Inside `state.Store.LockAuth(accountID)`, reload Vault state; reuse a valid token, otherwise call `UserSelf`, then refresh once. Only `ExplicitReauthenticate` may call `Login`. Map timeout, 5xx, 429 and parse failures to `AuthUnavailable`; map explicit refresh rejection to `AuthReauthRequired`.

- [x] **Step 5: Refactor Runner through Broker**

Remove direct `state.Auth` reads and `ensureParentTokenAfter`/`ensureLotteryToken` login branches. Runner requests a parent or lottery session from Broker, and calls a Broker renewal method once after upstream `401/403`. Keep existing action locks, idempotency keys and purchase reconciliation unchanged.

- [x] **Step 6: Verify auth and service regressions**

Run:

```bash
go test ./internal/auth ./internal/lottery ./internal/service -count=1
```

Expected: pass.

### Task 5: Add Safe Session Capability and Capacity Protection

**Files:**
- Create: `internal/auth/remote_sessions.go`
- Create: `internal/auth/remote_sessions_test.go`
- Modify: `internal/secret/vault.go`
- Modify: `internal/auth/broker.go`

- [x] **Step 1: Write policy tests**

```go
func TestUnsupportedSessionManagerNeverOffersDeletion(t *testing.T) {
    manager := auth.NewUnsupportedSessionManager()
    preview, err := manager.Preview(context.Background(), "account-a")
    if err != nil {
        t.Fatalf("Preview() error = %v", err)
    }
    if preview.Capability != auth.SessionUnsupported || preview.CandidateCount != 0 {
        t.Fatalf("unsafe preview: %#v", preview)
    }
}

func TestCleanupPolicyKeepsCurrentAndTwoDurableOwnedSessions(t *testing.T) {}
```

- [x] **Step 2: Run the red tests**

Run:

```bash
go test ./internal/auth -run 'Session|Cleanup' -count=1
```

Expected: fail because capability and policy types do not exist.

- [x] **Step 3: Implement the non-destructive capability layer**

Implement `RemoteSessionManager`, `NewUnsupportedSessionManager`, `SessionCapability`, `CleanupPreview`, `CleanupPolicy` and a 60-second preview registry. `Preview(context.Context, accountID)` returns `(CleanupPreview, error)`; the unsupported manager returns `SessionUnsupported` with zero candidates and no upstream call. Policy considers only `secret.ManagedSession` entries marked `Origin == secret.SessionOriginWorkbench` with a confirmed remote ID. It never selects the currently authenticated session, `Pinned` sessions or unknown sessions.

- [x] **Step 4: Enforce capacity protection only in explicit login**

Before `ExplicitReauthenticate` calls `Login`, obtain a preview. For readable/revocable managers at or above `SessionLimit - SessionSafetyMargin`, return `SessionCapacityProtected` unless a confirmed preview can safely free owned sessions. For `unsupported`, the web route needs a second explicit confirmation. Background intents never reach login or cleanup code.

- [x] **Step 5: Do not add a guessed remote revoke request**

Leave `internal/lottery/client.go` without a session-revoke URL. Create a separate follow-up plan only after the platform contract is captured and reviewed; its fixtures must prove current and unknown sessions are never revoked.

- [x] **Step 6: Verify capability tests**

Run:

```bash
go test ./internal/auth -count=1
```

Expected: pass.

### Task 6: Move Existing Services and Scheduler to Dynamic Accounts

**Files:**
- Modify: `internal/service/runner.go`
- Modify: `internal/service/subscriptions.go`
- Modify: `internal/service/draw_count.go`
- Modify: `internal/service/daily_claim.go`
- Modify: `internal/service/manual_draw.go`
- Modify: `internal/service/purchase.go`
- Modify: `internal/service/activity.go`
- Modify: `internal/service/auto_draw.go`
- Modify: `internal/service/runner_test.go`
- Modify: `internal/service/auto_draw_test.go`

- [x] **Step 1: Add scheduler red tests for enabled state and auth failure**

```go
func TestSchedulerPlansOnlyEnabledAccounts(t *testing.T) {}
func TestSchedulerSkipsReauthRequiredWithoutLogin(t *testing.T) {}
```

Expected behavior: only enabled account IDs receive plans; `AuthReauthRequired` produces a persisted skipped plan with no fake `Login` call.

- [x] **Step 2: Run the red tests**

Run:

```bash
go test ./internal/service -run 'DynamicAccount|Scheduler.*Reauth' -count=1
```

Expected: fail because scheduler still captures `cfg.Accounts`.

- [x] **Step 3: Resolve accounts through Repository**

Change Runner construction to take `account.Repository` and `auth.Broker`. Every action rejects missing/disabled accounts before acquiring a session. Preserve existing per-account action locks and persisted idempotency records.

- [x] **Step 4: Refactor AutoDrawScheduler**

Use `repository.ListEnabled()` inside `ensurePlans` rather than a fixed account slice. Skip a persisted plan if its account becomes disabled/deleted. Execute through `ScheduledAutomation`; map `AuthReauthRequired` to the safe skipped message and do not retry or log in.

- [x] **Step 5: Verify all service regressions**

Run:

```bash
go test ./internal/service -count=1
```

Expected: pass for subscription, check-in, daily claim, manual draw, purchases and scheduler behavior.

### Task 7: Implement Exact, Traceable Quota Values

**Files:**
- Create: `internal/quota/money.go`
- Create: `internal/quota/policy.go`
- Create: `internal/quota/policy_test.go`
- Modify: `internal/service/quota.go`
- Modify: `internal/service/subscriptions.go`
- Modify: `internal/service/activity.go`
- Modify: `internal/service/manual_draw.go`
- Modify: `internal/state/store.go`
- Modify: `internal/web/server.go`
- Modify: `internal/service/runner_test.go`
- Modify: `internal/web/server_test.go`

- [x] **Step 1: Write exact conversion tests**

```go
func TestQuotaPerUnitPolicyKeepsExactRemainder(t *testing.T) {
    amount, err := quota.ParseNative("1000001")
    if err != nil {
        t.Fatalf("ParseNative() error = %v", err)
    }
    policy, err := quota.NewQuotaPerUnitPolicy("500000")
    if err != nil {
        t.Fatalf("NewQuotaPerUnitPolicy() error = %v", err)
    }
    money := policy.Convert(amount, quota.Provenance{
        Source: "subscription.amount_total",
        ObservedAt: time.Unix(0, 0).UTC(),
    })
    if got, want := money.Value, "2.000002"; got != want {
        t.Fatalf("Value = %s, want %s", got, want)
    }
}
```

Also cover already-USD, missing unit configuration, negative remaining clamped to zero, historical snapshots and unverified JSON values.

- [x] **Step 2: Run the red quota tests**

Run:

```bash
go test ./internal/quota ./internal/service -run 'Quota|Money' -count=1
```

Expected: fail because exact amount and policy packages do not exist.

- [x] **Step 3: Implement `Money` and versioned policies**

Use `math/big.Rat` internally and a string at the JSON boundary:

```go
type Money struct {
    Currency string `json:"currency"`
    Value string `json:"value,omitempty"`
    Display string `json:"display,omitempty"`
    State State `json:"state"`
    Source string `json:"source"`
    Formula string `json:"formula,omitempty"`
    ObservedAt time.Time `json:"observed_at"`
}
```

Define `ParseNative(string) (NativeAmount, error)`, `NewQuotaPerUnitPolicy(string) (QuotaPerUnitPolicy, error)` and `QuotaPerUnitPolicy.Convert(NativeAmount, Provenance) Money`; the parser rejects blank, fractional and negative native amounts, and the policy constructor rejects zero or invalid units. Define `Provenance` with `Source` and `ObservedAt`, so every conversion test and persisted amount has an explicit origin. Implement `already-usd-v1` and `quota-per-unit-v1`. Save `quota_display_type`, `quota_per_unit`, `usd_exchange_rate` and custom-currency fields as snapshots; do not use exchange-rate fields until a reviewed policy explicitly authorizes them.

- [x] **Step 4: Migrate persisted and service money DTOs**

Replace persisted money `*float64` fields for purchase prices, check-in reward, draw delta, runtime logs and activity metrics with Money snapshots. Legacy floating-point records render as `unverified`; they are never silently reinterpreted under new rules.

- [x] **Step 5: Update safe APIs and page formatting**

Return Money objects for subscriptions, activity, check-in, draw and purchase. JavaScript only reads `display` for `confirmed` values; it never converts raw quota. Keep each source separate instead of adding balance, user usage, rewards and consumption.

- [x] **Step 6: Verify quota and action tests**

Run:

```bash
go test ./internal/quota ./internal/service ./internal/state ./internal/web -count=1
```

Expected: pass.

### Task 8: Expose Account/Auth/Session APIs and CSRF Protection

**Files:**
- Create: `internal/web/accounts.go`
- Create: `internal/web/accounts_test.go`
- Create: `internal/web/csrf.go`
- Create: `internal/web/csrf_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/server_test.go`

- [x] **Step 1: Write handler tests before routing changes**

Cover Basic Auth, CSRF, same-origin, create/edit/credentials, validate, explicit reauthentication, unsupported preview, delete, disabled action and secret redaction:

```go
func TestCreateAccountNeverReturnsCredentials(t *testing.T) {
    response := doJSON(t, handler, http.MethodPost, "/api/accounts", validCreateBody())
    if strings.Contains(response.Body.String(), "test-password") || strings.Contains(response.Body.String(), "test@example.test") {
        t.Fatal("credential leak")
    }
}
```

- [x] **Step 2: Run the red web tests**

Run:

```bash
go test ./internal/web -run 'Account|CSRF|SessionPreview' -count=1
```

Expected: fail because routes and middleware are absent.

- [x] **Step 3: Split account handlers from server bootstrap**

Move account-list and account-action routing from `server.go` into `accounts.go`. Keep `Server` as composition root for Store, Repository, Vault, Broker and Scheduler. Preserve all existing business endpoint paths.

- [x] **Step 4: Add CSRF and explicit DTOs**

Set a random same-site CSRF cookie on `GET /`; require matching `X-CSRF-Token` plus same-origin `Origin` or `Referer` for POST/PATCH/PUT/DELETE. Limit credential request bodies to 8 KiB. Serialize explicit response structs only; never marshal Vault, platform client or domain secrets.

- [x] **Step 5: Implement management routes**

Wire CRUD to Repository/Vault, validation to Broker with `ReadOnly`, reauthentication to Broker with `ExplicitReauthenticate`, and cleanup preview to the capability manager. Delete requires `confirmation == "DELETE"`, disables first, then removes only the selected account's state.

- [x] **Step 6: Verify web tests**

Run:

```bash
go test ./internal/web -count=1
```

Expected: pass.

### Task 9: Implement the Account-Management UI and Release Verification

**Files:**
- Modify: `internal/web/static/index.html`
- Modify: `internal/web/server_test.go`
- Modify: `README.md`
- Modify: `config.example.env`

- [x] **Step 1: Add failing static assertions**

```go
for _, marker := range []string{
    "data-add-account", "data-edit-account", "data-validate-auth",
    "data-reauthenticate", "data-session-preview", "data-delete-account",
} {
    if !bytes.Contains(indexHTML, []byte(marker)) { t.Fatalf("missing %s", marker) }
}
```

- [x] **Step 2: Implement dialogs and state rendering**

Keep the current embedded single-page file. Add native dialogs for create/edit, explicit reauthentication, cleanup preview and deletion. Clear password input values after every request. Extend the shared request helper to send the CSRF header, handle typed errors and insert only escaped text. Do not add all-account refresh.

- [x] **Step 3: Render session and money states**

Render badges and disabled controls for every auth state. `formatMoney` uses `money.display` only for `confirmed`; it renders the explicit unverified/unavailable messages otherwise. Keep existing check-in, claim, draw, activity and purchase controls in their card-local groups and preserve their endpoint paths.

- [x] **Step 4: Update operational documentation**

Document Vault-key setup, `lottery-bot migrate`, dynamic account management, explicit reauthentication, session-cleanup limitation, CSRF behavior and migration recovery. Remove all instructions to maintain five `ACCOUNT_*` credentials during normal service operation.

- [x] **Step 5: Run complete automated verification**

Run:

```bash
go test ./... -count=1
go vet ./...
go test -race ./internal/account ./internal/secret ./internal/auth ./internal/service ./internal/state ./internal/web -count=1
```

Expected: all commands pass. If the environment prohibits `httptest` listeners, report that exact restriction and run the unaffected package suites; do not classify it as a product failure.

- [ ] **Step 6: Perform controlled manual acceptance** *(requires a live
      non-production or explicitly authorized 0809 account; left for the
      operator — no platform access from this refactor session)*

With a non-production or explicitly authorized account, confirm: adding an account does not call login; refresh does not call login; refresh rejection requires reauthentication; automatic draw skips instead of logging in; UI/API/logs contain no secret; a recorded redacted platform example matches the selected quota policy. Do not issue any remote session revoke call.

## Coverage Review

| Requirement | Tasks |
| --- | --- |
| Dynamic user-managed accounts | 2, 3, 8, 9 |
| Token/session reuse without repeated login | 1, 4, 6 |
| Session-capacity protection | 5, 8, 9 |
| Exact, traceable USD amounts | 7, 9 |
| Existing actions and scheduler | 4, 6, 7, 8, 9 |
| Sensitive-data boundary | 2, 3, 4, 8, 9 |

Actual remote session revoke is intentionally deferred to a separately reviewed platform-contract implementation. This plan provides the safe fallback, capacity-protection framework and UI/API surface without guessing a destructive platform endpoint.
