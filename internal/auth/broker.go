package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/lottery"
	"skyeapi/lottery-bot/internal/secret"
	"skyeapi/lottery-bot/internal/state"
)

var (
	// ErrAuthUnavailable covers timeouts, network errors, 5xx, 429 and parse
	// failures. Retry may help; a password login must not happen.
	ErrAuthUnavailable = errors.New("authentication is temporarily unavailable")
	// ErrReauthRequired means the platform rejected the refresh or validation
	// path. Only an explicit user reauthentication may proceed from here.
	ErrReauthRequired = errors.New("explicit reauthentication required")
)

// Intent describes why a session is requested. Only ExplicitReauthenticate is
// allowed to consume a password login.
type Intent int

const (
	ReadOnly Intent = iota
	SideEffect
	ScheduledAutomation
	ExplicitReauthenticate
)

type SessionKind string

const (
	SessionParent  SessionKind = "parent"
	SessionLottery SessionKind = "lottery"
)

// PlatformClient is the authentication surface of the 0809 website client.
type PlatformClient interface {
	UserSelf(ctx context.Context, parentAccessToken string) (lottery.UserUsage, error)
	Refresh(ctx context.Context) (lottery.LoginResult, error)
	Login(ctx context.Context, credentials lottery.Credentials) (lottery.LoginResult, error)
	Bridge(ctx context.Context, parentAccessToken string, userID int64) (lottery.BridgeResult, error)
	Cookies() []state.Cookie
}

// ClientFactory builds a platform client seeded with the account's cookies.
type ClientFactory func([]state.Cookie) (PlatformClient, error)

// AcquiredSession is the broker's result: a usable token plus the cookies the
// caller should use for business requests.
type AcquiredSession struct {
	Token   string
	Cookies []state.Cookie
	UserID  int64
}

// Broker owns every authentication decision. It reuses saved tokens, validates
// them without side effects, refreshes via the saved cookie and only logs in
// with a password when the user explicitly reauthenticates. Per-account
// recovery is serialized so waiting requests reuse the persisted session
// instead of refreshing or logging in again.
type Broker struct {
	store     *state.Store
	vault     secret.Vault
	newClient ClientFactory
	now       func() time.Time

	// capacityHook is installed by the session-capability layer (Task 5) and
	// runs immediately before an explicit password login.
	capacityHook func(ctx context.Context, accountID string) error
}

func NewBroker(store *state.Store, vault secret.Vault, factory ClientFactory) *Broker {
	return &Broker{
		store:     store,
		vault:     vault,
		newClient: factory,
		now:       time.Now,
	}
}

// WithClock overrides the broker's time source. Callers with deterministic
// expiry requirements (tests, schedulers) use it for chaining.
func (b *Broker) WithClock(now func() time.Time) *Broker {
	b.now = now
	return b
}

// Acquire returns a usable session for the requested kind, following the
// fixed priority: saved token -> side-effect-free validation -> refresh ->
// explicit reauthentication.
func (b *Broker) Acquire(ctx context.Context, accountID string, intent Intent, kind SessionKind) (AcquiredSession, error) {
	bundle, err := b.loadBundle(ctx, accountID)
	if err != nil {
		return AcquiredSession{}, err
	}
	switch kind {
	case SessionParent:
		if usableToken(bundle.ParentAccessToken, bundle.ParentAccessExpiresAt, b.currentTime()) {
			return AcquiredSession{Token: bundle.ParentAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
		}
	case SessionLottery:
		if usableToken(bundle.LotteryAccessToken, bundle.LotteryAccessExpiresAt, b.currentTime()) {
			return AcquiredSession{Token: bundle.LotteryAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
		}
	default:
		return AcquiredSession{}, fmt.Errorf("unknown session kind %q", kind)
	}

	release := b.store.LockAuth(accountID)
	defer release()

	// A concurrent request may already have refreshed this account while this
	// request waited for the lock; always continue from the latest bundle.
	bundle, err = b.loadBundle(ctx, accountID)
	if err != nil {
		return AcquiredSession{}, err
	}
	if kind == SessionParent && usableToken(bundle.ParentAccessToken, bundle.ParentAccessExpiresAt, b.currentTime()) {
		return AcquiredSession{Token: bundle.ParentAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
	}
	if kind == SessionLottery && usableToken(bundle.LotteryAccessToken, bundle.LotteryAccessExpiresAt, b.currentTime()) {
		return AcquiredSession{Token: bundle.LotteryAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
	}

	if kind == SessionParent {
		token, err := b.parentSessionLocked(ctx, accountID, &bundle, intent)
		if err != nil {
			return AcquiredSession{}, err
		}
		return AcquiredSession{Token: token, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
	}

	// Lottery sessions ride on a parent session and the bridge endpoint.
	parentToken, err := b.parentSessionLocked(ctx, accountID, &bundle, intent)
	if err != nil {
		return AcquiredSession{}, err
	}
	if err := b.bridgeLotteryLocked(ctx, accountID, &bundle, parentToken); err != nil {
		return AcquiredSession{}, err
	}
	return AcquiredSession{Token: bundle.LotteryAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
}

// Reauthenticate performs the user-approved password login for one account.
func (b *Broker) Reauthenticate(ctx context.Context, accountID string) (AcquiredSession, error) {
	return b.Acquire(ctx, accountID, ExplicitReauthenticate, SessionParent)
}

// RenewParent replaces a parent token the platform has rejected. It refreshes
// at most once and never falls back to a password login.
func (b *Broker) RenewParent(ctx context.Context, accountID, rejectedToken string) (AcquiredSession, error) {
	release := b.store.LockAuth(accountID)
	defer release()

	bundle, err := b.loadBundle(ctx, accountID)
	if err != nil {
		return AcquiredSession{}, err
	}
	if bundle.ParentAccessToken != strings.TrimSpace(rejectedToken) && usableToken(bundle.ParentAccessToken, bundle.ParentAccessExpiresAt, b.currentTime()) {
		return AcquiredSession{Token: bundle.ParentAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
	}
	bundle.ParentAccessToken = ""
	bundle.ParentAccessExpiresAt = time.Time{}
	token, err := b.parentSessionLocked(ctx, accountID, &bundle, ReadOnly)
	if err != nil {
		return AcquiredSession{}, err
	}
	return AcquiredSession{Token: token, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
}

// RenewLottery replaces a lottery token the platform has rejected. It bridges
// once from the current parent session and refreshes the parent session once
// if the bridge rejects it. A password login never happens here.
func (b *Broker) RenewLottery(ctx context.Context, accountID, rejectedToken string) (AcquiredSession, error) {
	release := b.store.LockAuth(accountID)
	defer release()

	bundle, err := b.loadBundle(ctx, accountID)
	if err != nil {
		return AcquiredSession{}, err
	}
	if bundle.LotteryAccessToken != strings.TrimSpace(rejectedToken) && usableToken(bundle.LotteryAccessToken, bundle.LotteryAccessExpiresAt, b.currentTime()) {
		return AcquiredSession{Token: bundle.LotteryAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
	}
	bundle.LotteryAccessToken = ""
	bundle.LotteryAccessExpiresAt = time.Time{}

	parentToken := bundle.ParentAccessToken
	if !usableToken(parentToken, bundle.ParentAccessExpiresAt, b.currentTime()) {
		parentToken, err = b.parentSessionLocked(ctx, accountID, &bundle, ReadOnly)
		if err != nil {
			return AcquiredSession{}, err
		}
	}
	if err := b.bridgeLotteryLocked(ctx, accountID, &bundle, parentToken); err != nil {
		return AcquiredSession{}, err
	}
	return AcquiredSession{Token: bundle.LotteryAccessToken, Cookies: cookiesToState(bundle.Cookies), UserID: bundle.UserID}, nil
}

// parentSessionLocked must run under the account's auth lock. On success the
// bundle carries the cookies and user ID worth persisting.
func (b *Broker) parentSessionLocked(ctx context.Context, accountID string, bundle *secret.Bundle, intent Intent) (string, error) {
	if usableToken(bundle.ParentAccessToken, bundle.ParentAccessExpiresAt, b.currentTime()) {
		return bundle.ParentAccessToken, nil
	}

	client, err := b.newClient(cookiesToState(bundle.Cookies))
	if err != nil {
		return "", fmt.Errorf("%w: create platform client: %v", ErrAuthUnavailable, err)
	}

	if token := strings.TrimSpace(bundle.ParentAccessToken); token != "" && intent != ExplicitReauthenticate {
		// The locally stored expiry is only a hint; verify the cached token
		// with the server before doing anything with side effects.
		if _, err := client.UserSelf(ctx, token); err == nil {
			bundle.Cookies = cookiesFromState(client.Cookies())
			if err := b.persistLocked(ctx, accountID, bundle, account.AuthHealthy); err != nil {
				return "", err
			}
			return token, nil
		} else if isRejection(err) {
			bundle.ParentAccessToken = ""
			bundle.ParentAccessExpiresAt = time.Time{}
		} else {
			return "", fmt.Errorf("%w: validate saved session: %v", ErrAuthUnavailable, err)
		}
	} else if intent != ExplicitReauthenticate {
		bundle.ParentAccessToken = ""
		bundle.ParentAccessExpiresAt = time.Time{}
	}

	if intent != ExplicitReauthenticate {
		return b.refreshParentLocked(ctx, accountID, bundle, client)
	}
	return b.explicitLoginLocked(ctx, accountID, bundle, client)
}

// refreshParentLocked exchanges the saved refresh cookie for a new parent
// session. Refresh rejections surface as ErrReauthRequired; transient
// failures as ErrAuthUnavailable.
func (b *Broker) refreshParentLocked(ctx context.Context, accountID string, bundle *secret.Bundle, client PlatformClient) (string, error) {
	result, err := client.Refresh(ctx)
	if err != nil {
		if isRejection(err) {
			return "", fmt.Errorf("%w: refresh rejected", ErrReauthRequired)
		}
		return "", fmt.Errorf("%w: refresh saved session: %v", ErrAuthUnavailable, err)
	}
	if err := b.persistParentSessionLocked(ctx, accountID, bundle, client, result); err != nil {
		return "", err
	}
	return bundle.ParentAccessToken, nil
}

// explicitLoginLocked is the only path that consumes account credentials. It
// is reachable solely through ExplicitReauthenticate.
func (b *Broker) explicitLoginLocked(ctx context.Context, accountID string, bundle *secret.Bundle, client PlatformClient) (string, error) {
	if strings.TrimSpace(bundle.LoginName) == "" || bundle.Password == "" {
		return "", fmt.Errorf("%w: no stored credentials for this account", ErrReauthRequired)
	}
	if b.capacityHook != nil {
		if err := b.capacityHook(ctx, accountID); err != nil {
			return "", err
		}
	}
	result, err := client.Login(ctx, lottery.Credentials{Username: bundle.LoginName, Password: bundle.Password})
	if err != nil {
		if isRejection(err) {
			return "", fmt.Errorf("%w: login rejected", ErrReauthRequired)
		}
		return "", fmt.Errorf("%w: login request: %v", ErrAuthUnavailable, err)
	}
	if err := b.persistParentSessionLocked(ctx, accountID, bundle, client, result); err != nil {
		return "", err
	}
	return bundle.ParentAccessToken, nil
}

// bridgeLotteryLocked exchanges the parent session for a lottery session. A
// rejected bridge refreshes the parent session once and bridges once more.
func (b *Broker) bridgeLotteryLocked(ctx context.Context, accountID string, bundle *secret.Bundle, parentToken string) error {
	client, err := b.newClient(cookiesToState(bundle.Cookies))
	if err != nil {
		return fmt.Errorf("%w: create platform client: %v", ErrAuthUnavailable, err)
	}
	bridge, err := client.Bridge(ctx, parentToken, bundle.UserID)
	if err == nil {
		return b.persistLotterySessionLocked(ctx, accountID, bundle, client, bridge)
	}
	if !isRejection(err) {
		return fmt.Errorf("%w: bridge lottery session: %v", ErrAuthUnavailable, err)
	}

	// The parent token may have been accepted elsewhere but rejected by the
	// bridge endpoint. Refresh it once, then bridge once more.
	refreshed, refreshErr := b.refreshParentLocked(ctx, accountID, bundle, client)
	if refreshErr != nil {
		return refreshErr
	}
	bridge, err = client.Bridge(ctx, refreshed, bundle.UserID)
	if err != nil {
		if isRejection(err) {
			return fmt.Errorf("%w: bridge rejected after refresh", ErrReauthRequired)
		}
		return fmt.Errorf("%w: bridge lottery session: %v", ErrAuthUnavailable, err)
	}
	return b.persistLotterySessionLocked(ctx, accountID, bundle, client, bridge)
}

func (b *Broker) persistParentSessionLocked(ctx context.Context, accountID string, bundle *secret.Bundle, client PlatformClient, result lottery.LoginResult) error {
	if result.UserID <= 0 || strings.TrimSpace(result.AccessToken) == "" {
		return fmt.Errorf("%w: session response did not contain an access token and user ID", ErrAuthUnavailable)
	}
	bundle.UserID = result.UserID
	bundle.ParentAccessToken = strings.TrimSpace(result.AccessToken)
	bundle.ParentAccessExpiresAt = result.AccessExpiresAt
	bundle.LotteryAccessToken = ""
	bundle.LotteryAccessExpiresAt = time.Time{}
	bundle.Cookies = cookiesFromState(client.Cookies())
	return b.persistLocked(ctx, accountID, bundle, account.AuthHealthy)
}

func (b *Broker) persistLotterySessionLocked(ctx context.Context, accountID string, bundle *secret.Bundle, client PlatformClient, bridge lottery.BridgeResult) error {
	if strings.TrimSpace(bridge.AccessToken) == "" {
		return fmt.Errorf("%w: bridge response did not contain an access token", ErrAuthUnavailable)
	}
	bundle.LotteryAccessToken = strings.TrimSpace(bridge.AccessToken)
	bundle.LotteryAccessExpiresAt = bridge.ExpiresAt
	bundle.Cookies = cookiesFromState(client.Cookies())
	return b.persistLocked(ctx, accountID, bundle, account.AuthHealthy)
}

func (b *Broker) persistLocked(ctx context.Context, accountID string, bundle *secret.Bundle, health account.AuthHealth) error {
	if err := b.vault.Save(ctx, accountID, *bundle); err != nil {
		return fmt.Errorf("%w: persist session for %s: %v", ErrAuthUnavailable, accountID, err)
	}
	// Health is a display-only field; a failure here must not undo the saved
	// session.
	_ = b.store.SetAuthHealth(accountID, health)
	return nil
}

func (b *Broker) loadBundle(ctx context.Context, accountID string) (secret.Bundle, error) {
	bundle, err := b.vault.Load(ctx, accountID)
	if errors.Is(err, secret.ErrNotFound) {
		return secret.Bundle{}, nil
	}
	if err != nil {
		return secret.Bundle{}, fmt.Errorf("%w: load saved session: %v", ErrAuthUnavailable, err)
	}
	return bundle, nil
}

func (b *Broker) currentTime() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

func (b *Broker) setCapacityHook(hook func(ctx context.Context, accountID string) error) {
	b.capacityHook = hook
}

func isRejection(err error) bool {
	return lottery.IsStatus(err, 401) || lottery.IsStatus(err, 403)
}

func usableToken(token string, expiresAt time.Time, now time.Time) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	return expiresAt.IsZero() || expiresAt.After(now.Add(time.Minute))
}

func cookiesToState(values []secret.Cookie) []state.Cookie {
	if len(values) == 0 {
		return nil
	}
	cookies := make([]state.Cookie, 0, len(values))
	for _, value := range values {
		cookies = append(cookies, state.Cookie{
			Name:     value.Name,
			Value:    value.Value,
			Path:     value.Path,
			Domain:   value.Domain,
			Expires:  value.Expires,
			Secure:   value.Secure,
			HTTPOnly: value.HTTPOnly,
		})
	}
	return cookies
}

func cookiesFromState(values []state.Cookie) []secret.Cookie {
	if len(values) == 0 {
		return nil
	}
	cookies := make([]secret.Cookie, 0, len(values))
	for _, value := range values {
		cookies = append(cookies, secret.Cookie{
			Name:     value.Name,
			Value:    value.Value,
			Path:     value.Path,
			Domain:   value.Domain,
			Expires:  value.Expires,
			Secure:   value.Secure,
			HTTPOnly: value.HTTPOnly,
		})
	}
	return cookies
}
