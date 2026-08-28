package auth

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"skyeapi/lottery-bot/internal/secret"
)

// ErrSessionCapacityProtected blocks a new password login when the platform
// is at or above its session limit and no confirmed workbench session can be
// freed safely.
var ErrSessionCapacityProtected = errors.New("session capacity protection blocked the new login")

type SessionCapability string

const (
	// SessionUnsupported means no verified platform session-list or revoke
	// contract exists. The workbench must never pretend to clean up.
	SessionUnsupported SessionCapability = "unsupported"
	// SessionReadable means the platform exposes a verified session list.
	SessionReadable SessionCapability = "readable"
	// SessionRevocable means sessions can also be revoked through a verified
	// contract.
	SessionRevocable SessionCapability = "revocable"
)

type CleanupCandidate struct {
	RemoteID   string    `json:"remote_id"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Reason     string    `json:"reason"`
}

type CleanupPreview struct {
	Capability        SessionCapability  `json:"capability"`
	Candidates        []CleanupCandidate `json:"candidates"`
	CandidateCount    int                `json:"candidate_count"`
	KeepCount         int                `json:"keep_count"`
	EstimatedFree     int                `json:"estimated_free"`
	TotalKnown        int                `json:"total_known"`
	OwnedCount        int                `json:"owned_count"`
	TotalKnownSet     bool               `json:"total_known_set"`
	UnavailableReason string             `json:"unavailable_reason,omitempty"`
	GeneratedAt       time.Time          `json:"generated_at"`
}

// RemoteSessionManager describes what the workbench can safely know and do
// about remote sessions for one account.
type RemoteSessionManager interface {
	Capability() SessionCapability
	Preview(ctx context.Context, accountID string) (CleanupPreview, error)
}

// unsupportedSessionManager is the safe default while no platform session
// contract has been verified. It never contacts the platform.
type unsupportedSessionManager struct{}

func NewUnsupportedSessionManager() RemoteSessionManager {
	return unsupportedSessionManager{}
}

func (unsupportedSessionManager) Capability() SessionCapability {
	return SessionUnsupported
}

func (unsupportedSessionManager) Preview(_ context.Context, _ string) (CleanupPreview, error) {
	return CleanupPreview{
		Capability:        SessionUnsupported,
		Candidates:        []CleanupCandidate{},
		UnavailableReason: "平台会话查询与撤销接口尚未确认，远端会话清理不可用",
		GeneratedAt:       time.Now().UTC(),
	}, nil
}

// CleanupPolicy turns the workbench's own managed-session ledger into cleanup
// candidates. Only confirmed workbench-created sessions qualify; the currently
// authenticated session, pinned durable sessions and unknown sessions are
// always kept. DurableSessionLimit most recently used owned sessions stay.
type CleanupPolicy struct {
	DurableSessionLimit int
	Now                 func() time.Time
}

func (p CleanupPolicy) SelectCleanupCandidates(sessions []secret.ManagedSession, currentRemoteID string) []CleanupCandidate {
	limit := p.DurableSessionLimit
	if limit <= 0 {
		limit = 1
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}

	owned := make([]secret.ManagedSession, 0, len(sessions))
	for _, item := range sessions {
		if item.Origin != secret.SessionOriginWorkbench {
			continue
		}
		if item.RemoteID == "" || item.RemoteID == currentRemoteID || item.Pinned {
			continue
		}
		owned = append(owned, item)
	}
	sort.Slice(owned, func(left, right int) bool {
		return owned[left].LastSeenAt.After(owned[right].LastSeenAt)
	})

	candidates := make([]CleanupCandidate, 0, len(owned))
	for index, item := range owned {
		if index < limit {
			continue
		}
		reason := "已超出工作台保留的持久会话数"
		if item.LastSeenAt.Before(now.Add(-7 * 24 * time.Hour)) {
			reason = "长期未使用的工作台会话"
		}
		candidates = append(candidates, CleanupCandidate{
			RemoteID:   item.RemoteID,
			LastSeenAt: item.LastSeenAt,
			Reason:     reason,
		})
	}
	// Oldest sessions are cleaned up first.
	for left, right := 0, len(candidates)-1; left < right; left, right = left+1, right-1 {
		candidates[left], candidates[right] = candidates[right], candidates[left]
	}
	return candidates
}

// previewRegistry caches cleanup previews for 60 seconds so repeated explicit
// logins do not hammer a session-list endpoint.
type previewRegistry struct {
	mu      sync.Mutex
	entries map[string]previewEntry
	now     func() time.Time
}

const previewTTL = 60 * time.Second

type previewEntry struct {
	preview   CleanupPreview
	expiresAt time.Time
}

func newPreviewRegistry() *previewRegistry {
	return &previewRegistry{
		entries: make(map[string]previewEntry),
		now:     time.Now,
	}
}

func (r *previewRegistry) Get(accountID string) (CleanupPreview, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[accountID]
	if !ok {
		return CleanupPreview{}, false
	}
	if r.now().After(entry.expiresAt) {
		delete(r.entries, accountID)
		return CleanupPreview{}, false
	}
	return entry.preview, true
}

func (r *previewRegistry) Put(accountID string, preview CleanupPreview) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[accountID] = previewEntry{
		preview:   preview,
		expiresAt: r.now().Add(previewTTL),
	}
}

// CapacityGuard enforces the session-count budget immediately before an
// explicit password login.
type CapacityGuard struct {
	manager      RemoteSessionManager
	limit        int
	safetyMargin int
	previews     *previewRegistry
}

func NewCapacityGuard(manager RemoteSessionManager, sessionLimit, safetyMargin int) *CapacityGuard {
	return &CapacityGuard{
		manager:      manager,
		limit:        sessionLimit,
		safetyMargin: safetyMargin,
		previews:     newPreviewRegistry(),
	}
}

func (g *CapacityGuard) Manager() RemoteSessionManager {
	return g.manager
}

// Preview returns the cached or freshly generated cleanup preview.
func (g *CapacityGuard) Preview(ctx context.Context, accountID string) (CleanupPreview, error) {
	if preview, ok := g.previews.Get(accountID); ok {
		return preview, nil
	}
	preview, err := g.manager.Preview(ctx, accountID)
	if err != nil {
		return CleanupPreview{}, err
	}
	g.previews.Put(accountID, preview)
	return preview, nil
}

// BeforeLogin decides whether one more platform session may be created. A
// readable/revocable manager at or above the safe threshold requires a
// confirmed freeable workbench session; the unsupported manager defers to the
// web route's second explicit confirmation.
func (g *CapacityGuard) BeforeLogin(ctx context.Context, accountID string) error {
	if g == nil || g.manager == nil {
		return nil
	}
	preview, err := g.Preview(ctx, accountID)
	if err != nil {
		return err
	}
	if preview.Capability == SessionUnsupported {
		return nil
	}
	if !preview.TotalKnownSet {
		return nil
	}
	if preview.TotalKnown < g.limit-g.safetyMargin {
		return nil
	}
	if preview.CandidateCount > 0 {
		return nil
	}
	return ErrSessionCapacityProtected
}
