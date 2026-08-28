package service

import (
	"context"
	"fmt"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

type DailyClaimOutcome struct {
	Action          state.Action
	AlreadyRecorded bool
	Added           int
	Remaining       *int
}

func (r *Runner) ClaimDaily(ctx context.Context, accountID string) (DailyClaimOutcome, error) {
	if _, err := r.account(accountID); err != nil {
		return DailyClaimOutcome{}, err
	}
	date := r.today()
	releaseActionLock := r.store.LockAction(accountID, date, state.ActionDailyClaim)
	defer releaseActionLock()

	action, created, err := r.store.GetOrCreateAction(accountID, date, state.ActionDailyClaim)
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	if !created {
		switch {
		case action.Status == state.ActionCompleted:
			return dailyClaimRecordedOutcome(action), nil
		case action.Status == state.ActionFailed && action.Retryable && !action.SideEffectStarted:
			action, err = r.store.ResetRetryableAction(action.Key)
			if err != nil {
				return DailyClaimOutcome{}, err
			}
		case action.Status == state.ActionPending && !action.SideEffectStarted:
			// Resume a previously created-but-not-started claim after restart or same-process retry.
		case action.Status == state.ActionUnknown || action.SideEffectStarted:
			return r.reconcileDailyClaim(ctx, accountID, action)
		default:
			return dailyClaimRecordedOutcome(action), nil
		}
	}

	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return r.recordDailyClaimPreflightError(action, err)
	}
	beforeDashboard, sess, err := r.dashboardForClaim(ctx, client, accountID, sess)
	if err != nil {
		return r.recordDailyClaimPreflightError(action, err)
	}
	before, ok := beforeDashboard.Remaining()
	if !ok {
		return r.recordDailyClaimPreflightError(action, fmt.Errorf("抽奖次数接口没有返回 remaining"))
	}
	action, err = r.finishAction(action, func(value *state.Action) {
		value.ClaimBeforeRemaining = intPointer(before)
		value.ClaimAfterRemaining = nil
		value.Message = ""
		value.LastError = ""
		value.Retryable = false
	})
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	action, err = r.startAction(action)
	if err != nil {
		return DailyClaimOutcome{}, err
	}

	result, sess, err := r.claimWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return r.reconcileClaimAfterPostError(ctx, client, accountID, sess, action, before, err)
	}
	if !result.Success {
		updated, updateErr := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionFailed
			value.Retryable = true
			value.SideEffectStarted = false
			value.ClaimBeforeRemaining = intPointer(before)
			value.ClaimAfterRemaining = nil
			value.Message = firstNonEmpty(result.Message, "领取未成功")
			value.LastError = value.Message
		})
		if updateErr != nil {
			return DailyClaimOutcome{}, updateErr
		}
		return DailyClaimOutcome{
			Action:    updated,
			Added:     0,
			Remaining: intPointer(before),
		}, nil
	}

	afterDashboard := result.Dashboard
	if afterDashboard == nil {
		var dashboard lottery.Dashboard
		sess, dashboard, err = r.dashboardWithRecovery(ctx, client, accountID, sess)
		if err != nil {
			return r.reconcileClaimAfterPostError(ctx, client, accountID, sess, action, before, err)
		}
		afterDashboard = &dashboard
	}
	after, ok := afterDashboard.Remaining()
	if !ok {
		return r.reconcileClaimAfterPostError(ctx, client, accountID, sess, action, before, fmt.Errorf("抽奖次数接口没有返回 remaining"))
	}
	added := 0
	if after > before {
		added = after - before
	}
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.Retryable = false
		value.ClaimBeforeRemaining = intPointer(before)
		value.ClaimAfterRemaining = intPointer(after)
		value.Message = firstNonEmpty(result.Message, "今日领取成功")
		value.LastError = ""
	})
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	return DailyClaimOutcome{
		Action:    updated,
		Added:     added,
		Remaining: intPointer(after),
	}, nil
}

func (r *Runner) reconcileDailyClaim(ctx context.Context, accountID string, action state.Action) (DailyClaimOutcome, error) {
	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	sess, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	remaining, ok := dashboard.Remaining()
	if ok && action.ClaimBeforeRemaining != nil && remaining > *action.ClaimBeforeRemaining {
		added := remaining - *action.ClaimBeforeRemaining
		updated, err := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionCompleted
			value.Retryable = false
			value.ClaimAfterRemaining = intPointer(remaining)
			value.Message = "今日领取已对账完成"
			value.LastError = ""
		})
		if err != nil {
			return DailyClaimOutcome{}, err
		}
		return DailyClaimOutcome{
			Action:          updated,
			AlreadyRecorded: true,
			Added:           added,
			Remaining:       intPointer(remaining),
		}, nil
	}
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.Retryable = false
		value.Message = firstNonEmpty(value.Message, "领取结果未知")
	})
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	return DailyClaimOutcome{
		Action:          updated,
		AlreadyRecorded: true,
		Remaining:       copyIntPointerValue(ok, remaining),
	}, nil
}

func (r *Runner) reconcileClaimAfterPostError(ctx context.Context, client WebsiteClient, accountID string, sess session, action state.Action, before int, cause error) (DailyClaimOutcome, error) {
	sess, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err == nil {
		remaining, ok := dashboard.Remaining()
		if ok {
			if remaining > before {
				added := remaining - before
				updated, updateErr := r.finishAction(action, func(value *state.Action) {
					value.Status = state.ActionCompleted
					value.Retryable = false
					value.ClaimBeforeRemaining = intPointer(before)
					value.ClaimAfterRemaining = intPointer(remaining)
					value.Message = "今日领取已对账完成"
					value.LastError = ""
				})
				if updateErr != nil {
					return DailyClaimOutcome{}, updateErr
				}
				return DailyClaimOutcome{
					Action:          updated,
					AlreadyRecorded: true,
					Added:           added,
					Remaining:       intPointer(remaining),
				}, nil
			}
			updated, updateErr := r.finishAction(action, func(value *state.Action) {
				value.Status = state.ActionUnknown
				value.Retryable = false
				value.ClaimBeforeRemaining = intPointer(before)
				value.ClaimAfterRemaining = nil
				value.Message = safeError(cause)
				value.LastError = safeError(cause)
			})
			if updateErr != nil {
				return DailyClaimOutcome{}, updateErr
			}
			return DailyClaimOutcome{
				Action:    updated,
				Added:     0,
				Remaining: intPointer(remaining),
			}, nil
		}
	}
	updated, updateErr := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.Retryable = false
		value.ClaimBeforeRemaining = intPointer(before)
		value.ClaimAfterRemaining = nil
		value.Message = safeError(cause)
		value.LastError = safeError(cause)
	})
	if updateErr != nil {
		return DailyClaimOutcome{}, updateErr
	}
	return DailyClaimOutcome{Action: updated}, nil
}

func (r *Runner) recordDailyClaimPreflightError(action state.Action, cause error) (DailyClaimOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.Retryable = authRetryable(cause)
		value.SideEffectStarted = false
		value.Message = safeError(cause)
		value.LastError = value.Message
	})
	if err != nil {
		return DailyClaimOutcome{}, err
	}
	return DailyClaimOutcome{Action: updated}, nil
}

// acquireLotteryForAction obtains a lottery session plus a matching client
// for a side-effecting action.
func (r *Runner) acquireLotteryForAction(ctx context.Context, accountID string) (session, WebsiteClient, error) {
	sess, err := r.acquire(ctx, accountID, auth.SideEffect, auth.SessionLottery)
	if err != nil {
		return session{}, nil, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return session{}, nil, err
	}
	return sess, client, nil
}

// dashboardForClaim fetches the pre-claim dashboard with auth recovery.
func (r *Runner) dashboardForClaim(ctx context.Context, client WebsiteClient, accountID string, sess session) (lottery.Dashboard, session, error) {
	sess, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	return dashboard, sess, err
}

func dailyClaimRecordedOutcome(action state.Action) DailyClaimOutcome {
	return DailyClaimOutcome{
		Action:          action,
		AlreadyRecorded: true,
		Added:           dailyClaimAdded(action.ClaimBeforeRemaining, action.ClaimAfterRemaining),
		Remaining:       copyIntPointer(action.ClaimAfterRemaining),
	}
}

func dailyClaimAdded(before, after *int) int {
	if before == nil || after == nil || *after <= *before {
		return 0
	}
	return *after - *before
}

func copyIntPointer(pointer *int) *int {
	if pointer == nil {
		return nil
	}
	return intPointer(*pointer)
}

func copyIntPointerValue(ok bool, value int) *int {
	if !ok {
		return nil
	}
	return intPointer(value)
}
