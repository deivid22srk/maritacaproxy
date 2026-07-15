// Package account manages Maritaca accounts: storage, rotation, automatic
// creation via temporary email + browser-driven Auth0 verification.
package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/deivid22srk/maritacaproxy/internal/logger"
)

// Account represents a stored Maritaca account.
type Account struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Password      string    `json:"password"`           // encrypted at rest
	AccessToken   string    `json:"access_token"`       // encrypted at rest
	RefreshToken  string    `json:"refresh_token"`      // encrypted at rest
	TokenExpiry   time.Time `json:"token_expiry"`
	CooldownUntil time.Time `json:"cooldown_until"`
	InUse         bool      `json:"in_use"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsed      time.Time `json:"last_used"`
}

// Manager handles account storage and rotation.
type Manager struct {
	mu       sync.Mutex
	accounts []*Account
	storage  string
	encKey   []byte
	rrIdx    int
}

// NewManager creates a Manager with the given storage file path.
// The encryption key is derived from MARITACA_PROXY_ENCRYPTION_KEY env var
// or a default key (NOT recommended for production - set the env var!).
func NewManager(storagePath string) (*Manager, error) {
	if err := os.MkdirAll(filepath.Dir(storagePath), 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}

	keyHex := os.Getenv("MARITACA_PROXY_ENCRYPTION_KEY")
	if keyHex == "" {
		// Default - generate random 32-byte key and store it
		defaultKeyPath := filepath.Join(filepath.Dir(storagePath), ".enc_key")
		if _, err := os.Stat(defaultKeyPath); os.IsNotExist(err) {
			k := make([]byte, 32)
			if _, err := rand.Read(k); err != nil {
				return nil, err
			}
			if err := os.WriteFile(defaultKeyPath, []byte(hex.EncodeToString(k)), 0o600); err != nil {
				return nil, err
			}
			keyHex = hex.EncodeToString(k)
		} else {
			b, err := os.ReadFile(defaultKeyPath)
			if err != nil {
				return nil, err
			}
			keyHex = string(b)
		}
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid encryption key (must be 32 bytes hex)")
	}

	m := &Manager{
		storage: storagePath,
		encKey:  key,
	}
	if err := m.load(); err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	return m, nil
}

// load reads accounts from disk, decrypting sensitive fields.
func (m *Manager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.storage)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var raws []struct {
		ID            string    `json:"id"`
		Email         string    `json:"email"`
		Password      string    `json:"password"`
		AccessToken   string    `json:"access_token"`
		RefreshToken  string    `json:"refresh_token"`
		TokenExpiry   time.Time `json:"token_expiry"`
		CooldownUntil time.Time `json:"cooldown_until"`
		InUse         bool      `json:"in_use"`
		CreatedAt     time.Time `json:"created_at"`
		LastUsed      time.Time `json:"last_used"`
	}
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	m.accounts = make([]*Account, 0, len(raws))
	for _, r := range raws {
		acc := &Account{
			ID:            r.ID,
			Email:         r.Email,
			TokenExpiry:   r.TokenExpiry,
			CooldownUntil: r.CooldownUntil,
			InUse:         false, // reset on load
			CreatedAt:     r.CreatedAt,
			LastUsed:      r.LastUsed,
		}
		if r.Password != "" {
			if v, err := decrypt(m.encKey, r.Password); err == nil {
				acc.Password = v
			}
		}
		if r.AccessToken != "" {
			if v, err := decrypt(m.encKey, r.AccessToken); err == nil {
				acc.AccessToken = v
			}
		}
		if r.RefreshToken != "" {
			if v, err := decrypt(m.encKey, r.RefreshToken); err == nil {
				acc.RefreshToken = v
			}
		}
		m.accounts = append(m.accounts, acc)
	}
	logger.Info("[accounts] Loaded %d accounts", len(m.accounts))
	return nil
}

// save persists accounts to disk, encrypting sensitive fields.
func (m *Manager) save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	raws := make([]map[string]interface{}, 0, len(m.accounts))
	for _, a := range m.accounts {
		raw := map[string]interface{}{
			"id":             a.ID,
			"email":          a.Email,
			"token_expiry":   a.TokenExpiry,
			"cooldown_until": a.CooldownUntil,
			"in_use":         false, // don't persist in-use state
			"created_at":     a.CreatedAt,
			"last_used":      a.LastUsed,
		}
		if a.Password != "" {
			if v, err := encrypt(m.encKey, a.Password); err == nil {
				raw["password"] = v
			}
		}
		if a.AccessToken != "" {
			if v, err := encrypt(m.encKey, a.AccessToken); err == nil {
				raw["access_token"] = v
			}
		}
		if a.RefreshToken != "" {
			if v, err := encrypt(m.encKey, a.RefreshToken); err == nil {
				raw["refresh_token"] = v
			}
		}
		raws = append(raws, raw)
	}
	data, err := json.MarshalIndent(raws, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := m.storage + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.storage)
}

// Add adds a new account to the manager.
func (m *Manager) Add(acc *Account) error {
	m.mu.Lock()
	m.accounts = append(m.accounts, acc)
	m.mu.Unlock()
	return m.save()
}

// Remove deletes an account by ID.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	for i, a := range m.accounts {
		if a.ID == id {
			m.accounts = append(m.accounts[:i], m.accounts[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	return m.save()
}

// List returns a copy of all accounts.
func (m *Manager) List() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, len(m.accounts))
	copy(out, m.accounts)
	return out
}

// Get returns an account by ID.
func (m *Manager) Get(id string) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// GetByEmail returns an account by email.
func (m *Manager) GetByEmail(email string) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Email == email {
			return a
		}
	}
	return nil
}

// GetNextAvailable returns the next available account (round-robin),
// skipping those that are in use or on cooldown. The `exclude` set contains
// account IDs that should be skipped (already tried).
func (m *Manager) GetNextAvailable(exclude map[string]bool) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := 0; i < len(m.accounts); i++ {
		idx := (m.rrIdx + i) % len(m.accounts)
		acc := m.accounts[idx]
		if exclude[acc.ID] {
			continue
		}
		if acc.InUse {
			continue
		}
		if now.Before(acc.CooldownUntil) {
			continue
		}
		if acc.AccessToken == "" && acc.RefreshToken == "" {
			continue
		}
		m.rrIdx = (idx + 1) % len(m.accounts)
		return acc
	}
	return nil
}

// MarkInUse marks the account as in-use.
func (m *Manager) MarkInUse(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.ID == id {
			a.InUse = true
			a.LastUsed = time.Now()
		}
	}
}

// ReleaseInUse clears the in-use flag.
func (m *Manager) ReleaseInUse(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.ID == id {
			a.InUse = false
		}
	}
}

// MarkCooldown sets the cooldown_until time for an account.
func (m *Manager) MarkCooldown(id string, duration time.Duration, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.ID == id {
			a.CooldownUntil = time.Now().Add(duration)
			logger.Warn("[accounts] %s (%s) on cooldown for %v: %s", a.Email, a.ID, duration, reason)
		}
	}
	go m.save()
}

// UpdateTokens updates access/refresh tokens for an account.
func (m *Manager) UpdateTokens(id, accessToken, refreshToken string, expiresIn int) error {
	m.mu.Lock()
	for _, a := range m.accounts {
		if a.ID == id {
			a.AccessToken = accessToken
			if refreshToken != "" {
				a.RefreshToken = refreshToken
			}
			if expiresIn > 0 {
				a.TokenExpiry = time.Now().Add(time.Duration(expiresIn) * time.Second)
			}
			break
		}
	}
	m.mu.Unlock()
	return m.save()
}

// IsTokenExpired returns true if the access token has expired (or expires within 60s).
func (a *Account) IsTokenExpired() bool {
	return time.Now().Add(60 * time.Second).After(a.TokenExpiry)
}

// ─── AES-256-GCM encryption helpers ────────────────────────────────────────

func encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	enc := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(enc), nil
}

func decrypt(key []byte, ciphertextHex string) (string, error) {
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
