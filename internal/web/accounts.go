package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/auth"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/service"
	"skyeapi/lottery-bot/internal/state"
)

const (
	maxAccountLabelRunes = 64
)

// accountView is the public account card payload. It never carries login
// names, passwords, cookies or tokens.
type accountView struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	MaskedLoginName string    `json:"masked_login_name"`
	Status          string    `json:"status"`
	AuthHealth      string    `json:"auth_health"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type createAccountRequest struct {
	Label     string `json:"label"`
	LoginName string `json:"login_name"`
	Password  string `json:"password"`
}

type updateAccountRequest struct {
	Label     *string `json:"label,omitempty"`
	Status    *string `json:"status,omitempty"`
	LoginName *string `json:"login_name,omitempty"`
	Password  *string `json:"password,omitempty"`
}

type reauthenticateRequest struct {
	Confirm bool `json:"confirm"`
}

type deleteAccountRequest struct {
	Confirmation string `json:"confirmation"`
}

// handleAccounts serves GET /api/accounts (the card list) and POST
// /api/accounts (create a new dynamic account).
func (s *Server) handleAccounts(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		s.handleAccountList(writer, request)
	case http.MethodPost:
		s.handleAccountCreate(writer, request)
	default:
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
	}
}

func (s *Server) handleAccountList(writer http.ResponseWriter, request *http.Request) {
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
	records, err := store.AccountRegistry().List()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	today := time.Now().In(shanghaiLocation).Format("2006-01-02")
	accounts := make([]map[string]interface{}, 0, len(records))
	for _, record := range records {
		item := accountViewFromRecord(record, store.AuthHealth(record.ID))
		if action, ok := store.Action(record.ID, today, state.ActionCheckin); ok {
			item["checkin_status"] = action.Status
			item["checkin_message"] = action.Message
			if action.CheckinQuotaAwardedUSD != nil {
				award := *action.CheckinQuotaAwardedUSD
				item["checkin_quota_awarded"] = award
			}
		}
		if action, ok := store.Action(record.ID, today, state.ActionDailyClaim); ok {
			item["claim_status"] = action.Status
			item["claim_message"] = publicClaimMessage(action.Status, action.Status == state.ActionCompleted)
			item["claim_added"] = claimAdded(action)
			if remaining, ok := claimRemaining(action); ok {
				item["claim_remaining"] = remaining
			}
		}
		statusCtx, cancel := context.WithTimeout(request.Context(), 35*time.Second)
		checkinStatus, statusErr := runner.CheckinStatus(statusCtx, record.ID)
		cancel()
		if statusErr == nil {
			item["checkin_status"] = "pending"
			delete(item, "checkin_message")
			delete(item, "checkin_quota_awarded")
			if checkinStatus.CheckedInToday {
				if err := s.markCheckinCompleted(store, record.ID, today, checkinStatus); err != nil {
					writeStoreError(writer, err)
					return
				}
				item["checkin_status"] = state.ActionCompleted
				item["checkin_message"] = completedCheckinMessage(checkinStatus.TodayQuotaAwardedUSD)
				if checkinStatus.TodayQuotaAwardedUSD != nil {
					item["checkin_quota_awarded"] = *checkinStatus.TodayQuotaAwardedUSD
				}
			}
		}
		if snapshot, ok := store.Snapshot(record.ID, "subscriptions"); ok {
			var data struct {
				AccountID string          `json:"account_id"`
				Data      json.RawMessage `json:"-"`
			}
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID {
				item["subscription_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(record.ID, "draw-count"); ok {
			var data service.DrawCountReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID {
				item["draw_count_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(record.ID, "draw-history"); ok {
			var data service.DrawHistoryReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID {
				item["draw_history_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(record.ID, "activity"); ok {
			var data service.ActivityReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID {
				item["activity_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		if snapshot, ok := store.Snapshot(record.ID, "usage"); ok {
			item["usage_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
		}
		if snapshot, ok := store.Snapshot(record.ID, "token-usage"); ok {
			var data service.TokenUsageReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID && data.DayKey == today {
				item["token_usage_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
			}
		}
		accounts = append(accounts, item)
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"accounts": accounts})
}

func accountViewFromRecord(record account.Record, health account.AuthHealth) map[string]interface{} {
	if health == "" {
		health = account.AuthUnknown
	}
	return map[string]interface{}{
		"id":                record.ID,
		"label":             record.Label,
		"masked_login_name": record.MaskedLoginName,
		"status":            record.Status,
		"auth_health":       health,
		"checkin_status":    "pending",
		"claim_status":      "pending",
		"created_at":        record.CreatedAt,
		"updated_at":        record.UpdatedAt,
	}
}

func (s *Server) handleAccountCreate(writer http.ResponseWriter, request *http.Request) {
	var input createAccountRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	input.Label = strings.TrimSpace(input.Label)
	input.LoginName = strings.TrimSpace(input.LoginName)
	if input.Label == "" || len([]rune(input.Label)) > maxAccountLabelRunes {
		writeError(writer, http.StatusBadRequest, "账号显示名不能为空且不能超过 64 个字符")
		return
	}
	if input.LoginName == "" || input.Password == "" {
		writeError(writer, http.StatusBadRequest, "登录名和密码不能为空")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	vault, err := s.sharedVault()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	registry := store.AccountRegistry()
	record, err := registry.Create(account.Record{
		Label:           input.Label,
		MaskedLoginName: account.MaskLoginName(input.LoginName),
		Status:          account.StatusEnabled,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "账号创建失败："+safePublicText(err.Error()))
		return
	}
	bundle := secret.Bundle{LoginName: input.LoginName, Password: input.Password}
	if err := vault.Save(request.Context(), record.ID, bundle); err != nil {
		_ = registry.Delete(record.ID)
		log.Printf("vault save failed for new account: %v", err)
		writeError(writer, http.StatusInternalServerError, "凭据保存失败，请稍后重试")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account": accountViewFromRecord(record, account.AuthUnknown),
	})
}

// handleAccountActions routes /api/accounts/{id} and /api/accounts/{id}/{action}.
func (s *Server) handleAccountActions(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "accounts" || parts[2] == "" {
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}
	accountID := parts[2]
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	record, err := store.AccountRegistry().Get(accountID)
	if err != nil {
		writeError(writer, http.StatusNotFound, "账号不存在")
		return
	}

	if len(parts) == 3 {
		switch request.Method {
		case http.MethodPatch:
			s.handleAccountUpdate(writer, request, record)
		case http.MethodDelete:
			s.handleAccountDelete(writer, request, record)
		default:
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		}
		return
	}
	if len(parts) != 4 && !(len(parts) >= 5 && (parts[3] == "sessions" || parts[3] == "auto-tasks")) {
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}
	action := parts[3]
	if len(parts) >= 5 && parts[3] == "sessions" {
		action = action + "/" + strings.Join(parts[4:], "/")
	} else if len(parts) >= 5 && parts[3] == "auto-tasks" {
		action = action + "/" + strings.Join(parts[4:], "/")
	}
	switch action {
	case "checkin", "claim", "draw", "draw-history", "activity", "purchase-draw", "unlock-pass", "reauthenticate", "validate", "balance", "token-usage", "sessions/cleanup", "sessions/revoke", "sessions/revoke-others":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
	case "session-preview":
		if request.Method != http.MethodGet {
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
	case "draw-schedule", "auto-tasks":
		if request.Method != http.MethodGet && request.Method != http.MethodPut {
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
	case "auto-tasks/toggle":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
	default:
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}

	// Business actions require an enabled account; management actions are
	// allowed on disabled accounts so users can re-enable or inspect them.
	if isBusinessAction(action) && record.Status != account.StatusEnabled {
		writeError(writer, http.StatusConflict, "账号已停用，无法执行该操作")
		return
	}
	switch action {
	case "checkin":
		s.handleCheckinAction(writer, request, accountID)
	case "claim":
		s.handleClaimAction(writer, request, accountID)
	case "draw":
		s.handleDrawAction(writer, request, accountID)
	case "draw-history":
		s.handleDrawHistoryAction(writer, request, accountID)
	case "activity":
		s.handleActivityAction(writer, request, accountID)
	case "purchase-draw":
		s.handlePurchaseDrawAction(writer, request, accountID)
	case "unlock-pass":
		s.handleUnlockPassAction(writer, request, accountID)
	case "reauthenticate":
		s.handleAccountReauthenticate(writer, request, record)
	case "validate":
		s.handleAccountValidate(writer, request, record)
	case "session-preview":
		s.handleSessionPreview(writer, request, record)
	case "sessions/cleanup":
		s.handleSessionCleanup(writer, request, record)
	case "sessions/revoke":
		s.handleSessionRevoke(writer, request, record)
	case "sessions/revoke-others":
		s.handleSessionRevokeOthers(writer, request, record)
	case "draw-schedule":
		if request.Method == http.MethodGet {
			s.handleDrawScheduleGet(writer, request, record)
		} else {
			s.handleDrawSchedulePut(writer, request, record)
		}
	case "auto-tasks":
		if request.Method == http.MethodGet {
			s.handleAutoTasksGet(writer, request, record)
		} else {
			s.handleAutoTasksPut(writer, request, record)
		}
	case "auto-tasks/toggle":
		s.handleAutoTaskToggle(writer, request, record)
	case "balance":
		s.handleBalanceAction(writer, request, accountID)
	case "token-usage":
		s.handleTokenUsageAction(writer, request, accountID)
	}
}

func isBusinessAction(action string) bool {
	switch action {
	case "checkin", "claim", "draw", "draw-history", "activity", "purchase-draw", "unlock-pass", "balance", "token-usage":
		return true
	}
	return false
}

func (s *Server) handleAccountUpdate(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input updateAccountRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	registry := store.AccountRegistry()
	updated := record
	if input.Label != nil {
		updated.Label = strings.TrimSpace(*input.Label)
	}
	if input.Status != nil {
		switch account.Status(strings.TrimSpace(*input.Status)) {
		case account.StatusEnabled, account.StatusDisabled:
			updated.Status = account.Status(strings.TrimSpace(*input.Status))
		default:
			writeError(writer, http.StatusBadRequest, "账号状态仅支持 enabled 或 disabled")
			return
		}
	}
	if _, err := registry.Update(updated); err != nil {
		writeError(writer, http.StatusBadRequest, "账号更新失败："+safePublicText(err.Error()))
		return
	}
	if (input.LoginName != nil || input.Password != nil) && record.Status == account.StatusEnabled {
		vault, err := s.sharedVault()
		if err != nil {
			writeStoreError(writer, err)
			return
		}
		bundle, err := vault.Load(request.Context(), record.ID)
		if errors.Is(err, secret.ErrNotFound) {
			bundle = secret.Bundle{}
		} else if err != nil {
			log.Printf("vault load failed during update: %v", err)
			writeError(writer, http.StatusInternalServerError, "凭据读取失败，请稍后重试")
			return
		}
		if input.LoginName != nil {
			loginName := strings.TrimSpace(*input.LoginName)
			if loginName == "" {
				writeError(writer, http.StatusBadRequest, "登录名不能为空")
				return
			}
			bundle.LoginName = loginName
			if _, err := registry.Update(account.Record{
				ID:              record.ID,
				Label:           updated.Label,
				MaskedLoginName: account.MaskLoginName(loginName),
				Status:          updated.Status,
			}); err != nil {
				writeError(writer, http.StatusBadRequest, "账号更新失败："+safePublicText(err.Error()))
				return
			}
		}
		if input.Password != nil && *input.Password != "" {
			bundle.Password = *input.Password
		}
		if err := vault.Save(request.Context(), record.ID, bundle); err != nil {
			log.Printf("vault save failed during update: %v", err)
			writeError(writer, http.StatusInternalServerError, "凭据保存失败，请稍后重试")
			return
		}
	}
	refreshed, err := registry.Get(record.ID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account": accountViewFromRecord(refreshed, store.AuthHealth(record.ID)),
	})
}

func (s *Server) handleAccountDelete(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input deleteAccountRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if input.Confirmation != "DELETE" {
		writeError(writer, http.StatusBadRequest, "删除账号需要显式确认字段 confirmation=DELETE")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	vault, err := s.sharedVault()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	registry := store.AccountRegistry()
	// Disable first so no scheduler plan or background task can touch the
	// account while its data is being removed.
	if _, err := registry.Update(account.Record{
		ID:              record.ID,
		Label:           record.Label,
		MaskedLoginName: record.MaskedLoginName,
		Status:          account.StatusDisabled,
	}); err != nil {
		writeStoreError(writer, err)
		return
	}
	if err := vault.Delete(request.Context(), record.ID); err != nil {
		log.Printf("vault delete failed during account removal: %v", err)
		writeError(writer, http.StatusInternalServerError, "凭据删除失败，请稍后重试")
		return
	}
	if err := store.RemoveAccountScopedState(record.ID); err != nil {
		writeStoreError(writer, err)
		return
	}
	if err := registry.Delete(record.ID); err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"deleted": true, "account_id": record.ID})
}

// handleAccountValidate runs a read-only authentication probe. It must never
// create a session.
func (s *Server) handleAccountValidate(writer http.ResponseWriter, request *http.Request, record account.Record) {
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	broker, err := s.sharedBroker()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	_, err = broker.Acquire(ctx, record.ID, auth.ReadOnly, auth.SessionParent)
	health := account.AuthHealthy
	switch {
	case err == nil:
		health = account.AuthHealthy
	case errors.Is(err, auth.ErrReauthRequired):
		health = account.AuthNeedsReauth
	default:
		// Transient authentication problems leave the recorded health alone.
		health = store.AuthHealth(record.ID)
		if health == "" {
			health = account.AuthUnknown
		}
	}
	if err == nil || errors.Is(err, auth.ErrReauthRequired) {
		if err := s.bindRemoteUserAfterAuth(ctx, store, record.ID); err != nil {
			writeError(writer, http.StatusConflict, safePublicText(err.Error()))
			return
		}
	}
	response := map[string]interface{}{
		"account_id":  record.ID,
		"auth_health": health,
	}
	if errors.Is(err, auth.ErrReauthRequired) {
		response["message"] = "需要重新认证"
	} else if err != nil {
		response["message"] = "认证暂时不可用，请稍后重试"
	}
	writeJSON(writer, http.StatusOK, response)
}

// handleAccountReauthenticate performs the one explicit password login. It
// requires a second confirmation in the request body.
func (s *Server) handleAccountReauthenticate(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input reauthenticateRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(writer, http.StatusBadRequest, "重新认证需要显式确认字段 confirm=true")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	broker, err := s.sharedBroker()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	_, err = broker.Reauthenticate(ctx, record.ID)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrSessionCapacityProtected):
		writeError(writer, http.StatusConflict, "平台会话数量已达上限，且没有可安全释放的工作台会话")
		return
	case errors.Is(err, auth.ErrReauthRequired):
		writeError(writer, http.StatusConflict, "重新认证失败：登录被拒绝")
		return
	default:
		log.Printf("reauthenticate failed for account=%s: %v", record.ID, err)
		writeError(writer, http.StatusBadGateway, "认证暂时不可用，请稍后重试")
		return
	}
	if err := s.bindRemoteUserAfterAuth(ctx, store, record.ID); err != nil {
		writeError(writer, http.StatusConflict, safePublicText(err.Error()))
		return
	}
	refreshed, err := store.AccountRegistry().Get(record.ID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account": accountViewFromRecord(refreshed, account.AuthHealthy),
		"message": "重新认证成功",
	})
}

// bindRemoteUserAfterAuth records the remote user ID after the first
// successful authentication and enforces the one-to-one binding.
func (s *Server) bindRemoteUserAfterAuth(ctx context.Context, store *state.Store, accountID string) error {
	vault, err := s.sharedVault()
	if err != nil {
		return err
	}
	bundle, err := vault.Load(ctx, accountID)
	if err != nil {
		return nil
	}
	if bundle.UserID <= 0 {
		return nil
	}
	if err := store.AccountRegistry().SetRemoteUserID(accountID, bundle.UserID); err != nil {
		if errors.Is(err, account.ErrDuplicateRemoteUser) {
			return fmt.Errorf("该平台用户已绑定到其他本地账号，请先迁移或删除旧绑定")
		}
		return nil
	}
	return nil
}

// handleSessionPreview reports the remote-session cleanup capability with a
// live session list. Serialized per account via the auth lock.
func (s *Server) handleSessionPreview(writer http.ResponseWriter, request *http.Request, record account.Record) {
	guard, err := s.sharedGuard()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	release := store.LockAuth(record.ID)
	defer release()
	preview, err := guard.Preview(ctx, record.ID)
	if err != nil {
		log.Printf("session preview failed for account=%s: %v", record.ID, err)
		writeError(writer, http.StatusBadGateway, "会话预览暂时失败：认证可能已失效，请先重新认证")
		return
	}
	preview.GeneratedAt = time.Now().UTC()
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id": record.ID,
		"preview":    preview,
	})
}

type sessionCleanupRequest struct {
	Confirm bool `json:"confirm"`
}

type sessionRevokeRequest struct {
	SID string `json:"sid"`
}

// handleSessionRevoke revokes one session at the user's explicit request. If
// it was the current session the workbench marks the account as needing
// reauthentication.
func (s *Server) handleSessionRevoke(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input sessionRevokeRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	input.SID = strings.TrimSpace(input.SID)
	if input.SID == "" {
		writeError(writer, http.StatusBadRequest, "撤销会话需要提供 sid")
		return
	}
	s.revokeSessions(writer, request, record, func(ctx context.Context, manager *auth.PlatformSessionManager) (map[string]interface{}, error) {
		outcome, err := manager.RevokeOne(ctx, record.ID, input.SID)
		if err != nil {
			return nil, err
		}
		if outcome.Current {
			if store, storeErr := s.sharedStore(); storeErr == nil {
				_ = store.SetAuthHealth(record.ID, account.AuthNeedsReauth)
			}
		}
		return map[string]interface{}{
			"account_id":      record.ID,
			"revoked_sid":     input.SID,
			"current_revoked": outcome.Current,
		}, nil
	})
}

// handleSessionRevokeOthers revokes every session except the current one.
func (s *Server) handleSessionRevokeOthers(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input sessionCleanupRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(writer, http.StatusBadRequest, "退出其他所有会话需要显式确认字段 confirm=true")
		return
	}
	s.revokeSessions(writer, request, record, func(ctx context.Context, manager *auth.PlatformSessionManager) (map[string]interface{}, error) {
		result, err := manager.RevokeAllOthers(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"account_id": record.ID,
			"revoked":    result.Revoked,
			"failed":     result.Failed,
		}, nil
	})
}

// revokeSessions runs one revocation flow under the account's auth lock with
// uniform error handling.
func (s *Server) revokeSessions(writer http.ResponseWriter, request *http.Request, record account.Record, run func(context.Context, *auth.PlatformSessionManager) (map[string]interface{}, error)) {
	guard, err := s.sharedGuard()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	manager, ok := guard.Manager().(*auth.PlatformSessionManager)
	if !ok {
		writeError(writer, http.StatusConflict, "远端会话管理不可用：平台会话接口尚未确认")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	release := store.LockAuth(record.ID)
	defer release()
	payload, err := run(ctx, manager)
	if err != nil {
		log.Printf("session revoke failed for account=%s: %v", record.ID, err)
		writeSessionRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, payload)
}

func writeSessionRevokeError(writer http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(writer, http.StatusGatewayTimeout, "会话撤销超时，请稍后重试")
		return
	}
	if errors.Is(err, auth.ErrSessionResultPending) {
		writeError(writer, http.StatusBadGateway, "会话撤销请求已提交，但结果待确认，请刷新会话列表")
		return
	}
	var apiErr *lottery.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			writeError(writer, http.StatusUnauthorized, "认证已失效，请先重新认证")
			return
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			writeError(writer, http.StatusBadGateway, "平台网关暂时不可用，请稍后重试")
			return
		default:
			if apiErr.StatusCode >= 400 && apiErr.StatusCode <= 599 {
				writeError(writer, http.StatusBadGateway, fmt.Sprintf("平台会话接口返回 HTTP %d", apiErr.StatusCode))
				return
			}
		}
	}
	writeError(writer, http.StatusBadGateway, "会话撤销暂时失败，请稍后重试")
}

// handleSessionCleanup revokes the current cleanup candidates after an
// explicit confirmation. Unknown devices, the current session and pinned
// durable sessions are never touched.
func (s *Server) handleSessionCleanup(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input sessionCleanupRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(writer, http.StatusBadRequest, "会话清理需要显式确认字段 confirm=true")
		return
	}
	guard, err := s.sharedGuard()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	cleaner, ok := guard.Manager().(auth.SessionCleaner)
	if !ok {
		writeError(writer, http.StatusConflict, "远端会话清理不可用：平台会话接口尚未确认")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	release := store.LockAuth(record.ID)
	defer release()
	result, err := cleaner.Cleanup(ctx, record.ID)
	if err != nil {
		log.Printf("session cleanup failed for account=%s: %v", record.ID, err)
		writeSessionRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id": record.ID,
		"revoked":    result.Revoked,
		"failed":     result.Failed,
	})
}

type drawScheduleRequest struct {
	Schedules []state.AutoDrawSchedule `json:"schedules"`
}

type autoTaskToggleRequest struct {
	TaskID  string `json:"task_id"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) handleAutoTasksGet(writer http.ResponseWriter, request *http.Request, record account.Record) {
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"account_id": record.ID, "tasks": store.DrawSchedules(record.ID)})
}

func (s *Server) handleAutoTasksPut(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input drawScheduleRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	saved, err := store.SetDrawSchedules(record.ID, input.Schedules)
	if err != nil {
		writeError(writer, http.StatusBadRequest, safePublicText(err.Error()))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"account_id": record.ID, "tasks": saved, "schedules": saved})
}

func (s *Server) handleAutoTaskToggle(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input autoTaskToggleRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.TaskID == "" || input.Enabled == nil {
		writeError(writer, http.StatusBadRequest, "任务切换需要 task_id 和 enabled")
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	entries := store.DrawSchedules(record.ID)
	found := false
	for index := range entries {
		if entries[index].ID != input.TaskID {
			continue
		}
		entries[index].Enabled = *input.Enabled
		entries[index].TaskType = state.NormalizeTaskType(entries[index].TaskType)
		found = true
		break
	}
	if !found {
		writeError(writer, http.StatusNotFound, "自动任务不存在")
		return
	}
	saved, err := store.SetDrawSchedules(record.ID, entries)
	if err != nil {
		writeError(writer, http.StatusBadRequest, safePublicText(err.Error()))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"account_id": record.ID, "task_id": input.TaskID, "enabled": *input.Enabled, "tasks": saved})
}

func (s *Server) handleDrawScheduleGet(writer http.ResponseWriter, request *http.Request, record account.Record) {
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id": record.ID,
		"schedules":  store.DrawSchedules(record.ID),
	})
}

// handleDrawSchedulePut replaces the account's auto-draw schedule list.
func (s *Server) handleDrawSchedulePut(writer http.ResponseWriter, request *http.Request, record account.Record) {
	var input drawScheduleRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	store, err := s.sharedStore()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	saved, err := store.SetDrawSchedules(record.ID, input.Schedules)
	if err != nil {
		writeError(writer, http.StatusBadRequest, safePublicText(err.Error()))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id": record.ID,
		"schedules":  saved,
	})
}

// handleBalanceAction queries the account's remaining balance (user quota).
func (s *Server) handleBalanceAction(writer http.ResponseWriter, request *http.Request, accountID string) {
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
	usage, err := runner.QueryUsage(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "balance", accountID, err, "余额查询暂时失败，请稍后重试")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id":                 accountID,
		"quota_usd":                  usage.QuotaUSD,
		"used_quota_usd":             usage.UsedQuotaUSD,
		"request_count":              usage.RequestCount,
		"quota_conversion_available": usage.QuotaConversionAvailable,
		"quota_conversion_error":     usage.QuotaConversionError,
		"queried_at":                 time.Now().UTC(),
	})
}

// handleTokenUsageAction queries the account's current-day ranking record.
func (s *Server) handleTokenUsageAction(writer http.ResponseWriter, request *http.Request, accountID string) {
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
	report, err := runner.QueryTokenUsage(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "token-usage", accountID, err, "今日消耗查询暂时失败，请稍后重试")
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func safePublicText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 120 {
		return value[:120]
	}
	return value
}

func webDrawKey() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "draw:web:" + hex.EncodeToString(buffer), nil
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
	award := outcome.Action.CheckinQuotaAwardedUSD
	checkinMessage := outcome.Action.Message
	if outcome.Action.Status == state.ActionCompleted {
		if status, statusErr := runner.CheckinStatus(ctx, accountID); statusErr == nil {
			if status.CheckedInToday {
				if err := s.markCheckinCompleted(store, accountID, time.Now().In(shanghaiLocation).Format("2006-01-02"), status); err != nil {
					writeStoreError(writer, err)
					return
				}
				if status.TodayQuotaAwardedUSD != nil {
					award = status.TodayQuotaAwardedUSD
				}
			}
		}
		if award != nil {
			checkinMessage = completedCheckinMessage(award)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id":            accountID,
		"checkin_status":        outcome.Action.Status,
		"checkin_message":       checkinMessage,
		"checkin_quota_awarded": award,
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
	runner, err := s.runnerFor(store)
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
	outcome, err := runner.DrawAvailable(ctx, accountID, drawKey)
	if err != nil {
		if appendErr := appendDrawRuntimeLog(store, accountID, state.AutoDrawPlanFailed, "手动抽奖失败", "", nil); appendErr != nil {
			log.Printf("append manual draw failure log failed for account=%s: %v", accountID, appendErr)
		}
		writeUpstreamError(writer, "draw", accountID, err, "手动抽奖暂时失败，请稍后重试")
		return
	}
	var prizeLabel string
	if outcome.Result != nil {
		prizeLabel = firstNonEmpty(outcome.Result.Prize.Label, outcome.Result.Prize.ShortLabel)
	}
	response := struct {
		AccountID       string       `json:"account_id"`
		Skipped         bool         `json:"skipped"`
		RemainingBefore int          `json:"remaining_before"`
		Message         string       `json:"message"`
		PrizeLabel      string       `json:"prize_label,omitempty"`
		QuotaDeltaUSD   *quota.Money `json:"quota_delta_usd,omitempty"`
	}{
		AccountID:       accountID,
		Skipped:         outcome.Skipped,
		RemainingBefore: outcome.RemainingBefore,
		Message:         outcome.Message,
		PrizeLabel:      prizeLabel,
		QuotaDeltaUSD:   outcome.QuotaDeltaUSD,
	}
	status := state.AutoDrawPlanCompleted
	if outcome.Skipped {
		status = state.AutoDrawPlanSkipped
	}
	if appendErr := appendDrawRuntimeLog(store, accountID, status, outcome.Message, prizeLabel, outcome.QuotaDeltaUSD); appendErr != nil {
		log.Printf("append manual draw log failed for account=%s: %v", accountID, appendErr)
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) handleDrawHistoryAction(writer http.ResponseWriter, request *http.Request, accountID string) {
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
	report, err := runner.QueryDrawHistory(ctx, accountID)
	if err != nil {
		writeUpstreamError(writer, "draw-history", accountID, err, "抽奖记录暂时刷新失败，请稍后重试")
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func appendDrawRuntimeLog(store *state.Store, accountID string, status state.AutoDrawPlanStatus, message, prizeLabel string, quotaDelta *quota.Money) error {
	_, err := store.AppendRuntimeLog(state.RuntimeLog{
		OccurredAt:    time.Now().UTC(),
		AccountID:     accountID,
		WindowID:      "manual",
		Status:        status,
		Message:       message,
		PrizeLabel:    prizeLabel,
		QuotaDeltaUSD: quotaDelta,
	})
	return err
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
		PriceUSD  *quota.Money            `json:"price_usd,omitempty"`
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
			award := *status.TodayQuotaAwardedUSD
			value.CheckinQuotaAwardedUSD = &award
		}
		if status.TodayQuotaAwardedUSD != nil || value.CheckinQuotaAwardedUSD == nil {
			value.Message = completedCheckinMessage(status.TodayQuotaAwardedUSD)
		}
	})
	return err
}

func completedCheckinMessage(award *quota.Money) string {
	if award == nil {
		return "今日已签到"
	}
	if award.State != "confirmed" {
		return "今日已签到，获得额度待确认"
	}
	return fmt.Sprintf("今日已签到，获得额度：%s", award.Display)
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
