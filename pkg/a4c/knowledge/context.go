package knowledge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ExecContext binds a knowledge operation to the A4C activity that issued it.
//
// Identity comes from the token, never from a shared "current activity" file and
// never from a model-supplied argument. Each A4C activity receives its own token,
// injected into its host session by the coordinator. A delayed tool call carries
// the token of the activity that originated it, so it is attributed to that
// activity — or rejected if that activity has since completed.
type ExecContext struct {
	ProjectID       string    `json:"project_id"`
	InvestigationID string    `json:"investigation_id,omitempty"`
	TaskID          string    `json:"task_id"`
	ActivityID      string    `json:"activity_id"`
	Role            string    `json:"role"`
	Host            string    `json:"host,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	IssuedAt        time.Time `json:"issued_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// Errors returned by the context validator. All fail closed.
var (
	ErrNoContext      = errors.New("knowledge: no valid execution context")
	ErrContextExpired = errors.New("knowledge: execution context expired")
	ErrContextRevoked = errors.New("knowledge: execution context revoked (activity no longer active)")
	ErrContextForge   = errors.New("knowledge: execution context signature invalid")
	ErrContextProject = errors.New("knowledge: execution context project mismatch")
)

// DefaultContextTTL bounds how long a token remains usable.
const DefaultContextTTL = 4 * time.Hour

// SecretManager issues and validates execution-context tokens.
//
// The secret is generated once per workspace and stored at
// <workspace>/.a4c/knowledge.secret (mode 0600). It is never sent to a model or
// embedded in a prompt; only the derived token is.
type SecretManager struct {
	mu     sync.Mutex
	secret []byte
	path   string
}

// NewSecretManager loads or creates the workspace secret.
func NewSecretManager(workspaceDir string) (*SecretManager, error) {
	path := filepath.Join(workspaceDir, ".a4c", "knowledge.secret")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("knowledge: create secret dir: %w", err)
	}
	if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
		return &SecretManager{secret: data, path: path}, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("knowledge: generate secret: %w", err)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, fmt.Errorf("knowledge: persist secret: %w", err)
	}
	return &SecretManager{secret: secret, path: path}, nil
}

// Issue mints a signed token for an activity. ttl<=0 uses DefaultContextTTL.
func (m *SecretManager) Issue(ctx ExecContext, ttl time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ttl <= 0 {
		ttl = DefaultContextTTL
	}
	now := time.Now().UTC()
	ctx.IssuedAt = now
	ctx.ExpiresAt = now.Add(ttl)
	payload, err := json.Marshal(ctx)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + sig, nil
}

// Validate parses and authenticates a token, checks expiry, and verifies that
// the bound activity is still active via isActive. It returns the context.
//
// isActive is supplied by the caller (the knowledge store consults the A4C
// activity store). A completed activity is not active, so a delayed call from a
// finished activity fails closed instead of being attributed to a later one.
func (m *SecretManager) Validate(token string, isActive func(activityID string) bool) (*ExecContext, error) {
	if token == "" {
		return nil, ErrNoContext
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, ErrContextForge
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrContextForge
	}
	m.mu.Lock()
	secret := m.secret
	m.mu.Unlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[1])) {
		return nil, ErrContextForge
	}
	var ctx ExecContext
	if err := json.Unmarshal(payload, &ctx); err != nil {
		return nil, ErrContextForge
	}
	if ctx.ActivityID == "" || ctx.ProjectID == "" {
		return nil, ErrContextForge
	}
	if time.Now().UTC().After(ctx.ExpiresAt) {
		return nil, ErrContextExpired
	}
	if isActive != nil && !isActive(ctx.ActivityID) {
		return nil, ErrContextRevoked
	}
	return &ctx, nil
}

// Revoke records that an activity's tokens must no longer be accepted. It is a
// fast-path cache; the authoritative check remains isActive against the activity
// store, so a process restart does not resurrect a revoked context.
type Revoker struct {
	mu      sync.Mutex
	revoked map[string]time.Time
}

// NewRevoker creates an empty revoker.
func NewRevoker() *Revoker { return &Revoker{revoked: map[string]time.Time{}} }

// Revoke marks an activity's tokens invalid.
func (r *Revoker) Revoke(activityID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[activityID] = time.Now().UTC()
}

// IsRevoked reports whether an activity's tokens were explicitly revoked.
func (r *Revoker) IsRevoked(activityID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.revoked[activityID]
	return ok
}
