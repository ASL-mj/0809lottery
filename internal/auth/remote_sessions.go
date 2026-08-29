package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
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
	Sessions          []SessionPreviewItem `json:"sessions,omitempty"`
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

// BeforeLogin decides whether one more platform session may be created. At or
// above the safe threshold it first frees confirmed workbench-owned sessions
// (never the current, pinned or unknown ones) and re-checks; if capacity still
// cannot be proven the login is blocked. When capacity cannot be verified at
// all (dead token or network failure) the login proceeds — blocking it would
// leave the account with no recovery path.
func (g *CapacityGuard) BeforeLogin(ctx context.Context, accountID string) error {
	if g == nil || g.manager == nil {
		return nil
	}
	preview, err := g.manager.Preview(ctx, accountID)
	if err != nil {
		return nil
	}
	if preview.Capability == SessionUnsupported || !preview.TotalKnownSet {
		return nil
	}
	threshold := g.limit - g.safetyMargin
	if preview.TotalKnown < threshold {
		return nil
	}
	if cleaner, ok := g.manager.(SessionCleaner); ok && preview.CandidateCount > 0 {
		if _, err := cleaner.Cleanup(ctx, accountID); err == nil {
			if second, secondErr := g.manager.Preview(ctx, accountID); secondErr == nil && second.TotalKnown < threshold {
				return nil
			}
		}
	}
	return ErrSessionCapacityProtected
}


// SessionPreviewItem is one live platform session with the workbench's
// keep/candidate verdict. These are the account owner's own sessions; the
// raw user-agent string stays server-side and only a parsed device summary
// is exposed alongside the login method and IP.
type SessionPreviewItem struct {
	SID            string    `json:"sid"`
	Current        bool      `json:"current"`
	WorkbenchOwned bool      `json:"workbench_owned"`
	Verdict        string    `json:"verdict"`
	LoginMethod    string    `json:"login_method,omitempty"`
	IP             string    `json:"ip,omitempty"`
	Device         string    `json:"device,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	LastActiveAt   time.Time `json:"last_active_at"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
}

// CleanupResult reports which workbench sessions were revoked.
type CleanupResult struct {
	Revoked []string          `json:"revoked"`
	Failed  []FailedRevocation `json:"failed,omitempty"`
}

type FailedRevocation struct {
	SID    string `json:"sid"`
	Reason string `json:"reason"`
}

// SessionCleaner is implemented by managers that can revoke sessions.
type SessionCleaner interface {
	Cleanup(ctx context.Context, accountID string) (CleanupResult, error)
}

// PlatformSessionManager implements RemoteSessionManager against the verified
// /api/user/sessions contract. Only sessions the workbench ledger can prove
// it created are ever cleanup candidates; the current session, pinned
// sessions and unknown devices are never touched. Callers that need
// serialization must hold the account's auth lock themselves — the broker's
// capacity hook invokes this manager while already holding it.
type PlatformSessionManager struct {
	vault        secret.Vault
	newClient    ClientFactory
	durableLimit int
	workbenchUA  string
	now          func() time.Time
}

// NewPlatformSessionManager builds the live manager. workbenchUA is the
// User-Agent the workbench's own client sends; sessions logged in from the
// platform UI or other devices carry browser UAs instead.
func NewPlatformSessionManager(vault secret.Vault, factory ClientFactory, durableLimit int, workbenchUA string) *PlatformSessionManager {
	if durableLimit <= 0 {
		durableLimit = 1
	}
	return &PlatformSessionManager{
		vault:        vault,
		newClient:    factory,
		durableLimit: durableLimit,
		workbenchUA:  strings.TrimSpace(workbenchUA),
		now:          time.Now,
	}
}

func (m *PlatformSessionManager) Capability() SessionCapability {
	return SessionRevocable
}

// Preview lists the live sessions and classifies each one.
func (m *PlatformSessionManager) Preview(ctx context.Context, accountID string) (CleanupPreview, error) {
	bundle, client, err := m.load(ctx, accountID)
	if err != nil {
		return CleanupPreview{}, err
	}
	sessions, err := client.Sessions(ctx, bundle.ParentAccessToken)
	if err != nil {
		return CleanupPreview{}, err
	}
	current := lottery.SessionIDFromAccessToken(bundle.ParentAccessToken)
	preview := m.classify(bundle, sessions, current)
	return preview, nil
}

func (m *PlatformSessionManager) classify(bundle secret.Bundle, sessions []lottery.SessionInfo, currentSID string) CleanupPreview {
	now := m.now().UTC()
	ledger := make(map[string]secret.ManagedSession, len(bundle.ManagedSessions))
	for _, item := range bundle.ManagedSessions {
		if item.Origin == secret.SessionOriginWorkbench && strings.TrimSpace(item.RemoteID) != "" {
			ledger[item.RemoteID] = item
		}
	}

	live := make([]lottery.SessionInfo, 0, len(sessions))
	// A ledger entry only stays a candidate while the platform still lists it.
	candidatePool := make([]secret.ManagedSession, 0, len(ledger))
	items := make([]SessionPreviewItem, 0, len(sessions))
	for _, session := range sessions {
		owned := false
		if entry, ok := ledger[session.SID]; ok {
			owned = true
			entry.LastSeenAt = session.LastActive
			ledger[session.SID] = entry
			if !session.Current && !entry.Pinned {
				candidatePool = append(candidatePool, entry)
			}
		}
		isCurrent := session.Current || (currentSID != "" && session.SID == currentSID)
		live = append(live, session)
		device := lottery.DescribeUserAgent(session.UserAgent)
		if m.workbenchUA != "" && strings.Contains(session.UserAgent, m.workbenchUA) {
			device = "工作台"
		}
		items = append(items, SessionPreviewItem{
			SID:            session.SID,
			Current:        isCurrent,
			WorkbenchOwned: owned,
			LoginMethod:    session.LoginMethod,
			IP:             session.IP,
			Device:         device,
			CreatedAt:      session.CreatedAt,
			LastActiveAt:   session.LastActive,
			ExpiresAt:      session.ExpiresAt,
		})
	}
	sort.Slice(candidatePool, func(left, right int) bool {
		return candidatePool[left].LastSeenAt.After(candidatePool[right].LastSeenAt)
	})

	candidates := make([]CleanupCandidate, 0)
	itemsBySID := make(map[string]*SessionPreviewItem, len(items))
	for index := range items {
		itemsBySID[items[index].SID] = &items[index]
	}
	kept := 0
	for _, entry := range candidatePool {
		if kept < m.durableLimit {
			kept++
			if item, ok := itemsBySID[entry.RemoteID]; ok {
				item.Verdict = "keep"
			}
			continue
		}
		reason := "已超出工作台保留的持久会话数"
		if entry.LastSeenAt.Before(now.Add(-7 * 24 * time.Hour)) {
			reason = "长期未使用的工作台会话"
		}
		candidates = append(candidates, CleanupCandidate{RemoteID: entry.RemoteID, LastSeenAt: entry.LastSeenAt, Reason: reason})
		if item, ok := itemsBySID[entry.RemoteID]; ok {
			item.Verdict = "candidate"
		}
	}
	for index := range items {
		if items[index].Verdict == "" {
			items[index].Verdict = "keep"
		}
	}

	preview := CleanupPreview{
		Capability:     m.Capability(),
		Candidates:     candidates,
		CandidateCount: len(candidates),
		KeepCount:      len(live) - len(candidates),
		EstimatedFree:  len(candidates),
		TotalKnown:     len(live),
		TotalKnownSet:  true,
		OwnedCount:     len(ledger),
		GeneratedAt:    now,
		Sessions:       items,
	}
	return preview
}

// Cleanup revokes every current cleanup candidate and prunes the ledger.
func (m *PlatformSessionManager) Cleanup(ctx context.Context, accountID string) (CleanupResult, error) {
	result := CleanupResult{Revoked: []string{}, Failed: []FailedRevocation{}}
	bundle, err := m.vault.Load(ctx, accountID)
	if errors.Is(err, secret.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	client, err := m.newClient(cookiesToState(bundle.Cookies))
	if err != nil {
		return result, err
	}
	sessions, err := client.Sessions(ctx, bundle.ParentAccessToken)
	if err != nil {
		return result, err
	}
	current := lottery.SessionIDFromAccessToken(bundle.ParentAccessToken)
	liveBySID := make(map[string]lottery.SessionInfo, len(sessions))
	for _, session := range sessions {
		liveBySID[session.SID] = session
	}
	ledger := make(map[string]secret.ManagedSession, len(bundle.ManagedSessions))
	for _, item := range bundle.ManagedSessions {
		if item.Origin == secret.SessionOriginWorkbench && strings.TrimSpace(item.RemoteID) != "" {
			ledger[item.RemoteID] = item
		}
	}

	candidatePool := make([]secret.ManagedSession, 0)
	for sid, entry := range ledger {
		live, ok := liveBySID[sid]
		if !ok {
			// Session is gone platform-side; drop it from the ledger.
			delete(ledger, sid)
			continue
		}
		if live.Current || sid == current || entry.Pinned {
			continue
		}
		candidatePool = append(candidatePool, entry)
	}
	sort.Slice(candidatePool, func(left, right int) bool {
		return candidatePool[left].LastSeenAt.After(candidatePool[right].LastSeenAt)
	})
	// Keep the durable budget as spare sessions; revoke only the excess.
	keep := m.durableLimit
	if keep > len(candidatePool) {
		keep = len(candidatePool)
	}
	toRevoke := candidatePool[keep:]

	for _, entry := range toRevoke {
		if err := client.RevokeSession(ctx, bundle.ParentAccessToken, entry.RemoteID); err != nil {
			result.Failed = append(result.Failed, FailedRevocation{SID: entry.RemoteID, Reason: safeReason(err)})
			continue
		}
		result.Revoked = append(result.Revoked, entry.RemoteID)
		delete(ledger, entry.RemoteID)
	}
	if len(result.Revoked) > 0 {
		kept := bundle.ManagedSessions[:0]
		for _, item := range bundle.ManagedSessions {
			if _, revoked := ledger[item.RemoteID]; revoked || item.Origin != secret.SessionOriginWorkbench {
				kept = append(kept, item)
			}
		}
		bundle.ManagedSessions = kept
		if err := m.vault.Save(ctx, accountID, bundle); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (m *PlatformSessionManager) load(ctx context.Context, accountID string) (secret.Bundle, PlatformClient, error) {
	bundle, err := m.vault.Load(ctx, accountID)
	if err != nil {
		return secret.Bundle{}, nil, err
	}
	client, err := m.newClient(cookiesToState(bundle.Cookies))
	if err != nil {
		return secret.Bundle{}, nil, fmt.Errorf("%w: create platform client: %v", ErrAuthUnavailable, err)
	}
	return bundle, client, nil
}

func safeReason(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 120 {
		return message[:120]
	}
	return message
}
