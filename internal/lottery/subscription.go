package lottery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SubscriptionPlan is the public part of a subscription plan needed by the
// bot. The site returns plan metadata separately from user subscriptions.
type SubscriptionPlan struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type UserSubscription struct {
	ID          int    `json:"id"`
	PlanID      int    `json:"plan_id"`
	AmountTotal int64  `json:"amount_total"`
	AmountUsed  int64  `json:"amount_used"`
	StartTime   int64  `json:"start_time"`
	EndTime     int64  `json:"end_time"`
	Status      string `json:"status"`
}

type SubscriptionSummary struct {
	Subscription *UserSubscription `json:"subscription"`
}

type SubscriptionSelf struct {
	Subscriptions []SubscriptionSummary `json:"subscriptions"`
}

// StatusSettings contains the site-wide quota display configuration used by
// the web UI to render native quota units as money or tokens.
type StatusSettings struct {
	QuotaPerUnit               float64 `json:"quota_per_unit"`
	QuotaDisplayType           string  `json:"quota_display_type"`
	USDExchangeRate            float64 `json:"usd_exchange_rate"`
	CustomCurrencySymbol       string  `json:"custom_currency_symbol"`
	CustomCurrencyExchangeRate float64 `json:"custom_currency_exchange_rate"`
}

func (c *Client) SubscriptionPlans(ctx context.Context, parentAccessToken string) (map[int]string, error) {
	payload, err := c.getJSON(ctx, "/api/subscription/plans", parentAccessToken, "/topup")
	if err != nil {
		return nil, fmt.Errorf("subscription plans request: %w", err)
	}

	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode subscription plans response: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}

	data := envelope.Data
	if len(data) == 0 || string(data) == "null" {
		data = payload
	}
	var entries []struct {
		Plan SubscriptionPlan `json:"plan"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode subscription plans data: %w", err)
	}
	result := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.Plan.ID > 0 && strings.TrimSpace(entry.Plan.Title) != "" {
			result[entry.Plan.ID] = strings.TrimSpace(entry.Plan.Title)
		}
	}
	return result, nil
}

func (c *Client) SubscriptionSelf(ctx context.Context, parentAccessToken string) (SubscriptionSelf, error) {
	payload, err := c.getJSON(ctx, "/api/subscription/self", parentAccessToken, "/topup")
	if err != nil {
		return SubscriptionSelf{}, fmt.Errorf("subscription self request: %w", err)
	}

	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return SubscriptionSelf{}, fmt.Errorf("decode subscription self response: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return SubscriptionSelf{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}

	data := envelope.Data
	if len(data) == 0 || string(data) == "null" {
		data = payload
	}
	var result SubscriptionSelf
	if err := json.Unmarshal(data, &result); err != nil {
		return SubscriptionSelf{}, fmt.Errorf("decode subscription self data: %w", err)
	}
	if result.Subscriptions == nil {
		result.Subscriptions = []SubscriptionSummary{}
	}
	return result, nil
}

func (c *Client) Status(ctx context.Context, parentAccessToken string) (StatusSettings, error) {
	payload, err := c.getJSON(ctx, "/api/status", parentAccessToken, "/")
	if err != nil {
		return StatusSettings{}, fmt.Errorf("status request: %w", err)
	}

	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return StatusSettings{}, fmt.Errorf("decode status response: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return StatusSettings{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}

	data := envelope.Data
	if len(data) == 0 || string(data) == "null" {
		data = payload
	}
	var result StatusSettings
	if err := json.Unmarshal(data, &result); err != nil {
		return StatusSettings{}, fmt.Errorf("decode status data: %w", err)
	}
	return result, nil
}

func (c *Client) getJSON(ctx context.Context, path, bearerToken, referer string) ([]byte, error) {
	request, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken = strings.TrimSpace(bearerToken); bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if referer = strings.TrimSpace(referer); referer != "" {
		request.Header.Set("Referer", c.url(referer).String())
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError(response)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if err != nil {
		return nil, fmt.Errorf("read GET %s response: %w", path, err)
	}
	if len(payload) == 0 {
		return nil, errors.New("empty JSON response")
	}
	return payload, nil
}
