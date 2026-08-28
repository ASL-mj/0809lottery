package service

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/state"
)

const (
	autoDrawTickInterval    = time.Minute
	autoDrawOperationWindow = 2 * time.Minute
	autoDrawRetentionDays   = 7
)

type AutoDrawWindow struct {
	ID        string
	Label     string
	StartHour int
}

var autoDrawWindows = []AutoDrawWindow{
	{ID: "morning", Label: "早间 08:00–09:00", StartHour: 8},
	{ID: "afternoon", Label: "午间 13:00–14:00", StartHour: 13},
	{ID: "evening", Label: "晚间 18:00–19:00", StartHour: 18},
}

func AutoDrawWindows() []AutoDrawWindow {
	return append([]AutoDrawWindow(nil), autoDrawWindows...)
}

type AutoDrawExecutor func(context.Context, string, string) (DrawAvailableOutcome, error)

// AutoDrawScheduler persists one randomly scheduled draw for each configured
// account/window. A persisted idempotency key lets a plan safely resume after a
// process restart without creating a second draw request.
type AutoDrawScheduler struct {
	store            *state.Store
	accounts         []config.Account
	now              func() time.Time
	randomOffset     func(int) (int, error)
	draw             AutoDrawExecutor
	tickInterval     time.Duration
	operationTimeout time.Duration
}

func NewAutoDrawScheduler(cfg config.Config, store *state.Store, broker *auth.Broker) *AutoDrawScheduler {
	accounts := make([]config.Account, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	runner := NewRunner(cfg, store, broker)
	return &AutoDrawScheduler{
		store:            store,
		accounts:         accounts,
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

func (s *AutoDrawScheduler) ensurePlans(date string, now time.Time) ([]state.AutoDrawPlan, error) {
	existing := s.store.AutoDrawPlans(date)
	existingByAccountWindow := make(map[string]state.AutoDrawPlan, len(existing))
	for _, plan := range existing {
		existingByAccountWindow[plan.AccountID+"\x00"+plan.WindowID] = plan
	}
	missing := make([]state.AutoDrawPlan, 0, len(s.accounts)*len(autoDrawWindows))
	for _, account := range s.accounts {
		accountID := strings.TrimSpace(account.ID)
		if accountID == "" {
			continue
		}
		for _, window := range autoDrawWindows {
			if _, ok := existingByAccountWindow[accountID+"\x00"+window.ID]; ok {
				continue
			}
			offset, err := s.randomOffset(60 * 60)
			if err != nil {
				return nil, fmt.Errorf("schedule random time for %s/%s: %w", accountID, window.ID, err)
			}
			if offset < 0 || offset >= 60*60 {
				return nil, fmt.Errorf("schedule random time for %s/%s is outside window", accountID, window.ID)
			}
			plannedAt := time.Date(now.Year(), now.Month(), now.Day(), window.StartHour, 0, 0, 0, shanghaiLocation).Add(time.Duration(offset) * time.Second)
			missing = append(missing, state.AutoDrawPlan{
				Date:      date,
				AccountID: accountID,
				WindowID:  window.ID,
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

func autoDrawExecutionResult(outcome DrawAvailableOutcome, drawErr error) (state.AutoDrawPlanStatus, string, string, *float64) {
	if drawErr != nil {
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
