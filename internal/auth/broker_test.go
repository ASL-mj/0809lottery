package auth

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/state"
)

var brokerTestNow = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)

type fakeVault struct {
	mu      sync.Mutex
	bundles map[string]secret.Bundle
	saveErr error
}

func (f *fakeVault) Load(_ context.Context, accountID string) (secret.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bundle, ok := f.bundles[accountID]
	if !ok {
		return secret.Bundle{}, secret.ErrNotFound
	}
	return bundle, nil
}

func (f *fakeVault) Save(_ context.Context, accountID string, bundle secret.Bundle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.bundles[accountID] = bundle
	return nil
}

func (f *fakeVault) Delete(_ context.Context, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bundles, accountID)
	return nil
}

type fakePlatform struct {
	mu             sync.Mutex
	userSelfCalls  int
	refreshCalls   int
	loginCalls     int
	bridgeCalls    int
	userSelfErrs   []error
	refreshResults []lottery.LoginResult
	refreshErrs    []error
	loginResult    lottery.LoginResult
	loginErr       error
	bridgeResults  []lottery.BridgeResult
	bridgeErrs     []error
	cookies        []state.Cookie
	refreshEntered chan struct{}
	refreshRelease <-chan struct{}
}

func (f *fakePlatform) UserSelf(_ context.Context, _ string) (lottery.UserUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userSelfCalls++
	index := f.userSelfCalls - 1
	if index < len(f.userSelfErrs) && f.userSelfErrs[index] != nil {
		return lottery.UserUsage{}, f.userSelfErrs[index]
	}
	return lottery.UserUsage{}, nil
}

func (f *fakePlatform) Refresh(_ context.Context) (lottery.LoginResult, error) {
	f.mu.Lock()
	f.refreshCalls++
	index := f.refreshCalls - 1
	entered := f.refreshEntered
	release := f.refreshRelease
	var err error
	var result lottery.LoginResult
	switch {
	case index < len(f.refreshErrs) && f.refreshErrs[index] != nil:
		err = f.refreshErrs[index]
	case index < len(f.refreshResults):
		result = f.refreshResults[index]
	default:
		err = &lottery.APIError{StatusCode: http.StatusUnauthorized}
	}
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return lottery.LoginResult{}, err
	}
	return result, nil
}

func (f *fakePlatform) Login(_ context.Context, _ lottery.Credentials) (lottery.LoginResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls++
	if f.loginErr != nil {
		return lottery.LoginResult{}, f.loginErr
	}
	return f.loginResult, nil
}

func (f *fakePlatform) Bridge(_ context.Context, _ string, _ int64) (lottery.BridgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridgeCalls++
	index := f.bridgeCalls - 1
	if index < len(f.bridgeErrs) && f.bridgeErrs[index] != nil {
		return lottery.BridgeResult{}, f.bridgeErrs[index]
	}
	if index < len(f.bridgeResults) {
		return f.bridgeResults[index], nil
	}
	return lottery.BridgeResult{}, &lottery.APIError{StatusCode: http.StatusUnauthorized}
}

func (f *fakePlatform) Cookies() []state.Cookie {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]state.Cookie(nil), f.cookies...)
}

type brokerHarness struct {
	broker   *Broker
	vault    *fakeVault
	platform *fakePlatform
	store    *state.Store
}

func newBrokerHarness(t *testing.T, bundle secret.Bundle) *brokerHarness {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	vault := &fakeVault{bundles: map[string]secret.Bundle{"account-a": bundle}}
	platform := &fakePlatform{}
	broker := NewBroker(store, vault, func([]state.Cookie) (PlatformClient, error) { return platform, nil })
	broker.now = func() time.Time { return brokerTestNow }
	return &brokerHarness{broker: broker, vault: vault, platform: platform, store: store}
}

func expiredParentBundle() secret.Bundle {
	return secret.Bundle{
		UserID:                 7,
		ParentAccessToken:      "expired-parent",
		ParentAccessExpiresAt:  brokerTestNow.Add(-time.Hour),
		LotteryAccessToken:     "stale-lottery",
		LotteryAccessExpiresAt: brokerTestNow.Add(-time.Hour),
		Cookies: []secret.Cookie{{
			Name:  "new_api_refresh",
			Value: "refresh-cookie",
		}},
	}
}

// A read-only acquire whose refresh is explicitly forbidden by the platform
// must surface a reauthentication requirement instead of creating a new
// password session.
func TestAcquireReadOnlyDoesNotLoginAfterRefreshForbidden(t *testing.T) {
	harness := newBrokerHarness(t, expiredParentBundle())
	harness.platform.userSelfErrs = []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}}
	harness.platform.refreshErrs = []error{&lottery.APIError{StatusCode: http.StatusForbidden}}

	for _, kind := range []SessionKind{SessionParent, SessionLottery} {
		if _, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, kind); !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("Acquire(%s) error = %v, want ErrReauthRequired", kind, err)
		}
	}
	if harness.platform.loginCalls != 0 {
		t.Fatalf("Acquire logged in %d times", harness.platform.loginCalls)
	}
}

// Explicit reauthentication is the only intent allowed to consume a password
// login, and the persisted session must be reused afterwards.
func TestAcquireExplicitReauthenticateLogsInOnce(t *testing.T) {
	harness := newBrokerHarness(t, secret.Bundle{LoginName: "a@example.test", Password: "test-password"})
	harness.platform.refreshErrs = []error{&lottery.APIError{StatusCode: http.StatusForbidden}}
	harness.platform.loginResult = lottery.LoginResult{
		UserID:          9,
		AccessToken:     "fresh-parent",
		AccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
	}

	session, err := harness.broker.Acquire(context.Background(), "account-a", ExplicitReauthenticate, SessionParent)
	if err != nil {
		t.Fatalf("Acquire(ExplicitReauthenticate) error = %v", err)
	}
	if session.Token != "fresh-parent" || harness.platform.loginCalls != 1 {
		t.Fatalf("session = %q login calls = %d, want fresh-parent/1", session.Token, harness.platform.loginCalls)
	}

	reused, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, SessionParent)
	if err != nil || reused.Token != "fresh-parent" {
		t.Fatalf("second Acquire() = %q, %v", reused.Token, err)
	}
	if harness.platform.loginCalls != 1 {
		t.Fatalf("second Acquire logged in again: %d times", harness.platform.loginCalls)
	}
	bundle, err := harness.vault.Load(context.Background(), "account-a")
	if err != nil || bundle.ParentAccessToken != "fresh-parent" || bundle.UserID != 9 {
		t.Fatalf("persisted bundle = %#v, %v", bundle, err)
	}
}

// Timeout, server errors and unknown failures must map to AuthUnavailable and
// never degrade into a password login.
func TestAcquireDoesNotLoginAfterTimeoutOrServerError(t *testing.T) {
	t.Run("refresh server error", func(t *testing.T) {
		harness := newBrokerHarness(t, secret.Bundle{})
		harness.platform.refreshErrs = []error{&lottery.APIError{StatusCode: http.StatusInternalServerError}}
		if _, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, SessionParent); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrAuthUnavailable", err)
		}
		if harness.platform.loginCalls != 0 {
			t.Fatalf("server error logged in %d times", harness.platform.loginCalls)
		}
	})
	t.Run("refresh timeout", func(t *testing.T) {
		harness := newBrokerHarness(t, secret.Bundle{})
		harness.platform.refreshErrs = []error{context.DeadlineExceeded}
		if _, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, SessionParent); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrAuthUnavailable", err)
		}
		if harness.platform.loginCalls != 0 {
			t.Fatalf("timeout logged in %d times", harness.platform.loginCalls)
		}
	})
	t.Run("validation server error", func(t *testing.T) {
		harness := newBrokerHarness(t, expiredParentBundle())
		harness.platform.userSelfErrs = []error{&lottery.APIError{StatusCode: http.StatusBadGateway}}
		if _, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, SessionParent); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrAuthUnavailable", err)
		}
		if harness.platform.refreshCalls != 0 || harness.platform.loginCalls != 0 {
			t.Fatalf("validation failure called refresh=%d login=%d", harness.platform.refreshCalls, harness.platform.loginCalls)
		}
	})
}

// Concurrent acquires for one account must serialize and reuse the session
// persisted by the first request instead of refreshing twice.
func TestAcquireSerializesConcurrentRefreshes(t *testing.T) {
	harness := newBrokerHarness(t, expiredParentBundle())
	// Validation must fail so both requests fall through to the refresh path.
	harness.platform.userSelfErrs = []error{&lottery.APIError{StatusCode: http.StatusUnauthorized}}
	release := make(chan struct{})
	harness.platform.refreshEntered = make(chan struct{}, 1)
	harness.platform.refreshRelease = release
	harness.platform.refreshResults = []lottery.LoginResult{{
		UserID:          3,
		AccessToken:     "refreshed-parent",
		AccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
	}}

	type acquireResult struct {
		token string
		err   error
	}
	results := make(chan acquireResult, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := harness.broker.Acquire(context.Background(), "account-a", ReadOnly, SessionParent)
			results <- acquireResult{token: session.Token, err: err}
		}()
	}

	select {
	case <-harness.platform.refreshEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh never started")
	}
	close(release)
	wg.Wait()
	close(results)

	seen := 0
	for result := range results {
		seen++
		if result.err != nil {
			t.Fatalf("Acquire() error = %v", result.err)
		}
		if result.token != "refreshed-parent" {
			t.Fatalf("Acquire() token = %q", result.token)
		}
	}
	if seen != 2 {
		t.Fatalf("expected 2 acquire results, got %d", seen)
	}
	if harness.platform.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", harness.platform.refreshCalls)
	}
	if harness.platform.loginCalls != 0 {
		t.Fatalf("concurrent acquire logged in %d times", harness.platform.loginCalls)
	}
}

// After an upstream 401/403 the broker renews the lottery session exactly once
// through the bridge and persists the replacement token.
func TestRenewAfterLotteryUnauthorizedRetriesOnce(t *testing.T) {
	bundle := expiredParentBundle()
	bundle.ParentAccessToken = "valid-parent"
	bundle.ParentAccessExpiresAt = brokerTestNow.Add(2 * time.Hour)
	bundle.LotteryAccessToken = "current-lottery"
	bundle.LotteryAccessExpiresAt = brokerTestNow.Add(time.Hour)
	harness := newBrokerHarness(t, bundle)
	harness.platform.bridgeResults = []lottery.BridgeResult{{
		AccessToken: "bridged-lottery",
		ExpiresAt:   brokerTestNow.Add(3 * time.Hour),
	}}

	first, err := harness.broker.Acquire(context.Background(), "account-a", SideEffect, SessionLottery)
	if err != nil || first.Token != "current-lottery" {
		t.Fatalf("Acquire(SideEffect) = %q, %v", first.Token, err)
	}

	renewed, err := harness.broker.RenewLottery(context.Background(), "account-a", "current-lottery")
	if err != nil {
		t.Fatalf("RenewLottery() error = %v", err)
	}
	if renewed.Token != "bridged-lottery" {
		t.Fatalf("RenewLottery() = %q, want bridged-lottery", renewed.Token)
	}
	if harness.platform.bridgeCalls != 1 || harness.platform.loginCalls != 0 {
		t.Fatalf("renewal calls bridge=%d login=%d, want 1/0", harness.platform.bridgeCalls, harness.platform.loginCalls)
	}
	saved, err := harness.vault.Load(context.Background(), "account-a")
	if err != nil || saved.LotteryAccessToken != "bridged-lottery" {
		t.Fatalf("persisted lottery token = %#v, %v", saved.LotteryAccessToken, err)
	}
}
