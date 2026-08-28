package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

// ActivityReport emits traceable Money snapshots for every dollar figure.
// Tier arithmetic stays on floats internally; each published value carries its
// dashboard source and the already-usd-v1 rule.
type ActivityReport struct {
	AccountID             string       `json:"account_id"`
	TodaySpendUSD         quota.Money  `json:"today_spend_usd"`
	SpendTierReached      int          `json:"spend_tier_reached"`
	SpendTierTotal        int          `json:"spend_tier_total"`
	NextSpendThresholdUSD *quota.Money `json:"next_spend_threshold_usd,omitempty"`
	NextSpendRemainingUSD *quota.Money `json:"next_spend_remaining_usd,omitempty"`
	SpendBonusDraws       int          `json:"spend_bonus_draws"`
	LuckyPoints           int          `json:"lucky_points"`
	LuckyMaxPoints        int          `json:"lucky_max_points"`
	DrawPurchaseCostUSD   *quota.Money `json:"draw_purchase_cost_usd,omitempty"`
	PurchasedToday        int          `json:"purchased_today"`
	PurchasePending       int          `json:"purchase_pending"`
	PurchaseUnknown       int          `json:"purchase_unknown"`
	PassUnlockCostUSD     *quota.Money `json:"pass_unlock_cost_usd,omitempty"`
	PassUnlocked          bool         `json:"pass_unlocked"`
	DayKey                string       `json:"day_key"`
	QueriedAt             time.Time    `json:"queried_at"`
}

type spendTier struct {
	ThresholdUSD float64
	Draws        int
}

func (r *Runner) QueryActivity(ctx context.Context, accountID string) (ActivityReport, error) {
	account, err := r.account(accountID)
	if err != nil {
		return ActivityReport{}, err
	}
	dashboard, err := r.Dashboard(ctx, account.ID)
	if err != nil {
		return ActivityReport{}, err
	}
	report, err := r.storeActivitySnapshot(account.ID, dashboard)
	if err != nil {
		return ActivityReport{}, err
	}
	return *report, nil
}

func buildActivityReport(accountID string, dashboard lottery.Dashboard, queriedAt time.Time, today string) ActivityReport {
	report := ActivityReport{
		AccountID: accountID,
		QueriedAt: queriedAt,
		DayKey:    strings.TrimSpace(today),
	}
	todaySpend := 0.0
	if dashboard.Eligibility.TodaySpend != nil {
		todaySpend = *dashboard.Eligibility.TodaySpend
	}
	report.TodaySpendUSD = USDMoney(todaySpend, "dashboard.eligibility.today_spend", queriedAt)
	tiers := parseSpendTiers(dashboard.Rules.SpendTiers)
	report.SpendTierTotal = len(tiers)
	for _, tier := range tiers {
		if todaySpend+1e-9 < tier.ThresholdUSD {
			threshold := USDMoney(tier.ThresholdUSD, "dashboard.spend_tiers.threshold", queriedAt)
			remaining := USDMoney(tier.ThresholdUSD-todaySpend, "dashboard.spend_tiers.remaining", queriedAt)
			report.NextSpendThresholdUSD = &threshold
			report.NextSpendRemainingUSD = &remaining
			break
		}
		report.SpendTierReached++
	}
	if bonusDraws, ok := dashboard.SpendBonusDraws(); ok {
		report.SpendBonusDraws = bonusDraws
	} else if report.SpendTierReached > 0 && report.SpendTierReached <= len(tiers) {
		report.SpendBonusDraws = tiers[report.SpendTierReached-1].Draws
	}
	report.LuckyPoints = firstMapInt(dashboard.Lucky, "points", "currentPoints")
	report.LuckyMaxPoints = firstMapInt(dashboard.Lucky, "maxPoints", "max")
	if cost, ok := firstMapFloat(dashboard.Purchase, "cost", "unitCost", "price"); ok {
		costMoney := USDMoney(cost, "dashboard.purchase.cost", queriedAt)
		report.DrawPurchaseCostUSD = &costMoney
	}
	report.PurchasedToday = firstMapInt(dashboard.Purchase, "purchasedToday", "todayPurchased")
	report.PurchasePending = firstMapInt(dashboard.Purchase, "pendingCount")
	report.PurchaseUnknown = firstMapInt(dashboard.Purchase, "unknownCount")

	limit, _ := dashboard.EffectiveDrawLimit()
	if cost := limit.UnlockCost; cost != nil {
		passCost := USDMoney(float64(*cost), "dashboard.draw_limit.unlock_cost", queriedAt)
		report.PassUnlockCostUSD = &passCost
	}
	report.PassUnlocked = boolOrDefault(limit.Unlocked, false)
	if dayKey := strings.TrimSpace(limit.DayKey); dayKey != "" {
		report.DayKey = dayKey
	}
	return report
}

func (r *Runner) storeActivitySnapshot(accountID string, dashboard lottery.Dashboard) (*ActivityReport, error) {
	report := buildActivityReport(accountID, dashboard, r.now().UTC(), r.today())
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	if err := r.store.PutSnapshot(state.Snapshot{
		AccountID: accountID,
		Kind:      "activity",
		Data:      payload,
		QueriedAt: report.QueriedAt,
	}); err != nil {
		return nil, err
	}
	return &report, nil
}

func parseSpendTiers(source []map[string]any) []spendTier {
	tiers := make([]spendTier, 0, len(source))
	for _, item := range source {
		threshold, ok := firstMapFloat(item, "threshold", "amount")
		if !ok {
			continue
		}
		draws := firstMapInt(item, "bonusDraws", "draws")
		tiers = append(tiers, spendTier{ThresholdUSD: threshold, Draws: draws})
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].ThresholdUSD < tiers[j].ThresholdUSD })
	return tiers
}

func firstMapInt(values map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := anyInt(value); ok {
				return parsed
			}
		}
	}
	return 0
}

func firstMapFloat(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := anyFloat(value); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

func anyInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float32:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func anyFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
