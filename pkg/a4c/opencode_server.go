package a4c

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrOpenCodeNotInstalled = errors.New("opencode: CLI executable not found on system PATH")
	ErrOpenCodeServerFailed = errors.New("opencode: failed to start or connect to server")
)

// generateServerPassword returns a cryptographically random server password.
func generateServerPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// setBasicAuth attaches the OpenCode v2 HTTP Basic credentials (username
// "opencode") when a password is configured. It is a no-op without one, so
// mocks and passwordless servers keep working.
func setBasicAuth(req *http.Request, password string) {
	if password == "" {
		return
	}
	req.SetBasicAuth("opencode", password)
}

// OpenCodeServerManager manages either an external or child-process OpenCode server.
type OpenCodeServerManager struct {
	mu         sync.Mutex
	endpoint   string
	isManaged  bool
	cmd        *exec.Cmd
	binaryPath string
	password   string
}

// Password returns the server password used for HTTP Basic authentication
// (username "opencode"). It is empty when the server requires no auth.
func (m *OpenCodeServerManager) Password() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.password
}

// FindOpenCodeBinary locates the opencode CLI binary on the system.
func FindOpenCodeBinary() (string, error) {
	if p := os.Getenv("OPENCODE_BIN_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("%w: OPENCODE_BIN_PATH %q does not exist", ErrOpenCodeNotInstalled, p)
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	// Common Homebrew paths on macOS
	for _, candidate := range []string{
		"/opt/homebrew/bin/opencode",
		"/usr/local/bin/opencode",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", ErrOpenCodeNotInstalled
}

// NewManagedOpenCodeServer launches a managed local OpenCode server on loopback with a dynamic port.
func NewManagedOpenCodeServer(ctx context.Context, customPort int) (*OpenCodeServerManager, error) {
	binPath, err := FindOpenCodeBinary()
	if err != nil {
		return nil, err
	}

	port := customPort
	if port <= 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("opencode: find open port: %w", err)
		}
		port = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)

	// The v2 server always requires HTTP Basic auth (username "opencode").
	// OPENCODE_SERVER_PASSWORD pins a known password instead of consuming the
	// random one the server would print, so the adapter can authenticate.
	password, err := generateServerPassword()
	if err != nil {
		return nil, fmt.Errorf("opencode: generate server password: %w", err)
	}

	cmd := exec.Command(binPath, "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
	cmd.Env = append(os.Environ(), "OPENCODE_SERVER_PASSWORD="+password)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode: start server process: %w", err)
	}

	mgr := &OpenCodeServerManager{
		endpoint:   endpoint,
		isManaged:  true,
		cmd:        cmd,
		binaryPath: binPath,
		password:   password,
	}

	// Poll health check with timeout
	healthCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := mgr.WaitUntilHealthy(healthCtx); err != nil {
		_ = mgr.Stop()
		return nil, fmt.Errorf("%w: health check failed: %v", ErrOpenCodeServerFailed, err)
	}

	return mgr, nil
}

// opencodeServiceRegistration is the record the opencode CLI writes when it
// starts its shared background service.
type opencodeServiceRegistration struct {
	URL      string `json:"url"`
	Password string `json:"password"`
	Version  string `json:"version"`
}

// opencodeStateDirs lists the directories where the opencode CLI keeps its
// state, most specific first.
func opencodeStateDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "opencode"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "state", "opencode"))
	}
	return dirs
}

// DiscoverOpenCodeService locates the running shared OpenCode service by
// reading the registration the opencode CLI writes to its state directory,
// returning its endpoint and password.
//
// This matters because the service binds a DYNAMIC port. Assuming a fixed one
// does not merely fail when nothing is listening — on a real machine something
// else often is, and you end up talking to a different server whose API
// differs, which is far harder to diagnose than a connection error.
func DiscoverOpenCodeService() (endpoint, password string, ok bool) {
	for _, dir := range opencodeStateDirs() {
		data, err := os.ReadFile(filepath.Join(dir, "service.json"))
		if err != nil {
			continue
		}
		var reg opencodeServiceRegistration
		if err := json.Unmarshal(data, &reg); err != nil {
			continue
		}
		if reg.URL == "" {
			continue
		}
		return strings.TrimRight(reg.URL, "/"), reg.Password, true
	}
	return "", "", false
}

// NewExternalOpenCodeServer attaches to an existing OpenCode HTTP endpoint.
func NewExternalOpenCodeServer(ctx context.Context, endpoint string) (*OpenCodeServerManager, error) {
	if endpoint == "" {
		// Prefer the registered shared service over a fixed port, which on a
		// real machine is often a different process.
		if url, password, ok := DiscoverOpenCodeService(); ok {
			endpoint = url
			if os.Getenv("OPENCODE_SERVER_PASSWORD") == "" {
				_ = os.Setenv("OPENCODE_SERVER_PASSWORD", password)
			}
		} else {
			endpoint = "http://127.0.0.1:4096"
		}
	}
	endpoint = strings.TrimRight(endpoint, "/")

	mgr := &OpenCodeServerManager{
		endpoint:  endpoint,
		isManaged: false,
		password:  os.Getenv("OPENCODE_SERVER_PASSWORD"),
	}

	if err := mgr.CheckHealth(ctx); err != nil {
		return nil, fmt.Errorf("%w at %s: %v", ErrOpenCodeServerFailed, endpoint, err)
	}

	return mgr, nil
}

// Endpoint returns the base HTTP URL of the server.
func (m *OpenCodeServerManager) Endpoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoint
}

// IsManaged reports whether the server was spawned as a child process.
func (m *OpenCodeServerManager) IsManaged() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isManaged
}

// CheckHealth queries the /api/info endpoint once (v2 server info).
func (m *OpenCodeServerManager) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", m.endpoint+"/api/info", nil)
	if err != nil {
		return err
	}
	setBasicAuth(req, m.Password())

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned status %d", resp.StatusCode)
	}

	var res struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("decode server info response: %w", err)
	}
	if res.Version == "" {
		return errors.New("server info reported no version")
	}
	return nil
}

// WaitUntilHealthy polls /api/info until success or context cancellation.
func (m *OpenCodeServerManager) WaitUntilHealthy(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := m.CheckHealth(ctx); err == nil {
				return nil
			}
		}
	}
}

// Stop terminates the managed server process if applicable.
func (m *OpenCodeServerManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isManaged || m.cmd == nil || m.cmd.Process == nil {
		return nil
	}

	_ = m.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() {
		done <- m.cmd.Wait()
	}()

	select {
	case <-time.After(3 * time.Second):
		_ = m.cmd.Process.Kill()
	case <-done:
	}

	m.cmd = nil
	return nil
}
