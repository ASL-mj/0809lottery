package secret_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"skyeapi/lottery-bot/internal/secret"
)

func newTestVault(t *testing.T) *secret.FileVault {
	t.Helper()
	vault, err := secret.NewFileVault(
		filepath.Join(t.TempDir(), "vault", "secrets.json"),
		bytes.Repeat([]byte{7}, 32),
	)
	if err != nil {
		t.Fatalf("NewFileVault() error = %v", err)
	}
	return vault
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return payload
}

func fullBundle() secret.Bundle {
	return secret.Bundle{
		LoginName:              "user@example.test",
		Password:               "test-password",
		UserID:                 42,
		ParentAccessToken:      "parent-token-value",
		ParentAccessExpiresAt:  time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
		LotteryAccessToken:     "lottery-token-value",
		LotteryAccessExpiresAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		Cookies: []secret.Cookie{{
			Name:     "new_api_refresh",
			Value:    "refresh-cookie-value",
			Path:     "/api/user/auth",
			Domain:   "www.0809.one",
			Expires:  time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			Secure:   true,
			HTTPOnly: true,
		}},
		ManagedSessions: []secret.ManagedSession{{
			RemoteID:   "remote-session-1",
			Origin:     secret.SessionOriginWorkbench,
			Pinned:     true,
			LastSeenAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
		}},
	}
}

func TestFileVaultRoundTrip(t *testing.T) {
	vault := newTestVault(t)
	ctx := context.Background()
	bundle := fullBundle()

	requireNoError(t, vault.Save(ctx, "account-a", bundle))
	loaded, err := vault.Load(ctx, "account-a")
	requireNoError(t, err)
	if loaded.LoginName != bundle.LoginName || loaded.Password != bundle.Password || loaded.UserID != bundle.UserID {
		t.Fatalf("credential fields mismatch: %#v", loaded)
	}
	if loaded.ParentAccessToken != bundle.ParentAccessToken || !loaded.ParentAccessExpiresAt.Equal(bundle.ParentAccessExpiresAt) {
		t.Fatalf("parent token mismatch: %#v", loaded)
	}
	if loaded.LotteryAccessToken != bundle.LotteryAccessToken || !loaded.LotteryAccessExpiresAt.Equal(bundle.LotteryAccessExpiresAt) {
		t.Fatalf("lottery token mismatch: %#v", loaded)
	}
	if len(loaded.Cookies) != 1 || loaded.Cookies[0] != bundle.Cookies[0] {
		t.Fatalf("cookie mismatch: %#v", loaded.Cookies)
	}
	if len(loaded.ManagedSessions) != 1 || loaded.ManagedSessions[0] != bundle.ManagedSessions[0] {
		t.Fatalf("managed session mismatch: %#v", loaded.ManagedSessions)
	}

	if _, err := vault.Load(ctx, "account-b"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("Load(unknown account) error = %v, want ErrNotFound", err)
	}

	requireNoError(t, vault.Delete(ctx, "account-a"))
	if _, err := vault.Load(ctx, "account-a"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("Load(deleted account) error = %v, want ErrNotFound", err)
	}
	requireNoError(t, vault.Delete(ctx, "account-a"))
}

func TestFileVaultDoesNotPersistCleartext(t *testing.T) {
	vault := newTestVault(t)
	bundle := secret.Bundle{LoginName: "user@example.test", Password: "test-password"}
	requireNoError(t, vault.Save(context.Background(), "account-a", bundle))
	data := mustReadFile(t, vault.Path())
	if bytes.Contains(data, []byte(bundle.LoginName)) || bytes.Contains(data, []byte(bundle.Password)) {
		t.Fatal("vault persisted cleartext secret")
	}
}

func TestFileVaultRejectsWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault", "secrets.json")
	vault, err := secret.NewFileVault(path, bytes.Repeat([]byte{7}, 32))
	requireNoError(t, err)
	requireNoError(t, vault.Save(context.Background(), "account-a", fullBundle()))

	other, err := secret.NewFileVault(path, bytes.Repeat([]byte{9}, 32))
	requireNoError(t, err)
	if _, err := other.Load(context.Background(), "account-a"); err == nil || errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("Load() with wrong key error = %v, want decryption failure", err)
	}
}

func TestFileVaultRejectsInvalidKey(t *testing.T) {
	if _, err := secret.NewFileVault(filepath.Join(t.TempDir(), "v.json"), bytes.Repeat([]byte{1}, 31)); err == nil {
		t.Fatal("NewFileVault(31-byte key) error = nil")
	}
	if _, err := secret.NewFileVault(filepath.Join(t.TempDir(), "v.json"), nil); err == nil {
		t.Fatal("NewFileVault(nil key) error = nil")
	}
	if _, err := secret.NewFileVault("", bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("NewFileVault(empty path) error = nil")
	}
}

func TestFileVaultPersistsPrivateModes(t *testing.T) {
	vault := newTestVault(t)
	requireNoError(t, vault.Save(context.Background(), "account-a", fullBundle()))
	info, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatalf("stat vault file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("vault file mode = %o, want 600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(vault.Path()))
	if err != nil {
		t.Fatalf("stat vault directory: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0700 {
		t.Fatalf("vault directory mode = %o, want 700", mode)
	}
}

func TestFileVaultUsesFreshNoncePerSave(t *testing.T) {
	vault := newTestVault(t)
	ctx := context.Background()
	bundle := fullBundle()
	requireNoError(t, vault.Save(ctx, "account-a", bundle))
	first := mustReadFile(t, vault.Path())
	requireNoError(t, vault.Save(ctx, "account-a", bundle))
	second := mustReadFile(t, vault.Path())
	if bytes.Equal(first, second) {
		t.Fatal("two saves produced identical ciphertext; nonce is not fresh")
	}
}
