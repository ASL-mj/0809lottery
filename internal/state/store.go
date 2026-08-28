package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/quota"
)

// version is the newest state version written by this build. Version-3 files
// stay untouched until `lottery-bot migrate` upgrades them to version 4.
const version = 4

type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

type AuthState struct {
	UserID                 int64     `json:"user_id,omitempty"`
	ParentAccessToken      string    `json:"parent_access_token,omitempty"`
	ParentAccessExpiresAt  time.Time `json:"parent_access_expires_at,omitempty"`
	LotteryAccessToken     string    `json:"lottery_access_token,omitempty"`
	LotteryAccessExpiresAt time.Time `json:"lottery_access_expires_at,omitempty"`
	Cookies                []Cookie  `json:"cookies,omitempty"`
	UpdatedAt              time.Time `json:"updated_at,omitempty"`
}

type ActionKind string

const (
	ActionCheckin      ActionKind = "checkin"
	ActionDailyClaim   ActionKind = "daily_claim"
	ActionDrawPurchase ActionKind = "draw_purchase"
	ActionPassUnlock   ActionKind = "pass_unlock"
)

type ActionStatus string

const (
	ActionPending   ActionStatus = "pending"
	ActionCompleted ActionStatus = "completed"
	ActionFailed    ActionStatus = "failed"
	ActionUnknown   ActionStatus = "unknown"
)

type DrawSummary struct {
	DrawID             string    `json:"draw_id,omitempty"`
	PrizeID            string    `json:"prize_id,omitempty"`
	PrizeLabel         string    `json:"prize_label,omitempty"`
	PrizeShortLabel    string    `json:"prize_short_label,omitempty"`
	DrawStatus         string    `json:"draw_status,omitempty"`
	FulfillmentStatus  string    `json:"fulfillment_status,omitempty"`
	FulfillmentMessage string    `json:"fulfillment_message,omitempty"`
	EffectSummary      string    `json:"effect_summary,omitempty"`
	QuotaDelta         float64   `json:"quota_delta,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
}

type Action struct {
	Key                     string       `json:"key"`
	AccountID               string       `json:"account_id"`
	Date                    string       `json:"date"`
	Kind                    ActionKind   `json:"kind"`
	IdempotencyKey          string       `json:"idempotency_key,omitempty"`
	Status                  ActionStatus `json:"status"`
	Attempts                int          `json:"attempts,omitempty"`
	SideEffectStarted       bool         `json:"side_effect_started,omitempty"`
	Retryable               bool         `json:"retryable,omitempty"`
	PriceUSD                *quota.Money `json:"price_usd,omitempty"`
	ClaimBeforeRemaining    *int         `json:"claim_before_remaining,omitempty"`
	ClaimAfterRemaining     *int         `json:"claim_after_remaining,omitempty"`
	PurchaseBeforeToday     *int         `json:"purchase_before_today,omitempty"`
	PurchaseBeforeRemaining *int         `json:"purchase_before_remaining,omitempty"`
	PassBeforeUnlocked      *bool        `json:"pass_before_unlocked,omitempty"`
	CheckinQuotaAwarded     *float64     `json:"checkin_quota_awarded,omitempty"`
	CheckinQuotaAwardedUSD  *quota.Money `json:"checkin_quota_awarded_usd,omitempty"`
	Message                 string       `json:"message,omitempty"`
	LastError               string       `json:"last_error,omitempty"`
	Result                  *DrawSummary `json:"result,omitempty"`
	CreatedAt               time.Time    `json:"created_at"`
	UpdatedAt               time.Time    `json:"updated_at"`
}

type Snapshot struct {
	AccountID string          `json:"account_id"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
	QueriedAt time.Time       `json:"queried_at"`
}

type AutoDrawPlanStatus string

const (
	AutoDrawPlanPending   AutoDrawPlanStatus = "pending"
	AutoDrawPlanRunning   AutoDrawPlanStatus = "running"
	AutoDrawPlanCompleted AutoDrawPlanStatus = "completed"
	AutoDrawPlanSkipped   AutoDrawPlanStatus = "skipped"
	AutoDrawPlanFailed    AutoDrawPlanStatus = "failed"
)

type AutoDrawPlan struct {
	Key            string             `json:"key"`
	Date           string             `json:"date"`
	AccountID      string             `json:"account_id"`
	WindowID       string             `json:"window_id"`
	PlannedAt      time.Time          `json:"planned_at"`
	IdempotencyKey string             `json:"idempotency_key,omitempty"`
	Status         AutoDrawPlanStatus `json:"status"`
	ExecutedAt     time.Time          `json:"executed_at,omitempty"`
	Message        string             `json:"message,omitempty"`
	PrizeLabel     string             `json:"prize_label,omitempty"`
	QuotaDeltaUSD  *quota.Money       `json:"quota_delta_usd,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

type RuntimeLog struct {
	ID            string             `json:"id"`
	OccurredAt    time.Time          `json:"occurred_at"`
	AccountID     string             `json:"account_id"`
	WindowID      string             `json:"window_id"`
	Status        AutoDrawPlanStatus `json:"status"`
	Message       string             `json:"message,omitempty"`
	PrizeLabel    string             `json:"prize_label,omitempty"`
	QuotaDeltaUSD *quota.Money       `json:"quota_delta_usd,omitempty"`
}

type diskState struct {
	Version   int                       `json:"version"`
	Accounts  map[string]account.Record `json:"accounts,omitempty"`
	AuthHealth map[string]account.AuthHealth `json:"auth_health,omitempty"`
	// LegacyAuth bridges version-3 authentication tokens for runners that have
	// not moved to the secret vault yet. Migrated version-4 files never
	// contain it.
	LegacyAuth map[string]AuthState    `json:"legacy_auth,omitempty"`
	// DrawSchedules holds per-account user-defined auto-draw schedules.
	DrawSchedules map[string][]AutoDrawSchedule `json:"draw_schedules,omitempty"`
	Actions   map[string]Action        `json:"actions"`
	Snapshots map[string]Snapshot      `json:"snapshots"`
	Plans     map[string]AutoDrawPlan  `json:"plans,omitempty"`
	Logs      []RuntimeLog             `json:"logs,omitempty"`
}

// diskStateV3 mirrors the version-3 file shape, where `accounts` held raw
// authentication state instead of account records.
type diskStateV3 struct {
	Version   int                     `json:"version"`
	Accounts  map[string]AuthState    `json:"accounts"`
	Actions   map[string]Action       `json:"actions"`
	Snapshots map[string]Snapshot     `json:"snapshots"`
	Plans     map[string]AutoDrawPlan `json:"plans,omitempty"`
	Logs      []RuntimeLog            `json:"logs,omitempty"`
}

type Store struct {
	mu            sync.Mutex
	actionLocksMu sync.Mutex
	authLocksMu   sync.Mutex
	path          string
	lockFile      *os.File
	data          diskState
	actionLocks   map[string]*actionLock
	authLocks     map[string]*actionLock
}

type actionLock struct {
	mu   sync.Mutex
	refs int
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("acquire state lock: %w", err)
	}
	store := &Store{
		path:     path,
		lockFile: lockFile,
		data: diskState{
			Version:    version,
			Accounts:   make(map[string]account.Record),
			AuthHealth: make(map[string]account.AuthHealth),
			LegacyAuth: make(map[string]AuthState),
			Actions:    make(map[string]Action),
			Snapshots:  make(map[string]Snapshot),
			Plans:      make(map[string]AutoDrawPlan),
			DrawSchedules: make(map[string][]AutoDrawSchedule),
			Logs:       make([]RuntimeLog, 0),
		},
		actionLocks: make(map[string]*actionLock),
		authLocks:   make(map[string]*actionLock),
	}
	if err := store.load(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.lockFile == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
	err := s.lockFile.Close()
	s.lockFile = nil
	return err
}

func (s *Store) Auth(accountID string) AuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyAuth(s.data.LegacyAuth[accountID])
}

func (s *Store) PutAuth(accountID string, auth AuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth.UpdatedAt = time.Now().UTC()
	previous, existed := s.data.LegacyAuth[accountID]
	if s.data.LegacyAuth == nil {
		s.data.LegacyAuth = make(map[string]AuthState)
	}
	s.data.LegacyAuth[accountID] = copyAuth(auth)
	if err := s.persistLocked(); err != nil {
		if existed {
			s.data.LegacyAuth[accountID] = previous
		} else {
			delete(s.data.LegacyAuth, accountID)
		}
		return err
	}
	return nil
}

func (s *Store) GetOrCreateAction(accountID, date string, kind ActionKind) (Action, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	date = strings.TrimSpace(date)
	if accountID == "" || date == "" || kind == "" {
		return Action{}, false, errors.New("account ID, date, and action kind are required")
	}
	key := actionKey(accountID, date, kind)
	if existing, ok := s.data.Actions[key]; ok {
		return copyAction(existing), false, nil
	}
	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		return Action{}, false, err
	}
	now := time.Now().UTC()
	action := Action{
		Key:            key,
		AccountID:      accountID,
		Date:           date,
		Kind:           kind,
		IdempotencyKey: idempotencyKey,
		Status:         ActionPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.data.Actions[key] = action
	if err := s.persistLocked(); err != nil {
		delete(s.data.Actions, key)
		return Action{}, false, err
	}
	return copyAction(action), true, nil
}

func (s *Store) UpdateAction(key string, update func(*Action)) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.data.Actions[key]
	if !ok {
		return Action{}, fmt.Errorf("action %q not found", key)
	}
	previous := copyAction(action)
	update(&action)
	action.UpdatedAt = time.Now().UTC()
	s.data.Actions[key] = copyAction(action)
	if err := s.persistLocked(); err != nil {
		s.data.Actions[key] = previous
		return Action{}, err
	}
	return copyAction(action), nil
}

func (s *Store) Action(accountID, date string, kind ActionKind) (Action, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := actionKey(strings.TrimSpace(accountID), strings.TrimSpace(date), kind)
	action, ok := s.data.Actions[key]
	return copyAction(action), ok
}

func (s *Store) LockAction(accountID, date string, kind ActionKind) func() {
	key := actionKey(strings.TrimSpace(accountID), strings.TrimSpace(date), kind)

	s.actionLocksMu.Lock()
	entry := s.actionLocks[key]
	if entry == nil {
		entry = &actionLock{}
		s.actionLocks[key] = entry
	}
	entry.refs++
	s.actionLocksMu.Unlock()

	entry.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			s.actionLocksMu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(s.actionLocks, key)
			}
			s.actionLocksMu.Unlock()
		})
	}
}

// LockAuth serializes session refresh or password login for one account. A
// waiting request can reuse the session persisted by the first request rather
// than creating another server-side device session.
func (s *Store) LockAuth(accountID string) func() {
	key := strings.TrimSpace(accountID)

	s.authLocksMu.Lock()
	entry := s.authLocks[key]
	if entry == nil {
		entry = &actionLock{}
		s.authLocks[key] = entry
	}
	entry.refs++
	s.authLocksMu.Unlock()

	entry.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			s.authLocksMu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(s.authLocks, key)
			}
			s.authLocksMu.Unlock()
		})
	}
}

func (s *Store) ResetRetryableAction(key string) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.data.Actions[key]
	if !ok {
		return Action{}, fmt.Errorf("action %q not found", key)
	}
	if !action.Retryable || action.SideEffectStarted {
		return Action{}, fmt.Errorf("action %q is not retryable", key)
	}
	previous := copyAction(action)
	action.Status = ActionPending
	action.Retryable = false
	action.LastError = ""
	action.Message = ""
	action.UpdatedAt = time.Now().UTC()
	s.data.Actions[key] = copyAction(action)
	if err := s.persistLocked(); err != nil {
		s.data.Actions[key] = previous
		return Action{}, err
	}
	return copyAction(action), nil
}

func (s *Store) RotateRepeatableAction(key string) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.data.Actions[key]
	if !ok {
		return Action{}, fmt.Errorf("action %q not found", key)
	}
	if action.SideEffectStarted {
		return Action{}, fmt.Errorf("action %q cannot rotate after side effect started", key)
	}
	if action.Status != ActionCompleted && !(action.Status == ActionFailed && action.Retryable) {
		return Action{}, fmt.Errorf("action %q is not repeatable", key)
	}
	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		return Action{}, err
	}
	previous := copyAction(action)
	action.IdempotencyKey = idempotencyKey
	action.Status = ActionPending
	action.Attempts = 0
	action.SideEffectStarted = false
	action.Retryable = false
	action.PriceUSD = nil
	action.ClaimBeforeRemaining = nil
	action.ClaimAfterRemaining = nil
	action.PurchaseBeforeToday = nil
	action.PurchaseBeforeRemaining = nil
	action.PassBeforeUnlocked = nil
	action.CheckinQuotaAwarded = nil
	action.CheckinQuotaAwardedUSD = nil
	action.Message = ""
	action.LastError = ""
	action.Result = nil
	action.UpdatedAt = time.Now().UTC()
	s.data.Actions[key] = copyAction(action)
	if err := s.persistLocked(); err != nil {
		s.data.Actions[key] = previous
		return Action{}, err
	}
	return copyAction(action), nil
}

func (s *Store) Actions() []Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	actions := make([]Action, 0, len(s.data.Actions))
	for _, action := range s.data.Actions {
		actions = append(actions, copyAction(action))
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].CreatedAt.Before(actions[j].CreatedAt) })
	return actions
}

func (s *Store) PutSnapshot(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID := strings.TrimSpace(snapshot.AccountID)
	kind := strings.TrimSpace(snapshot.Kind)
	if accountID == "" || kind == "" {
		return errors.New("snapshot account ID and kind are required")
	}
	snapshot.AccountID = accountID
	snapshot.Kind = kind
	snapshot.Data = append(json.RawMessage(nil), snapshot.Data...)
	if snapshot.QueriedAt.IsZero() {
		snapshot.QueriedAt = time.Now().UTC()
	}
	key := snapshotKey(accountID, kind)
	previous, existed := s.data.Snapshots[key]
	s.data.Snapshots[key] = snapshot
	if err := s.persistLocked(); err != nil {
		if existed {
			s.data.Snapshots[key] = previous
		} else {
			delete(s.data.Snapshots, key)
		}
		return err
	}
	return nil
}

func (s *Store) Snapshot(accountID, kind string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.data.Snapshots[snapshotKey(accountID, kind)]
	if !ok {
		return Snapshot{}, false
	}
	snapshot.Data = append(json.RawMessage(nil), snapshot.Data...)
	return snapshot, true
}

func (s *Store) Snapshots(accountID string) []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	snapshots := make([]Snapshot, 0)
	for _, snapshot := range s.data.Snapshots {
		if accountID != "" && snapshot.AccountID != accountID {
			continue
		}
		snapshot.Data = append(json.RawMessage(nil), snapshot.Data...)
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].QueriedAt.After(snapshots[j].QueriedAt) })
	return snapshots
}

func (s *Store) EnsureAutoDrawPlans(plans []AutoDrawPlan) ([]AutoDrawPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]AutoDrawPlan, len(plans))
	insertedKeys := make([]string, 0)
	pending := make(map[string]AutoDrawPlan)

	for i, input := range plans {
		plan, err := normalizeAutoDrawPlan(input, false)
		if err != nil {
			return nil, err
		}
		if existing, ok := s.data.Plans[plan.Key]; ok {
			results[i] = copyAutoDrawPlan(existing)
			continue
		}
		if created, ok := pending[plan.Key]; ok {
			results[i] = copyAutoDrawPlan(created)
			continue
		}
		pending[plan.Key] = plan
		insertedKeys = append(insertedKeys, plan.Key)
		results[i] = copyAutoDrawPlan(plan)
	}

	for _, key := range insertedKeys {
		s.data.Plans[key] = copyAutoDrawPlan(pending[key])
	}
	if err := s.persistLocked(); err != nil {
		for _, key := range insertedKeys {
			delete(s.data.Plans, key)
		}
		return nil, err
	}
	return results, nil
}

func (s *Store) AutoDrawPlans(date string) []AutoDrawPlan {
	s.mu.Lock()
	defer s.mu.Unlock()

	date = strings.TrimSpace(date)
	plans := make([]AutoDrawPlan, 0)
	for _, plan := range s.data.Plans {
		if date != "" && plan.Date != date {
			continue
		}
		plans = append(plans, copyAutoDrawPlan(plan))
	}
	sort.Slice(plans, func(i, j int) bool {
		if !plans[i].PlannedAt.Equal(plans[j].PlannedAt) {
			return plans[i].PlannedAt.Before(plans[j].PlannedAt)
		}
		if plans[i].WindowID != plans[j].WindowID {
			return plans[i].WindowID < plans[j].WindowID
		}
		return plans[i].Key < plans[j].Key
	})
	return plans
}

func (s *Store) AutoDrawPlan(key string) (AutoDrawPlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, ok := s.data.Plans[strings.TrimSpace(key)]
	return copyAutoDrawPlan(plan), ok
}

func (s *Store) LockAutoDrawPlan(key string) func() {
	return s.lockByKey(strings.TrimSpace(key))
}

func (s *Store) BeginAutoDrawPlan(key string) (AutoDrawPlan, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key = strings.TrimSpace(key)
	plan, ok := s.data.Plans[key]
	if !ok {
		return AutoDrawPlan{}, false, fmt.Errorf("auto draw plan %q not found", key)
	}
	if isTerminalAutoDrawStatus(plan.Status) {
		return copyAutoDrawPlan(plan), false, nil
	}

	previous := copyAutoDrawPlan(plan)
	plan.Status = AutoDrawPlanRunning
	plan.ExecutedAt = time.Time{}
	plan.Message = ""
	plan.PrizeLabel = ""
	plan.QuotaDeltaUSD = nil
	plan.UpdatedAt = time.Now().UTC()
	s.data.Plans[key] = copyAutoDrawPlan(plan)
	if err := s.persistLocked(); err != nil {
		s.data.Plans[key] = previous
		return AutoDrawPlan{}, false, err
	}
	return copyAutoDrawPlan(plan), true, nil
}

func (s *Store) FinishAutoDrawPlan(key string, status AutoDrawPlanStatus, message, prize string, delta *quota.Money, executedAt time.Time) (AutoDrawPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key = strings.TrimSpace(key)
	plan, ok := s.data.Plans[key]
	if !ok {
		return AutoDrawPlan{}, fmt.Errorf("auto draw plan %q not found", key)
	}
	status, err := normalizeFinishedAutoDrawStatus(status)
	if err != nil {
		return AutoDrawPlan{}, err
	}
	message, err = normalizeDisplayText("message", message, 512)
	if err != nil {
		return AutoDrawPlan{}, err
	}
	prize, err = normalizeDisplayText("prize label", prize, 256)
	if err != nil {
		return AutoDrawPlan{}, err
	}

	previous := copyAutoDrawPlan(plan)
	plan.Status = status
	plan.Message = message
	plan.PrizeLabel = prize
	plan.QuotaDeltaUSD = copyMoney(delta)
	if executedAt.IsZero() {
		executedAt = time.Now().UTC()
	} else {
		executedAt = executedAt.UTC()
	}
	plan.ExecutedAt = executedAt
	plan.UpdatedAt = time.Now().UTC()
	s.data.Plans[key] = copyAutoDrawPlan(plan)
	if err := s.persistLocked(); err != nil {
		s.data.Plans[key] = previous
		return AutoDrawPlan{}, err
	}
	return copyAutoDrawPlan(plan), nil
}

func (s *Store) AppendRuntimeLog(log RuntimeLog) (RuntimeLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value, err := normalizeRuntimeLog(log)
	if err != nil {
		return RuntimeLog{}, err
	}
	previousLen := len(s.data.Logs)
	s.data.Logs = append(s.data.Logs, copyRuntimeLog(value))
	if err := s.persistLocked(); err != nil {
		s.data.Logs = s.data.Logs[:previousLen]
		return RuntimeLog{}, err
	}
	return copyRuntimeLog(value), nil
}

func (s *Store) RuntimeLogs(limit int) []RuntimeLog {
	s.mu.Lock()
	defer s.mu.Unlock()

	logs := make([]RuntimeLog, 0, len(s.data.Logs))
	for i := len(s.data.Logs) - 1; i >= 0; i-- {
		logs = append(logs, copyRuntimeLog(s.data.Logs[i]))
		if limit > 0 && len(logs) >= limit {
			break
		}
	}
	return logs
}

func (s *Store) PruneAutoDrawData(beforeDate string, beforeTime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	beforeDate = strings.TrimSpace(beforeDate)
	if beforeDate != "" {
		if _, err := time.Parse("2006-01-02", beforeDate); err != nil {
			return fmt.Errorf("invalid prune date %q: %w", beforeDate, err)
		}
	}
	if !beforeTime.IsZero() {
		beforeTime = beforeTime.UTC()
	}

	previousPlans := make(map[string]AutoDrawPlan, len(s.data.Plans))
	for key, plan := range s.data.Plans {
		previousPlans[key] = copyAutoDrawPlan(plan)
	}
	previousLogs := make([]RuntimeLog, len(s.data.Logs))
	for i, log := range s.data.Logs {
		previousLogs[i] = copyRuntimeLog(log)
	}

	if beforeDate != "" {
		for key, plan := range s.data.Plans {
			if plan.Date < beforeDate {
				delete(s.data.Plans, key)
			}
		}
	}
	if !beforeTime.IsZero() {
		for key, plan := range s.data.Plans {
			if plan.PlannedAt.Before(beforeTime) {
				delete(s.data.Plans, key)
			}
		}
		logs := s.data.Logs[:0]
		for _, log := range s.data.Logs {
			if log.OccurredAt.Before(beforeTime) {
				continue
			}
			logs = append(logs, log)
		}
		s.data.Logs = logs
	}
	if err := s.persistLocked(); err != nil {
		s.data.Plans = previousPlans
		s.data.Logs = previousLogs
		return err
	}
	return nil
}

func (s *Store) load() error {
	payload, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	if probe.Version > version {
		return fmt.Errorf("state version %d is newer than supported version %d", probe.Version, version)
	}
	if probe.Version >= 4 {
		if err := json.Unmarshal(payload, &s.data); err != nil {
			return fmt.Errorf("decode state: %w", err)
		}
	} else {
		// Version 3 (or a headerless legacy file): `accounts` holds raw
		// authentication state. Keep the file at version 3; only the explicit
		// migration rewrites it as version 4.
		var legacy diskStateV3
		if err := json.Unmarshal(payload, &legacy); err != nil {
			return fmt.Errorf("decode state: %w", err)
		}
		s.data.Version = 3
		s.data.LegacyAuth = legacy.Accounts
		s.data.Actions = legacy.Actions
		s.data.Snapshots = legacy.Snapshots
		s.data.Plans = legacy.Plans
		s.data.Logs = legacy.Logs
	}
	if s.data.Version == 0 {
		s.data.Version = 3
	}
	if s.data.Accounts == nil {
		s.data.Accounts = make(map[string]account.Record)
	}
	if s.data.AuthHealth == nil {
		s.data.AuthHealth = make(map[string]account.AuthHealth)
	}
	if s.data.LegacyAuth == nil {
		s.data.LegacyAuth = make(map[string]AuthState)
	}
	if s.data.Actions == nil {
		s.data.Actions = make(map[string]Action)
	}
	if s.data.Snapshots == nil {
		s.data.Snapshots = make(map[string]Snapshot)
	}
	if s.data.Plans == nil {
		s.data.Plans = make(map[string]AutoDrawPlan)
	}
	if s.data.DrawSchedules == nil {
		s.data.DrawSchedules = make(map[string][]AutoDrawSchedule)
	}
	if s.data.Logs == nil {
		s.data.Logs = make([]RuntimeLog, 0)
	}
	return nil
}

func (s *Store) persistLocked() error {
	var payload []byte
	var err error
	if s.data.Version <= 3 {
		legacy := diskStateV3{
			Version:   3,
			Accounts:  s.data.LegacyAuth,
			Actions:   s.data.Actions,
			Snapshots: s.data.Snapshots,
			Plans:     s.data.Plans,
			Logs:      s.data.Logs,
		}
		payload, err = json.MarshalIndent(legacy, "", "  ")
	} else {
		payload, err = json.MarshalIndent(s.data, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary state: %w", err)
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func newIdempotencyKey() (string, error) {
	return newID("draw")
}

func newAutoDrawIdempotencyKey() (string, error) {
	return newID("draw:auto")
}

func actionKey(accountID, date string, kind ActionKind) string {
	return fmt.Sprintf("%s:%s:%s", date, kind, accountID)
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	return strings.TrimSpace(prefix) + ":" + hex.EncodeToString(value[:]), nil
}

func copyAuth(value AuthState) AuthState {
	value.Cookies = append([]Cookie(nil), value.Cookies...)
	return value
}

func copyAutoDrawPlan(value AutoDrawPlan) AutoDrawPlan {
	value.QuotaDeltaUSD = copyMoney(value.QuotaDeltaUSD)
	return value
}

func copyRuntimeLog(value RuntimeLog) RuntimeLog {
	value.QuotaDeltaUSD = copyMoney(value.QuotaDeltaUSD)
	return value
}

func copyMoney(value *quota.Money) *quota.Money {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyAction(value Action) Action {
	if value.Result != nil {
		result := *value.Result
		value.Result = &result
	}
	value.PriceUSD = copyMoney(value.PriceUSD)
	if value.ClaimBeforeRemaining != nil {
		before := *value.ClaimBeforeRemaining
		value.ClaimBeforeRemaining = &before
	}
	if value.ClaimAfterRemaining != nil {
		after := *value.ClaimAfterRemaining
		value.ClaimAfterRemaining = &after
	}
	if value.CheckinQuotaAwarded != nil {
		quota := *value.CheckinQuotaAwarded
		value.CheckinQuotaAwarded = &quota
	}
	value.CheckinQuotaAwardedUSD = copyMoney(value.CheckinQuotaAwardedUSD)
	if value.PurchaseBeforeToday != nil {
		beforeToday := *value.PurchaseBeforeToday
		value.PurchaseBeforeToday = &beforeToday
	}
	if value.PurchaseBeforeRemaining != nil {
		beforeRemaining := *value.PurchaseBeforeRemaining
		value.PurchaseBeforeRemaining = &beforeRemaining
	}
	if value.PassBeforeUnlocked != nil {
		beforeUnlocked := *value.PassBeforeUnlocked
		value.PassBeforeUnlocked = &beforeUnlocked
	}
	return value
}

func copyFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func normalizeAutoDrawPlan(plan AutoDrawPlan, preserveTimestamps bool) (AutoDrawPlan, error) {
	date := strings.TrimSpace(plan.Date)
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return AutoDrawPlan{}, fmt.Errorf("invalid auto draw date %q: %w", date, err)
	}
	accountID := strings.TrimSpace(plan.AccountID)
	windowID := strings.TrimSpace(plan.WindowID)
	if accountID == "" || windowID == "" {
		return AutoDrawPlan{}, errors.New("auto draw account ID and window ID are required")
	}
	derivedKey := autoDrawPlanKey(date, accountID, windowID)
	key := strings.TrimSpace(plan.Key)
	if key != "" && key != derivedKey {
		return AutoDrawPlan{}, fmt.Errorf("auto draw plan key %q does not match %q", key, derivedKey)
	}

	status, err := normalizeAutoDrawStatus(plan.Status)
	if err != nil {
		return AutoDrawPlan{}, err
	}
	if status == "" {
		status = AutoDrawPlanPending
	}

	message, err := normalizeDisplayText("message", plan.Message, 512)
	if err != nil {
		return AutoDrawPlan{}, err
	}
	prize, err := normalizeDisplayText("prize label", plan.PrizeLabel, 256)
	if err != nil {
		return AutoDrawPlan{}, err
	}

	idempotencyKey := strings.TrimSpace(plan.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey, err = newAutoDrawIdempotencyKey()
		if err != nil {
			return AutoDrawPlan{}, err
		}
	}

	now := time.Now().UTC()
	normalized := AutoDrawPlan{
		Key:            derivedKey,
		Date:           date,
		AccountID:      accountID,
		WindowID:       windowID,
		PlannedAt:      plan.PlannedAt.UTC(),
		IdempotencyKey: idempotencyKey,
		Status:         status,
		ExecutedAt:     plan.ExecutedAt.UTC(),
		Message:        message,
		PrizeLabel:     prize,
		QuotaDeltaUSD:  copyMoney(plan.QuotaDeltaUSD),
		CreatedAt:      plan.CreatedAt.UTC(),
		UpdatedAt:      plan.UpdatedAt.UTC(),
	}
	if normalized.PlannedAt.IsZero() {
		normalized.PlannedAt = now
	}
	if !preserveTimestamps || normalized.CreatedAt.IsZero() {
		normalized.CreatedAt = now
	}
	if !preserveTimestamps || normalized.UpdatedAt.IsZero() {
		normalized.UpdatedAt = now
	}
	if !isTerminalAutoDrawStatus(normalized.Status) {
		normalized.ExecutedAt = time.Time{}
		normalized.Message = ""
		normalized.PrizeLabel = ""
		normalized.QuotaDeltaUSD = nil
	}
	return normalized, nil
}

func normalizeRuntimeLog(log RuntimeLog) (RuntimeLog, error) {
	accountID := strings.TrimSpace(log.AccountID)
	windowID := strings.TrimSpace(log.WindowID)
	if accountID == "" || windowID == "" {
		return RuntimeLog{}, errors.New("runtime log account ID and window ID are required")
	}
	status, err := normalizeAutoDrawStatus(log.Status)
	if err != nil {
		return RuntimeLog{}, err
	}
	if status == "" {
		return RuntimeLog{}, errors.New("runtime log status is required")
	}
	message, err := normalizeDisplayText("message", log.Message, 512)
	if err != nil {
		return RuntimeLog{}, err
	}
	prize, err := normalizeDisplayText("prize label", log.PrizeLabel, 256)
	if err != nil {
		return RuntimeLog{}, err
	}
	occurredAt := log.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	id := strings.TrimSpace(log.ID)
	if id == "" {
		id, err = newID("runtime")
		if err != nil {
			return RuntimeLog{}, err
		}
	}
	return RuntimeLog{
		ID:            id,
		OccurredAt:    occurredAt,
		AccountID:     accountID,
		WindowID:      windowID,
		Status:        status,
		Message:       message,
		PrizeLabel:    prize,
		QuotaDeltaUSD: copyMoney(log.QuotaDeltaUSD),
	}, nil
}

func normalizeAutoDrawStatus(status AutoDrawPlanStatus) (AutoDrawPlanStatus, error) {
	switch AutoDrawPlanStatus(strings.TrimSpace(string(status))) {
	case "":
		return "", nil
	case AutoDrawPlanPending, AutoDrawPlanRunning, AutoDrawPlanCompleted, AutoDrawPlanSkipped, AutoDrawPlanFailed:
		return AutoDrawPlanStatus(strings.TrimSpace(string(status))), nil
	default:
		return "", fmt.Errorf("invalid auto draw status %q", status)
	}
}

func normalizeFinishedAutoDrawStatus(status AutoDrawPlanStatus) (AutoDrawPlanStatus, error) {
	status, err := normalizeAutoDrawStatus(status)
	if err != nil {
		return "", err
	}
	if !isTerminalAutoDrawStatus(status) {
		return "", fmt.Errorf("auto draw finish requires terminal status, got %q", status)
	}
	return status, nil
}

func isTerminalAutoDrawStatus(status AutoDrawPlanStatus) bool {
	return status == AutoDrawPlanCompleted || status == AutoDrawPlanSkipped || status == AutoDrawPlanFailed
}

func normalizeDisplayText(label, value string, maxLen int) (string, error) {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if maxLen > 0 && len(value) > maxLen {
		return "", fmt.Errorf("%s exceeds %d characters", label, maxLen)
	}
	if containsSensitiveDisplayText(value) {
		return "已隐藏敏感详情", nil
	}
	return value, nil
}

func containsSensitiveDisplayText(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{"token", "cookie", "password", "idempotency", "bearer ", "access_token", "refresh_token", "令牌", "密码", "凭证"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func autoDrawPlanKey(date, accountID, windowID string) string {
	return date + ":" + windowID + ":" + accountID
}

func snapshotKey(accountID, kind string) string {
	return strings.TrimSpace(accountID) + ":" + strings.TrimSpace(kind)
}

func (s *Store) lockByKey(key string) func() {
	s.actionLocksMu.Lock()
	entry := s.actionLocks[key]
	if entry == nil {
		entry = &actionLock{}
		s.actionLocks[key] = entry
	}
	entry.refs++
	s.actionLocksMu.Unlock()

	entry.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			s.actionLocksMu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(s.actionLocks, key)
			}
			s.actionLocksMu.Unlock()
		})
	}
}
