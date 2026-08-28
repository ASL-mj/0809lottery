package lottery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var shanghaiLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

// CheckinStatus is the upstream state for the current Shanghai calendar day.
// TodayQuotaAwarded is nil when the upstream record does not include an award.
type CheckinStatus struct {
	CheckedInToday    bool
	TodayQuotaAwarded *float64
}

type CheckinEligibility struct {
	CanCheckin bool
	Remaining  float64
	Required   float64
}

func (c *Client) CheckinStatus(ctx context.Context, parentAccessToken string) (CheckinStatus, error) {
	if strings.TrimSpace(parentAccessToken) == "" {
		return CheckinStatus{}, errors.New("check-in status requires a parent access token")
	}
	payload, err := c.getJSON(ctx, "/api/user/checkin", parentAccessToken, "/profile")
	if err != nil {
		return CheckinStatus{}, fmt.Errorf("check-in status request: %w", err)
	}
	return decodeCheckinStatus(payload, time.Now().In(shanghaiLocation))
}

func (c *Client) CheckinEligibility(ctx context.Context, parentAccessToken string, userID int64) (CheckinEligibility, error) {
	if strings.TrimSpace(parentAccessToken) == "" {
		return CheckinEligibility{}, errors.New("check-in eligibility requires a parent access token")
	}
	if userID <= 0 {
		return CheckinEligibility{}, errors.New("check-in eligibility requires a positive user ID")
	}

	request, err := c.newRequest(ctx, http.MethodGet, "/api/custom/daily_tokens", nil)
	if err != nil {
		return CheckinEligibility{}, err
	}
	query := request.URL.Query()
	query.Set("user_id", fmt.Sprintf("%d", userID))
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(parentAccessToken))
	request.Header.Set("Referer", c.url("/profile").String())

	response, err := c.http.Do(request)
	if err != nil {
		return CheckinEligibility{}, fmt.Errorf("GET %s: %w", request.URL.Path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return CheckinEligibility{}, responseError(response)
	}

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if err != nil {
		return CheckinEligibility{}, fmt.Errorf("read GET %s response: %w", request.URL.Path, err)
	}
	if len(payload) == 0 {
		return CheckinEligibility{}, errors.New("empty JSON response")
	}
	return decodeCheckinEligibility(payload)
}

func decodeCheckinStatus(payload []byte, now time.Time) (CheckinStatus, error) {
	var envelope struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Stats struct {
				CheckedInToday bool `json:"checked_in_today"`
				Records        []struct {
					CheckinDate  string   `json:"checkin_date"`
					QuotaAwarded *float64 `json:"quota_awarded"`
				} `json:"records"`
			} `json:"stats"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return CheckinStatus{}, fmt.Errorf("decode check-in status response: %w", err)
	}
	if envelope.Success == nil {
		return CheckinStatus{}, errors.New("check-in status response did not contain an explicit success state")
	}
	if !*envelope.Success {
		return CheckinStatus{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}

	status := CheckinStatus{CheckedInToday: envelope.Data.Stats.CheckedInToday}
	if !status.CheckedInToday {
		return status, nil
	}
	today := now.In(shanghaiLocation).Format("2006-01-02")
	for _, record := range envelope.Data.Stats.Records {
		if strings.TrimSpace(record.CheckinDate) == today && record.QuotaAwarded != nil {
			quotaAwarded := *record.QuotaAwarded
			status.TodayQuotaAwarded = &quotaAwarded
			break
		}
	}
	return status, nil
}

func decodeCheckinEligibility(payload []byte) (CheckinEligibility, error) {
	var envelope struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Data    struct {
			CanCheckin *bool   `json:"can_checkin"`
			Remaining  float64 `json:"remaining"`
			Required   float64 `json:"required"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return CheckinEligibility{}, fmt.Errorf("decode check-in eligibility response: %w", err)
	}
	if envelope.Success == nil {
		return CheckinEligibility{}, errors.New("check-in eligibility response did not contain an explicit success state")
	}
	if !*envelope.Success {
		return CheckinEligibility{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}
	if envelope.Data.CanCheckin == nil {
		return CheckinEligibility{}, errors.New("check-in eligibility response did not contain can_checkin")
	}
	return CheckinEligibility{
		CanCheckin: *envelope.Data.CanCheckin,
		Remaining:  envelope.Data.Remaining,
		Required:   envelope.Data.Required,
	}, nil
}
