package service

import (
	"context"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

type PurchaseOutcome struct {
	AccountID       string          `json:"account_id"`
	Status          string          `json:"status"`
	Message         string          `json:"message"`
	PriceUSD        *quota.Money    `json:"price_usd,omitempty"`
	Remaining       *int            `json:"remaining,omitempty"`
	Activity        *ActivityReport `json:"activity,omitempty"`
	Action          state.Action    `json:"-"`
	AlreadyRecorded bool            `json:"-"`
}

func (r *Runner) PurchaseDraw(ctx context.Context, accountID string) (PurchaseOutcome, error) {
	if _, err := r.account(accountID); err != nil {
		return PurchaseOutcome{}, err
	}
	date := r.today()
	callStarted := time.Now().UTC()
	release := r.store.LockAction(accountID, date, state.ActionDrawPurchase)
	defer release()

	action, created, err := r.store.GetOrCreateAction(accountID, date, state.ActionDrawPurchase)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if !created {
		switch {
		case action.Status == state.ActionUnknown || action.SideEffectStarted:
			return r.reconcilePurchaseDraw(ctx, accountID, action)
		case action.Status == state.ActionCompleted:
			if !action.UpdatedAt.Before(callStarted) {
				return purchaseOutcomeFromAction(accountID, action, nil, nil, true), nil
			}
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		case action.Status == state.ActionFailed && action.Retryable && !action.SideEffectStarted:
			if !action.UpdatedAt.Before(callStarted) {
				return purchaseOutcomeFromAction(accountID, action, nil, nil, true), nil
			}
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		case action.Status == state.ActionPending && !action.SideEffectStarted:
			// Resume a created-but-not-started purchase after restart.
		default:
			return purchaseOutcomeFromAction(accountID, action, nil, nil, true), nil
		}
	}

	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return r.recordPurchasePreflightError(accountID, action, nil, err)
	}
	sess, beforeDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return r.recordPurchasePreflightError(accountID, action, nil, err)
	}
	price := r.dashboardPurchasePrice(beforeDashboard)
	beforeRemaining := dashboardRemainingPointer(beforeDashboard)
	beforePurchasedRemaining := dashboardPurchasedRemainingPointer(beforeDashboard)
	beforeToday := dashboardPurchasedToday(beforeDashboard)
	action, err = r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyMoneyPointer(price)
		value.PurchaseBeforeToday = intPointer(beforeToday)
		value.PurchaseBeforeRemaining = copyIntPointer(beforePurchasedRemaining)
		value.PassBeforeUnlocked = nil
		value.Message = ""
		value.LastError = ""
		value.Retryable = false
	})
	if err != nil {
		return PurchaseOutcome{}, err
	}
	action, err = r.startAction(action)
	if err != nil {
		return PurchaseOutcome{}, err
	}

	result, sess, err := r.purchaseDrawWithRecovery(ctx, client, accountID, sess, action.IdempotencyKey)
	if err != nil {
		if isExplicitInsufficient(err) {
			updated, updateErr := r.finishAction(action, func(value *state.Action) {
				value.Status = state.ActionFailed
				value.Retryable = true
				value.SideEffectStarted = false
				value.Message = safeError(err)
				value.LastError = value.Message
			})
			if updateErr != nil {
				return PurchaseOutcome{}, updateErr
			}
			return purchaseOutcomeFromAction(accountID, updated, beforeRemaining, nil, false), nil
		}
		return r.reconcilePurchaseAfterPostError(ctx, client, accountID, sess, action, price, err)
	}

	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return r.finishPurchaseWithoutProof(accountID, action, price, nil, result.Status, safeError(err))
	}
	return r.finishPurchaseDrawFromDashboard(accountID, action, price, afterDashboard, result.Status, "")
}

func (r *Runner) UnlockDailyPass(ctx context.Context, accountID string) (PurchaseOutcome, error) {
	if _, err := r.account(accountID); err != nil {
		return PurchaseOutcome{}, err
	}
	date := r.today()
	release := r.store.LockAction(accountID, date, state.ActionPassUnlock)
	defer release()

	action, created, err := r.store.GetOrCreateAction(accountID, date, state.ActionPassUnlock)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if !created {
		switch {
		case action.Status == state.ActionCompleted:
			return purchaseOutcomeFromAction(accountID, action, nil, nil, true), nil
		case action.Status == state.ActionUnknown || action.SideEffectStarted:
			return r.reconcileUnlockDailyPass(ctx, accountID, action)
		case action.Status == state.ActionPending && !action.SideEffectStarted:
			// Resume a created-but-not-started unlock after restart.
		case action.Status == state.ActionFailed && action.Retryable && !action.SideEffectStarted:
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		default:
			return purchaseOutcomeFromAction(accountID, action, nil, nil, true), nil
		}
	}

	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return r.recordPurchasePreflightError(accountID, action, nil, err)
	}
	sess, beforeDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return r.recordPurchasePreflightError(accountID, action, nil, err)
	}
	price := r.dashboardPassPrice(beforeDashboard)
	beforeUnlocked := dashboardPassUnlockedForToday(beforeDashboard, r.today())
	beforeRemaining := dashboardRemainingPointer(beforeDashboard)
	action, err = r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyMoneyPointer(price)
		value.PassBeforeUnlocked = boolPointerValue(beforeUnlocked)
		value.PurchaseBeforeToday = nil
		value.PurchaseBeforeRemaining = nil
		value.Message = ""
		value.LastError = ""
		value.Retryable = false
	})
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if beforeUnlocked {
		updated, updateErr := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionCompleted
			value.SideEffectStarted = false
			value.Retryable = false
			value.Message = "今日通行证已解锁"
			value.LastError = ""
		})
		if updateErr != nil {
			return PurchaseOutcome{}, updateErr
		}
		report, snapshotErr := r.storeActivitySnapshot(accountID, beforeDashboard)
		if snapshotErr != nil {
			return PurchaseOutcome{}, snapshotErr
		}
		return purchaseOutcomeFromAction(accountID, updated, beforeRemaining, report, false), nil
	}
	action, err = r.startAction(action)
	if err != nil {
		return PurchaseOutcome{}, err
	}

	result, sess, err := r.unlockDailyPassWithRecovery(ctx, client, accountID, sess, action.IdempotencyKey)
	if err != nil {
		if isExplicitInsufficient(err) {
			updated, updateErr := r.finishAction(action, func(value *state.Action) {
				value.Status = state.ActionFailed
				value.Retryable = true
				value.SideEffectStarted = false
				value.Message = safeError(err)
				value.LastError = value.Message
			})
			if updateErr != nil {
				return PurchaseOutcome{}, updateErr
			}
			return purchaseOutcomeFromAction(accountID, updated, beforeRemaining, nil, false), nil
		}
		return r.reconcileUnlockAfterPostError(ctx, client, accountID, sess, action, price, err)
	}

	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return r.finishPurchaseWithoutProof(accountID, action, price, nil, result.Status, safeError(err))
	}
	return r.finishUnlockFromDashboard(accountID, action, price, afterDashboard, result.Status, "")
}

func (r *Runner) reconcilePurchaseDraw(ctx context.Context, accountID string, action state.Action) (PurchaseOutcome, error) {
	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if purchaseDashboardProvesSuccess(action, afterDashboard) {
		return r.finishPurchaseDrawFromDashboard(accountID, action, copyMoneyPointer(action.PriceUSD), afterDashboard, "", "购买抽奖次数已对账完成")
	}
	if action.Status == state.ActionPending {
		updated, updateErr := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionPending
			value.SideEffectStarted = true
			value.Retryable = false
			value.Message = firstNonEmpty(value.Message, "购买处理中")
		})
		if updateErr != nil {
			return PurchaseOutcome{}, updateErr
		}
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
	}
	updated, updateErr := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.SideEffectStarted = true
		value.Retryable = false
		value.Message = firstNonEmpty(value.Message, "购买结果未知")
	})
	if updateErr != nil {
		return PurchaseOutcome{}, updateErr
	}
	return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
}

func (r *Runner) reconcileUnlockDailyPass(ctx context.Context, accountID string, action state.Action) (PurchaseOutcome, error) {
	sess, client, err := r.acquireLotteryForAction(ctx, accountID)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if dashboardPassUnlockedForToday(afterDashboard, r.today()) {
		return r.finishUnlockFromDashboard(accountID, action, copyMoneyPointer(action.PriceUSD), afterDashboard, "", "今日通行证已对账完成")
	}
	if action.Status == state.ActionPending {
		updated, updateErr := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionPending
			value.SideEffectStarted = true
			value.Retryable = false
			value.Message = firstNonEmpty(value.Message, "通行证解锁处理中")
		})
		if updateErr != nil {
			return PurchaseOutcome{}, updateErr
		}
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
	}
	updated, updateErr := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionUnknown
		value.SideEffectStarted = true
		value.Retryable = false
		value.Message = firstNonEmpty(value.Message, "通行证解锁结果未知")
	})
	if updateErr != nil {
		return PurchaseOutcome{}, updateErr
	}
	return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
}

func (r *Runner) reconcilePurchaseAfterPostError(ctx context.Context, client WebsiteClient, accountID string, sess session, action state.Action, price *quota.Money, cause error) (PurchaseOutcome, error) {
	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err == nil {
		if purchaseDashboardProvesSuccess(action, afterDashboard) {
			return r.finishPurchaseDrawFromDashboard(accountID, action, price, afterDashboard, "", "购买抽奖次数已对账完成")
		}
		return r.finishPurchaseWithoutProof(accountID, action, price, dashboardRemainingPointer(afterDashboard), "", safeError(cause))
	}
	return r.finishPurchaseWithoutProof(accountID, action, price, nil, "", safeError(cause))
}

func (r *Runner) reconcileUnlockAfterPostError(ctx context.Context, client WebsiteClient, accountID string, sess session, action state.Action, price *quota.Money, cause error) (PurchaseOutcome, error) {
	sess, afterDashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err == nil {
		if dashboardPassUnlockedForToday(afterDashboard, r.today()) {
			return r.finishUnlockFromDashboard(accountID, action, price, afterDashboard, "", "今日通行证已对账完成")
		}
		return r.finishPurchaseWithoutProof(accountID, action, price, dashboardRemainingPointer(afterDashboard), "", safeError(cause))
	}
	return r.finishPurchaseWithoutProof(accountID, action, price, nil, "", safeError(cause))
}

func (r *Runner) finishPurchaseDrawFromDashboard(accountID string, action state.Action, price *quota.Money, afterDashboard lottery.Dashboard, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
	if purchaseDashboardProvesSuccess(action, afterDashboard) {
		report, err := r.storeActivitySnapshot(accountID, afterDashboard)
		if err != nil {
			return PurchaseOutcome{}, err
		}
		updated, err := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionCompleted
			value.SideEffectStarted = false
			value.Retryable = false
			value.Message = firstNonEmpty(fallbackMessage, "购买抽奖次数成功")
			value.LastError = ""
			value.PriceUSD = copyMoneyPointer(price)
		})
		if err != nil {
			return PurchaseOutcome{}, err
		}
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), report, false), nil
	}
	return r.finishPurchaseWithoutProof(accountID, action, price, dashboardRemainingPointer(afterDashboard), operationStatus, fallbackMessage)
}

func (r *Runner) finishUnlockFromDashboard(accountID string, action state.Action, price *quota.Money, afterDashboard lottery.Dashboard, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
	if dashboardPassUnlockedForToday(afterDashboard, r.today()) {
		report, err := r.storeActivitySnapshot(accountID, afterDashboard)
		if err != nil {
			return PurchaseOutcome{}, err
		}
		updated, err := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionCompleted
			value.SideEffectStarted = false
			value.Retryable = false
			value.Message = firstNonEmpty(fallbackMessage, "今日通行证解锁成功")
			value.LastError = ""
			value.PriceUSD = copyMoneyPointer(price)
		})
		if err != nil {
			return PurchaseOutcome{}, err
		}
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), report, false), nil
	}
	return r.finishPurchaseWithoutProof(accountID, action, price, dashboardRemainingPointer(afterDashboard), operationStatus, fallbackMessage)
}

func (r *Runner) finishPurchaseWithoutProof(accountID string, action state.Action, price *quota.Money, remaining *int, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyMoneyPointer(price)
		value.Retryable = false
		if isOperationPendingStatus(operationStatus) {
			value.Status = state.ActionPending
			value.SideEffectStarted = true
			value.Message = firstNonEmpty(fallbackMessage, "购买处理中")
			value.LastError = ""
			return
		}
		value.Status = state.ActionUnknown
		value.SideEffectStarted = true
		value.Message = firstNonEmpty(fallbackMessage, "购买结果未知")
		value.LastError = value.Message
	})
	if err != nil {
		return PurchaseOutcome{}, err
	}
	return purchaseOutcomeFromAction(accountID, updated, remaining, nil, false), nil
}

func (r *Runner) recordPurchasePreflightError(accountID string, action state.Action, remaining *int, cause error) (PurchaseOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.Retryable = authRetryable(cause)
		value.SideEffectStarted = false
		value.Message = safeError(cause)
		value.LastError = value.Message
	})
	if err != nil {
		return PurchaseOutcome{}, err
	}
	return purchaseOutcomeFromAction(accountID, updated, remaining, nil, false), nil
}

func (r *Runner) purchaseDrawWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session, idempotencyKey string) (lottery.OperationResult, session, error) {
	result, err := client.PurchaseDraw(ctx, sess.token, idempotencyKey)
	if err == nil {
		return result, sess, nil
	}
	if !lottery.IsStatus(err, 401) && !lottery.IsStatus(err, 403) {
		return lottery.OperationResult{}, sess, err
	}
	renewed, err := r.renewLottery(ctx, accountID, sess.token)
	if err != nil {
		return lottery.OperationResult{}, sess, err
	}
	retryClient, clientErr := r.clientFor(renewed)
	if clientErr != nil {
		return lottery.OperationResult{}, renewed, clientErr
	}
	result, err = retryClient.PurchaseDraw(ctx, renewed.token, idempotencyKey)
	return result, renewed, err
}

func (r *Runner) unlockDailyPassWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session, idempotencyKey string) (lottery.OperationResult, session, error) {
	result, err := client.UnlockDrawLimit(ctx, sess.token, idempotencyKey)
	if err == nil {
		return result, sess, nil
	}
	if !lottery.IsStatus(err, 401) && !lottery.IsStatus(err, 403) {
		return lottery.OperationResult{}, sess, err
	}
	renewed, err := r.renewLottery(ctx, accountID, sess.token)
	if err != nil {
		return lottery.OperationResult{}, sess, err
	}
	retryClient, clientErr := r.clientFor(renewed)
	if clientErr != nil {
		return lottery.OperationResult{}, renewed, clientErr
	}
	result, err = retryClient.UnlockDrawLimit(ctx, renewed.token, idempotencyKey)
	return result, renewed, err
}

func purchaseOutcomeFromAction(accountID string, action state.Action, remaining *int, activity *ActivityReport, alreadyRecorded bool) PurchaseOutcome {
	return PurchaseOutcome{
		AccountID:       accountID,
		Status:          string(action.Status),
		Message:         action.Message,
		PriceUSD:        copyMoneyPointer(action.PriceUSD),
		Remaining:       copyIntPointer(remaining),
		Activity:        copyActivityReport(activity),
		Action:          action,
		AlreadyRecorded: alreadyRecorded,
	}
}

func purchaseDashboardProvesSuccess(action state.Action, dashboard lottery.Dashboard) bool {
	if action.PurchaseBeforeToday != nil && dashboardPurchasedToday(dashboard) > *action.PurchaseBeforeToday {
		return true
	}
	if action.PurchaseBeforeRemaining == nil {
		return false
	}
	afterPurchasedRemaining, ok := dashboardPurchasedRemaining(dashboard)
	return ok && afterPurchasedRemaining > *action.PurchaseBeforeRemaining
}

// dashboardPurchasePrice snapshots the live purchase price under the
// already-usd-v1 rule; prices are platform-reported USD values.
func (r *Runner) dashboardPurchasePrice(dashboard lottery.Dashboard) *quota.Money {
	if cost, ok := firstMapFloat(dashboard.Purchase, "cost", "unitCost", "price"); ok {
		price := USDMoney(cost, "dashboard.purchase.cost", r.now().UTC())
		return &price
	}
	return nil
}

func (r *Runner) dashboardPassPrice(dashboard lottery.Dashboard) *quota.Money {
	limit, _ := dashboard.EffectiveDrawLimit()
	if limit.UnlockCost == nil {
		return nil
	}
	price := USDMoney(float64(*limit.UnlockCost), "dashboard.draw_limit.unlock_cost", r.now().UTC())
	return &price
}

func copyMoneyPointer(value *quota.Money) *quota.Money {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func dashboardPurchasedToday(dashboard lottery.Dashboard) int {
	return firstMapInt(dashboard.Purchase, "purchasedToday", "todayPurchased")
}

func dashboardPurchasedRemaining(dashboard lottery.Dashboard) (int, bool) {
	limit, ok := dashboard.EffectiveDrawLimit()
	if !ok || limit.PurchasedRemaining == nil {
		return 0, false
	}
	return *limit.PurchasedRemaining, true
}

func dashboardPurchasedRemainingPointer(dashboard lottery.Dashboard) *int {
	value, ok := dashboardPurchasedRemaining(dashboard)
	return copyIntPointerValue(ok, value)
}

func dashboardRemainingPointer(dashboard lottery.Dashboard) *int {
	value, ok := dashboard.Remaining()
	return copyIntPointerValue(ok, value)
}

func dashboardPassUnlockedForToday(dashboard lottery.Dashboard, today string) bool {
	limit, _ := dashboard.EffectiveDrawLimit()
	if !boolOrDefault(limit.Unlocked, false) {
		return false
	}
	dayKey := strings.TrimSpace(limit.DayKey)
	return dayKey != "" && dayKey == strings.TrimSpace(today)
}

func copyActivityReport(report *ActivityReport) *ActivityReport {
	if report == nil {
		return nil
	}
	value := *report
	return &value
}

func boolPointerValue(value bool) *bool {
	return &value
}

func isOperationPendingStatus(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "pending" || status == "processing"
}

func isExplicitInsufficient(err error) bool {
	if lottery.IsStatus(err, 402) {
		return true
	}
	message := strings.ToLower(safeError(err))
	for _, phrase := range []string{
		"insufficient balance",
		"insufficient quota",
		"balance is insufficient",
		"quota is insufficient",
		"余额不足",
		"额度不足",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}
