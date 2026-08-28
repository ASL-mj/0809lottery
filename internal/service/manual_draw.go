package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

type DrawAvailableOutcome struct {
	Skipped         bool
	RemainingBefore int
	Result          *lottery.DrawResult `json:"-"`
	QuotaDeltaUSD   *float64
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
	quotaDeltaUSD := r.quotaDeltaUSDForDraw(ctx, client, accountID, result)
	return DrawAvailableOutcome{
		RemainingBefore: remaining,
		Result:          &result,
		QuotaDeltaUSD:   quotaDeltaUSD,
		Message:         "手动抽奖成功",
	}, nil
}

// DrawAvailableScheduled is the scheduler's entry point; it runs with the
// ScheduledAutomation intent so it can never trigger a password login.
func (r *Runner) DrawAvailableScheduled(ctx context.Context, accountID, idempotencyKey string) (DrawAvailableOutcome, error) {
	return r.DrawAvailable(ctx, accountID, idempotencyKey, auth.ScheduledAutomation)
}

// quotaDeltaUSDForDraw resolves the dollar value of a draw reward on a
// best-effort basis; a failing parent session never invalidates the draw.
func (r *Runner) quotaDeltaUSDForDraw(ctx context.Context, client WebsiteClient, accountID string, result lottery.DrawResult) *float64 {
	if result.Effect.QuotaDelta == 0 {
		return nil
	}
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return nil
	}
	settings, statusErr := client.Status(ctx, sess.token)
	if statusErr != nil && subscriptionAuthError(statusErr) {
		renewed, renewErr := r.renewParent(ctx, accountID, sess.token)
		if renewErr != nil {
			return nil
		}
		retryClient, clientErr := r.clientFor(renewed)
		if clientErr != nil {
			return nil
		}
		settings, statusErr = retryClient.Status(ctx, renewed.token)
		sess = renewed
		client = retryClient
	}
	if statusErr != nil {
		return nil
	}
	quotaDeltaUSD, ok := QuotaAmountUSD(result.Effect.QuotaDelta, settings)
	if !ok {
		return nil
	}
	return quotaDeltaUSD
}
