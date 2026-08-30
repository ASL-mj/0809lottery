package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
)

// AccountRegistry persists sanitized account metadata inside the state file
// and implements account.Repository. Login names and passwords live only in
// the secret vault.
type AccountRegistry struct {
	store *Store
}

var _ account.Repository = (*AccountRegistry)(nil)

func (s *Store) AccountRegistry() *AccountRegistry {
	return &AccountRegistry{store: s}
}

func (r *AccountRegistry) List() ([]account.Record, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.listLocked(), nil
}

func (r *AccountRegistry) ListEnabled() ([]account.Record, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	records := make([]account.Record, 0)
	for _, record := range r.listLocked() {
		if record.Status == account.StatusEnabled {
			records = append(records, record)
		}
	}
	return records, nil
}

func (r *AccountRegistry) listLocked() []account.Record {
	records := make([]account.Record, 0, len(r.store.data.Accounts))
	for _, record := range r.store.data.Accounts {
		records = append(records, record)
	}
	// Newest accounts sort last: creation time ascending, ID as tiebreak for
	// records created in the same instant.
	sort.Slice(records, func(left, right int) bool {
		if !records[left].CreatedAt.Equal(records[right].CreatedAt) {
			return records[left].CreatedAt.Before(records[right].CreatedAt)
		}
		return records[left].ID < records[right].ID
	})
	return records
}

// nextSequentialAccountID returns the next code after the persisted
// watermark. On first use the watermark is seeded from the highest existing
// sequential ID so codes created before the watermark never collide.
func (r *AccountRegistry) nextSequentialAccountID() (string, error) {
	watermark := r.store.data.AccountIDSeq
	if watermark == "" {
		maxLetter, maxDigit := 0, 0
		for id := range r.store.data.Accounts {
			suffix := strings.TrimPrefix(id, "account-")
			if len(suffix) == 2 && suffix[0] >= 'a' && suffix[0] <= 'z' && suffix[1] >= '1' && suffix[1] <= '9' {
				letter, digit := int(suffix[0]-'a'), int(suffix[1]-'0')
				if letter > maxLetter || (letter == maxLetter && digit > maxDigit) {
					maxLetter, maxDigit = letter, digit
				}
			}
		}
		if maxLetter > 0 || maxDigit > 0 {
			watermark = "account-" + string(rune('a'+maxLetter)) + strconv.Itoa(maxDigit)
		}
	}
	return newAccountID(watermark)
}

func (r *AccountRegistry) Get(id string) (account.Record, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	record, ok := r.store.data.Accounts[strings.TrimSpace(id)]
	if !ok {
		return account.Record{}, account.ErrNotFound
	}
	return record, nil
}

func (r *AccountRegistry) Create(record account.Record) (account.Record, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	if strings.TrimSpace(record.ID) == "" {
		id, err := r.nextSequentialAccountID()
		if err != nil {
			return account.Record{}, err
		}
		record.ID = id
	}
	record.ID = strings.TrimSpace(record.ID)
	if record.Status == "" {
		record.Status = account.StatusEnabled
	}
	if err := record.Validate(); err != nil {
		return account.Record{}, err
	}
	if _, ok := r.store.data.Accounts[record.ID]; ok {
		return account.Record{}, fmt.Errorf("account %q already exists", record.ID)
	}
	if err := r.ensureRemoteUserIDFreeLocked(record.ID, record.RemoteUserID); err != nil {
		return account.Record{}, err
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	record.UpdatedAt = now
	previousSeq := r.store.data.AccountIDSeq
	r.store.data.AccountIDSeq = record.ID
	r.store.data.Accounts[record.ID] = record
	if err := r.store.persistLocked(); err != nil {
		delete(r.store.data.Accounts, record.ID)
		r.store.data.AccountIDSeq = previousSeq
		return account.Record{}, err
	}
	return record, nil
}

func (r *AccountRegistry) Update(record account.Record) (account.Record, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	id := strings.TrimSpace(record.ID)
	previous, ok := r.store.data.Accounts[id]
	if !ok {
		return account.Record{}, account.ErrNotFound
	}
	updated := previous
	updated.Label = strings.TrimSpace(record.Label)
	updated.MaskedLoginName = strings.TrimSpace(record.MaskedLoginName)
	updated.Status = record.Status
	if updated.Status == "" {
		updated.Status = previous.Status
	}
	if err := updated.Validate(); err != nil {
		return account.Record{}, err
	}
	updated.UpdatedAt = time.Now().UTC()
	r.store.data.Accounts[id] = updated
	if err := r.store.persistLocked(); err != nil {
		r.store.data.Accounts[id] = previous
		return account.Record{}, err
	}
	return updated, nil
}

func (r *AccountRegistry) SetRemoteUserID(id string, userID int64) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	id = strings.TrimSpace(id)
	previous, ok := r.store.data.Accounts[id]
	if !ok {
		return account.ErrNotFound
	}
	if err := r.ensureRemoteUserIDFreeLocked(id, userID); err != nil {
		return err
	}
	updated := previous
	updated.RemoteUserID = userID
	updated.UpdatedAt = time.Now().UTC()
	r.store.data.Accounts[id] = updated
	if err := r.store.persistLocked(); err != nil {
		r.store.data.Accounts[id] = previous
		return err
	}
	return nil
}

func (r *AccountRegistry) Delete(id string) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	id = strings.TrimSpace(id)
	previous, ok := r.store.data.Accounts[id]
	if !ok {
		return account.ErrNotFound
	}
	delete(r.store.data.Accounts, id)
	if err := r.store.persistLocked(); err != nil {
		r.store.data.Accounts[id] = previous
		return err
	}
	return nil
}

// ensureRemoteUserIDFreeLocked enforces that one remote user ID is bound to at
// most one local account unless the same account already holds it.
func (r *AccountRegistry) ensureRemoteUserIDFreeLocked(accountID string, userID int64) error {
	if userID <= 0 {
		return nil
	}
	for id, record := range r.store.data.Accounts {
		if id != accountID && record.RemoteUserID == userID {
			return account.ErrDuplicateRemoteUser
		}
	}
	return nil
}

// newAccountID advances the persisted sequential watermark to the next
// "letter + digit" account code: account-a1 … account-a9, then account-b1 …
// account-z9. The watermark (not the live set) drives allocation, so deleted
// codes are never reused. Legacy account-a…account-e IDs carry no digit and
// are ignored when the watermark is first initialized.
func newAccountID(watermark string) (string, error) {
	letter, digit := 0, 0
	if watermark != "" {
		suffix := strings.TrimPrefix(watermark, "account-")
		if len(suffix) == 2 && suffix[0] >= 'a' && suffix[0] <= 'z' && suffix[1] >= '1' && suffix[1] <= '9' {
			letter, digit = int(suffix[0]-'a'), int(suffix[1]-'0')
		}
	}
	digit++
	if digit > 9 {
		letter++
		digit = 1
	}
	if letter > 'z'-'a' {
		return "", fmt.Errorf("sequential account IDs are exhausted (a1 through z9)")
	}
	return "account-" + string(rune('a'+letter)) + strconv.Itoa(digit), nil
}

// nextSequentialAccountID returns the next code after the persisted
// watermark, seeding it from the highest existing sequential ID on first use.
func (s *Store) nextSequentialAccountID() (string, error) {
	return newAccountID(s.data.AccountIDSeq)
}

const maxAccountID = "account-z9"

// AuthHealth returns the sanitized authentication health of one account.
func (s *Store) AuthHealth(accountID string) account.AuthHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AuthHealth[strings.TrimSpace(accountID)]
}

func (s *Store) SetAuthHealth(accountID string, health account.AuthHealth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return fmt.Errorf("account ID is required")
	}
	previous, existed := s.data.AuthHealth[accountID]
	if s.data.AuthHealth == nil {
		s.data.AuthHealth = make(map[string]account.AuthHealth)
	}
	s.data.AuthHealth[accountID] = health
	if err := s.persistLocked(); err != nil {
		if existed {
			s.data.AuthHealth[accountID] = previous
		} else {
			delete(s.data.AuthHealth, accountID)
		}
		return err
	}
	return nil
}

// RemoveAccountScopedState deletes one account's actions, snapshots, plans and
// runtime logs. Other accounts are untouched. The registry entry itself is
// removed separately through AccountRegistry.Delete.
func (s *Store) RemoveAccountScopedState(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return fmt.Errorf("account ID is required")
	}

	previousActions := make(map[string]Action, len(s.data.Actions))
	for key, action := range s.data.Actions {
		previousActions[key] = copyAction(action)
	}
	previousSnapshots := make(map[string]Snapshot, len(s.data.Snapshots))
	for key, snapshot := range s.data.Snapshots {
		snapshot.Data = append(json.RawMessage(nil), snapshot.Data...)
		previousSnapshots[key] = snapshot
	}
	previousSchedules := s.data.DrawSchedules[accountID]
	previousPlans := make(map[string]AutoDrawPlan, len(s.data.Plans))
	for key, plan := range s.data.Plans {
		previousPlans[key] = copyAutoDrawPlan(plan)
	}
	previousLogs := make([]RuntimeLog, len(s.data.Logs))
	for index, log := range s.data.Logs {
		previousLogs[index] = copyRuntimeLog(log)
	}

	for key, action := range s.data.Actions {
		if action.AccountID == accountID {
			delete(s.data.Actions, key)
		}
	}
	prefix := accountID + ":"
	for key := range s.data.Snapshots {
		if strings.HasPrefix(key, prefix) {
			delete(s.data.Snapshots, key)
		}
	}
	for key, plan := range s.data.Plans {
		if plan.AccountID == accountID {
			delete(s.data.Plans, key)
		}
	}
	logs := s.data.Logs[:0]
	for _, log := range s.data.Logs {
		if log.AccountID == accountID {
			continue
		}
		logs = append(logs, log)
	}
	s.data.Logs = logs
	delete(s.data.DrawSchedules, accountID)

	if err := s.persistLocked(); err != nil {
		s.data.Actions = previousActions
		s.data.Snapshots = previousSnapshots
		s.data.Plans = previousPlans
		s.data.Logs = previousLogs
		if previousSchedules != nil {
			s.data.DrawSchedules[accountID] = previousSchedules
		}
		return err
	}
	return nil
}
