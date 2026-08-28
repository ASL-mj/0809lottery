package lottery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"skyeapi/lottery-bot/internal/quota"
	"skyeapi/lottery-bot/internal/state"
)

const maxErrorBody = 64 << 10

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("lottery API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("lottery API returned HTTP %d: %s", e.StatusCode, e.Message)
}

func IsStatus(err error, statusCode int) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == statusCode
}

func IsTransient(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return true
	}
	return apiError.StatusCode == http.StatusTooManyRequests || apiError.StatusCode >= http.StatusInternalServerError
}

type LoginResult struct {
	UserID          int64
	AccessToken     string
	AccessExpiresAt time.Time
}

// Credentials carries one account's login material. It decouples the platform
// client from configuration and from the account registry.
type Credentials struct {
	Username string
	Password string
}

type BridgeResult struct {
	AccessToken string
	ExpiresAt   time.Time
}

type Dashboard struct {
	Eligibility DashboardEligibility `json:"eligibility"`
	BonusDraws  *int                 `json:"spendBonusDraws"`
	Rules       DashboardRules       `json:"rules"`
	Lucky       map[string]any       `json:"lucky"`
	Purchase    map[string]any       `json:"purchase"`
	DrawLimit   DrawLimit            `json:"drawLimit"`
}

type DashboardRules struct {
	SpendTiers []map[string]any `json:"spendTiers"`
}

type DashboardEligibility struct {
	Remaining       *int      `json:"remaining"`
	SpendBonusDraws *int      `json:"spendBonusDraws"`
	TodaySpend      *float64  `json:"todaySpend"`
	DailyUsed       *int      `json:"dailyUsed"`
	DrawLimit       DrawLimit `json:"drawLimit"`
}

type DrawLimit struct {
	FreeLimit          *int   `json:"freeLimit"`
	Unlocked           *bool  `json:"unlocked"`
	FreeUsed           *int   `json:"freeUsed"`
	DailyUsed          *int   `json:"dailyUsed"`
	EarnedRemaining    *int   `json:"earnedRemaining"`
	PurchasedRemaining *int   `json:"purchasedRemaining"`
	Remaining          *int   `json:"remaining"`
	LockedRemaining    *int   `json:"lockedRemaining"`
	UnlockCost         *int   `json:"unlockCost"`
	Status             string `json:"status"`
	DayKey             string `json:"dayKey"`
}

func (dashboard Dashboard) Remaining() (int, bool) {
	limit, ok := dashboard.EffectiveDrawLimit()
	if !ok || limit.Remaining == nil {
		return 0, false
	}
	return *limit.Remaining, true
}

func (dashboard Dashboard) SpendBonusDraws() (int, bool) {
	if dashboard.Eligibility.SpendBonusDraws == nil {
		if dashboard.BonusDraws == nil {
			return 0, false
		}
		return *dashboard.BonusDraws, true
	}
	return *dashboard.Eligibility.SpendBonusDraws, true
}

func (dashboard Dashboard) EffectiveDrawLimit() (DrawLimit, bool) {
	result := dashboard.DrawLimit
	mergeDrawLimit(&result, dashboard.Eligibility.DrawLimit)
	if result.Remaining == nil {
		result.Remaining = dashboard.Eligibility.Remaining
	}
	if result.DailyUsed == nil {
		result.DailyUsed = dashboard.Eligibility.DailyUsed
	}
	if result.DailyUsed == nil {
		result.DailyUsed = result.FreeUsed
	}
	return result, result.Remaining != nil
}

func mergeDrawLimit(destination *DrawLimit, fallback DrawLimit) {
	if destination.FreeLimit == nil {
		destination.FreeLimit = fallback.FreeLimit
	}
	if destination.Unlocked == nil {
		destination.Unlocked = fallback.Unlocked
	}
	if destination.FreeUsed == nil {
		destination.FreeUsed = fallback.FreeUsed
	}
	if destination.DailyUsed == nil {
		destination.DailyUsed = fallback.DailyUsed
	}
	if destination.EarnedRemaining == nil {
		destination.EarnedRemaining = fallback.EarnedRemaining
	}
	if destination.PurchasedRemaining == nil {
		destination.PurchasedRemaining = fallback.PurchasedRemaining
	}
	if destination.Remaining == nil {
		destination.Remaining = fallback.Remaining
	}
	if destination.LockedRemaining == nil {
		destination.LockedRemaining = fallback.LockedRemaining
	}
	if destination.UnlockCost == nil {
		destination.UnlockCost = fallback.UnlockCost
	}
	if strings.TrimSpace(destination.Status) == "" {
		destination.Status = fallback.Status
	}
	if strings.TrimSpace(destination.DayKey) == "" {
		destination.DayKey = fallback.DayKey
	}
}

type ClaimResult struct {
	Success   bool
	Message   string
	Dashboard *Dashboard
}

type CheckinResult struct {
	Success      bool
	Message      string
	QuotaAwarded float64
	CheckinDate  string
}

type UserUsage struct {
	Quota                    int64        `json:"quota"`
	UsedQuota                int64        `json:"used_quota"`
	RequestCount             int64        `json:"request_count"`
	QuotaUSD                 *quota.Money `json:"-"`
	UsedQuotaUSD             *quota.Money `json:"-"`
	QuotaConversionAvailable bool         `json:"-"`
	QuotaConversionError     string       `json:"-"`
}

// MarshalJSON keeps native quota values out of any encoded output and emits
// traceable Money snapshots only.
func (usage UserUsage) MarshalJSON() ([]byte, error) {
	type display struct {
		RequestCount             int64        `json:"request_count"`
		QuotaUSD                 *quota.Money `json:"quota_usd,omitempty"`
		UsedQuotaUSD             *quota.Money `json:"used_quota_usd,omitempty"`
		QuotaConversionAvailable bool         `json:"quota_conversion_available"`
		QuotaConversionError     string       `json:"quota_conversion_error,omitempty"`
	}
	return json.Marshal(display{
		RequestCount:             usage.RequestCount,
		QuotaUSD:                 usage.QuotaUSD,
		UsedQuotaUSD:             usage.UsedQuotaUSD,
		QuotaConversionAvailable: usage.QuotaConversionAvailable,
		QuotaConversionError:     usage.QuotaConversionError,
	})
}

type Prize struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	ShortLabel  string `json:"shortLabel"`
	Description string `json:"description"`
}

type Effect struct {
	Summary    string    `json:"summary"`
	QuotaDelta float64   `json:"quotaDelta"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type DrawResult struct {
	ID                 string `json:"id"`
	PrizeID            string `json:"prizeId"`
	Prize              Prize  `json:"prize"`
	Status             string `json:"status"`
	FulfillmentStatus  string `json:"fulfillmentStatus"`
	FulfillmentMessage string `json:"fulfillmentMessage"`
	Effect             Effect `json:"effect"`
}

func (result DrawResult) Summary() state.DrawSummary {
	return state.DrawSummary{
		DrawID:             result.ID,
		PrizeID:            result.PrizeID,
		PrizeLabel:         result.Prize.Label,
		PrizeShortLabel:    result.Prize.ShortLabel,
		DrawStatus:         result.Status,
		FulfillmentStatus:  result.FulfillmentStatus,
		FulfillmentMessage: result.FulfillmentMessage,
		EffectSummary:      result.Effect.Summary,
		QuotaDelta:         result.Effect.QuotaDelta,
		ExpiresAt:          result.Effect.ExpiresAt,
	}
}

type OperationResult struct {
	Status string `json:"status"`
}

type Client struct {
	baseURL   *url.URL
	http      *http.Client
	cookies   *trackedCookieJar
	userAgent string
}

// trackedCookieJar keeps the attributes omitted by cookiejar.Jar.Cookies.
// In particular, the refresh cookie is scoped to /api/user/auth and would be
// lost if we only inspected cookies that apply to the site root.
type trackedCookieJar struct {
	jar    http.CookieJar
	mu     sync.Mutex
	values map[string]state.Cookie
}

func newTrackedCookieJar() (*trackedCookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &trackedCookieJar{
		jar:    jar,
		values: make(map[string]state.Cookie),
	}, nil
}

func (j *trackedCookieJar) Cookies(target *url.URL) []*http.Cookie {
	return j.jar.Cookies(target)
}

func (j *trackedCookieJar) SetCookies(target *url.URL, values []*http.Cookie) {
	j.jar.SetCookies(target, values)
	if target == nil {
		return
	}

	now := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, value := range values {
		if value == nil || strings.TrimSpace(value.Name) == "" {
			continue
		}
		cookie := state.Cookie{
			Name:     value.Name,
			Value:    value.Value,
			Path:     cookiePath(value.Path, target.Path),
			Domain:   value.Domain,
			Expires:  value.Expires.UTC(),
			Secure:   value.Secure,
			HTTPOnly: value.HttpOnly,
		}
		key := cookieKey(cookie)
		if value.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			delete(j.values, key)
			continue
		}
		j.values[key] = cookie
	}
}

func (j *trackedCookieJar) snapshot() []state.Cookie {
	now := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]state.Cookie, 0, len(j.values))
	for key, value := range j.values {
		if !value.Expires.IsZero() && !value.Expires.After(now) {
			delete(j.values, key)
			continue
		}
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Domain != result[right].Domain {
			return result[left].Domain < result[right].Domain
		}
		if result[left].Path != result[right].Path {
			return result[left].Path < result[right].Path
		}
		return result[left].Name < result[right].Name
	})
	return result
}

func cookiePath(value, requestPath string) string {
	if strings.HasPrefix(value, "/") {
		return value
	}
	if !strings.HasPrefix(requestPath, "/") {
		return "/"
	}
	index := strings.LastIndex(requestPath, "/")
	if index <= 0 {
		return "/"
	}
	return requestPath[:index]
}

func cookieKey(value state.Cookie) string {
	return strings.ToLower(value.Name) + "\x00" + strings.ToLower(value.Domain) + "\x00" + value.Path
}

func NewClient(baseURL, userAgent string, cookies []state.Cookie) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse lottery base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("parse lottery base URL: scheme and host are required")
	}
	jar, err := newTrackedCookieJar()
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	if len(cookies) > 0 {
		jar.SetCookies(parsed, restoreCookies(cookies))
	}
	return &Client{
		baseURL: parsed,
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
		cookies:   jar,
		userAgent: userAgent,
	}, nil
}

func (c *Client) Cookies() []state.Cookie {
	if c.cookies == nil {
		return nil
	}
	return c.cookies.snapshot()
}

func (c *Client) Login(ctx context.Context, credentials Credentials) (LoginResult, error) {
	payload, err := json.Marshal(map[string]string{
		"username": credentials.Username,
		"password": credentials.Password,
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("encode login payload: %w", err)
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/api/user/login?turnstile=", bytes.NewReader(payload))
	if err != nil {
		return LoginResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/sign-in").String())

	response, err := c.http.Do(request)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return LoginResult{}, responseError(response)
	}
	return decodeSessionResult(response.Body, response.StatusCode, "login")
}

// Refresh exchanges the persisted refresh cookie for a new parent access
// token. It must be attempted before a password login so a normal token
// expiry does not create another device session.
func (c *Client) Refresh(ctx context.Context) (LoginResult, error) {
	request, err := c.newRequest(ctx, http.MethodPost, "/api/user/auth/refresh", nil)
	if err != nil {
		return LoginResult{}, err
	}
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/profile").String())

	response, err := c.http.Do(request)
	if err != nil {
		return LoginResult{}, fmt.Errorf("refresh request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return LoginResult{}, responseError(response)
	}
	return decodeSessionResult(response.Body, response.StatusCode, "refresh")
}

func decodeSessionResult(body io.Reader, statusCode int, action string) (LoginResult, error) {
	var envelope struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			AccessToken     string `json:"access_token"`
			AccessExpiresAt int64  `json:"access_expires_at"`
			User            struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return LoginResult{}, fmt.Errorf("decode %s response: %w", action, err)
	}
	if !envelope.Success {
		return LoginResult{}, &APIError{StatusCode: statusCode, Message: safeMessage(envelope.Message)}
	}
	if strings.TrimSpace(envelope.Data.AccessToken) == "" || envelope.Data.User.ID <= 0 {
		return LoginResult{}, fmt.Errorf("%s response did not contain an access token and user ID", action)
	}
	return LoginResult{
		UserID:          envelope.Data.User.ID,
		AccessToken:     strings.TrimSpace(envelope.Data.AccessToken),
		AccessExpiresAt: time.Unix(envelope.Data.AccessExpiresAt, 0).UTC(),
	}, nil
}

func (c *Client) Bridge(ctx context.Context, parentAccessToken string, userID int64) (BridgeResult, error) {
	if userID <= 0 {
		return BridgeResult{}, errors.New("bridge requires a user ID")
	}
	payload, err := json.Marshal(map[string]string{"uid": strconv.FormatInt(userID, 10)})
	if err != nil {
		return BridgeResult{}, fmt.Errorf("encode bridge payload: %w", err)
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/lottery/api/bridge/session", bytes.NewReader(payload))
	if err != nil {
		return BridgeResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/lottery/").String())
	if parentAccessToken = strings.TrimSpace(parentAccessToken); parentAccessToken != "" {
		request.Header.Set("Authorization", "Bearer "+parentAccessToken)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return BridgeResult{}, fmt.Errorf("bridge request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return BridgeResult{}, responseError(response)
	}
	var payloadResult struct {
		AccessToken    string          `json:"accessToken"`
		TokenExpiresAt json.RawMessage `json:"tokenExpiresAt"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payloadResult); err != nil {
		return BridgeResult{}, fmt.Errorf("decode bridge response: %w", err)
	}
	if strings.TrimSpace(payloadResult.AccessToken) == "" {
		return BridgeResult{}, errors.New("bridge response did not contain an access token")
	}
	return BridgeResult{
		AccessToken: strings.TrimSpace(payloadResult.AccessToken),
		ExpiresAt:   parseExpiry(payloadResult.TokenExpiresAt),
	}, nil
}

func (c *Client) Draw(ctx context.Context, lotteryAccessToken, idempotencyKey string) (DrawResult, error) {
	if strings.TrimSpace(lotteryAccessToken) == "" {
		return DrawResult{}, errors.New("draw requires a lottery access token")
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return DrawResult{}, errors.New("draw requires an idempotency key")
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/lottery/api/draw", bytes.NewReader(nil))
	if err != nil {
		return DrawResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(lotteryAccessToken))
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/lottery/").String())

	response, err := c.http.Do(request)
	if err != nil {
		return DrawResult{}, fmt.Errorf("draw request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return DrawResult{}, responseError(response)
	}
	var result DrawResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return DrawResult{}, fmt.Errorf("decode draw response: %w", err)
	}
	return result, nil
}

func (c *Client) PurchaseDraw(ctx context.Context, lotteryAccessToken, idempotencyKey string) (OperationResult, error) {
	return c.postLotteryOperation(ctx, "/lottery/api/draw-purchases", lotteryAccessToken, idempotencyKey, "purchase draw")
}

func (c *Client) UnlockDrawLimit(ctx context.Context, lotteryAccessToken, idempotencyKey string) (OperationResult, error) {
	return c.postLotteryOperation(ctx, "/lottery/api/draw-limit/unlock", lotteryAccessToken, idempotencyKey, "unlock draw limit")
}

func (c *Client) Dashboard(ctx context.Context, lotteryAccessToken string) (Dashboard, error) {
	if strings.TrimSpace(lotteryAccessToken) == "" {
		return Dashboard{}, errors.New("dashboard requires a lottery access token")
	}
	request, err := c.newRequest(ctx, http.MethodGet, "/lottery/api/dashboard", nil)
	if err != nil {
		return Dashboard{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(lotteryAccessToken))
	request.Header.Set("Referer", c.url("/lottery/").String())

	response, err := c.http.Do(request)
	if err != nil {
		return Dashboard{}, fmt.Errorf("dashboard request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Dashboard{}, responseError(response)
	}
	var payload json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Dashboard{}, fmt.Errorf("decode dashboard response: %w", err)
	}
	return decodeDashboard(payload)
}

func (c *Client) ClaimDaily(ctx context.Context, lotteryAccessToken string) (ClaimResult, error) {
	if strings.TrimSpace(lotteryAccessToken) == "" {
		return ClaimResult{}, errors.New("daily claim requires a lottery access token")
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/lottery/api/check-ins/claim", nil)
	if err != nil {
		return ClaimResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(lotteryAccessToken))
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/lottery/").String())

	response, err := c.http.Do(request)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("daily claim request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ClaimResult{}, responseError(response)
	}
	return decodeClaim(response.Body)
}

func (c *Client) Checkin(ctx context.Context, parentAccessToken string) (CheckinResult, error) {
	if strings.TrimSpace(parentAccessToken) == "" {
		return CheckinResult{}, errors.New("check-in requires a parent access token")
	}
	request, err := c.newRequest(ctx, http.MethodPost, "/api/user/checkin", nil)
	if err != nil {
		return CheckinResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(parentAccessToken))
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/profile").String())

	response, err := c.http.Do(request)
	if err != nil {
		return CheckinResult{}, fmt.Errorf("check-in request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return CheckinResult{}, responseError(response)
	}
	return decodeCheckin(response.Body)
}

func (c *Client) UserSelf(ctx context.Context, parentAccessToken string) (UserUsage, error) {
	if strings.TrimSpace(parentAccessToken) == "" {
		return UserUsage{}, errors.New("user usage requires a parent access token")
	}
	payload, err := c.getJSON(ctx, "/api/user/self", parentAccessToken, "/profile")
	if err != nil {
		return UserUsage{}, fmt.Errorf("user usage request: %w", err)
	}

	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return UserUsage{}, fmt.Errorf("decode user usage response: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return UserUsage{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}
	data := envelope.Data
	if len(data) == 0 || string(data) == "null" {
		data = payload
	}
	var wrapped struct {
		User *UserUsage `json:"user"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.User != nil {
		return *wrapped.User, nil
	}
	var result UserUsage
	if err := json.Unmarshal(data, &result); err != nil {
		return UserUsage{}, fmt.Errorf("decode user usage data: %w", err)
	}
	return result, nil
}

func (c *Client) postLotteryOperation(ctx context.Context, path, lotteryAccessToken, idempotencyKey, action string) (OperationResult, error) {
	if strings.TrimSpace(lotteryAccessToken) == "" {
		return OperationResult{}, fmt.Errorf("%s requires a lottery access token", action)
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return OperationResult{}, fmt.Errorf("%s requires an idempotency key", action)
	}
	request, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(nil))
	if err != nil {
		return OperationResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(lotteryAccessToken))
	request.Header.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey))
	request.Header.Set("Origin", c.baseURL.String())
	request.Header.Set("Referer", c.url("/lottery/").String())

	response, err := c.http.Do(request)
	if err != nil {
		return OperationResult{}, fmt.Errorf("%s request: %w", action, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return OperationResult{}, responseError(response)
	}
	return decodeOperation(response.Body, action)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.url(path).String(), body)
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", method, err)
	}
	request.Header.Set("Accept", "application/json")
	if strings.TrimSpace(c.userAgent) != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	return request, nil
}

func (c *Client) url(path string) *url.URL {
	parsed := *c.baseURL
	if strings.HasPrefix(path, "/") {
		parsed.Path = path
	} else {
		parsed.Path = "/" + path
	}
	parsed.RawQuery = ""
	if index := strings.IndexByte(path, '?'); index >= 0 {
		parsed.Path = path[:index]
		parsed.RawQuery = path[index+1:]
	}
	return &parsed
}

func restoreCookies(values []state.Cookie) []*http.Cookie {
	cookies := make([]*http.Cookie, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.Name) == "" || value.Value == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:     value.Name,
			Value:    value.Value,
			Path:     value.Path,
			Domain:   value.Domain,
			Expires:  value.Expires,
			Secure:   value.Secure,
			HttpOnly: value.HTTPOnly,
		})
	}
	return cookies
}

func responseError(response *http.Response) error {
	deferred := io.LimitReader(response.Body, maxErrorBody)
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(deferred).Decode(&payload); err == nil {
		return &APIError{StatusCode: response.StatusCode, Message: safeMessage(payload.Message)}
	}
	return &APIError{StatusCode: response.StatusCode}
}

func decodeDashboard(payload json.RawMessage) (Dashboard, error) {
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Dashboard{}, fmt.Errorf("decode dashboard envelope: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return Dashboard{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		payload = envelope.Data
	}
	var dashboard Dashboard
	if err := json.Unmarshal(payload, &dashboard); err != nil {
		return Dashboard{}, fmt.Errorf("decode dashboard payload: %w", err)
	}
	if _, ok := dashboard.Remaining(); !ok {
		return Dashboard{}, errors.New("dashboard response did not contain a usable draw count")
	}
	return dashboard, nil
}

func decodeClaim(reader io.Reader) (ClaimResult, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maxErrorBody))
	if err != nil {
		return ClaimResult{}, fmt.Errorf("read daily claim response: %w", err)
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ClaimResult{}, fmt.Errorf("decode daily claim response: %w", err)
	}
	if envelope.Success != nil {
		return ClaimResult{Success: *envelope.Success, Message: safeMessage(envelope.Message)}, nil
	}

	// The current endpoint returns the refreshed dashboard directly instead of
	// wrapping it in a {success, message} envelope. A valid remaining count is
	// the endpoint's successful response; the runner still compares it with the
	// pre-claim dashboard before drawing.
	dashboard, dashboardErr := decodeDashboard(payload)
	if dashboardErr == nil {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = "领取请求已处理"
		}
		return ClaimResult{Success: true, Message: safeMessage(message), Dashboard: &dashboard}, nil
	}
	return ClaimResult{}, errors.New("daily claim response did not contain an explicit success state or dashboard")
}

func decodeCheckin(reader io.Reader) (CheckinResult, error) {
	var envelope struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Data    struct {
			QuotaAwarded float64 `json:"quota_awarded"`
			CheckinDate  string  `json:"checkin_date"`
		} `json:"data"`
	}
	if err := json.NewDecoder(reader).Decode(&envelope); err != nil {
		return CheckinResult{}, fmt.Errorf("decode check-in response: %w", err)
	}
	if envelope.Success == nil {
		return CheckinResult{}, errors.New("check-in response did not contain an explicit success state")
	}
	return CheckinResult{
		Success:      *envelope.Success,
		Message:      safeMessage(envelope.Message),
		QuotaAwarded: envelope.Data.QuotaAwarded,
		CheckinDate:  strings.TrimSpace(envelope.Data.CheckinDate),
	}, nil
}

func decodeOperation(reader io.Reader, action string) (OperationResult, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maxErrorBody))
	if err != nil {
		return OperationResult{}, fmt.Errorf("read %s response: %w", action, err)
	}
	var envelope struct {
		Success   *bool           `json:"success"`
		Message   string          `json:"message"`
		Data      json.RawMessage `json:"data"`
		Operation json.RawMessage `json:"operation"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return OperationResult{}, fmt.Errorf("decode %s response: %w", action, err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return OperationResult{}, &APIError{StatusCode: http.StatusOK, Message: safeMessage(envelope.Message)}
	}

	candidates := []json.RawMessage{payload}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		candidates = append([]json.RawMessage{envelope.Data}, candidates...)
	}
	if len(envelope.Operation) > 0 && string(envelope.Operation) != "null" {
		candidates = append([]json.RawMessage{envelope.Operation}, candidates...)
	}
	for _, candidate := range candidates {
		if result, ok := decodeOperationCandidate(candidate); ok {
			return result, nil
		}
	}
	return OperationResult{}, fmt.Errorf("%s response did not contain an operation status", action)
}

func decodeOperationCandidate(payload json.RawMessage) (OperationResult, bool) {
	var nested struct {
		Operation *OperationResult `json:"operation"`
	}
	if err := json.Unmarshal(payload, &nested); err == nil && nested.Operation != nil && strings.TrimSpace(nested.Operation.Status) != "" {
		return OperationResult{Status: strings.TrimSpace(nested.Operation.Status)}, true
	}
	var direct OperationResult
	if err := json.Unmarshal(payload, &direct); err == nil && strings.TrimSpace(direct.Status) != "" {
		return OperationResult{Status: strings.TrimSpace(direct.Status)}, true
	}
	return OperationResult{}, false
}

func parseExpiry(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Now().UTC().Add(24 * time.Hour)
	}
	var numeric float64
	if err := json.Unmarshal(raw, &numeric); err == nil && numeric > 0 {
		if numeric > 1_000_000_000_000 {
			return time.UnixMilli(int64(numeric)).UTC()
		}
		return time.Unix(int64(numeric), 0).UTC()
	}
	var textual string
	if err := json.Unmarshal(raw, &textual); err == nil {
		if parsed, err := time.Parse(time.RFC3339, textual); err == nil {
			return parsed.UTC()
		}
	}
	return time.Now().UTC().Add(24 * time.Hour)
}

func safeMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}
