package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/service"
	"skyeapi/lottery-bot/internal/state"
)

//go:embed static/index.html
var indexHTML []byte

const (
	maxRequestBodySize = 8 << 10
	maxRuntimeLogs     = 200
)

var shanghaiLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

type Server struct {
	cfg     config.Config
	storeMu sync.Mutex
	store   *state.Store
	broker  *auth.Broker
	// vaultFactory is overridden in tests to bridge legacy persisted auth
	// state; production always uses the encrypted file vault.
	vaultFactory func(store *state.Store) (secret.Vault, error)
}

func NewServer(cfg config.Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) Run(ctx context.Context) error {
	store, err := s.sharedStore()
	if err != nil {
		return fmt.Errorf("open account workbench state: %w", err)
	}
	server := &http.Server{
		Addr:              s.cfg.WebAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      4 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", s.cfg.WebAddr)
	if err != nil {
		_ = s.Close()
		return err
	}

	broker, err := s.sharedBroker()
	if err != nil {
		_ = s.Close()
		return fmt.Errorf("create session broker: %w", err)
	}
	schedulerCtx, cancelScheduler := context.WithCancel(ctx)
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		service.NewAutoDrawScheduler(s.cfg, store, broker).Run(schedulerCtx)
	}()

	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancel()
		case <-shutdownDone:
		}
	}()
	defer close(shutdownDone)
	defer func() {
		_ = s.Close()
	}()
	defer func() {
		cancelScheduler()
		<-schedulerDone
	}()

	log.Printf("account workbench listening on %s", s.cfg.WebAddr)
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	s.storeMu.Lock()
	store := s.store
	broker := s.broker
	s.store = nil
	s.broker = nil
	s.storeMu.Unlock()
	_ = broker
	if store == nil {
		return nil
	}
	return store.Close()
}

func (s *Server) sharedStore() (*state.Store, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	return s.sharedStoreLocked()
}

func (s *Server) sharedStoreLocked() (*state.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	store, err := state.Open(s.cfg.StatePath)
	if err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}

// sharedBroker lazily builds the process-wide session broker over the shared
// state store and the encrypted vault.
func (s *Server) sharedBroker() (*auth.Broker, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.broker != nil {
		return s.broker, nil
	}
	store, err := s.sharedStoreLocked()
	if err != nil {
		return nil, err
	}
	vaultFactory := s.vaultFactory
	if vaultFactory == nil {
		vaultFactory = func(*state.Store) (secret.Vault, error) {
			return secret.NewFileVault(s.cfg.VaultPath, s.cfg.VaultKey)
		}
	}
	vault, err := vaultFactory(store)
	if err != nil {
		return nil, err
	}
	s.broker = auth.NewBroker(store, vault, func(cookies []state.Cookie) (auth.PlatformClient, error) {
		return lottery.NewClient(s.cfg.BaseURL, s.cfg.UserAgent, cookies)
	})
	return s.broker, nil
}

func (s *Server) runnerFor(store *state.Store) (*service.Runner, error) {
	broker, err := s.sharedBroker()
	if err != nil {
		return nil, err
	}
	return service.NewRunner(s.cfg, store, store.AccountRegistry(), broker), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/accounts/", s.handleAccountAction)
	mux.HandleFunc("/api/draw-count/query", s.handleDrawCountQuery)
	mux.HandleFunc("/api/subscriptions/query", s.handleSubscriptionQuery)
	mux.HandleFunc("/api/auto-draw-status", s.handleAutoDrawStatus)
	mux.HandleFunc("/api/runtime-logs", s.handleRuntimeLogs)
	return s.withSecurityHeaders(s.withBasicAuth(mux))
}

func (s *Server) handleRuntimeLogs(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	type runtimeLogView struct {
		ID            string    `json:"id"`
		OccurredAt    time.Time `json:"occurred_at"`
		AccountID     string    `json:"account_id"`
		AccountLabel  string    `json:"account_label,omitempty"`
		WindowID      string    `json:"window_id"`
		Status        string    `json:"status"`
		Message       string    `json:"message"`
		PrizeLabel    string    `json:"prize_label,omitempty"`
		QuotaDeltaUSD *float64  `json:"quota_delta_usd,omitempty"`
	}
	logs := store.RuntimeLogs(maxRuntimeLogs)
	response := make([]runtimeLogView, 0, len(logs))
	for _, entry := range logs {
		label := ""
		if account, ok := s.cfg.Accounts[entry.AccountID]; ok {
			label = account.Label
		}
		response = append(response, runtimeLogView{
			ID:            entry.ID,
			OccurredAt:    entry.OccurredAt,
			AccountID:     entry.AccountID,
			AccountLabel:  label,
			WindowID:      entry.WindowID,
			Status:        string(entry.Status),
			Message:       publicRuntimeLogText(entry.Message),
			PrizeLabel:    publicRuntimeLogText(entry.PrizeLabel),
			QuotaDeltaUSD: entry.QuotaDeltaUSD,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"logs": response})
}

func (s *Server) handleAutoDrawStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	type windowView struct {
		WindowID      string     `json:"window_id"`
		PlannedAt     *time.Time `json:"planned_at,omitempty"`
		ExecutedAt    *time.Time `json:"executed_at,omitempty"`
		Status        string     `json:"status"`
		Message       string     `json:"message,omitempty"`
		PrizeLabel    string     `json:"prize_label,omitempty"`
		QuotaDeltaUSD *float64   `json:"quota_delta_usd,omitempty"`
	}
	type accountView struct {
		AccountID string       `json:"account_id"`
		Windows   []windowView `json:"windows"`
	}

	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	plansByAccountWindow := make(map[string]state.AutoDrawPlan)
	for _, plan := range store.AutoDrawPlans(today) {
		plansByAccountWindow[plan.AccountID+"\x00"+plan.WindowID] = plan
	}
	ids := make([]string, 0, len(s.cfg.Accounts))
	for id := range s.cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	autoDrawWindows := service.AutoDrawWindows()
	accounts := make([]accountView, 0, len(ids))
	for _, accountID := range ids {
		windows := make([]windowView, 0, len(autoDrawWindows))
		for _, window := range autoDrawWindows {
			view := windowView{WindowID: window.ID, Status: string(state.AutoDrawPlanPending)}
			if plan, ok := plansByAccountWindow[accountID+"\x00"+window.ID]; ok {
				view.Status = string(plan.Status)
				view.PlannedAt = timePointer(plan.PlannedAt)
				view.ExecutedAt = timePointer(plan.ExecutedAt)
				view.Message = publicRuntimeLogText(plan.Message)
				view.PrizeLabel = publicRuntimeLogText(plan.PrizeLabel)
				view.QuotaDeltaUSD = plan.QuotaDeltaUSD
			}
			windows = append(windows, view)
		}
		accounts = append(accounts, accountView{AccountID: accountID, Windows: windows})
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"date": today, "accounts": accounts})
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func publicRuntimeLogText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"token", "cookie", "password", "idempotency", "bearer ", "access_token", "refresh_token", "令牌", "密码", "凭证"} {
		if strings.Contains(lower, marker) {
			return "已隐藏敏感详情"
		}
	}
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func (s *Server) handleIndex(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		writeError(writer, http.StatusNotFound, "页面不存在")
		return
	}
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write(indexHTML)
}

func (s *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"ok": true, "service": "account-workbench", "time": time.Now().UTC()})
}

func (s *Server) handleAccounts(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}

	ids := make([]string, 0, len(s.cfg.Accounts))
	for id := range s.cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	accounts := make([]map[string]interface{}, 0, len(ids))
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	for _, id := range ids {
		account := s.cfg.Accounts[id]
		item := map[string]interface{}{
			"id":             id,
			"label":          account.Label,
			"username":       account.Username,
			"checkin_status": "pending",
			"claim_status":   "pending",
		}
		var storedCheckinAwardUSD *float64
		if action, ok := store.Action(id, today, state.ActionCheckin); ok {
			item["checkin_status"] = action.Status
			item["checkin_message"] = action.Message
			if action.CheckinQuotaAwardedUSD != nil {
				awardUSD := *action.CheckinQuotaAwardedUSD
				storedCheckinAwardUSD = &awardUSD
				item["checkin_quota_awarded"] = awardUSD
			}
		}
		if action, ok := store.Action(id, today, state.ActionDailyClaim); ok {
			item["claim_status"] = action.Status
			item["claim_message"] = publicClaimMessage(action.Status, action.Status == state.ActionCompleted)
			item["claim_added"] = claimAdded(action)
			if remaining, ok := claimRemaining(action); ok {
				item["claim_remaining"] = remaining
			}
		}
		statusCtx, cancel := context.WithTimeout(request.Context(), 35*time.Second)
		checkinStatus, statusErr := runner.CheckinStatus(statusCtx, id)
		cancel()
		if statusErr == nil {
			item["checkin_status"] = "pending"
			delete(item, "checkin_message")
			delete(item, "checkin_quota_awarded")
			if checkinStatus.CheckedInToday {
				if err := s.markCheckinCompleted(store, id, today, checkinStatus); err != nil {
					writeStoreError(writer, err)
					return
				}
				item["checkin_status"] = state.ActionCompleted
				item["checkin_message"] = completedCheckinMessage(checkinStatus.TodayQuotaAwardedUSD)
				if checkinStatus.TodayQuotaAwardedUSD != nil {
					item["checkin_quota_awarded"] = *checkinStatus.TodayQuotaAwardedUSD
				} else if storedCheckinAwardUSD != nil {
					item["checkin_quota_awarded"] = *storedCheckinAwardUSD
				}
			}
		}
		if snapshot, ok := store.Snapshot(id, "subscriptions"); ok {
			var data struct {
				AccountID string          `json:"account_id"`
				Data      json.RawMessage `json:"-"`
			}
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == id {
				item["subscription_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(id, "draw-count"); ok {
			var data service.DrawCountReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == id {
				item["draw_count_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(id, "activity"); ok {
			var data service.ActivityReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == id {
				item["activity_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		accounts = append(accounts, item)
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"accounts": accounts})
}

func (s *Server) handleAccountAction(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "accounts" || parts[2] == "" {
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}
	action := parts[3]
	switch action {
	case "checkin", "claim", "draw", "activity", "purchase-draw", "unlock-pass":
	default:
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	accountID := parts[2]
	if _, ok := s.cfg.Accounts[accountID]; !ok {
		writeError(writer, http.StatusNotFound, "账号不存在")
		return
	}
	switch action {
	case "checkin":
		s.handleCheckinAction(writer, request, accountID)
	case "claim":
		s.handleClaimAction(writer, request, accountID)
	case "draw":
		s.handleDrawAction(writer, request, accountID)
	case "activity":
		s.handleActivityAction(writer, request, accountID)
	case "purchase-draw":
		s.handlePurchaseDrawAction(writer, request, accountID)
	case "unlock-pass":
		s.handleUnlockPassAction(writer, request, accountID)
	}
}

func (s *Server) handleCheckinAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	outcome, err := runner.Checkin(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "checkin", accountID, err, "签到暂时失败，请稍后重试")
		return
	}
	awardUSD := outcome.Action.CheckinQuotaAwardedUSD
	checkinMessage := outcome.Action.Message
	if outcome.Action.Status == state.ActionCompleted {
		if status, statusErr := runner.CheckinStatus(ctx, accountID); statusErr == nil {
			if status.CheckedInToday {
				if err := s.markCheckinCompleted(store, accountID, time.Now().In(shanghaiLocation).Format("2006-01-02"), status); err != nil {
					writeStoreError(writer, err)
					return
				}
				if status.TodayQuotaAwardedUSD != nil {
					awardUSD = status.TodayQuotaAwardedUSD
				}
			}
		}
		if awardUSD != nil {
			checkinMessage = completedCheckinMessage(awardUSD)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id":            accountID,
		"checkin_status":        outcome.Action.Status,
		"checkin_message":       checkinMessage,
		"checkin_quota_awarded": awardUSD,
		"already_completed":     outcome.AlreadyRecorded && outcome.Action.Status == state.ActionCompleted,
	})
}

func (s *Server) handleClaimAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	outcome, err := runner.ClaimDaily(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "claim", accountID, err, "领取每日抽奖暂时失败，请稍后重试")
		return
	}
	response := struct {
		AccountID        string `json:"account_id"`
		ClaimStatus      string `json:"claim_status"`
		ClaimMessage     string `json:"claim_message"`
		AlreadyCompleted bool   `json:"already_completed"`
		Added            int    `json:"added"`
		Remaining        *int   `json:"remaining,omitempty"`
	}{
		AccountID:        accountID,
		ClaimStatus:      string(outcome.Action.Status),
		ClaimMessage:     publicClaimMessage(outcome.Action.Status, outcome.AlreadyRecorded && outcome.Action.Status == state.ActionCompleted),
		AlreadyCompleted: outcome.AlreadyRecorded && outcome.Action.Status == state.ActionCompleted,
		Added:            outcome.Added,
		Remaining:        outcome.Remaining,
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) handleDrawAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	drawKey, err := webDrawKey()
	if err != nil {
		log.Printf("web draw key generation failed for account=%s: %v", accountID, err)
		writeError(writer, http.StatusInternalServerError, "手动抽奖暂时不可用，请稍后重试")
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	outcome, err := runner.DrawAvailable(ctx, accountID, drawKey)
	if err != nil {
		writeUpstreamError(writer, "draw", accountID, err, "手动抽奖暂时失败，请稍后重试")
		return
	}
	var prizeLabel string
	if outcome.Result != nil {
		prizeLabel = firstNonEmpty(outcome.Result.Prize.Label, outcome.Result.Prize.ShortLabel)
	}
	response := struct {
		AccountID       string   `json:"account_id"`
		Skipped         bool     `json:"skipped"`
		RemainingBefore int      `json:"remaining_before"`
		Message         string   `json:"message"`
		PrizeLabel      string   `json:"prize_label,omitempty"`
		QuotaDeltaUSD   *float64 `json:"quota_delta_usd,omitempty"`
	}{
		AccountID:       accountID,
		Skipped:         outcome.Skipped,
		RemainingBefore: outcome.RemainingBefore,
		Message:         outcome.Message,
		PrizeLabel:      prizeLabel,
		QuotaDeltaUSD:   outcome.QuotaDeltaUSD,
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) handleActivityAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	ctx, cancel := context.WithTimeout(request.Context(), 90*time.Second)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	report, err := runner.QueryActivity(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "activity", accountID, err, "活动信息暂时刷新失败，请稍后重试")
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) handlePurchaseDrawAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	s.handlePurchaseAction(writer, request, accountID, "purchase-draw", "购买抽奖次数暂时失败，请稍后重试", func(ctx context.Context, runner *service.Runner) (service.PurchaseOutcome, error) {
		return runner.PurchaseDraw(ctx, accountID)
	})
}

func (s *Server) handleUnlockPassAction(writer http.ResponseWriter, request *http.Request, accountID string) {
	s.handlePurchaseAction(writer, request, accountID, "unlock-pass", "购买通行证暂时失败，请稍后重试", func(ctx context.Context, runner *service.Runner) (service.PurchaseOutcome, error) {
		return runner.UnlockDailyPass(ctx, accountID)
	})
}

func (s *Server) handlePurchaseAction(writer http.ResponseWriter, request *http.Request, accountID, action, fallback string, run func(context.Context, *service.Runner) (service.PurchaseOutcome, error)) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	outcome, err := run(ctx, runner)
	if err != nil {
		writeUpstreamError(writer, action, accountID, err, fallback)
		return
	}
	response := struct {
		AccountID string                  `json:"account_id"`
		Status    string                  `json:"status"`
		Message   string                  `json:"message"`
		PriceUSD  *float64                `json:"price_usd,omitempty"`
		Remaining *int                    `json:"remaining,omitempty"`
		Activity  *service.ActivityReport `json:"activity,omitempty"`
	}{
		AccountID: accountID,
		Status:    outcome.Status,
		Message:   outcome.Message,
		PriceUSD:  outcome.PriceUSD,
		Remaining: outcome.Remaining,
		Activity:  outcome.Activity,
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) handleDrawCountQuery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	if input.AccountID == "" || input.AccountID == "all" {
		writeError(writer, http.StatusBadRequest, "抽奖次数查询需要指定一个账号")
		return
	}
	if _, ok := s.cfg.Accounts[input.AccountID]; !ok {
		writeError(writer, http.StatusNotFound, "账号不存在")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 90*time.Second)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	report, err := runner.QueryDrawCount(ctx, input.AccountID)
	if err != nil {
		writeUpstreamError(writer, "draw-count", input.AccountID, err, "抽奖次数查询暂时失败，请稍后重试")
		return
	}
	payload, err := json.Marshal(report)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法保存抽奖次数查询结果")
		return
	}
	if err := store.PutSnapshot(state.Snapshot{AccountID: input.AccountID, Kind: "draw-count", Data: payload, QueriedAt: report.QueriedAt}); err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) handleSubscriptionQuery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	if input.AccountID == "" || input.AccountID == "all" {
		writeError(writer, http.StatusBadRequest, "订阅查询需要指定一个账号")
		return
	}
	if _, ok := s.cfg.Accounts[input.AccountID]; !ok {
		writeError(writer, http.StatusNotFound, "账号不存在")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 90*time.Second)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	runner, err := s.runnerFor(store)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	report, err := runner.QuerySubscriptions(ctx, input.AccountID)
	if err != nil {
		writeUpstreamError(writer, "subscriptions", input.AccountID, err, "订阅查询暂时失败，请稍后重试")
		return
	}
	response := singleSubscriptionView(input.AccountID, report)
	payload, err := json.Marshal(response)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "无法保存订阅查询结果")
		return
	}
	if err := store.PutSnapshot(state.Snapshot{AccountID: input.AccountID, Kind: "subscriptions", Data: payload, QueriedAt: time.Now().UTC()}); err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func singleSubscriptionView(accountID string, report service.SubscriptionReport) map[string]interface{} {
	account := service.AccountSubscriptionReport{Account: accountID, QueryError: "未返回账号订阅结果"}
	if len(report.Accounts) == 1 {
		account = report.Accounts[0]
	}
	subscriptions := make([]map[string]interface{}, 0, len(account.Subscriptions))
	for _, subscription := range account.Subscriptions {
		subscriptions = append(subscriptions, map[string]interface{}{
			"id": subscription.ID, "plan_title": subscription.PlanTitle, "total_usd": subscription.TotalAmountUSD,
			"remaining_usd": subscription.RemainingAmountUSD, "end_time": subscription.EndTime,
			"unlimited": subscription.Unlimited,
		})
	}
	return map[string]interface{}{
		"account_id": accountID, "subscriptions": subscriptions, "query_error": account.QueryError,
		"queried_at": time.Now().UTC(),
	}
}

func (s *Server) markCheckinCompleted(store *state.Store, accountID, today string, status service.CheckinStatusReport) error {
	action, _, err := store.GetOrCreateAction(accountID, today, state.ActionCheckin)
	if err != nil {
		return err
	}
	_, err = store.UpdateAction(action.Key, func(value *state.Action) {
		value.Status = state.ActionCompleted
		value.Retryable = false
		value.SideEffectStarted = true
		value.LastError = ""
		if status.TodayQuotaAwardedUSD != nil {
			awardUSD := *status.TodayQuotaAwardedUSD
			value.CheckinQuotaAwardedUSD = &awardUSD
		}
		if status.TodayQuotaAwardedUSD != nil || value.CheckinQuotaAwardedUSD == nil {
			value.Message = completedCheckinMessage(status.TodayQuotaAwardedUSD)
		}
	})
	return err
}

func completedCheckinMessage(awardUSD *float64) string {
	if awardUSD == nil {
		return "今日已签到"
	}
	return fmt.Sprintf("今日已签到，获得额度：$%.2f", *awardUSD)
}

func claimAdded(action state.Action) int {
	if action.ClaimBeforeRemaining == nil || action.ClaimAfterRemaining == nil {
		return 0
	}
	added := *action.ClaimAfterRemaining - *action.ClaimBeforeRemaining
	if added < 0 {
		return 0
	}
	return added
}

func claimRemaining(action state.Action) (int, bool) {
	if action.ClaimAfterRemaining != nil {
		return *action.ClaimAfterRemaining, true
	}
	if action.ClaimBeforeRemaining != nil {
		return *action.ClaimBeforeRemaining, true
	}
	return 0, false
}

func publicClaimMessage(status state.ActionStatus, alreadyCompleted bool) string {
	switch status {
	case state.ActionCompleted:
		if alreadyCompleted {
			return "今日已领取"
		}
		return "领取成功"
	case state.ActionFailed:
		return "领取失败，请重试"
	case state.ActionUnknown:
		return "领取结果待确认"
	case state.ActionPending:
		return "领取处理中"
	default:
		return "领取处理中"
	}
}

func webDrawKey() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "draw:web:" + hex.EncodeToString(buffer), nil
}

func writeUpstreamError(writer http.ResponseWriter, action, accountID string, err error, message string) {
	log.Printf("web %s failed for account=%s: %v", action, accountID, err)
	writeError(writer, http.StatusBadGateway, message)
}

func (s *Server) withBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.WebUser)) == 1
		passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.WebPass)) == 1
		if !ok || !usernameOK || !passwordOK {
			writer.Header().Set("WWW-Authenticate", `Basic realm="0809 Account Workbench", charset="UTF-8"`)
			writeError(writer, http.StatusUnauthorized, "需要工作台管理员认证")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(writer, request)
	})
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, destination interface{}) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodySize)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("请求格式错误")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("请求只能包含一个 JSON 对象")
	}
	return nil
}

func writeStoreError(writer http.ResponseWriter, err error) {
	writeError(writer, http.StatusServiceUnavailable, "状态存储暂时不可用，请稍后重试")
}

func writeJSON(writer http.ResponseWriter, status int, value interface{}) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
