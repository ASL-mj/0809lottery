package state

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"skyeapi/lottery-bot/internal/account"
)

// AutoDrawScheduleKind selects how a plan's time is derived.
const (
	AutoDrawScheduleFixed  = "fixed"
	AutoDrawScheduleRandom = "random"
)

const (
	maxDrawSchedulesPerAccount = 12
	drawScheduleIDPrefix       = "sched-"
)

// AutoDrawSchedule is one user-defined auto-draw entry for an account. Times
// are Beijing time (Asia/Shanghai) HH:MM strings. A fixed entry draws once at
// Start; a random entry draws once at a random second within [Start, End).
type AutoDrawSchedule struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

// DrawSchedules returns the persisted schedule entries for one account.
func (s *Store) DrawSchedules(accountID string) []AutoDrawSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.data.DrawSchedules[strings.TrimSpace(accountID)]
	return append([]AutoDrawSchedule(nil), entries...)
}

// SetDrawSchedules replaces the whole schedule list for one account. Entries
// are validated and normalized; IDs are preserved when provided so already
// persisted plans stay addressable, and generated for new entries.
func (s *Store) SetDrawSchedules(accountID string, entries []AutoDrawSchedule) ([]AutoDrawSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account ID is required")
	}
	if _, ok := s.data.Accounts[accountID]; !ok {
		return nil, account.ErrNotFound
	}
	if len(entries) > maxDrawSchedulesPerAccount {
		return nil, fmt.Errorf("at most %d draw schedules per account are supported", maxDrawSchedulesPerAccount)
	}

	previous := s.data.DrawSchedules[accountID]
	normalized := make([]AutoDrawSchedule, 0, len(entries))
	usedIDs := make(map[string]bool, len(entries))
	for _, entry := range entries {
		normalizedEntry, err := normalizeDrawSchedule(entry, usedIDs)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, normalizedEntry)
	}
	sort.Slice(normalized, func(left, right int) bool {
		if normalized[left].Start != normalized[right].Start {
			return normalized[left].Start < normalized[right].Start
		}
		return normalized[left].ID < normalized[right].ID
	})
	if s.data.DrawSchedules == nil {
		s.data.DrawSchedules = make(map[string][]AutoDrawSchedule)
	}
	s.data.DrawSchedules[accountID] = normalized
	if err := s.persistLocked(); err != nil {
		s.data.DrawSchedules[accountID] = previous
		return nil, err
	}
	return append([]AutoDrawSchedule(nil), normalized...), nil
}

func normalizeDrawSchedule(entry AutoDrawSchedule, usedIDs map[string]bool) (AutoDrawSchedule, error) {
	kind := strings.ToLower(strings.TrimSpace(entry.Kind))
	if kind != AutoDrawScheduleFixed && kind != AutoDrawScheduleRandom {
		return AutoDrawSchedule{}, fmt.Errorf("抽奖计划类型仅支持 fixed 或 random")
	}
	start, err := normalizeClock(entry.Start)
	if err != nil {
		return AutoDrawSchedule{}, fmt.Errorf("开始时间无效：%w", err)
	}
	normalized := AutoDrawSchedule{ID: strings.TrimSpace(entry.ID), Kind: kind, Start: start}
	switch kind {
	case AutoDrawScheduleFixed:
		if strings.TrimSpace(entry.End) != "" {
			return AutoDrawSchedule{}, errors.New("固定时间抽奖不需要结束时间")
		}
	case AutoDrawScheduleRandom:
		end, err := normalizeClock(entry.End)
		if err != nil {
			return AutoDrawSchedule{}, fmt.Errorf("结束时间无效：%w", err)
		}
		if clockMinutes(start) >= clockMinutes(end) {
			return AutoDrawSchedule{}, fmt.Errorf("随机时段必须早于结束时间（暂不支持跨零点）")
		}
		normalized.End = end
	}
	if normalized.ID == "" {
		id, err := newID(drawScheduleIDPrefix)
		if err != nil {
			return AutoDrawSchedule{}, err
		}
		normalized.ID = id
	}
	if usedIDs[normalized.ID] {
		return AutoDrawSchedule{}, fmt.Errorf("抽奖计划 ID %q 重复", normalized.ID)
	}
	usedIDs[normalized.ID] = true
	return normalized, nil
}

// normalizeClock validates and canonicalizes an HH:MM string.
func normalizeClock(value string) (string, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return "", fmt.Errorf("时间 %q 必须是 HH:MM 格式", value)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return "", fmt.Errorf("小时 %q 必须在 0-23 之间", parts[0])
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return "", fmt.Errorf("分钟 %q 必须在 0-59 之间", parts[1])
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), nil
}

func clockMinutes(value string) int {
	parts := strings.Split(value, ":")
	hour, _ := strconv.Atoi(parts[0])
	minute, _ := strconv.Atoi(parts[1])
	return hour*60 + minute
}
