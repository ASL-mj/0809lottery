package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

const defaultDailyFreeDrawLimit = 3

type DrawCountReport struct {
	AccountID          string    `json:"account_id"`
	Remaining          int       `json:"remaining"`
	LockedRemaining    int       `json:"locked_remaining"`
	EarnedRemaining    int       `json:"earned_remaining"`
	PurchasedRemaining int       `json:"purchased_remaining"`
	DailyUsed          int       `json:"daily_used"`
	FreeLimit          int       `json:"free_limit"`
	Unlocked           bool      `json:"unlocked"`
	Status             string    `json:"status"`
	DayKey             string    `json:"day_key"`
	QueriedAt          time.Time `json:"queried_at"`
}

func (r *Runner) QueryDrawCount(ctx context.Context, accountID string) (DrawCountReport, error) {
	account, err := r.account(accountID)
	if err != nil {
		return DrawCountReport{}, err
	}
	dashboard, err := r.Dashboard(ctx, account.ID)
	if err != nil {
		return DrawCountReport{}, err
	}
	limit, ok := dashboard.EffectiveDrawLimit()
	if !ok || limit.Remaining == nil {
		return DrawCountReport{}, errors.New("dashboard did not contain a current draw count")
	}
	freeLimit := intOrDefault(limit.FreeLimit, defaultDailyFreeDrawLimit)
	status := strings.TrimSpace(limit.Status)
	if status == "" {
		if boolOrDefault(limit.Unlocked, false) {
			status = "unlocked"
		} else {
			status = "unknown"
		}
	}
	dayKey := strings.TrimSpace(limit.DayKey)
	if dayKey == "" {
		dayKey = r.today()
	}
	return DrawCountReport{
		AccountID:          account.ID,
		Remaining:          intOrDefault(limit.Remaining, 0),
		LockedRemaining:    intOrDefault(limit.LockedRemaining, 0),
		EarnedRemaining:    intOrDefault(limit.EarnedRemaining, 0),
		PurchasedRemaining: intOrDefault(limit.PurchasedRemaining, 0),
		DailyUsed:          intOrDefault(limit.DailyUsed, 0),
		FreeLimit:          freeLimit,
		Unlocked:           boolOrDefault(limit.Unlocked, false),
		Status:             status,
		DayKey:             dayKey,
		QueriedAt:          r.now().UTC(),
	}, nil
}

func intOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
