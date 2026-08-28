package account

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusEnabled  Status = "enabled"
	StatusDisabled Status = "disabled"
)

// AuthHealth is the public, sanitized authentication state shown on the card.
// It never carries tokens or cookies.
type AuthHealth string

const (
	AuthHealthy     AuthHealth = "healthy"
	AuthNeedsReauth AuthHealth = "needs_reauth"
	AuthUnknown     AuthHealth = "unknown"
)

type Record struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	MaskedLoginName string    `json:"masked_login_name"`
	Status          Status    `json:"status"`
	RemoteUserID    int64     `json:"remote_user_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MaskLoginName reduces a login name to its first character plus a fixed
// mask, keeping the domain when present. The raw value never enters the
// account registry.
func MaskLoginName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	local, domain := value, ""
	if index := strings.LastIndex(value, "@"); index >= 0 {
		local, domain = value[:index], value[index+1:]
	}
	runes := []rune(local)
	masked := "***"
	if len(runes) > 0 {
		masked = string(runes[0]) + "***"
	}
	if domain == "" {
		return masked
	}
	return masked + "@" + domain
}

func (r Record) Validate() error {
	if !validID(r.ID) {
		return fmt.Errorf("account ID %q must match [a-z0-9] followed by lowercase letters, digits or dashes", r.ID)
	}
	label := strings.TrimSpace(r.Label)
	if label == "" {
		return errors.New("account label is required")
	}
	if len([]rune(label)) > 64 {
		return errors.New("account label exceeds 64 characters")
	}
	if strings.Contains(r.MaskedLoginName, " ") {
		return errors.New("account masked login name must not contain spaces")
	}
	if isUnmaskedLoginName(r.MaskedLoginName) {
		return errors.New("account login name must be masked before it is stored")
	}
	switch r.Status {
	case StatusEnabled, StatusDisabled:
	default:
		return fmt.Errorf("account status %q is not supported", r.Status)
	}
	if r.RemoteUserID < 0 {
		return errors.New("account remote user ID must not be negative")
	}
	return nil
}

// isUnmaskedLoginName reports whether the value still looks like a full login
// name: an "@" whose local side keeps more than the fixed mask.
func isUnmaskedLoginName(value string) bool {
	index := strings.LastIndex(value, "@")
	if index < 0 {
		return false
	}
	return !strings.Contains(value[:index], "***")
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for index, char := range id {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case char == '-':
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
