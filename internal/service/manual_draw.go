package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/config"
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

func (r *Runner) DrawAvailable(ctx context.Context, accountID, idempotencyKey string) (DrawAvailableOutcome, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return DrawAvailableOutcome{}, fmt.Errorf("手动抽奖幂等键不能为空")
	}

	account, err := r.account(accountID)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	release := r.store.LockAction(account.ID, r.today(), state.ActionKind("manual_draw"))
	defer release()

	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return DrawAvailableOutcome{}, fmt.Errorf("create website client: %w", err)
	}
	auth, lotteryToken, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	dashboard, auth, lotteryToken, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	remaining, ok := dashboard.Remaining()
	if !ok {
		return DrawAvailableOutcome{}, fmt.Errorf("抽奖次数接口没有返回 remaining")
	}
	if remaining <= 0 {
		r.putManualDrawAuthBestEffort(account.ID, auth)
		return DrawAvailableOutcome{
			Skipped:         true,
			RemainingBefore: remaining,
			Message:         "当前没有可用抽奖次数，已跳过手动抽奖",
		}, nil
	}

	result, auth, err := r.drawDirectWithRecovery(ctx, client, account, auth, lotteryToken, idempotencyKey)
	if err != nil {
		return DrawAvailableOutcome{}, err
	}
	quotaDeltaUSD, auth := r.quotaDeltaUSDForDraw(ctx, client, account, auth, result)
	r.putManualDrawAuthBestEffort(account.ID, auth)
	return DrawAvailableOutcome{
		RemainingBefore: remaining,
		Result:          &result,
		QuotaDeltaUSD:   quotaDeltaUSD,
		Message:         "手动抽奖成功",
	}, nil
}

func (r *Runner) drawDirectWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken, idempotencyKey string) (lottery.DrawResult, state.AuthState, error) {
	result, err := client.Draw(ctx, lotteryToken, idempotencyKey)
	if err == nil {
		return result, auth, nil
	}
	if lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403) {
		auth.LotteryAccessToken = ""
		auth.LotteryAccessExpiresAt = time.Time{}
		auth, lotteryToken, err = r.ensureLotteryToken(ctx, client, account, auth)
		if err != nil {
			return lottery.DrawResult{}, auth, err
		}
		result, err = client.Draw(ctx, lotteryToken, idempotencyKey)
		return result, auth, err
	}
	if !lottery.IsTransient(err) {
		return lottery.DrawResult{}, auth, err
	}
	if err := r.wait(ctx, 2*time.Second); err != nil {
		return lottery.DrawResult{}, auth, err
	}
	result, err = client.Draw(ctx, lotteryToken, idempotencyKey)
	return result, auth, err
}

func (r *Runner) quotaDeltaUSDForDraw(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, result lottery.DrawResult) (*float64, state.AuthState) {
	if result.Effect.QuotaDelta == 0 {
		return nil, auth
	}

	auth, parentToken, ok := r.ensureParentTokenForDrawStatus(ctx, client, account, auth)
	if !ok {
		return nil, auth
	}
	settings, err := client.Status(ctx, parentToken)
	if err != nil && subscriptionAuthError(err) {
		auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
		ok = err == nil
		if !ok {
			return nil, auth
		}
		settings, err = client.Status(ctx, parentToken)
	}
	if err != nil {
		return nil, auth
	}
	quotaDeltaUSD, ok := QuotaAmountUSD(result.Effect.QuotaDelta, settings)
	if !ok {
		return nil, auth
	}
	return quotaDeltaUSD, auth
}

func (r *Runner) ensureParentTokenForDrawStatus(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState) (state.AuthState, string, bool) {
	originalLotteryToken := auth.LotteryAccessToken
	originalLotteryExpiry := auth.LotteryAccessExpiresAt

	refreshedAuth, parentToken, err := r.ensureParentToken(ctx, client, account, auth)
	if err != nil {
		return auth, "", false
	}
	if strings.TrimSpace(refreshedAuth.LotteryAccessToken) == "" && strings.TrimSpace(originalLotteryToken) != "" {
		refreshedAuth.LotteryAccessToken = originalLotteryToken
		refreshedAuth.LotteryAccessExpiresAt = originalLotteryExpiry
	}
	return refreshedAuth, parentToken, true
}

func (r *Runner) putManualDrawAuthBestEffort(accountID string, auth state.AuthState) {
	_ = r.store.PutAuth(accountID, auth)
}
