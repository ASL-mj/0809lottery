package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	vaultKeySize   = 32
	vaultFileMode  = 0600
	vaultDirMode   = 0700
	nonceSizeBytes = 12
)

// FileVault persists one AES-256-GCM encrypted bundle per account in a single
// JSON file. Every Save encrypts the written bundle with a fresh random nonce
// and replaces the file atomically.
type FileVault struct {
	mu   sync.Mutex
	path string
	key  []byte
}

func NewFileVault(path string, key []byte) (*FileVault, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("vault path is required")
	}
	if len(key) != vaultKeySize {
		return nil, fmt.Errorf("vault key must be exactly %d bytes", vaultKeySize)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, vaultDirMode); err != nil {
		return nil, fmt.Errorf("create vault directory: %w", err)
	}
	return &FileVault{
		path: path,
		key:  append([]byte(nil), key...),
	}, nil
}

func (v *FileVault) Path() string {
	return v.path
}

func (v *FileVault) Load(_ context.Context, accountID string) (Bundle, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return Bundle{}, errors.New("vault account ID is required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entries, err := v.readFile()
	if err != nil {
		return Bundle{}, err
	}
	encoded, ok := entries.Accounts[accountID]
	if !ok {
		return Bundle{}, ErrNotFound
	}
	plaintext, err := v.decrypt(encoded)
	if err != nil {
		return Bundle{}, err
	}
	var bundle Bundle
	if err := json.Unmarshal(plaintext, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode vault bundle: %w", err)
	}
	return bundle, nil
}

func (v *FileVault) Save(_ context.Context, accountID string, bundle Bundle) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("vault account ID is required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entries, err := v.readFile()
	if err != nil {
		return err
	}
	bundle.UpdatedAt = time.Now().UTC()
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("encode vault bundle: %w", err)
	}
	encoded, err := v.encrypt(plaintext)
	if err != nil {
		return err
	}
	if entries.Accounts == nil {
		entries.Accounts = make(map[string]string)
	}
	previous, existed := entries.Accounts[accountID]
	entries.Accounts[accountID] = encoded
	if err := v.writeFile(entries); err != nil {
		if existed {
			entries.Accounts[accountID] = previous
		} else {
			delete(entries.Accounts, accountID)
		}
		return err
	}
	return nil
}

func (v *FileVault) Delete(_ context.Context, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("vault account ID is required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entries, err := v.readFile()
	if err != nil {
		return err
	}
	if _, ok := entries.Accounts[accountID]; !ok {
		return nil
	}
	previous := entries.Accounts[accountID]
	delete(entries.Accounts, accountID)
	if err := v.writeFile(entries); err != nil {
		entries.Accounts[accountID] = previous
		return err
	}
	return nil
}

type vaultFile struct {
	Version  int               `json:"version"`
	Accounts map[string]string `json:"accounts"`
}

func (v *FileVault) readFile() (vaultFile, error) {
	payload, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return vaultFile{Version: 1}, nil
	}
	if err != nil {
		return vaultFile{}, fmt.Errorf("read vault file: %w", err)
	}
	var entries vaultFile
	if err := json.Unmarshal(payload, &entries); err != nil {
		return vaultFile{}, fmt.Errorf("decode vault file: %w", err)
	}
	if entries.Version > 1 {
		return vaultFile{}, fmt.Errorf("vault version %d is newer than supported version 1", entries.Version)
	}
	return entries, nil
}

func (v *FileVault) writeFile(entries vaultFile) error {
	entries.Version = 1
	payload, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode vault file: %w", err)
	}
	directory := filepath.Dir(v.path)
	temporary, err := os.CreateTemp(directory, ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary vault: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(vaultFileMode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary vault: %w", err)
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary vault: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary vault: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary vault: %w", err)
	}
	if err := os.Rename(temporaryName, v.path); err != nil {
		return fmt.Errorf("replace vault: %w", err)
	}
	return nil
}

func (v *FileVault) encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", fmt.Errorf("initialize vault cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize vault GCM: %w", err)
	}
	nonce := make([]byte, nonceSizeBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate vault nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (v *FileVault) decrypt(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode vault entry: %w", err)
	}
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, fmt.Errorf("initialize vault cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize vault GCM: %w", err)
	}
	if len(raw) < nonceSizeBytes {
		return nil, errors.New("vault entry is too short")
	}
	plaintext, err := gcm.Open(nil, raw[:nonceSizeBytes], raw[nonceSizeBytes:], nil)
	if err != nil {
		return nil, errors.New("decrypt vault entry: key mismatch or corrupted entry")
	}
	return plaintext, nil
}
