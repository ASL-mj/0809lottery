package service

import (
	"context"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

type PurchaseOutcome struct {
	AccountID       string          `json:"account_id"`
	Status          string          `json:"status"`
	Message         string          `json:"message"`
	PriceUSD        *float64        `json:"price_usd,omitempty"`
	Remaining       *int            `json:"remaining,omitempty"`
	Activity        *ActivityReport `json:"activity,omitempty"`
	Action          state.Action    `json:"-"`
	AlreadyRecorded bool            `json:"-"`
}

func (r *Runner) PurchaseDraw(ctx context.Context, accountID string) (PurchaseOutcome, error) {
	account, err := r.account(accountID)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	date := r.today()
	callStarted := time.Now().UTC()
	release := r.store.LockAction(account.ID, date, state.ActionDrawPurchase)
	defer release()

	action, created, err := r.store.GetOrCreateAction(account.ID, date, state.ActionDrawPurchase)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if !created {
		switch {
		case action.Status == state.ActionUnknown || action.SideEffectStarted:
			return r.reconcilePurchaseDraw(ctx, account, action)
		case action.Status == state.ActionCompleted:
			if !action.UpdatedAt.Before(callStarted) {
				return purchaseOutcomeFromAction(account.ID, action, nil, nil, true), nil
			}
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		case action.Status == state.ActionFailed && action.Retryable && !action.SideEffectStarted:
			if !action.UpdatedAt.Before(callStarted) {
				return purchaseOutcomeFromAction(account.ID, action, nil, nil, true), nil
			}
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		case action.Status == state.ActionPending && !action.SideEffectStarted:
			// Resume a created-but-not-started purchase after restart.
		default:
			return purchaseOutcomeFromAction(account.ID, action, nil, nil, true), nil
		}
	}

	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	auth, lotteryToken, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	beforeDashboard, auth, lotteryToken, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	price := dashboardPurchasePrice(beforeDashboard)
	beforeRemaining := dashboardRemainingPointer(beforeDashboard)
	beforePurchasedRemaining := dashboardPurchasedRemainingPointer(beforeDashboard)
	beforeToday := dashboardPurchasedToday(beforeDashboard)
	action, err = r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyFloatPointer(price)
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

	result, auth, lotteryToken, err := r.purchaseDrawWithRecovery(ctx, client, account, auth, lotteryToken, action.IdempotencyKey)
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
			if putErr := r.store.PutAuth(account.ID, auth); putErr != nil {
				return PurchaseOutcome{}, putErr
			}
			return purchaseOutcomeFromAction(account.ID, updated, beforeRemaining, nil, false), nil
		}
		return r.reconcilePurchaseAfterPostError(ctx, client, account, auth, lotteryToken, action, price, err)
	}

	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return r.finishPurchaseWithoutProof(account.ID, auth, action, price, nil, result.Status, safeError(err))
	}
	return r.finishPurchaseDrawFromDashboard(account.ID, auth, action, price, afterDashboard, result.Status, "")
}

func (r *Runner) UnlockDailyPass(ctx context.Context, accountID string) (PurchaseOutcome, error) {
	account, err := r.account(accountID)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	date := r.today()
	release := r.store.LockAction(account.ID, date, state.ActionPassUnlock)
	defer release()

	action, created, err := r.store.GetOrCreateAction(account.ID, date, state.ActionPassUnlock)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if !created {
		switch {
		case action.Status == state.ActionCompleted:
			return purchaseOutcomeFromAction(account.ID, action, nil, nil, true), nil
		case action.Status == state.ActionUnknown || action.SideEffectStarted:
			return r.reconcileUnlockDailyPass(ctx, account, action)
		case action.Status == state.ActionPending && !action.SideEffectStarted:
			// Resume a created-but-not-started unlock after restart.
		case action.Status == state.ActionFailed && action.Retryable && !action.SideEffectStarted:
			action, err = r.store.RotateRepeatableAction(action.Key)
			if err != nil {
				return PurchaseOutcome{}, err
			}
		default:
			return purchaseOutcomeFromAction(account.ID, action, nil, nil, true), nil
		}
	}

	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	auth, lotteryToken, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	beforeDashboard, auth, lotteryToken, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return r.recordPurchasePreflightError(account.ID, action, nil, err)
	}
	price := dashboardPassPrice(beforeDashboard)
	beforeUnlocked := dashboardPassUnlockedForToday(beforeDashboard, r.today())
	beforeRemaining := dashboardRemainingPointer(beforeDashboard)
	action, err = r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyFloatPointer(price)
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
		report, snapshotErr := r.storeActivitySnapshot(account.ID, beforeDashboard)
		if snapshotErr != nil {
			return PurchaseOutcome{}, snapshotErr
		}
		r.putPurchaseAuthBestEffort(account.ID, auth)
		return purchaseOutcomeFromAction(account.ID, updated, beforeRemaining, report, false), nil
	}
	action, err = r.startAction(action)
	if err != nil {
		return PurchaseOutcome{}, err
	}

	result, auth, lotteryToken, err := r.unlockDailyPassWithRecovery(ctx, client, account, auth, lotteryToken, action.IdempotencyKey)
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
			if putErr := r.store.PutAuth(account.ID, auth); putErr != nil {
				return PurchaseOutcome{}, putErr
			}
			return purchaseOutcomeFromAction(account.ID, updated, beforeRemaining, nil, false), nil
		}
		return r.reconcileUnlockAfterPostError(ctx, client, account, auth, lotteryToken, action, price, err)
	}

	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return r.finishPurchaseWithoutProof(account.ID, auth, action, price, nil, result.Status, safeError(err))
	}
	return r.finishUnlockFromDashboard(account.ID, auth, action, price, afterDashboard, result.Status, "")
}

func (r *Runner) reconcilePurchaseDraw(ctx context.Context, account config.Account, action state.Action) (PurchaseOutcome, error) {
	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	auth, lotteryToken, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if purchaseDashboardProvesSuccess(action, afterDashboard) {
		return r.finishPurchaseDrawFromDashboard(account.ID, auth, action, copyFloatPointer(action.PriceUSD), afterDashboard, "", "购买抽奖次数已对账完成")
	}
	if putErr := r.store.PutAuth(account.ID, auth); putErr != nil {
		return PurchaseOutcome{}, putErr
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
		return purchaseOutcomeFromAction(account.ID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
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
	return purchaseOutcomeFromAction(account.ID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
}

func (r *Runner) reconcileUnlockDailyPass(ctx context.Context, account config.Account, action state.Action) (PurchaseOutcome, error) {
	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	auth, lotteryToken, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err != nil {
		return PurchaseOutcome{}, err
	}
	if dashboardPassUnlockedForToday(afterDashboard, r.today()) {
		return r.finishUnlockFromDashboard(account.ID, auth, action, copyFloatPointer(action.PriceUSD), afterDashboard, "", "今日通行证已对账完成")
	}
	if putErr := r.store.PutAuth(account.ID, auth); putErr != nil {
		return PurchaseOutcome{}, putErr
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
		return purchaseOutcomeFromAction(account.ID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
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
	return purchaseOutcomeFromAction(account.ID, updated, dashboardRemainingPointer(afterDashboard), nil, true), nil
}

func (r *Runner) reconcilePurchaseAfterPostError(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken string, action state.Action, price *float64, cause error) (PurchaseOutcome, error) {
	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err == nil {
		if purchaseDashboardProvesSuccess(action, afterDashboard) {
			return r.finishPurchaseDrawFromDashboard(account.ID, auth, action, price, afterDashboard, "", "购买抽奖次数已对账完成")
		}
		return r.finishPurchaseWithoutProof(account.ID, auth, action, price, dashboardRemainingPointer(afterDashboard), "", safeError(cause))
	}
	return r.finishPurchaseWithoutProof(account.ID, auth, action, price, nil, "", safeError(cause))
}

func (r *Runner) reconcileUnlockAfterPostError(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken string, action state.Action, price *float64, cause error) (PurchaseOutcome, error) {
	afterDashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, lotteryToken)
	if err == nil {
		if dashboardPassUnlockedForToday(afterDashboard, r.today()) {
			return r.finishUnlockFromDashboard(account.ID, auth, action, price, afterDashboard, "", "今日通行证已对账完成")
		}
		return r.finishPurchaseWithoutProof(account.ID, auth, action, price, dashboardRemainingPointer(afterDashboard), "", safeError(cause))
	}
	return r.finishPurchaseWithoutProof(account.ID, auth, action, price, nil, "", safeError(cause))
}

func (r *Runner) finishPurchaseDrawFromDashboard(accountID string, auth state.AuthState, action state.Action, price *float64, afterDashboard lottery.Dashboard, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
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
			value.PriceUSD = copyFloatPointer(price)
		})
		if err != nil {
			return PurchaseOutcome{}, err
		}
		r.putPurchaseAuthBestEffort(accountID, auth)
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), report, false), nil
	}
	return r.finishPurchaseWithoutProof(accountID, auth, action, price, dashboardRemainingPointer(afterDashboard), operationStatus, fallbackMessage)
}

func (r *Runner) finishUnlockFromDashboard(accountID string, auth state.AuthState, action state.Action, price *float64, afterDashboard lottery.Dashboard, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
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
			value.PriceUSD = copyFloatPointer(price)
		})
		if err != nil {
			return PurchaseOutcome{}, err
		}
		r.putPurchaseAuthBestEffort(accountID, auth)
		return purchaseOutcomeFromAction(accountID, updated, dashboardRemainingPointer(afterDashboard), report, false), nil
	}
	return r.finishPurchaseWithoutProof(accountID, auth, action, price, dashboardRemainingPointer(afterDashboard), operationStatus, fallbackMessage)
}

func (r *Runner) finishPurchaseWithoutProof(accountID string, auth state.AuthState, action state.Action, price *float64, remaining *int, operationStatus, fallbackMessage string) (PurchaseOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.PriceUSD = copyFloatPointer(price)
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
	if err := r.store.PutAuth(accountID, auth); err != nil {
		return PurchaseOutcome{}, err
	}
	return purchaseOutcomeFromAction(accountID, updated, remaining, nil, false), nil
}

func (r *Runner) recordPurchasePreflightError(accountID string, action state.Action, remaining *int, cause error) (PurchaseOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionFailed
		value.Retryable = true
		value.SideEffectStarted = false
		value.Message = safeError(cause)
		value.LastError = value.Message
	})
	if err != nil {
		return PurchaseOutcome{}, err
	}
	return purchaseOutcomeFromAction(accountID, updated, remaining, nil, false), nil
}

func (r *Runner) purchaseDrawWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken, idempotencyKey string) (lottery.OperationResult, state.AuthState, string, error) {
	result, err := client.PurchaseDraw(ctx, lotteryToken, idempotencyKey)
	if err == nil {
		return result, auth, lotteryToken, nil
	}
	if !lottery.IsStatus(err, 401) && !lottery.IsStatus(err, 403) {
		return lottery.OperationResult{}, auth, lotteryToken, err
	}
	auth.LotteryAccessToken = ""
	auth.LotteryAccessExpiresAt = time.Time{}
	auth, lotteryToken, err = r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return lottery.OperationResult{}, auth, lotteryToken, err
	}
	result, err = client.PurchaseDraw(ctx, lotteryToken, idempotencyKey)
	return result, auth, lotteryToken, err
}

func (r *Runner) unlockDailyPassWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken, idempotencyKey string) (lottery.OperationResult, state.AuthState, string, error) {
	result, err := client.UnlockDrawLimit(ctx, lotteryToken, idempotencyKey)
	if err == nil {
		return result, auth, lotteryToken, nil
	}
	if !lottery.IsStatus(err, 401) && !lottery.IsStatus(err, 403) {
		return lottery.OperationResult{}, auth, lotteryToken, err
	}
	auth.LotteryAccessToken = ""
	auth.LotteryAccessExpiresAt = time.Time{}
	auth, lotteryToken, err = r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return lottery.OperationResult{}, auth, lotteryToken, err
	}
	result, err = client.UnlockDrawLimit(ctx, lotteryToken, idempotencyKey)
	return result, auth, lotteryToken, err
}

func purchaseOutcomeFromAction(accountID string, action state.Action, remaining *int, activity *ActivityReport, alreadyRecorded bool) PurchaseOutcome {
	return PurchaseOutcome{
		AccountID:       accountID,
		Status:          string(action.Status),
		Message:         action.Message,
		PriceUSD:        copyFloatPointer(action.PriceUSD),
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

func dashboardPurchasePrice(dashboard lottery.Dashboard) *float64 {
	if cost, ok := firstMapFloat(dashboard.Purchase, "cost", "unitCost", "price"); ok {
		return &cost
	}
	return nil
}

func dashboardPassPrice(dashboard lottery.Dashboard) *float64 {
	limit, _ := dashboard.EffectiveDrawLimit()
	if limit.UnlockCost == nil {
		return nil
	}
	price := float64(*limit.UnlockCost)
	return &price
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

func dashboardPassUnlocked(dashboard lottery.Dashboard) bool {
	limit, _ := dashboard.EffectiveDrawLimit()
	return boolOrDefault(limit.Unlocked, false)
}

func dashboardPassUnlockedForToday(dashboard lottery.Dashboard, today string) bool {
	limit, _ := dashboard.EffectiveDrawLimit()
	if !boolOrDefault(limit.Unlocked, false) {
		return false
	}
	dayKey := strings.TrimSpace(limit.DayKey)
	return dayKey != "" && dayKey == strings.TrimSpace(today)
}

func copyFloatPointer(pointer *float64) *float64 {
	if pointer == nil {
		return nil
	}
	value := *pointer
	return &value
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

func (r *Runner) putPurchaseAuthBestEffort(accountID string, auth state.AuthState) {
	if r.putAuth != nil {
		_ = r.putAuth(accountID, auth)
		return
	}
	_ = r.store.PutAuth(accountID, auth)
}
