package knowledge

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContext_IssueAndValidate(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSecretManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := sm.Issue(ExecContext{ProjectID: "skiff", TaskID: "T-1", ActivityID: "A-1", Role: "coder", Host: "copilot"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tok, ".") != 1 {
		t.Fatalf("token should be payload.sig, got %q", tok)
	}
	ctx, err := sm.Validate(tok, func(id string) bool { return id == "A-1" })
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if ctx.ProjectID != "skiff" || ctx.ActivityID != "A-1" || ctx.Role != "coder" || ctx.Host != "copilot" {
		t.Fatalf("context fields lost: %+v", ctx)
	}
}

func TestContext_ExpiredFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSecretManager(dir)
	tok, _ := sm.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, -time.Hour) // negative -> default; craft short via direct
	// Craft an expired token by issuing with a tiny TTL through the manager.
	expired, _ := sm.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, time.Nanosecond)
	time.Sleep(5 * time.Millisecond)
	if _, err := sm.Validate(expired, nil); !errors.Is(err, ErrContextExpired) {
		t.Fatalf("expected ErrContextExpired, got %v", err)
	}
	_ = tok
}

func TestContext_ForgedSignatureRejected(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSecretManager(dir)
	tok, _ := sm.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, time.Hour)
	parts := strings.Split(tok, ".")
	forged := parts[0] + ".deadbeef"
	if _, err := sm.Validate(forged, nil); !errors.Is(err, ErrContextForge) {
		t.Fatalf("expected ErrContextForge, got %v", err)
	}
	// Tampered payload with original signature.
	if _, err := sm.Validate(parts[0]+"x."+parts[1], nil); !errors.Is(err, ErrContextForge) {
		t.Fatalf("tampered payload must fail, got %v", err)
	}
}

func TestContext_CompletedActivityFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSecretManager(dir)
	tok, _ := sm.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, time.Hour)
	// Delayed call after A-1 completed: validator reports not active.
	if _, err := sm.Validate(tok, func(id string) bool { return false }); !errors.Is(err, ErrContextRevoked) {
		t.Fatalf("expected ErrContextRevoked for a completed activity, got %v", err)
	}
}

func TestContext_SecretPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sm1, _ := NewSecretManager(dir)
	tok, _ := sm1.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, time.Hour)
	// Simulate restart: a NEW SecretManager over the same dir must accept tok.
	sm2, _ := NewSecretManager(dir)
	if _, err := sm2.Validate(tok, nil); err != nil {
		t.Fatalf("token must survive a restart (secret reloaded), got %v", err)
	}
}

func TestContext_RevokerBlocksToken(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSecretManager(dir)
	r := NewRevoker()
	tok, _ := sm.Issue(ExecContext{ProjectID: "p", ActivityID: "A-1"}, time.Hour)
	active := func(id string) bool { return !r.IsRevoked(id) }
	if _, err := sm.Validate(tok, active); err != nil {
		t.Fatalf("should validate while active: %v", err)
	}
	r.Revoke("A-1")
	if _, err := sm.Validate(tok, active); !errors.Is(err, ErrContextRevoked) {
		t.Fatalf("expected revoked after Revoke, got %v", err)
	}
}

func TestConfig_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ReadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatalf("absent config must be disabled by default")
	}
	if Enabled(dir) {
		t.Fatalf("Enabled() must be false with no config")
	}
	// Missing file is not an error.
	if _, err := ReadConfig(filepath.Join(dir, "nope")); err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
}
