package secret

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound reports that the vault holds no secret for the requested
// account. Callers treat it as "no saved session" rather than a failure.
var ErrNotFound = errors.New("secret not found")

// Bundle is the only model that carries authentication secrets. It never
// crosses the API boundary, the page, or ordinary log files; the account
// registry persists only sanitized metadata beside it.
type Bundle struct {
	LoginName              string           `json:"login_name,omitempty"`
	Password               string           `json:"password,omitempty"`
	UserID                 int64            `json:"user_id,omitempty"`
	ParentAccessToken      string           `json:"parent_access_token,omitempty"`
	ParentAccessExpiresAt  time.Time        `json:"parent_access_expires_at,omitempty"`
	LotteryAccessToken     string           `json:"lottery_access_token,omitempty"`
	LotteryAccessExpiresAt time.Time        `json:"lottery_access_expires_at,omitempty"`
	Cookies                []Cookie         `json:"cookies,omitempty"`
	ManagedSessions        []ManagedSession `json:"managed_sessions,omitempty"`
	UpdatedAt              time.Time        `json:"updated_at,omitempty"`
}

// Cookie and ManagedSession belong to package secret so the encrypted bundle
// does not import state or auth and create a package cycle.
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

type SessionOrigin string

const SessionOriginWorkbench SessionOrigin = "workbench"

// ManagedSession records remote sessions the workbench can prove it created.
// Unknown sessions are never eligible for cleanup.
type ManagedSession struct {
	RemoteID   string       `json:"remote_id"`
	Origin     SessionOrigin `json:"origin"`
	Pinned     bool         `json:"pinned,omitempty"`
	LastSeenAt time.Time    `json:"last_seen_at,omitempty"`
}

// Vault stores one secret bundle per account.
type Vault interface {
	Load(context.Context, string) (Bundle, error)
	Save(context.Context, string, Bundle) error
	Delete(context.Context, string) error
}
