package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/state"
)

// Until the platform exposes a verified session-list contract the manager must
// refuse to offer any deletion candidate and must not call the platform.
func TestUnsupportedSessionManagerNeverOffersDeletion(t *testing.T) {
	manager := NewUnsupportedSessionManager()
	preview, err := manager.Preview(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if preview.Capability != SessionUnsupported || preview.CandidateCount != 0 {
		t.Fatalf("unsafe preview: %#v", preview)
	}
	if preview.UnavailableReason == "" {
		t.Fatal("unsupported preview must explain why cleanup is unavailable")
	}
}

func TestCleanupPolicyKeepsCurrentAndTwoDurableOwnedSessions(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	sessions := []secret.ManagedSession{
		{RemoteID: "s-oldest", Origin: secret.SessionOriginWorkbench, LastSeenAt: base.Add(-4 * time.Hour)},
		{RemoteID: "s-old", Origin: secret.SessionOriginWorkbench, LastSeenAt: base.Add(-3 * time.Hour)},
		{RemoteID: "s-recent", Origin: secret.SessionOriginWorkbench, LastSeenAt: base.Add(-2 * time.Hour)},
		{RemoteID: "s-newest", Origin: secret.SessionOriginWorkbench, LastSeenAt: base.Add(-1 * time.Hour)},
		{RemoteID: "s-pinned", Origin: secret.SessionOriginWorkbench, Pinned: true, LastSeenAt: base.Add(-5 * time.Hour)},
		{RemoteID: "s-current", Origin: secret.SessionOriginWorkbench, LastSeenAt: base},
		// Unknown sessions are never cleanup candidates.
		{RemoteID: "", Origin: secret.SessionOriginWorkbench, LastSeenAt: base.Add(-6 * time.Hour)},
		{RemoteID: "s-user-device", Origin: secret.SessionOrigin("user"), LastSeenAt: base.Add(-30 * time.Minute)},
	}

	policy := CleanupPolicy{DurableSessionLimit: 2, Now: func() time.Time { return base }}
	candidates := policy.SelectCleanupCandidates(sessions, "s-current")

	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v, want the two oldest owned sessions", candidates)
	}
	if candidates[0].RemoteID != "s-oldest" || candidates[1].RemoteID != "s-old" {
		t.Fatalf("candidates ordered oldest first: %#v", candidates)
	}
	for _, kept := range []string{"s-current", "s-pinned", "s-newest", "s-recent", "s-user-device"} {
		for _, candidate := range candidates {
			if candidate.RemoteID == kept {
				t.Fatalf("session %s must never be a cleanup candidate", kept)
			}
		}
	}
}

func TestPreviewRegistryExpiresEntriesAfterSixtySeconds(t *testing.T) {
	registry := newPreviewRegistry()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clock := now
	registry.now = func() time.Time { return clock }

	registry.Put("account-a", CleanupPreview{Capability: SessionUnsupported})
	if _, ok := registry.Get("account-a"); !ok {
		t.Fatal("fresh preview missing")
	}
	clock = now.Add(59 * time.Second)
	if _, ok := registry.Get("account-a"); !ok {
		t.Fatal("preview expired before 60 seconds")
	}
	clock = now.Add(61 * time.Second)
	if _, ok := registry.Get("account-a"); ok {
		t.Fatal("stale preview survived its 60-second lifetime")
	}
}

func TestCapacityGuardBlocksLoginWithoutFreeableSessions(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fullPreview := CleanupPreview{
		Capability:     SessionRevocable,
		TotalKnown:     50,
		OwnedCount:     4,
		CandidateCount: 0,
		TotalKnownSet:  true,
		GeneratedAt:    base,
	}
	freeablePreview := fullPreview
	freeablePreview.Candidates = []CleanupCandidate{{RemoteID: "s-old", Reason: "oldest owned session"}}
	freeablePreview.CandidateCount = 1
	freeablePreview.EstimatedFree = 1

	guard := NewCapacityGuard(&stubSessionManager{preview: fullPreview}, 50, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); !errors.Is(err, ErrSessionCapacityProtected) {
		t.Fatalf("BeforeLogin() error = %v, want ErrSessionCapacityProtected", err)
	}

	guard = NewCapacityGuard(&stubSessionManager{preview: freeablePreview, afterCleanup: CleanupPreview{
		Capability: SessionRevocable, TotalKnown: 44, TotalKnownSet: true,
	}}, 50, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); err != nil {
		t.Fatalf("BeforeLogin() with freeable candidates error = %v", err)
	}

	// Below the safety margin nothing blocks the login: threshold = 100-5 = 95.
	guard = NewCapacityGuard(&stubSessionManager{preview: fullPreview}, 100, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); err != nil {
		t.Fatalf("BeforeLogin() below threshold error = %v", err)
	}

	// The unsupported capability never blocks; the web route requires the
	// second explicit confirmation instead.
	guard = NewCapacityGuard(NewUnsupportedSessionManager(), 50, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); err != nil {
		t.Fatalf("BeforeLogin() with unsupported manager error = %v", err)
	}
}

func TestExplicitLoginHonorsCapacityGuard(t *testing.T) {
	harness := newBrokerHarness(t, secret.Bundle{LoginName: "a@example.test", Password: "test-password"})
	harness.platform.loginResult = lottery.LoginResult{
		UserID:          9,
		AccessToken:     "fresh-parent",
		AccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
	}
	calls := 0
	harness.broker.SetCapacityGuard(func(context.Context, string) error {
		calls++
		return ErrSessionCapacityProtected
	})

	if _, err := harness.broker.Acquire(context.Background(), "account-a", ExplicitReauthenticate, SessionParent); !errors.Is(err, ErrSessionCapacityProtected) {
		t.Fatalf("Acquire() error = %v, want ErrSessionCapacityProtected", err)
	}
	if harness.platform.loginCalls != 0 {
		t.Fatalf("guarded login still called Login %d times", harness.platform.loginCalls)
	}
	if calls != 1 {
		t.Fatalf("capacity guard calls = %d, want 1", calls)
	}
}

type stubSessionManager struct {
	preview      CleanupPreview
	afterCleanup CleanupPreview
	cleanups     int
}

func (s *stubSessionManager) Capability() SessionCapability {
	return s.preview.Capability
}

func (s *stubSessionManager) Preview(context.Context, string) (CleanupPreview, error) {
	return s.preview, nil
}

func (s *stubSessionManager) Cleanup(context.Context, string) (CleanupResult, error) {
	s.cleanups++
	if s.afterCleanup.Capability != "" {
		s.preview = s.afterCleanup
	}
	return CleanupResult{Revoked: []string{"s-old"}}, nil
}

func testJWTWithSID(sid string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"` + sid + `","iss":"new-api"}`))
	return "header." + payload + ".signature"
}

// The manager must only offer workbench-owned sessions as candidates; the
// current session and unknown devices are always kept.
func TestPlatformSessionManagerPreviewClassifiesSessions(t *testing.T) {
	harness := newBrokerHarness(t, secret.Bundle{
		UserID:                7,
		ParentAccessToken:     testJWTWithSID("sid-current"),
		ParentAccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
		ManagedSessions: []secret.ManagedSession{
			{RemoteID: "sid-old", Origin: secret.SessionOriginWorkbench, LastSeenAt: brokerTestNow.Add(-8 * time.Hour)},
			{RemoteID: "sid-recent", Origin: secret.SessionOriginWorkbench, LastSeenAt: brokerTestNow.Add(-time.Hour)},
			{RemoteID: "sid-pinned", Origin: secret.SessionOriginWorkbench, Pinned: true, LastSeenAt: brokerTestNow.Add(-9 * time.Hour)},
		},
	})
	harness.platform.sessionsResult = []lottery.SessionInfo{
		{SID: "sid-current", Current: true, LastActive: brokerTestNow},
		{SID: "sid-old", LastActive: brokerTestNow.Add(-8 * time.Hour)},
		{SID: "sid-recent", LastActive: brokerTestNow.Add(-time.Hour)},
		{SID: "sid-pinned", LastActive: brokerTestNow.Add(-9 * time.Hour)},
		{SID: "sid-user-phone", LastActive: brokerTestNow.Add(-30 * time.Minute)},
	}

	manager := NewPlatformSessionManager(harness.vault, func([]state.Cookie) (PlatformClient, error) {
		return harness.platform, nil
	}, 1)
	preview, err := manager.Preview(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if preview.Capability != SessionRevocable || preview.TotalKnown != 5 || !preview.TotalKnownSet {
		t.Fatalf("preview header = %#v", preview)
	}
	if preview.CandidateCount != 1 || preview.Candidates[0].RemoteID != "sid-old" {
		t.Fatalf("candidates = %#v, want only sid-old", preview.Candidates)
	}
	for _, item := range preview.Sessions {
		if item.Verdict == "" {
			t.Fatalf("session %s has an empty verdict: %#v", item.SID, item)
		}
		if item.SID == "sid-current" && (item.Verdict == "candidate" || !item.Current) {
			t.Fatalf("current session misclassified: %#v", item)
		}
		if item.SID == "sid-user-phone" && item.Verdict == "candidate" {
			t.Fatalf("unknown device became a candidate: %#v", item)
		}
		if item.SID == "sid-pinned" && item.Verdict == "candidate" {
			t.Fatalf("pinned session became a candidate: %#v", item)
		}
	}
}

// Cleanup revokes exactly the candidates and prunes the ledger.
func TestPlatformSessionManagerCleanupRevokesOnlyOwned(t *testing.T) {
	harness := newBrokerHarness(t, secret.Bundle{
		UserID:                7,
		ParentAccessToken:     testJWTWithSID("sid-current"),
		ParentAccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
		ManagedSessions: []secret.ManagedSession{
			{RemoteID: "sid-old", Origin: secret.SessionOriginWorkbench, LastSeenAt: brokerTestNow.Add(-8 * time.Hour)},
			{RemoteID: "sid-recent", Origin: secret.SessionOriginWorkbench, LastSeenAt: brokerTestNow.Add(-time.Hour)},
		},
	})
	harness.platform.sessionsResult = []lottery.SessionInfo{
		{SID: "sid-current", Current: true, LastActive: brokerTestNow},
		{SID: "sid-old", LastActive: brokerTestNow.Add(-8 * time.Hour)},
		{SID: "sid-recent", LastActive: brokerTestNow.Add(-time.Hour)},
		{SID: "sid-user-phone", LastActive: brokerTestNow.Add(-30 * time.Minute)},
	}
	manager := NewPlatformSessionManager(harness.vault, func([]state.Cookie) (PlatformClient, error) {
		return harness.platform, nil
	}, 1)

	result, err := manager.Cleanup(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if len(result.Revoked) != 1 || result.Revoked[0] != "sid-old" {
		t.Fatalf("Cleanup() revoked = %#v, want [sid-old]", result.Revoked)
	}
	if harness.platform.revokeCalls != 1 {
		t.Fatalf("revoke calls = %d, want 1", harness.platform.revokeCalls)
	}
	bundle, err := harness.vault.Load(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("vault load: %v", err)
	}
	for _, item := range bundle.ManagedSessions {
		if item.RemoteID == "sid-old" {
			t.Fatal("revoked session stayed in the ledger")
		}
	}
}

// An explicit login records the new platform session in the ledger.
func TestBrokerRecordsManagedSessionAfterLogin(t *testing.T) {
	harness := newBrokerHarness(t, secret.Bundle{LoginName: "a@example.test", Password: "test-password"})
	harness.platform.refreshErrs = []error{&lottery.APIError{StatusCode: http.StatusForbidden}}
	harness.platform.loginResult = lottery.LoginResult{
		UserID:          9,
		AccessToken:     testJWTWithSID("sid-new-login"),
		AccessExpiresAt: brokerTestNow.Add(2 * time.Hour),
	}

	if _, err := harness.broker.Acquire(context.Background(), "account-a", ExplicitReauthenticate, SessionParent); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	bundle, err := harness.vault.Load(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("vault load: %v", err)
	}
	found := false
	for _, item := range bundle.ManagedSessions {
		if item.RemoteID == "sid-new-login" && item.Origin == secret.SessionOriginWorkbench {
			found = true
		}
	}
	if !found {
		t.Fatalf("new session was not recorded: %#v", bundle.ManagedSessions)
	}
}
