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
		if snapshot, ok := store.Snapshot(record.ID, "activity"); ok {
			var data service.ActivityReport
			if json.Unmarshal(snapshot.Data, &data) == nil && data.AccountID == record.ID {
				item["activity_snapshot"] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
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
	if len(parts) != 4 {
		writeError(writer, http.StatusNotFound, "操作不存在")
		return
	}
	action := parts[3]
	switch action {
	case "checkin", "claim", "draw", "activity", "purchase-draw", "unlock-pass", "reauthenticate", "validate":
		if request.Method != http.MethodPost {
			writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
	case "session-preview":
		if request.Method != http.MethodGet {
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
	}
}

func isBusinessAction(action string) bool {
	switch action {
	case "checkin", "claim", "draw", "activity", "purchase-draw", "unlock-pass":
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
		"account":     accountViewFromRecord(refreshed, account.AuthHealthy),
		"message":     "重新认证成功",
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

// handleSessionPreview reports the remote-session cleanup capability. With
// the unsupported manager it explicitly says cleanup is unavailable.
func (s *Server) handleSessionPreview(writer http.ResponseWriter, request *http.Request, record account.Record) {
	guard, err := s.sharedGuard()
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	preview, err := guard.Preview(ctx, record.ID)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "会话预览暂时失败，请稍后重试")
		return
	}
	preview.GeneratedAt = time.Now().UTC()
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"account_id": record.ID,
		"preview":    preview,
	})
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
		PriceUSD  *quota.Money          `json:"price_usd,omitempty"`
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
