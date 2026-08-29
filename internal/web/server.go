package web

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/config"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
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
	guard   *auth.CapacityGuard
	vault   secret.Vault
	csrfMu  sync.Mutex
	csrf    string
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
	s.vault = nil
	s.guard = nil
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
	return s.sharedBrokerLocked()
}

func (s *Server) sharedBrokerLocked() (*auth.Broker, error) {
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
	s.broker = auth.NewBroker(store, vault, s.platformClientFactory())
	return s.broker, nil
}

// sharedVault exposes the process-wide secret vault for account management.
func (s *Server) sharedVault() (secret.Vault, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	return s.sharedVaultLocked()
}

func (s *Server) sharedVaultLocked() (secret.Vault, error) {
	if s.vault != nil {
		return s.vault, nil
	}
	if _, err := s.sharedStoreLocked(); err != nil {
		return nil, err
	}
	vaultFactory := s.vaultFactory
	if vaultFactory == nil {
		vaultFactory = func(*state.Store) (secret.Vault, error) {
			return secret.NewFileVault(s.cfg.VaultPath, s.cfg.VaultKey)
		}
	}
	vault, err := vaultFactory(s.store)
	if err != nil {
		return nil, err
	}
	s.vault = vault
	return s.vault, nil
}

func (s *Server) platformClientFactory() auth.ClientFactory {
	return func(cookies []state.Cookie) (auth.PlatformClient, error) {
		return lottery.NewClient(s.cfg.BaseURL, s.cfg.UserAgent, cookies)
	}
}

// sharedGuard builds the session-capacity guard and installs it as the
// broker's only password-login gate.
func (s *Server) sharedGuard() (*auth.CapacityGuard, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.guard != nil {
		return s.guard, nil
	}
	if _, err := s.sharedBrokerLocked(); err != nil {
		return nil, err
	}
	vault, err := s.sharedVaultLocked()
	if err != nil {
		return nil, err
	}
	manager := auth.NewPlatformSessionManager(vault, s.platformClientFactory(), s.cfg.DurableSessionLimit, s.cfg.UserAgent)
	s.guard = auth.NewCapacityGuard(manager, s.cfg.SessionLimit, s.cfg.SessionSafetyMargin)
	s.broker.SetCapacityGuard(s.guard.BeforeLogin)
	return s.guard, nil
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
	mux.HandleFunc("/api/accounts/", s.handleAccountActions)
	mux.HandleFunc("/api/draw-count/query", s.handleDrawCountQuery)
	mux.HandleFunc("/api/subscriptions/query", s.handleSubscriptionQuery)
	mux.HandleFunc("/api/auto-draw-status", s.handleAutoDrawStatus)
	mux.HandleFunc("/api/runtime-logs", s.handleRuntimeLogs)
	return s.withSecurityHeaders(s.withBasicAuth(s.withCSRF(mux)))
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
		PrizeLabel    string        `json:"prize_label,omitempty"`
		QuotaDeltaUSD *quota.Money  `json:"quota_delta_usd,omitempty"`
	}
	registry := store.AccountRegistry()
	logs := store.RuntimeLogs(maxRuntimeLogs)
	response := make([]runtimeLogView, 0, len(logs))
	for _, entry := range logs {
		label := ""
		if record, err := registry.Get(entry.AccountID); err == nil {
			label = record.Label
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
	type planView struct {
		ScheduleID    string       `json:"schedule_id"`
		Label         string       `json:"label"`
		PlannedAt     *time.Time   `json:"planned_at,omitempty"`
		ExecutedAt    *time.Time   `json:"executed_at,omitempty"`
		Status        string       `json:"status"`
		Message       string       `json:"message,omitempty"`
		PrizeLabel    string       `json:"prize_label,omitempty"`
		QuotaDeltaUSD *quota.Money `json:"quota_delta_usd,omitempty"`
	}
	type scheduleView struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Start string `json:"start"`
		End   string `json:"end,omitempty"`
		Label string `json:"label"`
	}
	type accountView struct {
		AccountID string         `json:"account_id"`
		Schedules []scheduleView `json:"schedules"`
		Plans     []planView     `json:"plans"`
	}

	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	plansByAccount := make(map[string][]state.AutoDrawPlan)
	for _, plan := range store.AutoDrawPlans(today) {
		plansByAccount[plan.AccountID] = append(plansByAccount[plan.AccountID], plan)
	}
	records, err := store.AccountRegistry().List()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	accounts := make([]accountView, 0, len(records))
	for _, record := range records {
		schedules := store.DrawSchedules(record.ID)
		scheduleViews := make([]scheduleView, 0, len(schedules))
		labels := make(map[string]string, len(schedules))
		for _, entry := range schedules {
			label := service.ScheduleLabel(entry)
			labels[entry.ID] = label
			scheduleViews = append(scheduleViews, scheduleView{
				ID: entry.ID, Kind: entry.Kind, Start: entry.Start, End: entry.End, Label: label,
			})
		}
		plans := make([]planView, 0, len(plansByAccount[record.ID]))
		for _, plan := range plansByAccount[record.ID] {
			plans = append(plans, planView{
				ScheduleID:    plan.WindowID,
				Label:         labels[plan.WindowID],
				PlannedAt:     timePointer(plan.PlannedAt),
				ExecutedAt:    timePointer(plan.ExecutedAt),
				Status:        string(plan.Status),
				Message:       publicRuntimeLogText(plan.Message),
				PrizeLabel:    publicRuntimeLogText(plan.PrizeLabel),
				QuotaDeltaUSD: plan.QuotaDeltaUSD,
			})
		}
		accounts = append(accounts, accountView{
			AccountID: record.ID,
			Schedules: scheduleViews,
			Plans:     plans,
		})
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
	s.setCSRFCookie(writer)
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
	if store, err := s.sharedStore(); err != nil {
		writeStoreError(writer, err)
		return
	} else if err := requireKnownAccount(store, input.AccountID); err != nil {
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
	if store, err := s.sharedStore(); err != nil {
		writeStoreError(writer, err)
		return
	} else if err := requireKnownAccount(store, input.AccountID); err != nil {
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


// requireKnownAccount checks the registry before any upstream query.
func requireKnownAccount(store *state.Store, accountID string) error {
	if _, err := store.AccountRegistry().Get(strings.TrimSpace(accountID)); err != nil {
		return err
	}
	return nil
}
