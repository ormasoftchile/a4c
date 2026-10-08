package a4c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrOpenCodeNotInstalled = errors.New("opencode: CLI executable not found on system PATH")
	ErrOpenCodeServerFailed = errors.New("opencode: failed to start or connect to server")
)

// OpenCodeServerManager manages either an external or child-process OpenCode server.
type OpenCodeServerManager struct {
	mu         sync.Mutex
	endpoint   string
	isManaged  bool
	cmd        *exec.Cmd
	binaryPath string
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

	cmd := exec.Command(binPath, "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
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

// NewExternalOpenCodeServer attaches to an existing OpenCode HTTP endpoint.
func NewExternalOpenCodeServer(ctx context.Context, endpoint string) (*OpenCodeServerManager, error) {
	if endpoint == "" {
		endpoint = "http://127.0.0.1:4096"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	mgr := &OpenCodeServerManager{
		endpoint:  endpoint,
		isManaged: false,
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

// CheckHealth queries the /global/health endpoint once.
func (m *OpenCodeServerManager) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", m.endpoint+"/global/health", nil)
	if err != nil {
		return err
	}

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
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if !res.Healthy {
		return errors.New("server reported unhealthy")
	}
	return nil
}

// WaitUntilHealthy polls /global/health until success or context cancellation.
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
