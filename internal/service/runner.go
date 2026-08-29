package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

var shanghaiLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

// WebsiteClient is the business surface the runner drives. Authentication
// (login, refresh, bridge) lives exclusively in the auth broker.
type WebsiteClient interface {
	UserSelf(context.Context, string) (lottery.UserUsage, error)
	RankingsUsers(context.Context, string, int64, string) (lottery.UserRanking, error)
	Draw(context.Context, string, string) (lottery.DrawResult, error)
	PurchaseDraw(context.Context, string, string) (lottery.OperationResult, error)
	UnlockDrawLimit(context.Context, string, string) (lottery.OperationResult, error)
	Dashboard(context.Context, string) (lottery.Dashboard, error)
	ClaimDaily(context.Context, string) (lottery.ClaimResult, error)
	Checkin(context.Context, string) (lottery.CheckinResult, error)
	CheckinStatus(context.Context, string) (lottery.CheckinStatus, error)
	CheckinEligibility(context.Context, string, int64) (lottery.CheckinEligibility, error)
	SubscriptionPlans(context.Context, string) (map[int]string, error)
	SubscriptionSelf(context.Context, string) (lottery.SubscriptionSelf, error)
	Status(context.Context, string) (lottery.StatusSettings, error)
}

type ClientFactory func([]state.Cookie) (WebsiteClient, error)

// session is one acquired broker session: the token plus the cookies the
// caller should use for business requests.
type session struct {
	token   string
	cookies []state.Cookie
	userID  int64
}

type Runner struct {
	config    config.Config
	store     *state.Store
	repo      account.Repository
	broker    *auth.Broker
	newClient ClientFactory
	now       func() time.Time
	wait      func(context.Context, time.Duration) error
}

type ActionOutcome struct {
	Action          state.Action
	AlreadyRecorded bool
}

type CheckinStatusReport struct {
	CheckedInToday       bool
	TodayQuotaAwardedUSD *quota.Money
}

func NewRunner(cfg config.Config, store *state.Store, repo account.Repository, broker *auth.Broker) *Runner {
	return NewRunnerWithFactory(cfg, store, repo, broker, func(cookies []state.Cookie) (WebsiteClient, error) {
		return lottery.NewClient(cfg.BaseURL, cfg.UserAgent, cookies)
	})
}

func NewRunnerWithFactory(cfg config.Config, store *state.Store, repo account.Repository, broker *auth.Broker, factory ClientFactory) *Runner {
	return &Runner{
		config:    cfg,
		store:     store,
		repo:      repo,
		broker:    broker,
		newClient: factory,
		now:       time.Now,
		wait:      wait,
	}
}

func (r *Runner) acquire(ctx context.Context, accountID string, intent auth.Intent, kind auth.SessionKind) (session, error) {
	acquired, err := r.broker.Acquire(ctx, accountID, intent, kind)
	if err != nil {
		return session{}, err
	}
	return session{token: acquired.Token, cookies: acquired.Cookies, userID: acquired.UserID}, nil
}

func (r *Runner) renewParent(ctx context.Context, accountID, rejected string) (session, error) {
	acquired, err := r.broker.RenewParent(ctx, accountID, rejected)
	if err != nil {
		return session{}, err
	}
	return session{token: acquired.Token, cookies: acquired.Cookies, userID: acquired.UserID}, nil
}

func (r *Runner) renewLottery(ctx context.Context, accountID, rejected string) (session, error) {
	acquired, err := r.broker.RenewLottery(ctx, accountID, rejected)
	if err != nil {
		return session{}, err
	}
	return session{token: acquired.Token, cookies: acquired.Cookies, userID: acquired.UserID}, nil
}

func (r *Runner) clientFor(sess session) (WebsiteClient, error) {
	client, err := r.newClient(sess.cookies)
	if err != nil {
		return nil, fmt.Errorf("create website client: %w", err)
	}
	return client, nil
}

// authRetryable reports whether an authentication failure can resolve without
// the user reauthenticating. Only explicit reauthentication requirements are
// terminal; transient auth problems stay retryable.
func authRetryable(err error) bool {
	return !errors.Is(err, auth.ErrReauthRequired)
}

func (r *Runner) Dashboard(ctx context.Context, accountID string) (lottery.Dashboard, error) {
	if _, err := r.account(accountID); err != nil {
		return lottery.Dashboard{}, err
	}
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionLottery)
	if err != nil {
		return lottery.Dashboard{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return lottery.Dashboard{}, err
	}
	sess, dashboard, err := r.dashboardWithRecovery(ctx, client, accountID, sess)
	if err != nil {
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
	if !created {
		switch action.Status {
		case state.ActionCompleted, state.ActionUnknown:
			return ActionOutcome{Action: action, AlreadyRecorded: true}, nil
		case state.ActionFailed:
			newAction, outcome, err := r.reconcileFailedCheckin(ctx, account.ID, action)
			if err != nil {
				return ActionOutcome{}, err
			}
			if outcome != nil {
				return *outcome, nil
			}
			if newAction.Status == state.ActionPending && !newAction.SideEffectStarted {
				// Upstream shows the check-in did not happen; run it fresh.
				action = newAction
				break
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

	sess, client, err := r.acquireParentForAction(ctx, account.ID)
	if err != nil {
		return r.recordActionError(action, err, false, authRetryable(err))
	}
	if sess.userID > 0 {
		eligibility, renewedSess, eligErr := r.checkinEligibilityWithRecovery(ctx, client, account.ID, sess)
		switch {
		case eligErr != nil:
			// A failed eligibility precheck only blocks the check-in when the
			// session itself is broken; other precheck errors are soft.
			if subscriptionAuthError(eligErr) || errors.Is(eligErr, auth.ErrReauthRequired) || errors.Is(eligErr, auth.ErrAuthUnavailable) {
				return r.recordActionError(action, eligErr, false, authRetryable(eligErr))
			}
		case !eligibility.CanCheckin:
			message := "今日活跃度不足，暂时无法换算签到所需额度"
			if settings, settingsErr := client.Status(ctx, sess.token); settingsErr == nil {
				if remaining := QuotaMoney(float64(eligibility.Remaining), settings, "checkin.required_spend", r.now().UTC()); remaining.State == quota.StateConfirmed {
					message = fmt.Sprintf("今日活跃度不足，距离签到还需消耗 %s", remaining.Display)
				}
			}
			return r.recordActionError(action, errors.New(message), false, true)
		default:
			sess = renewedSess
		}
	}
	action, err = r.startAction(action)
	if err != nil {
		return ActionOutcome{}, err
	}
	result, err := client.Checkin(ctx, sess.token)
	if err != nil {
		if subscriptionAuthError(err) {
			sess, err = r.renewParent(ctx, account.ID, sess.token)
			if err == nil {
				client, clientErr := r.clientFor(sess)
				if clientErr == nil {
					result, err = client.Checkin(ctx, sess.token)
				} else {
					err = clientErr
				}
			}
		}
		if err != nil {
			return r.recordActionError(action, err, isUnknown(err), false)
		}
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

// reconcileFailedCheckin checks the upstream truth for a locally failed
// check-in. A nil outcome means the caller should retry the action itself;
// newAction then carries the reopened (pending) action.
func (r *Runner) reconcileFailedCheckin(ctx context.Context, accountID string, action state.Action) (state.Action, *ActionOutcome, error) {
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return action, nil, nil
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return action, nil, nil
	}
	status, settings, err := fetchCheckinDisplayStatus(ctx, client, sess.token)
	if err != nil {
		if subscriptionAuthError(err) {
			renewed, renewErr := r.renewParent(ctx, accountID, sess.token)
			if renewErr == nil {
				if retryClient, clientErr := r.clientFor(renewed); clientErr == nil {
					if retryStatus, retrySettings, statusErr := fetchCheckinDisplayStatus(ctx, retryClient, renewed.token); statusErr == nil {
						status, settings = retryStatus, retrySettings
						err = nil
					}
				}
			}
		}
		if err != nil {
			return action, nil, nil
		}
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
				reward := QuotaMoney(*status.TodayQuotaAwarded, settings, "checkin.quota_awarded", r.now().UTC())
				if reward.State == quota.StateConfirmed {
					value.CheckinQuotaAwardedUSD = &reward
					value.Message = fmt.Sprintf("今日已签到，获得额度：%s", reward.Display)
				}
			}
		})
		if updateErr != nil {
			return action, nil, updateErr
		}
		return action, &ActionOutcome{Action: updated, AlreadyRecorded: true}, nil
	}
	reset, resetErr := r.resetFailedCheckinAction(action)
	if resetErr != nil {
		return action, nil, resetErr
	}
	return reset, nil, nil
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
	if _, err := r.account(accountID); err != nil {
		return CheckinStatusReport{}, err
	}
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	status, settings, sess, err := r.checkinDisplayStatusWithRecovery(ctx, client, accountID, sess)
	if err != nil {
		return CheckinStatusReport{}, err
	}
	report := CheckinStatusReport{CheckedInToday: status.CheckedInToday}
	if status.TodayQuotaAwarded != nil {
		reward := QuotaMoney(*status.TodayQuotaAwarded, settings, "checkin.quota_awarded", r.now().UTC())
		if reward.State == quota.StateConfirmed {
			report.TodayQuotaAwardedUSD = &reward
		}
	}
	return report, nil
}

func (r *Runner) QueryUsage(ctx context.Context, accountID string) (lottery.UserUsage, error) {
	if _, err := r.account(accountID); err != nil {
		return lottery.UserUsage{}, err
	}
	sess, err := r.acquire(ctx, accountID, auth.ReadOnly, auth.SessionParent)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	usage, err := client.UserSelf(ctx, sess.token)
	if err != nil {
		if !lottery.IsStatus(err, 401) {
			return lottery.UserUsage{}, err
		}
		sess, err = r.renewParent(ctx, accountID, sess.token)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		client, err = r.clientFor(sess)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		usage, err = client.UserSelf(ctx, sess.token)
		if err != nil {
			return lottery.UserUsage{}, err
		}
	}
	settings, statusErr := client.Status(ctx, sess.token)
	if statusErr != nil && subscriptionAuthError(statusErr) {
		sess, err = r.renewParent(ctx, accountID, sess.token)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		client, err = r.clientFor(sess)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		usage, err = client.UserSelf(ctx, sess.token)
		if err != nil {
			return lottery.UserUsage{}, err
		}
		settings, statusErr = client.Status(ctx, sess.token)
	}
	if statusErr == nil {
		usage = normalizeUsage(usage, settings)
	} else {
		usage.QuotaConversionAvailable = false
		usage.QuotaConversionError = "无法获取美元换算配置"
	}
	payload, err := json.Marshal(usage)
	if err != nil {
		return lottery.UserUsage{}, err
	}
	if err := r.store.PutSnapshot(state.Snapshot{AccountID: accountID, Kind: "usage", Data: payload, QueriedAt: r.now().UTC()}); err != nil {
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

// acquireParentForAction obtains a parent session plus a matching client for
// a side-effecting action.
func (r *Runner) acquireParentForAction(ctx context.Context, accountID string) (session, WebsiteClient, error) {
	sess, err := r.acquire(ctx, accountID, auth.SideEffect, auth.SessionParent)
	if err != nil {
		return session{}, nil, err
	}
	client, err := r.clientFor(sess)
	if err != nil {
		return session{}, nil, err
	}
	return sess, client, nil
}

func (r *Runner) dashboardWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session) (session, lottery.Dashboard, error) {
	dashboard, err := client.Dashboard(ctx, sess.token)
	if err == nil {
		return sess, dashboard, nil
	}
	if lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403) {
		renewed, renewErr := r.renewLottery(ctx, accountID, sess.token)
		if renewErr != nil {
			return sess, lottery.Dashboard{}, renewErr
		}
		retryClient, clientErr := r.clientFor(renewed)
		if clientErr != nil {
			return renewed, lottery.Dashboard{}, clientErr
		}
		dashboard, err = retryClient.Dashboard(ctx, renewed.token)
		return renewed, dashboard, err
	}
	if !lottery.IsTransient(err) {
		return sess, lottery.Dashboard{}, err
	}
	if err := r.wait(ctx, 2*time.Second); err != nil {
		return sess, lottery.Dashboard{}, err
	}
	dashboard, err = client.Dashboard(ctx, sess.token)
	return sess, dashboard, err
}

func (r *Runner) claimWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session) (lottery.ClaimResult, session, error) {
	result, err := client.ClaimDaily(ctx, sess.token)
	if err == nil {
		return result, sess, nil
	}
	if !lottery.IsStatus(err, 401) {
		return lottery.ClaimResult{}, sess, err
	}
	renewed, err := r.renewLottery(ctx, accountID, sess.token)
	if err != nil {
		return lottery.ClaimResult{}, sess, err
	}
	retryClient, clientErr := r.clientFor(renewed)
	if clientErr != nil {
		return lottery.ClaimResult{}, renewed, clientErr
	}
	result, err = retryClient.ClaimDaily(ctx, renewed.token)
	return result, renewed, err
}

func (r *Runner) checkinEligibilityWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session) (lottery.CheckinEligibility, session, error) {
	eligibility, err := client.CheckinEligibility(ctx, sess.token, sess.userID)
	if err == nil {
		return eligibility, sess, nil
	}
	if !subscriptionAuthError(err) {
		return lottery.CheckinEligibility{}, sess, err
	}
	renewed, err := r.renewParent(ctx, accountID, sess.token)
	if err != nil {
		return lottery.CheckinEligibility{}, sess, err
	}
	retryClient, clientErr := r.clientFor(renewed)
	if clientErr != nil {
		return lottery.CheckinEligibility{}, renewed, clientErr
	}
	eligibility, err = retryClient.CheckinEligibility(ctx, renewed.token, renewed.userID)
	return eligibility, renewed, err
}

func (r *Runner) checkinDisplayStatusWithRecovery(ctx context.Context, client WebsiteClient, accountID string, sess session) (lottery.CheckinStatus, lottery.StatusSettings, session, error) {
	status, settings, err := fetchCheckinDisplayStatus(ctx, client, sess.token)
	if err == nil {
		return status, settings, sess, nil
	}
	if !subscriptionAuthError(err) {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, sess, err
	}
	renewed, err := r.renewParent(ctx, accountID, sess.token)
	if err != nil {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, sess, err
	}
	retryClient, clientErr := r.clientFor(renewed)
	if clientErr != nil {
		return lottery.CheckinStatus{}, lottery.StatusSettings{}, renewed, clientErr
	}
	status, settings, err = fetchCheckinDisplayStatus(ctx, retryClient, renewed.token)
	return status, settings, renewed, err
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

func (r *Runner) today() string {
	return r.now().In(shanghaiLocation).Format("2006-01-02")
}

// account resolves the registry record and refuses missing or disabled
// accounts before any session is acquired.
func (r *Runner) account(id string) (account.Record, error) {
	record, err := r.repo.Get(strings.TrimSpace(id))
	if errors.Is(err, account.ErrNotFound) {
		return account.Record{}, fmt.Errorf("unknown account %q", id)
	}
	if err != nil {
		return account.Record{}, err
	}
	if record.Status != account.StatusEnabled {
		return account.Record{}, fmt.Errorf("账号已停用，无法执行该操作")
	}
	return record, nil
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
