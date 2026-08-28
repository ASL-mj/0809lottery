package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
)

type SubscriptionItem struct {
	ID                 int       `json:"id"`
	PlanTitle          string    `json:"plan_title"`
	TotalAmount        int64     `json:"-"`
	RemainingAmount    int64     `json:"-"`
	TotalAmountUSD     *float64  `json:"total_usd,omitempty"`
	RemainingAmountUSD *float64  `json:"remaining_usd,omitempty"`
	EndTime            time.Time `json:"end_time"`
	Unlimited          bool      `json:"unlimited"`
}

type AccountSubscriptionReport struct {
	Account           string             `json:"account"`
	Subscriptions     []SubscriptionItem `json:"subscriptions"`
	RemainingTotal    int64              `json:"-"`
	RemainingTotalUSD *float64           `json:"remaining_total_usd,omitempty"`
	HasUnlimited      bool               `json:"has_unlimited"`
	QueryError        string             `json:"query_error"`
}

type SubscriptionReport struct {
	Accounts                   []AccountSubscriptionReport `json:"accounts"`
	DisplaySettings            lottery.StatusSettings      `json:"-"`
	HasDisplaySettings         bool                        `json:"-"`
	ActiveAccountCount         int                         `json:"active_account_count"`
	ActiveSubscriptionCount    int                         `json:"active_subscription_count"`
	FiniteTotalAmount          int64                       `json:"-"`
	FiniteRemainingAmount      int64                       `json:"-"`
	FiniteTotalAmountUSD       *float64                    `json:"finite_total_usd,omitempty"`
	FiniteRemainingAmountUSD   *float64                    `json:"finite_remaining_usd,omitempty"`
	UnlimitedSubscriptionCount int                         `json:"unlimited_subscription_count"`
}

// QuerySubscriptions reads active subscriptions for one account or all
// configured accounts. Per-account failures are returned inside the report so
// one broken login does not hide the other accounts' results.
func (r *Runner) QuerySubscriptions(ctx context.Context, accountID string) (SubscriptionReport, error) {
	accountIDs, err := r.subscriptionAccountIDs(accountID)
	if err != nil {
		return SubscriptionReport{}, err
	}

	report := SubscriptionReport{
		Accounts: make([]AccountSubscriptionReport, 0, len(accountIDs)),
	}
	for _, id := range accountIDs {
		accountReport, settings, settingsOK := r.queryAccountSubscriptions(ctx, id)
		if settingsOK && !report.HasDisplaySettings {
			report.DisplaySettings = settings
			report.HasDisplaySettings = true
		}
		report.Accounts = append(report.Accounts, accountReport)
		if len(accountReport.Subscriptions) > 0 {
			report.ActiveAccountCount++
		}
		report.ActiveSubscriptionCount += len(accountReport.Subscriptions)
		for _, subscription := range accountReport.Subscriptions {
			if subscription.Unlimited {
				report.UnlimitedSubscriptionCount++
				continue
			}
			report.FiniteTotalAmount += subscription.TotalAmount
			report.FiniteRemainingAmount += subscription.RemainingAmount
		}
	}
	if report.HasDisplaySettings {
		report.FiniteTotalAmountUSD, _ = QuotaUSD(report.FiniteTotalAmount, report.DisplaySettings)
		report.FiniteRemainingAmountUSD, _ = QuotaUSD(report.FiniteRemainingAmount, report.DisplaySettings)
		for index := range report.Accounts {
			account := &report.Accounts[index]
			account.RemainingTotalUSD, _ = QuotaUSD(account.RemainingTotal, report.DisplaySettings)
			for subscriptionIndex := range account.Subscriptions {
				subscription := &account.Subscriptions[subscriptionIndex]
				subscription.TotalAmountUSD, _ = QuotaUSD(subscription.TotalAmount, report.DisplaySettings)
				subscription.RemainingAmountUSD, _ = QuotaUSD(subscription.RemainingAmount, report.DisplaySettings)
			}
		}
	}
	return report, nil
}

func (r *Runner) subscriptionAccountIDs(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "all" {
		records, err := r.repo.List()
		if err != nil {
			return nil, err
		}
		result := make([]string, 0, len(records))
		for _, record := range records {
			result = append(result, record.ID)
		}
		return result, nil
	}
	if _, err := r.repo.Get(value); err != nil {
		return nil, fmt.Errorf("unknown account %q", value)
	}
	return []string{value}, nil
}

func (r *Runner) queryAccountSubscriptions(ctx context.Context, accountID string) (AccountSubscriptionReport, lottery.StatusSettings, bool) {
	if _, err := r.account(accountID); err != nil {
		return AccountSubscriptionReport{Account: accountID, QueryError: safeError(err)}, lottery.StatusSettings{}, false
	}
	report := AccountSubscriptionReport{Account: accountID}

	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		report.QueryError = safeError(err)
		return report, lottery.StatusSettings{}, false
	}
	client, err := r.clientFor(sess)
	if err != nil {
		report.QueryError = safeError(fmt.Errorf("create website client: %w", err))
		return report, lottery.StatusSettings{}, false
	}

	plans, subscriptions, settings, settingsOK, err := fetchSubscriptionData(ctx, client, sess.token)
	if err != nil {
		if subscriptionAuthError(err) {
			renewed, renewErr := r.renewParent(ctx, accountID, sess.token)
			if renewErr == nil {
				if retryClient, clientErr := r.clientFor(renewed); clientErr == nil {
					plans, subscriptions, settings, settingsOK, err = fetchSubscriptionData(ctx, retryClient, renewed.token)
				} else {
					err = clientErr
				}
			} else {
				err = renewErr
			}
		}
	}
	if err != nil {
		report.QueryError = safeError(err)
		return report, settings, settingsOK
	}

	now := r.now().Unix()
	for _, summary := range subscriptions {
		subscription := summary.Subscription
		if subscription == nil || subscription.Status != "active" || subscription.EndTime <= now {
			continue
		}
		unlimited := subscription.AmountTotal <= 0
		remaining := int64(0)
		if !unlimited {
			remaining = subscription.AmountTotal - subscription.AmountUsed
			if remaining < 0 {
				remaining = 0
			}
		}
		report.Subscriptions = append(report.Subscriptions, SubscriptionItem{
			ID:              subscription.ID,
			PlanTitle:       firstNonEmpty(plans[subscription.PlanID], fmt.Sprintf("订阅 #%d", subscription.ID)),
			TotalAmount:     subscription.AmountTotal,
			RemainingAmount: remaining,
			EndTime:         time.Unix(subscription.EndTime, 0).In(shanghaiLocation),
			Unlimited:       unlimited,
		})
		if unlimited {
			report.HasUnlimited = true
		} else {
			report.RemainingTotal += remaining
		}
	}
	sort.Slice(report.Subscriptions, func(i, j int) bool {
		if report.Subscriptions[i].EndTime.Equal(report.Subscriptions[j].EndTime) {
			return report.Subscriptions[i].ID < report.Subscriptions[j].ID
		}
		return report.Subscriptions[i].EndTime.Before(report.Subscriptions[j].EndTime)
	})
	return report, settings, settingsOK
}

func fetchSubscriptionData(ctx context.Context, client WebsiteClient, parentToken string) (map[int]string, []lottery.SubscriptionSummary, lottery.StatusSettings, bool, error) {
	plans, err := client.SubscriptionPlans(ctx, parentToken)
	if err != nil {
		return nil, nil, lottery.StatusSettings{}, false, err
	}
	self, err := client.SubscriptionSelf(ctx, parentToken)
	if err != nil {
		return nil, nil, lottery.StatusSettings{}, false, err
	}
	settings, statusErr := client.Status(ctx, parentToken)
	if statusErr != nil {
		if subscriptionAuthError(statusErr) {
			return nil, nil, lottery.StatusSettings{}, false, statusErr
		}
		// Subscription data is still useful when the public display settings
		// endpoint is temporarily unavailable; the formatter will use native units.
		return plans, self.Subscriptions, lottery.StatusSettings{}, false, nil
	}
	return plans, self.Subscriptions, settings, true, nil
}

func subscriptionAuthError(err error) bool {
	return lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403)
}
