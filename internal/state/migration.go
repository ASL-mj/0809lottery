package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"skyeapi/lottery-bot/internal/account"
	"skyeapi/lottery-bot/internal/secret"
)

// LegacyAccount is one environment-provided credential set consumed by the
// one-time version-3 migration.
type LegacyAccount struct {
	ID        string
	Label     string
	LoginName string
	Password  string
}

type MigrationResult struct {
	Migrated int
	Message  string
}

// MigrateV3 imports the fixed five-account credentials plus every saved
// token, cookie and user ID into the secret vault, rereads each entry to
// verify the write, and only then rewrites the state file as version 4. The
// version-3 file stays byte-identical when any vault write fails, and a
// version-4 file returns an idempotent "no migration needed" result.
func MigrateV3(ctx context.Context, path string, vault secret.Vault, legacy []LegacyAccount) (MigrationResult, error) {
	if vault == nil {
		return MigrationResult{}, errors.New("secret vault is required")
	}
	if len(legacy) == 0 {
		return MigrationResult{}, errors.New("legacy accounts are required")
	}
	for _, item := range legacy {
		if strings.TrimSpace(item.ID) == "" {
			return MigrationResult{}, errors.New("legacy account ID is required")
		}
		if strings.TrimSpace(item.LoginName) == "" || item.Password == "" {
			return MigrationResult{}, fmt.Errorf("legacy account %s is missing its login name or password", item.ID)
		}
	}

	store, err := Open(path)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("open state for migration: %w", err)
	}
	defer store.Close()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.data.Version >= 4 {
		return MigrationResult{Message: "no migration needed"}, nil
	}

	now := time.Now().UTC()
	records := make(map[string]account.Record, len(legacy))
	for _, item := range legacy {
		authState := store.data.LegacyAuth[item.ID]
		record := account.Record{
			ID:              item.ID,
			Label:           strings.TrimSpace(item.Label),
			MaskedLoginName: account.MaskLoginName(item.LoginName),
			Status:          account.StatusEnabled,
			RemoteUserID:    authState.UserID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if record.Label == "" {
			record.Label = item.ID
		}
		if err := record.Validate(); err != nil {
			return MigrationResult{}, fmt.Errorf("legacy account %s is invalid: %w", item.ID, err)
		}
		bundle := secret.Bundle{
			LoginName:              strings.TrimSpace(item.LoginName),
			Password:               item.Password,
			UserID:                 authState.UserID,
			ParentAccessToken:      authState.ParentAccessToken,
			ParentAccessExpiresAt:  authState.ParentAccessExpiresAt,
			LotteryAccessToken:     authState.LotteryAccessToken,
			LotteryAccessExpiresAt: authState.LotteryAccessExpiresAt,
			Cookies:                vaultCookiesFromState(authState.Cookies),
			UpdatedAt:              now,
		}
		if err := vault.Save(ctx, item.ID, bundle); err != nil {
			return MigrationResult{}, fmt.Errorf("store secret for %s: %w", item.ID, err)
		}
		reread, err := vault.Load(ctx, item.ID)
		if err != nil {
			return MigrationResult{}, fmt.Errorf("reread secret for %s: %w", item.ID, err)
		}
		// Save stamps UpdatedAt itself; the comparison covers the secrets.
		expected := bundle
		expected.UpdatedAt = time.Time{}
		reread.UpdatedAt = time.Time{}
		if !bundlesEqual(reread, expected) {
			return MigrationResult{}, fmt.Errorf("reread secret for %s does not match the saved bundle", item.ID)
		}
		records[item.ID] = record
	}

	previousVersion := store.data.Version
	previousAccounts := store.data.Accounts
	previousHealth := store.data.AuthHealth
	previousLegacyAuth := store.data.LegacyAuth
	store.data.Version = 4
	store.data.Accounts = records
	store.data.AuthHealth = make(map[string]account.AuthHealth, len(records))
	for id := range records {
		store.data.AuthHealth[id] = account.AuthUnknown
	}
	// The vault now holds the credentials; the rewritten file stays free of
	// authentication secrets.
	store.data.LegacyAuth = nil
	if err := store.persistLocked(); err != nil {
		store.data.Version = previousVersion
		store.data.Accounts = previousAccounts
		store.data.AuthHealth = previousHealth
		store.data.LegacyAuth = previousLegacyAuth
		return MigrationResult{}, fmt.Errorf("write version-4 state: %w", err)
	}
	return MigrationResult{Migrated: len(records), Message: fmt.Sprintf("migrated %d accounts", len(records))}, nil
}

func vaultCookiesFromState(values []Cookie) []secret.Cookie {
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

func bundlesEqual(left, right secret.Bundle) bool {
	return reflect.DeepEqual(left, right)
}
