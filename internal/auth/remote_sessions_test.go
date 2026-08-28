package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
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

	guard := NewCapacityGuard(stubSessionManager{preview: fullPreview}, 50, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); !errors.Is(err, ErrSessionCapacityProtected) {
		t.Fatalf("BeforeLogin() error = %v, want ErrSessionCapacityProtected", err)
	}

	guard = NewCapacityGuard(stubSessionManager{preview: freeablePreview}, 50, 5)
	if err := guard.BeforeLogin(context.Background(), "account-a"); err != nil {
		t.Fatalf("BeforeLogin() with freeable candidates error = %v", err)
	}

	// Below the safety margin nothing blocks the login: threshold = 100-5 = 95.
	guard = NewCapacityGuard(stubSessionManager{preview: fullPreview}, 100, 5)
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
	preview CleanupPreview
}

func (s stubSessionManager) Capability() SessionCapability {
	return s.preview.Capability
}

func (s stubSessionManager) Preview(context.Context, string) (CleanupPreview, error) {
	return s.preview, nil
}
