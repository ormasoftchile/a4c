package a4c_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestDiscoverOpenCodeService reads the registration the opencode CLI writes.
// Both the XDG and home locations are redirected so the test never reads the
// developer's real service registration.
func TestDiscoverOpenCodeService(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", t.TempDir())

	if _, _, ok := a4c.DiscoverOpenCodeService(); ok {
		t.Fatalf("expected no service when no registration exists")
	}

	dir := filepath.Join(state, "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A trailing slash must not survive into the endpoint.
	reg := `{"id":"abc","version":"2.0.29","url":"http://127.0.0.1:49374/","password":"secret","pid":1}`
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(reg), 0o600); err != nil {
		t.Fatalf("write registration: %v", err)
	}

	endpoint, password, ok := a4c.DiscoverOpenCodeService()
	if !ok {
		t.Fatalf("expected to discover the registered service")
	}
	if endpoint != "http://127.0.0.1:49374" {
		t.Errorf("endpoint = %q, want %q", endpoint, "http://127.0.0.1:49374")
	}
	if password != "secret" {
		t.Errorf("password not carried through")
	}
}

// TestDiscoverOpenCodeService_IgnoresMalformedRegistration: a corrupt file must
// not be mistaken for a usable endpoint.
func TestDiscoverOpenCodeService_IgnoresMalformedRegistration(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(state, "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte("{ not json "), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, ok := a4c.DiscoverOpenCodeService(); ok {
		t.Errorf("a malformed registration must not be treated as a discovered service")
	}

	// A registration with no URL is equally unusable.
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(`{"id":"abc"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, ok := a4c.DiscoverOpenCodeService(); ok {
		t.Errorf("a registration without a url must not count as discovered")
	}
}
