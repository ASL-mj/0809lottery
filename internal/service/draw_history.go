package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

// DrawHistoryRecord is the public, persisted representation of one platform
// lottery result. It deliberately excludes balances, wheel indexes and other
// dashboard internals.
type DrawHistoryRecord struct {
	ID                 string       `json:"id,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	Status             string       `json:"status,omitempty"`
	FulfillmentStatus  string       `json:"fulfillment_status,omitempty"`
	FulfillmentMessage string       `json:"fulfillment_message,omitempty"`
	PrizeLabel         string       `json:"prize_label,omitempty"`
	PrizeShortLabel    string       `json:"prize_short_label,omitempty"`
	PrizeDescription   string       `json:"prize_description,omitempty"`
	PrizeGrade         string       `json:"prize_grade,omitempty"`
	PrizeValidityHours int          `json:"prize_validity_hours,omitempty"`
	EffectSummary      string       `json:"effect_summary,omitempty"`
	EffectAccessLabel  string       `json:"effect_access_label,omitempty"`
	ExpiresAt          *time.Time   `json:"expires_at,omitempty"`
	QuotaDeltaUSD      *quota.Money `json:"quota_delta_usd,omitempty"`
}

type DrawHistoryReport struct {
	AccountID string              `json:"account_id"`
	History   []DrawHistoryRecord `json:"history"`
	QueriedAt time.Time           `json:"queried_at"`
}

// QueryDrawHistory fetches the platform dashboard using the cached lottery
// session, converts reward amounts to USD when the current conversion policy is
// available, and stores only the safe history projection.
func (r *Runner) QueryDrawHistory(ctx context.Context, accountID string) (DrawHistoryReport, error) {
	if _, err := r.account(accountID); err != nil {
		return DrawHistoryReport{}, err
	}
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionLottery)
	if err != nil {
		return DrawHistoryReport{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return DrawHistoryReport{}, err
	}
	_, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return DrawHistoryReport{}, err
	}

	var settings lottery.StatusSettings
	if len(dashboard.History) > 0 {
		settings, _ = r.statusSettingsForDisplay(ctx, accountID)
	}
	queriedAt := r.now().UTC()
	report := DrawHistoryReport{
		AccountID: accountID,
		History:   make([]DrawHistoryRecord, 0, len(dashboard.History)),
		QueriedAt: queriedAt,
	}
	for _, entry := range dashboard.History {
		createdAt := entry.CreatedAt.UTC()
		if createdAt.IsZero() {
			createdAt = queriedAt
		}
		validityHours := entry.Prize.ValidityHours
		if validityHours == 0 {
			validityHours = entry.Effect.ValidityHours
		}
		record := DrawHistoryRecord{
			ID:                 safeHistoryText(entry.ID, 128),
			CreatedAt:          createdAt,
			Status:             safeHistoryText(entry.Status, 64),
			FulfillmentStatus:  safeHistoryText(entry.FulfillmentStatus, 64),
			FulfillmentMessage: safeHistoryText(entry.FulfillmentMessage, 256),
			PrizeLabel:         safeHistoryText(entry.Prize.Label, 256),
			PrizeShortLabel:    safeHistoryText(entry.Prize.ShortLabel, 128),
			PrizeDescription:   safeHistoryText(entry.Prize.Description, 256),
			PrizeGrade:         safeHistoryText(entry.Prize.Grade, 32),
			PrizeValidityHours: validityHours,
			EffectSummary:      safeHistoryText(entry.Effect.Summary, 256),
			EffectAccessLabel:  safeHistoryText(firstNonEmpty(entry.Effect.AccessLabel, entry.Effect.KeyLabel), 64),
		}
		if !entry.Effect.ExpiresAt.IsZero() {
			expiresAt := entry.Effect.ExpiresAt.UTC()
			record.ExpiresAt = &expiresAt
		}
		if quotaAmount, source, alreadyUSD, ok := drawHistoryQuotaAmount(entry); ok {
			money := QuotaMoney(quotaAmount, settings, source, queriedAt)
			if alreadyUSD {
				money = USDMoney(quotaAmount, source, queriedAt)
			}
			record.QuotaDeltaUSD = &money
		}
		report.History = append(report.History, record)
	}

	payload, err := json.Marshal(report)
	if err != nil {
		return DrawHistoryReport{}, err
	}
	if err := r.store.PutSnapshot(state.Snapshot{
		AccountID: accountID,
		Kind:      "draw-history",
		Data:      payload,
		QueriedAt: queriedAt,
	}); err != nil {
		return DrawHistoryReport{}, err
	}
	return report, nil
}

// drawHistoryQuotaAmount resolves the reward amount from the prize itself.
// The effect delta describes the applied balance change and is retained as a
// fallback for older or partial history responses that omit prize.quotaAmount.
func drawHistoryQuotaAmount(entry lottery.DrawHistoryEntry) (float64, string, bool, bool) {
	if entry.Prize.QuotaAmount > 0 {
		return entry.Prize.QuotaAmount, "draw.history.prize.quota_amount", true, true
	}
	if entry.Effect.QuotaDelta > 0 {
		return entry.Effect.QuotaDelta, "draw.history.effect.quota_delta", false, true
	}
	return 0, "", false, false
}

func (r *Runner) statusSettingsForDisplay(ctx context.Context, accountID string) (lottery.StatusSettings, bool) {
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return lottery.StatusSettings{}, false
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return lottery.StatusSettings{}, false
	}
	settings, err := client.Status(ctx, sess.token)
	if err != nil && subscriptionAuthError(err) {
		sess, err = r.renewParent(ctx, accountID, sess.token)
		if err == nil {
			client, err = r.clientFor(sess)
			if err == nil {
				settings, err = client.Status(ctx, sess.token)
			}
		}
	}
	if err != nil {
		return lottery.StatusSettings{}, false
	}
	return settings, true
}

func safeHistoryText(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	lower := strings.ToLower(value)
	for _, marker := range []string{"bearer ", "cookie", "password", "idempotency", "access_token", "parent_access", "lottery_access"} {
		if strings.Contains(lower, marker) {
			return "已隐藏敏感详情"
		}
	}
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
