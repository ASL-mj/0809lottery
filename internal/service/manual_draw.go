package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

type DrawAvailableOutcome struct {
	Skipped         bool
	RemainingBefore int
	Result          *lottery.DrawResult `json:"-"`
	QuotaDeltaUSD   *quota.Money
	Message         string
}

// DrawAvailable performs one idempotent manual draw. Callers may override the
// authentication intent: the scheduler uses auth.ScheduledAutomation while web
// requests default to auth.SideEffect.
func (r *Runner) DrawAvailable(ctx context.Context, accountID, idempotencyKey string, intents ...auth.Intent) (DrawAvailableOutcome, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return DrawAvailableOutcome{}, fmt.Errorf("手动抽奖幂等键不能为空")
	}
	intent := auth.SideEffect
	if len(intents) > 0 {
		intent = intents[0]
	}

	if _, err := r.account(accountID); err != nil {
		return DrawAvailableOutcome{}, err
	}
	release := r.store.LockAction(accountID, r.today(), state.ActionKind("manual_draw"))
	defer release()

	sess, err := r.acquire(ctx, accountID, intent, auth.SessionLottery)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	sess, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	remaining, ok := dashboard.Remaining()
	if !ok {
		return DrawAvailableOutcome{}, fmt.Errorf("抽奖次数接口没有返回 remaining")
	}
	if remaining <= 0 {
		return DrawAvailableOutcome{
			Skipped:         true,
			RemainingBefore: remaining,
			Message:         "当前没有可用抽奖次数，已跳过手动抽奖",
		}, nil
	}

	result, err := client.Draw(ctx, sess.token, idempotencyKey)
	if err != nil {
		if lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403) {
			sess, err = r.renewLottery(ctx, accountID, sess.token)
			if err == nil {
				retryClient, clientErr := r.clientFor(sess)
				if clientErr == nil {
					result, err = retryClient.Draw(ctx, sess.token, idempotencyKey)
				} else {
					err = clientErr
				}
			}
		} else if lottery.IsTransient(err) {
			if waitErr := r.wait(ctx, 2*time.Second); waitErr == nil {
				result, err = client.Draw(ctx, sess.token, idempotencyKey)
			} else {
				err = waitErr
			}
		}
		if err != nil {
			return DrawAvailableOutcome{}, err
		}
	}
	quotaDelta := r.quotaDeltaMoneyForDraw(ctx, client, accountID, result)
	return DrawAvailableOutcome{
		RemainingBefore: remaining,
		Result:          &result,
		QuotaDeltaUSD:   quotaDelta,
		Message:         "手动抽奖成功",
	}, nil
}

// DrawAvailableScheduled is the scheduler's entry point; it runs with the
// ScheduledAutomation intent so it can never trigger a password login.
func (r *Runner) DrawAvailableScheduled(ctx context.Context, accountID, idempotencyKey string) (DrawAvailableOutcome, error) {
	return r.DrawAvailable(ctx, accountID, idempotencyKey, auth.ScheduledAutomation)
}

// quotaDeltaMoneyForDraw resolves the dollar value of a draw reward on a
// best-effort basis; a failing parent session never invalidates the draw.
// Without a verified conversion rule the snapshot is explicitly unavailable.
func (r *Runner) quotaDeltaMoneyForDraw(ctx context.Context, client WebsiteClient, accountID string, result lottery.DrawResult) *quota.Money {
	quotaAmount, source, alreadyUSD, ok := drawResultQuotaAmount(result)
	if !ok {
		return nil
	}
	observedAt := r.now().UTC()
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		delta := quota.UnavailableUSD(source, observedAt)
		return &delta
	}
	settings, statusErr := client.Status(ctx, sess.token)
	if statusErr != nil && subscriptionAuthError(statusErr) {
		renewed, renewErr := r.renewParent(ctx, accountID, sess.token)
		if renewErr != nil {
			delta := quota.UnavailableUSD(source, observedAt)
			return &delta
		}
		retryClient, clientErr := r.clientFor(renewed)
		if clientErr != nil {
			delta := quota.UnavailableUSD(source, observedAt)
			return &delta
		}
		settings, statusErr = retryClient.Status(ctx, renewed.token)
		client = retryClient
	}
	if statusErr != nil {
		delta := quota.UnavailableUSD(source, observedAt)
		return &delta
	}
	delta := QuotaMoney(quotaAmount, settings, source, observedAt)
	if alreadyUSD {
		delta = USDMoney(quotaAmount, source, observedAt)
	}
	return &delta
}

func drawResultQuotaAmount(result lottery.DrawResult) (float64, string, bool, bool) {
	if result.Prize.QuotaAmount > 0 {
		return result.Prize.QuotaAmount, "draw.prize.quota_amount", true, true
	}
	if result.Effect.QuotaDelta > 0 {
		return result.Effect.QuotaDelta, "draw.effect.quota_delta", false, true
	}
	return 0, "", false, false
}
