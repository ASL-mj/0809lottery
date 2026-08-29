package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

// TokenUsageReport is the current account's ranking record for one period.
// The upstream native quota value is deliberately kept out of the public
// report; only its verified USD conversion is exposed.
type TokenUsageReport struct {
	AccountID                string      `json:"account_id"`
	Period                   string      `json:"period"`
	DayKey                   string      `json:"day_key"`
	TotalTokens              int64       `json:"total_tokens"`
	PromptTokens             int64       `json:"prompt_tokens"`
	CompletionTokens         int64       `json:"completion_tokens"`
	CallCount                int64       `json:"call_count"`
	ConsumedQuotaUSD         quota.Money `json:"consumed_quota_usd"`
	QuotaConversionAvailable bool        `json:"quota_conversion_available"`
	QuotaConversionError     string      `json:"quota_conversion_error,omitempty"`
	SourceUpdatedAt          *time.Time  `json:"source_updated_at,omitempty"`
	QueriedAt                time.Time   `json:"queried_at"`
}

// QueryTokenUsage reads the authenticated account row from the daily user
// rankings endpoint and stores a sanitized per-account snapshot.
func (r *Runner) QueryTokenUsage(ctx context.Context, accountID string) (TokenUsageReport, error) {
	account, err := r.account(accountID)
	if err != nil {
		return TokenUsageReport{}, err
	}
	sess, err := r.acquire(ctx, account.ID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return TokenUsageReport{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return TokenUsageReport{}, err
	}
	ranking, err := client.RankingsUsers(ctx, sess.token, sess.userID, lottery.RankingPeriodToday)
	if lottery.IsStatus(err, 401) {
		sess, err = r.renewParent(ctx, account.ID, sess.token)
		if err != nil {
			return TokenUsageReport{}, err
		}
		client, err = r.clientFor(sess)
		if err != nil {
			return TokenUsageReport{}, err
		}
		ranking, err = client.RankingsUsers(ctx, sess.token, sess.userID, lottery.RankingPeriodToday)
	}
	if err != nil {
		return TokenUsageReport{}, err
	}
	user, ok := ranking.FindUser(sess.userID)
	if !ok {
		return TokenUsageReport{}, fmt.Errorf("user rankings response did not contain the current account")
	}
	queriedAt := r.now().UTC()
	money := QuotaMoney(float64(user.TotalQuota), lottery.StatusSettings{QuotaPerUnit: ranking.Billing.QuotaPerUnit}, "rankings.total_quota", queriedAt)
	report := TokenUsageReport{
		AccountID:                account.ID,
		Period:                   ranking.Period,
		DayKey:                   r.today(),
		TotalTokens:              user.TotalTokens,
		PromptTokens:             user.PromptTokens,
		CompletionTokens:         user.CompletionTokens,
		CallCount:                user.CallCount,
		ConsumedQuotaUSD:         money,
		QuotaConversionAvailable: money.State == quota.StateConfirmed,
		QueriedAt:                queriedAt,
	}
	if !report.QuotaConversionAvailable {
		report.QuotaConversionError = "无法获取美元换算配置"
	}
	if !ranking.UpdatedAt.IsZero() {
		updatedAt := ranking.UpdatedAt.UTC()
		report.SourceUpdatedAt = &updatedAt
	}
	if strings.TrimSpace(report.Period) == "" {
		report.Period = lottery.RankingPeriodToday
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return TokenUsageReport{}, err
	}
	if err := r.store.PutSnapshot(state.Snapshot{
		AccountID: account.ID,
		Kind:      "token-usage",
		Data:      payload,
		QueriedAt: report.QueriedAt,
	}); err != nil {
		return TokenUsageReport{}, err
	}
	return report, nil
}
