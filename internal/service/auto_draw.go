package service

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

const (
	autoDrawTickInterval    = time.Minute
	autoDrawOperationWindow = 2 * time.Minute
	autoDrawRetentionDays   = 7
)

// ScheduleLabel renders a human-readable description of a user-defined
// auto-draw schedule entry.
func ScheduleLabel(entry state.AutoDrawSchedule) string {
	if entry.Kind == state.AutoDrawScheduleRandom {
		return fmt.Sprintf("每天 %s–%s 随机", entry.Start, entry.End)
	}
	return fmt.Sprintf("每天 %s", entry.Start)
}

type AutoDrawExecutor func(context.Context, string, string) (DrawAvailableOutcome, error)

// AutoDrawScheduler persists one randomly scheduled draw for each configured
// account/window. A persisted idempotency key lets a plan safely resume after a
// process restart without creating a second draw request.
type AutoDrawScheduler struct {
	store            *state.Store
	repo             account.Repository
	now              func() time.Time
	randomOffset     func(int) (int, error)
	draw             AutoDrawExecutor
	tickInterval     time.Duration
	operationTimeout time.Duration
}

func NewAutoDrawScheduler(cfg config.Config, store *state.Store, broker *auth.Broker) *AutoDrawScheduler {
	runner := NewRunner(cfg, store, store.AccountRegistry(), broker)
	return &AutoDrawScheduler{
		store:            store,
		repo:             store.AccountRegistry(),
		now:              time.Now,
		randomOffset:     randomSecondOffset,
		draw:             runner.DrawAvailableScheduled,
		tickInterval:     autoDrawTickInterval,
		operationTimeout: autoDrawOperationWindow,
	}
}

// Run performs an immediate reconciliation on startup, then checks once per
// minute. It stops only when the supplied context is cancelled.
func (s *AutoDrawScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
		log.Printf("auto draw scheduler tick failed: %v", err)
	}

	interval := s.tickInterval
	if interval <= 0 {
		interval = autoDrawTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				log.Printf("auto draw scheduler tick failed: %v", err)
			}
		}
	}
}

// Tick generates today's missing plans, prunes data beyond the retention
// boundary, and completes every due plan exactly once.
func (s *AutoDrawScheduler) Tick(ctx context.Context) error {
	if s == nil || s.store == nil {
		return errors.New("auto draw scheduler store is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.currentTime()
	date := now.Format("2006-01-02")
	cutoff := now.AddDate(0, 0, -autoDrawRetentionDays)
	if err := s.store.PruneAutoDrawData(cutoff.Format("2006-01-02"), cutoff.UTC()); err != nil {
		return fmt.Errorf("prune auto draw data: %w", err)
	}
	plans, err := s.ensurePlans(date, now)
	if err != nil {
		return err
	}

	var tickError error
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if plan.PlannedAt.After(now) || autoDrawPlanTerminal(plan.Status) {
			continue
		}
		if err := s.executePlan(ctx, plan.Key); err != nil {
			tickError = errors.Join(tickError, err)
		}
	}
	return tickError
}

// ensurePlans materializes today's pending plan for every enabled account's
// schedule entry. Window entries whose end has already passed today are
// skipped; they start again the next day.
func (s *AutoDrawScheduler) ensurePlans(date string, now time.Time) ([]state.AutoDrawPlan, error) {
	enabled, err := s.repo.ListEnabled()
	if err != nil {
		return nil, fmt.Errorf("list enabled accounts: %w", err)
	}
	existing := s.store.AutoDrawPlans(date)
	existingByAccountSchedule := make(map[string]state.AutoDrawPlan, len(existing))
	for _, plan := range existing {
		existingByAccountSchedule[plan.AccountID+"\x00"+plan.WindowID] = plan
	}
	missing := make([]state.AutoDrawPlan, 0)
	for _, record := range enabled {
		accountID := strings.TrimSpace(record.ID)
		if accountID == "" {
			continue
		}
		for _, entry := range s.store.DrawSchedules(record.ID) {
			if _, ok := existingByAccountSchedule[accountID+"\x00"+entry.ID]; ok {
				continue
			}
			plannedAt, ok := s.scheduleTime(date, entry, now)
			if !ok {
				continue
			}
			missing = append(missing, state.AutoDrawPlan{
				Date:      date,
				AccountID: accountID,
				WindowID:  entry.ID,
				PlannedAt: plannedAt.UTC(),
				Status:    state.AutoDrawPlanPending,
			})
		}
	}
	if len(missing) > 0 {
		if _, err := s.store.EnsureAutoDrawPlans(missing); err != nil {
			return nil, fmt.Errorf("persist auto draw plans: %w", err)
		}
	}
	return s.store.AutoDrawPlans(date), nil
}

// scheduleTime resolves when the schedule should fire today (Beijing time).
// ok=false means today's window is already over, so no plan is created.
func (s *AutoDrawScheduler) scheduleTime(date string, entry state.AutoDrawSchedule, now time.Time) (time.Time, bool) {
	day, err := time.ParseInLocation("2006-01-02", date, shanghaiLocation)
	if err != nil {
		return time.Time{}, false
	}
	startMinute := clockMinutesOf(entry.Start)
	startAt := time.Date(day.Year(), day.Month(), day.Day(), startMinute/60, startMinute%60, 0, 0, shanghaiLocation)
	if entry.Kind != state.AutoDrawScheduleRandom {
		return startAt, startAt.After(now)
	}
	endMinute := clockMinutesOf(entry.End)
	endAt := time.Date(day.Year(), day.Month(), day.Day(), endMinute/60, endMinute%60, 0, 0, shanghaiLocation)
	// A window added mid-flight draws randomly within the remaining part.
	effectiveStart := startAt
	if effectiveStart.Before(now) {
		effectiveStart = now
	}
	if !endAt.After(effectiveStart) {
		return time.Time{}, false
	}
	duration := int(endAt.Sub(effectiveStart).Seconds())
	if duration > 3600 {
		duration = 3600
	}
	offset, err := s.randomOffset(duration)
	if err != nil {
		return time.Time{}, false
	}
	return effectiveStart.Add(time.Duration(offset) * time.Second), true
}

func clockMinutesOf(value string) int {
	parts := strings.Split(value, ":")
	hour, _ := strconv.Atoi(parts[0])
	minute, _ := strconv.Atoi(parts[1])
	return hour*60 + minute
}

func (s *AutoDrawScheduler) executePlan(parent context.Context, key string) error {
	release := s.store.LockAutoDrawPlan(key)
	defer release()

	plan, shouldExecute, err := s.store.BeginAutoDrawPlan(key)
	if err != nil {
		return fmt.Errorf("begin auto draw plan: %w", err)
	}
	if !shouldExecute {
		return nil
	}
	if record, recordErr := s.repo.Get(plan.AccountID); recordErr != nil || record.Status != account.StatusEnabled {
		// The account disappeared or was disabled after planning; the window
		// must not spend its draw opportunity nor touch authentication.
		finished, finishErr := s.store.FinishAutoDrawPlan(key, state.AutoDrawPlanSkipped, "账号已停用或删除，跳过本次自动抽奖", "", nil, s.currentTime().UTC())
		if finishErr != nil {
			return fmt.Errorf("finish auto draw plan: %w", finishErr)
		}
		if _, err := s.store.AppendRuntimeLog(state.RuntimeLog{
			OccurredAt: finished.ExecutedAt,
			AccountID:  finished.AccountID,
			WindowID:   finished.WindowID,
			Status:     finished.Status,
			Message:    finished.Message,
		}); err != nil {
			return fmt.Errorf("append auto draw runtime log: %w", err)
		}
		return nil
	}

	operationTimeout := s.operationTimeout
	if operationTimeout <= 0 {
		operationTimeout = autoDrawOperationWindow
	}
	ctx, cancel := context.WithTimeout(parent, operationTimeout)
	outcome, drawErr := s.draw(ctx, plan.AccountID, plan.IdempotencyKey)
	cancel()
	if errors.Is(drawErr, context.Canceled) && parent.Err() != nil {
		// The service is stopping, not a draw failure. Keep this plan in its
		// persisted running state so a future process can resume it with the
		// same idempotency key instead of burning the window.
		return parent.Err()
	}

	executedAt := s.currentTime().UTC()
	status, message, prizeLabel, quotaDeltaUSD := autoDrawExecutionResult(outcome, drawErr)
	finished, err := s.store.FinishAutoDrawPlan(key, status, message, prizeLabel, quotaDeltaUSD, executedAt)
	if err != nil {
		return fmt.Errorf("finish auto draw plan: %w", err)
	}
	if _, err := s.store.AppendRuntimeLog(state.RuntimeLog{
		OccurredAt:    executedAt,
		AccountID:     finished.AccountID,
		WindowID:      finished.WindowID,
		Status:        finished.Status,
		Message:       finished.Message,
		PrizeLabel:    finished.PrizeLabel,
		QuotaDeltaUSD: finished.QuotaDeltaUSD,
	}); err != nil {
		return fmt.Errorf("append auto draw runtime log: %w", err)
	}
	return nil
}

func autoDrawExecutionResult(outcome DrawAvailableOutcome, drawErr error) (state.AutoDrawPlanStatus, string, string, *quota.Money) {
	if drawErr != nil {
		if errors.Is(drawErr, auth.ErrReauthRequired) {
			return state.AutoDrawPlanSkipped, "登录状态失效，需要重新认证，已跳过本次自动抽奖", "", nil
		}
		return state.AutoDrawPlanFailed, autoDrawFailureMessage(drawErr), "", nil
	}
	if outcome.Skipped {
		return state.AutoDrawPlanSkipped, "无可用抽奖次数，已跳过", "", nil
	}
	prizeLabel := ""
	if outcome.Result != nil {
		prizeLabel = safeAutoDrawDisplay(outcome.Result.Prize.ShortLabel)
		if prizeLabel == "" {
			prizeLabel = safeAutoDrawDisplay(outcome.Result.Prize.Label)
		}
	}
	return state.AutoDrawPlanCompleted, "自动抽奖成功", prizeLabel, outcome.QuotaDeltaUSD
}

func autoDrawFailureMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "抽奖请求超时"
	}
	if errors.Is(err, context.Canceled) {
		return "抽奖任务已取消"
	}
	var apiErr *lottery.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		if apiErr.StatusCode >= 400 && apiErr.StatusCode <= 599 {
			return fmt.Sprintf("抽奖接口返回 HTTP %d", apiErr.StatusCode)
		}
	}
	return "抽奖服务请求失败"
}

func safeAutoDrawDisplay(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if len(value) > 120 {
		return value[:120]
	}
	return value
}

func autoDrawPlanTerminal(status state.AutoDrawPlanStatus) bool {
	return status == state.AutoDrawPlanCompleted || status == state.AutoDrawPlanSkipped || status == state.AutoDrawPlanFailed
}

func (s *AutoDrawScheduler) currentTime() time.Time {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	return now.In(shanghaiLocation)
}

func randomSecondOffset(limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("random limit must be positive")
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}
