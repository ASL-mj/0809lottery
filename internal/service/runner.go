package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

var shanghaiLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

type WebsiteClient interface {
	Login(context.Context, config.Account) (lottery.LoginResult, error)
	Refresh(context.Context) (lottery.LoginResult, error)
	Bridge(context.Context, string, int64) (lottery.BridgeResult, error)
	Draw(context.Context, string, string) (lottery.DrawResult, error)
	PurchaseDraw(context.Context, string, string) (lottery.OperationResult, error)
	UnlockDrawLimit(context.Context, string, string) (lottery.OperationResult, error)
	Dashboard(context.Context, string) (lottery.Dashboard, error)
	ClaimDaily(context.Context, string) (lottery.ClaimResult, error)
	Checkin(context.Context, string) (lottery.CheckinResult, error)
	CheckinStatus(context.Context, string) (lottery.CheckinStatus, error)
	CheckinEligibility(context.Context, string, int64) (lottery.CheckinEligibility, error)
	UserSelf(context.Context, string) (lottery.UserUsage, error)
	SubscriptionPlans(context.Context, string) (map[int]string, error)
	SubscriptionSelf(context.Context, string) (lottery.SubscriptionSelf, error)
	Status(context.Context, string) (lottery.StatusSettings, error)
	Cookies() []state.Cookie
}

type ClientFactory func([]state.Cookie) (WebsiteClient, error)

type Runner struct {
	config    config.Config
	store     *state.Store
	newClient ClientFactory
	putAuth   func(string, state.AuthState) error
	now       func() time.Time
	wait      func(context.Context, time.Duration) error
}

type ActionOutcome struct {
	Action          state.Action
	AlreadyRecorded bool
}

type CheckinStatusReport struct {
	CheckedInToday       bool
	TodayQuotaAwardedUSD *float64
}

func NewRunner(cfg config.Config, store *state.Store) *Runner {
	return NewRunnerWithFactory(cfg, store, func(cookies []state.Cookie) (WebsiteClient, error) {
		return lottery.NewClient(cfg.BaseURL, cfg.UserAgent, cookies)
	})
}

func NewRunnerWithFactory(cfg config.Config, store *state.Store, factory ClientFactory) *Runner {
	return &Runner{
		config:    cfg,
		store:     store,
		newClient: factory,
		putAuth:   store.PutAuth,
		now:       time.Now,
		wait:      wait,
	}
}

func (r *Runner) Dashboard(ctx context.Context, accountID string) (lottery.Dashboard, error) {
	account, err := r.account(accountID)
	if err != nil {
		return lottery.Dashboard{}, err
	}
	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return lottery.Dashboard{}, fmt.Errorf("create website client: %w", err)
	}
	auth, token, err := r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return lottery.Dashboard{}, err
	}
	dashboard, auth, _, err := r.dashboardWithRecovery(ctx, client, account, auth, token)
	if err != nil {
		return lottery.Dashboard{}, err
	}
	if err := r.store.PutAuth(account.ID, auth); err != nil {
		return lottery.Dashboard{}, err
	}
	return dashboard, nil
}

func (r *Runner) Checkin(ctx context.Context, accountID string) (ActionOutcome, error) {
	account, err := r.account(accountID)
	if err != nil {
		return ActionOutcome{}, err
	}
	action, created, err := r.store.GetOrCreateAction(account.ID, r.today(), state.ActionCheckin)
	if err != nil {
		return ActionOutcome{}, err
	}
	auth := r.store.Auth(account.ID)
	var client WebsiteClient
	var parentToken string
	if !created {
		switch action.Status {
		case state.ActionCompleted, state.ActionUnknown:
			return ActionOutcome{Action: action, AlreadyRecorded: true}, nil
		case state.ActionFailed:
			client, err = r.newClient(auth.Cookies)
			if err == nil {
				auth, parentToken, err = r.ensureParentToken(ctx, client, account, auth)
			}
			if err == nil {
				var status lottery.CheckinStatus
				var settings lottery.StatusSettings
				status, settings, auth, parentToken, err = r.fetchCheckinDisplayStatus(ctx, client, account, auth, parentToken)
				if err == nil {
					if putErr := r.store.PutAuth(account.ID, auth); putErr != nil {
						return ActionOutcome{}, putErr
					}
					if status.CheckedInToday {
						updated, updateErr := r.finishAction(action, func(value *state.Action) {
							value.Status = state.ActionCompleted
							value.Retryable = false
							value.LastError = ""
							value.Message = "今日已签到"
							if status.TodayQuotaAwarded != nil {
								quotaAwarded := *status.TodayQuotaAwarded
								value.CheckinQuotaAwarded = &quotaAwarded
							}
							if status.TodayQuotaAwarded != nil {
								rewardUSD, ok := QuotaAmountUSD(*status.TodayQuotaAwarded, settings)
								if ok {
									value.CheckinQuotaAwardedUSD = rewardUSD
									value.Message = fmt.Sprintf("今日已签到，获得额度：$%.2f", *rewardUSD)
								}
							}
						})
						if updateErr != nil {
							return ActionOutcome{}, updateErr
						}
						return ActionOutcome{Action: updated, AlreadyRecorded: true}, nil
					}
					action, err = r.resetFailedCheckinAction(action)
					if err != nil {
						return ActionOutcome{}, err
					}
					break
				}
			}
			if action.Retryable && !action.SideEffectStarted {
				action, err = r.store.ResetRetryableAction(action.Key)
				if err != nil {
					return ActionOutcome{}, err
				}
				break
			}
			return ActionOutcome{Action: action, AlreadyRecorded: true}, nil
		default:
			return ActionOutcome{Action: action, AlreadyRecorded: true}, nil
		}
	}

	if client == nil {
		client, err = r.newClient(auth.Cookies)
		if err != nil {
			return r.recordActionError(action, err, false, true)
		}
	}
	if strings.TrimSpace(parentToken) == "" {
		auth, parentToken, err = r.ensureParentToken(ctx, client, account, auth)
		if err != nil {
			return r.recordActionError(action, err, false, true)
		}
	}
	if auth.UserID > 0 {
		var eligibility lottery.CheckinEligibility
		eligibility, auth, parentToken, err = r.checkinEligibilityWithRecovery(ctx, client, account, auth, parentToken)
		if err != nil && subscriptionAuthError(err) {
			return r.recordActionError(action, err, false, true)
		}
		if err == nil && !eligibility.CanCheckin {
			message := "今日活跃度不足，暂时无法换算签到所需额度"
			if settings, settingsErr := client.Status(ctx, parentToken); settingsErr == nil {
				if remainingUSD, ok := QuotaAmountUSD(eligibility.Remaining, settings); ok {
					message = fmt.Sprintf("今日活跃度不足，距离签到还需消耗 $%.2f", *remainingUSD)
				}
			}
			return r.recordActionError(action, errors.New(message), false, true)
		}
	}
	action, err = r.startAction(action)
	if err != nil {
		return ActionOutcome{}, err
	}
	result, auth, err := r.checkinWithRecovery(ctx, client, account, auth, parentToken)
	if err != nil {
		return r.recordActionError(action, err, isUnknown(err), false)
	}
	if err := r.store.PutAuth(account.ID, auth); err != nil {
		return ActionOutcome{}, err
	}
	if !result.Success {
		updated, updateErr := r.finishAction(action, func(value *state.Action) {
			value.Status = state.ActionFailed
			value.Retryable = true
			value.SideEffectStarted = false
			value.Message = firstNonEmpty(result.Message, "签到未成功")
			value.LastError = value.Message
		})
		if updateErr != nil {
			return ActionOutcome{}, updateErr
		}
		return ActionOutcome{Action: updated}, nil
	}
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.Retryable = false
		value.LastError = ""
		value.Message = firstNonEmpty(result.Message, "签到成功")
		quotaAwarded := result.QuotaAwarded
		value.CheckinQuotaAwarded = &quotaAwarded
	})
	if err != nil {
		return ActionOutcome{}, err
	}
	return ActionOutcome{Action: updated}, nil
}

func (r *Runner) resetFailedCheckinAction(action state.Action) (state.Action, error) {
	return r.store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionPending
		value.SideEffectStarted = false
		value.Retryable = false
		value.CheckinQuotaAwarded = nil
		value.CheckinQuotaAwardedUSD = nil
		value.Message = ""
		value.LastError = ""
	})
}

func (r *Runner) CheckinStatus(ctx context.Context, accountID string) (CheckinStatusReport, error) {
	account, err := r.account(accountID)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return CheckinStatusReport{}, fmt.Errorf("create website client: %w", err)
	}
	auth, parentToken, err := r.ensureParentToken(ctx, client, account, auth)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	status, settings, auth, _, err := r.fetchCheckinDisplayStatus(ctx, client, account, auth, parentToken)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	if err := r.store.PutAuth(account.ID, auth); err != nil {
		return CheckinStatusReport{}, err
	}
	report := CheckinStatusReport{CheckedInToday: status.CheckedInToday}
	if status.TodayQuotaAwarded != nil {
		report.TodayQuotaAwardedUSD, _ = QuotaAmountUSD(*status.TodayQuotaAwarded, settings)
	}
	return report, nil
}

func (r *Runner) QueryUsage(ctx context.Context, accountID string) (lottery.UserUsage, error) {
	account, err := r.account(accountID)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	auth := r.store.Auth(account.ID)
	client, err := r.newClient(auth.Cookies)
	if err != nil {
		return lottery.UserUsage{}, fmt.Errorf("create website client: %w", err)
	}
	auth, parentToken, err := r.ensureParentToken(ctx, client, account, auth)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	usage, err := client.UserSelf(ctx, parentToken)
	if err != nil {
		if !lottery.IsStatus(err, 401) {
			return lottery.UserUsage{}, err
		}
		auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		usage, err = client.UserSelf(ctx, parentToken)
		if err != nil {
			return lottery.UserUsage{}, err
		}
	}
	settings, statusErr := client.Status(ctx, parentToken)
	if statusErr != nil && subscriptionAuthError(statusErr) {
		auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		usage, err = client.UserSelf(ctx, parentToken)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		settings, statusErr = client.Status(ctx, parentToken)
	}
	if statusErr == nil {
		usage = normalizeUsage(usage, settings)
	} else {
		usage.QuotaConversionAvailable = false
		usage.QuotaConversionError = "无法获取美元换算配置"
	}
	if err := r.store.PutAuth(account.ID, auth); err != nil {
		return lottery.UserUsage{}, err
	}
	payload, err := json.Marshal(usage)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	if err := r.store.PutSnapshot(state.Snapshot{AccountID: account.ID, Kind: "usage", Data: payload, QueriedAt: r.now().UTC()}); err != nil {
		return lottery.UserUsage{}, err
	}
	return usage, nil
}

func (r *Runner) startAction(action state.Action) (state.Action, error) {
	return r.store.UpdateAction(action.Key, func(value *state.Action) {
		value.SideEffectStarted = true
		value.Attempts++
	})
}

func (r *Runner) finishAction(action state.Action, update func(*state.Action)) (state.Action, error) {
	return r.store.UpdateAction(action.Key, update)
}

func (r *Runner) recordActionError(action state.Action, cause error, unknown, retryable bool) (ActionOutcome, error) {
	updated, err := r.finishAction(action, func(value *state.Action) {
		value.LastError = safeError(cause)
		value.Message = value.LastError
		value.Retryable = retryable
		switch {
		case retryable:
			value.Status = state.ActionFailed
		case unknown:
			value.Status = state.ActionUnknown
		default:
			value.Status = state.ActionFailed
		}
	})
	if err != nil {
		return ActionOutcome{}, err
	}
	return ActionOutcome{Action: updated}, nil
}

func (r *Runner) ensureParentToken(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState) (state.AuthState, string, error) {
	return r.ensureParentTokenAfter(ctx, client, account, auth, "")
}

func (r *Runner) refreshParentToken(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, rejectedToken string) (state.AuthState, string, error) {
	return r.ensureParentTokenAfter(ctx, client, account, auth, rejectedToken)
}

func (r *Runner) ensureParentTokenAfter(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, rejectedToken string) (state.AuthState, string, error) {
	rejectedToken = strings.TrimSpace(rejectedToken)
	if rejectedToken == "" && usableToken(auth.ParentAccessToken, auth.ParentAccessExpiresAt, r.now()) {
		return auth, auth.ParentAccessToken, nil
	}

	release := r.store.LockAuth(account.ID)
	defer release()

	// A concurrent request may already have refreshed this account while this
	// request waited for the lock. Always use that latest persisted state.
	auth = r.store.Auth(account.ID)
	if rejectedToken == "" {
		if usableToken(auth.ParentAccessToken, auth.ParentAccessExpiresAt, r.now()) {
			return auth, auth.ParentAccessToken, nil
		}
	} else if strings.TrimSpace(auth.ParentAccessToken) != rejectedToken && usableToken(auth.ParentAccessToken, auth.ParentAccessExpiresAt, r.now()) {
		return auth, auth.ParentAccessToken, nil
	} else if strings.TrimSpace(auth.ParentAccessToken) == rejectedToken {
		// This exact token has already been rejected by an upstream endpoint, so
		// do not validate it again. Prefer its refresh cookie instead.
		auth.ParentAccessToken = ""
		auth.ParentAccessExpiresAt = time.Time{}
		auth.LotteryAccessToken = ""
		auth.LotteryAccessExpiresAt = time.Time{}
	}

	// The locally stored expiry is only a hint. Verify the cached access token
	// with the server before creating another login session.
	if token := strings.TrimSpace(auth.ParentAccessToken); token != "" {
		if _, err := client.UserSelf(ctx, token); err == nil {
			auth.Cookies = client.Cookies()
			if err := r.store.PutAuth(account.ID, auth); err != nil {
				return state.AuthState{}, "", err
			}
			return auth, token, nil
		} else if !subscriptionAuthError(err) {
			return state.AuthState{}, "", fmt.Errorf("validate saved session for account %s: %w", account.ID, err)
		}
	}

	// A rejected cached token may still have a valid refresh cookie. Refreshing
	// preserves the existing server session and avoids consuming a device slot.
	refreshed, err := client.Refresh(ctx)
	if err == nil {
		return r.persistParentSession(account.ID, client, auth, refreshed)
	}
	if !subscriptionAuthError(err) {
		return state.AuthState{}, "", fmt.Errorf("refresh saved session for account %s: %w", account.ID, err)
	}

	// Password login is deliberately the last resort, and is attempted once
	// for this authentication operation only.
	login, err := client.Login(ctx, account)
	if err != nil {
		return state.AuthState{}, "", fmt.Errorf("login account %s: %w", account.ID, err)
	}
	return r.persistParentSession(account.ID, client, auth, login)
}

func (r *Runner) dashboardWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken string) (lottery.Dashboard, state.AuthState, string, error) {
	dashboard, err := client.Dashboard(ctx, lotteryToken)
	if err == nil {
		return dashboard, auth, lotteryToken, nil
	}
	if lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403) {
		auth.LotteryAccessToken = ""
		auth.LotteryAccessExpiresAt = time.Time{}
		auth, lotteryToken, err = r.ensureLotteryToken(ctx, client, account, auth)
		if err != nil {
			return lottery.Dashboard{}, auth, lotteryToken, err
		}
		dashboard, err = client.Dashboard(ctx, lotteryToken)
		return dashboard, auth, lotteryToken, err
	}
	if !lottery.IsTransient(err) {
		return lottery.Dashboard{}, auth, lotteryToken, err
	}
	if err := r.wait(ctx, 2*time.Second); err != nil {
		return lottery.Dashboard{}, auth, lotteryToken, err
	}
	dashboard, err = client.Dashboard(ctx, lotteryToken)
	return dashboard, auth, lotteryToken, err
}

func (r *Runner) claimWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, lotteryToken string) (lottery.ClaimResult, state.AuthState, error) {
	result, err := client.ClaimDaily(ctx, lotteryToken)
	if err == nil {
		return result, auth, nil
	}
	if !lottery.IsStatus(err, 401) {
		return lottery.ClaimResult{}, auth, err
	}
	auth.LotteryAccessToken = ""
	auth.LotteryAccessExpiresAt = time.Time{}
	auth, lotteryToken, err = r.ensureLotteryToken(ctx, client, account, auth)
	if err != nil {
		return lottery.ClaimResult{}, auth, err
	}
	result, err = client.ClaimDaily(ctx, lotteryToken)
	return result, auth, err
}

func (r *Runner) checkinWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, parentToken string) (lottery.CheckinResult, state.AuthState, error) {
	result, err := client.Checkin(ctx, parentToken)
	if err == nil {
		return result, auth, nil
	}
	if !subscriptionAuthError(err) {
		return lottery.CheckinResult{}, auth, err
	}
	auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
	if err != nil {
		return lottery.CheckinResult{}, auth, err
	}
	result, err = client.Checkin(ctx, parentToken)
	return result, auth, err
}

func (r *Runner) checkinEligibilityWithRecovery(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, parentToken string) (lottery.CheckinEligibility, state.AuthState, string, error) {
	eligibility, err := client.CheckinEligibility(ctx, parentToken, auth.UserID)
	if err == nil {
		return eligibility, auth, parentToken, nil
	}
	if !subscriptionAuthError(err) {
		return lottery.CheckinEligibility{}, auth, parentToken, err
	}
	auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
	if err != nil {
		return lottery.CheckinEligibility{}, auth, parentToken, err
	}
	eligibility, err = client.CheckinEligibility(ctx, parentToken, auth.UserID)
	return eligibility, auth, parentToken, err
}

func (r *Runner) fetchCheckinDisplayStatus(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState, parentToken string) (lottery.CheckinStatus, lottery.StatusSettings, state.AuthState, string, error) {
	status, settings, err := fetchCheckinDisplayStatus(ctx, client, parentToken)
	if err == nil {
		return status, settings, auth, parentToken, nil
	}
	if !subscriptionAuthError(err) {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, auth, parentToken, err
	}
	auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
	if err != nil {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, auth, parentToken, err
	}
	status, settings, err = fetchCheckinDisplayStatus(ctx, client, parentToken)
	return status, settings, auth, parentToken, err
}

func fetchCheckinDisplayStatus(ctx context.Context, client WebsiteClient, parentToken string) (lottery.CheckinStatus, lottery.StatusSettings, error) {
	status, err := client.CheckinStatus(ctx, parentToken)
	if err != nil {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, err
	}
	settings, err := client.Status(ctx, parentToken)
	if err != nil {
		if !subscriptionAuthError(err) {
			return status, lottery.StatusSettings{}, nil
		}
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, err
	}
	return status, settings, nil
}

func (r *Runner) ensureLotteryToken(ctx context.Context, client WebsiteClient, account config.Account, auth state.AuthState) (state.AuthState, string, error) {
	if usableToken(auth.LotteryAccessToken, auth.LotteryAccessExpiresAt, r.now()) {
		return auth, auth.LotteryAccessToken, nil
	}
	auth, parentToken, err := r.ensureParentToken(ctx, client, account, auth)
	if err != nil {
		return state.AuthState{}, "", err
	}
	if auth.UserID <= 0 {
		return state.AuthState{}, "", errors.New("saved session did not contain a user ID")
	}
	bridge, err := client.Bridge(ctx, parentToken, auth.UserID)
	if err == nil {
		auth.LotteryAccessToken = bridge.AccessToken
		auth.LotteryAccessExpiresAt = bridge.ExpiresAt
		auth.Cookies = client.Cookies()
		if err := r.store.PutAuth(account.ID, auth); err != nil {
			return state.AuthState{}, "", err
		}
		return auth, bridge.AccessToken, nil
	}
	if !subscriptionAuthError(err) {
		return state.AuthState{}, "", fmt.Errorf("refresh lottery session: %w", err)
	}

	// The parent token may have been accepted by /self but rejected by the
	// bridge endpoint. Drop it and let ensureParentToken try the refresh cookie
	// before considering a new password login.
	auth, parentToken, err = r.refreshParentToken(ctx, client, account, auth, parentToken)
	if err != nil {
		return state.AuthState{}, "", err
	}
	if auth.UserID <= 0 {
		return state.AuthState{}, "", errors.New("refreshed session did not contain a user ID")
	}
	bridge, err = client.Bridge(ctx, parentToken, auth.UserID)
	if err != nil {
		return state.AuthState{}, "", fmt.Errorf("bridge lottery session: %w", err)
	}
	auth.LotteryAccessToken = bridge.AccessToken
	auth.LotteryAccessExpiresAt = bridge.ExpiresAt
	auth.Cookies = client.Cookies()
	if err := r.store.PutAuth(account.ID, auth); err != nil {
		return state.AuthState{}, "", err
	}
	return auth, bridge.AccessToken, nil
}

func (r *Runner) persistParentSession(accountID string, client WebsiteClient, auth state.AuthState, session lottery.LoginResult) (state.AuthState, string, error) {
	if session.UserID <= 0 || strings.TrimSpace(session.AccessToken) == "" {
		return state.AuthState{}, "", errors.New("session response did not contain an access token and user ID")
	}
	auth.UserID = session.UserID
	auth.ParentAccessToken = session.AccessToken
	auth.ParentAccessExpiresAt = session.AccessExpiresAt
	auth.LotteryAccessToken = ""
	auth.LotteryAccessExpiresAt = time.Time{}
	auth.Cookies = client.Cookies()
	if err := r.store.PutAuth(accountID, auth); err != nil {
		return state.AuthState{}, "", err
	}
	return auth, session.AccessToken, nil
}

func (r *Runner) today() string {
	return r.now().In(shanghaiLocation).Format("2006-01-02")
}

func (r *Runner) account(id string) (config.Account, error) {
	account, ok := r.config.Accounts[id]
	if !ok {
		return config.Account{}, fmt.Errorf("unknown account %q", id)
	}
	return account, nil
}

func usableToken(token string, expiresAt time.Time, now time.Time) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	return expiresAt.IsZero() || expiresAt.After(now.Add(time.Minute))
}

func isUnknown(err error) bool {
	return lottery.IsTransient(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 240 {
		return message[:240]
	}
	return message
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "-"
}

func intPointer(value int) *int {
	return &value
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
