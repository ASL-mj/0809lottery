package web

import (
	"encoding/json"
	"net/http"
	"time"

	"skyeapi/lottery-bot/internal/state"
)

// handleBootstrap serves the local, side-effect-free state needed to paint the
// first screen. Remote metrics remain explicit account actions so page loads
// cannot create a serial upstream request waterfall.
func (s *Server) handleBootstrap(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	store, err := s.sharedStore()
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
	plansByAccount := make(map[string][]state.AutoDrawPlan)
	for _, plan := range store.AutoDrawPlans(today) {
		plansByAccount[plan.AccountID] = append(plansByAccount[plan.AccountID], plan)
	}
	accounts := make([]map[string]interface{}, 0, len(records))
	for _, record := range records {
		item := accountViewFromRecord(record, store.AuthHealth(record.ID))
		addLocalActionViews(item, store, record.ID, today)
		item["draw_schedules"] = store.DrawSchedules(record.ID)
		item["auto_draw_plans"] = plansByAccount[record.ID]
		addSnapshotViews(item, store, record.ID, today)
		accounts = append(accounts, item)
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{
		"accounts":   accounts,
		"logs":       localRuntimeLogViews(store),
		"fetched_at": time.Now().UTC(),
	})
}

func addLocalActionViews(item map[string]interface{}, store *state.Store, accountID, today string) {
	if action, ok := store.Action(accountID, today, state.ActionCheckin); ok {
		item["checkin_status"] = action.Status
		item["checkin_message"] = action.Message
		if action.CheckinQuotaAwardedUSD != nil {
			item["checkin_quota_awarded"] = *action.CheckinQuotaAwardedUSD
		}
	}
	if action, ok := store.Action(accountID, today, state.ActionDailyClaim); ok {
		item["claim_status"] = action.Status
		item["claim_message"] = publicClaimMessage(action.Status, action.Status == state.ActionCompleted)
		item["claim_added"] = claimAdded(action)
		if remaining, ok := claimRemaining(action); ok {
			item["claim_remaining"] = remaining
		}
	}
}

func addSnapshotViews(item map[string]interface{}, store *state.Store, accountID, today string) {
	for _, snapshot := range store.Snapshots(accountID) {
		if snapshot.AccountID != accountID {
			continue
		}
		var value map[string]interface{}
		if json.Unmarshal(snapshot.Data, &value) != nil {
			continue
		}
		if snapshot.Kind == "token-usage" {
			if day, _ := value["day_key"].(string); day != "" && day != today {
				continue
			}
		}
		item[snapshotField(snapshot.Kind)] = map[string]interface{}{"data": json.RawMessage(snapshot.Data), "queried_at": snapshot.QueriedAt}
	}
}

func snapshotField(kind string) string {
	switch kind {
	case "subscriptions":
		return "subscription_snapshot"
	case "draw-count":
		return "draw_count_snapshot"
	case "draw-history":
		return "draw_history_snapshot"
	case "activity":
		return "activity_snapshot"
	case "usage":
		return "usage_snapshot"
	case "token-usage":
		return "token_usage_snapshot"
	default:
		return "snapshot_" + kind
	}
}

func localRuntimeLogViews(store *state.Store) []map[string]interface{} {
	registry := store.AccountRegistry()
	logs := store.RuntimeLogs(maxRuntimeLogs)
	result := make([]map[string]interface{}, 0, len(logs))
	for _, entry := range logs {
		label := ""
		if record, err := registry.Get(entry.AccountID); err == nil {
			label = record.Label
		}
		result = append(result, map[string]interface{}{
			"id": entry.ID, "occurred_at": entry.OccurredAt, "account_id": entry.AccountID,
			"account_label": label, "window_id": entry.WindowID, "task_type": entry.TaskType,
			"status": entry.Status, "message": publicRuntimeLogText(entry.Message),
			"prize_label": publicRuntimeLogText(entry.PrizeLabel), "quota_delta_usd": entry.QuotaDeltaUSD,
		})
	}
	return result
}
