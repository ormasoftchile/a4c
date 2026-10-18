package a4c_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

// TestExportOpenCodeAgents_PreservesExistingConfig: installing must merge, not
// clobber. A project that already configures its model, permissions, other MCP
// servers, or its own default_agent must keep all of it.
func TestExportOpenCodeAgents_PreservesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	existing := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "anthropic/claude-sonnet-4-5",
  "default_agent": "build",
  "permissions": [{"action": "shell", "resource": "git push *", "effect": "deny"}],
  "mcp": {"other-server": {"type": "local", "command": ["foo"]}}
}`
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatalf("seed opencode.json: %v", err)
	}

	if err := a4c.ExportOpenCodeAgents(dir); err != nil {
		t.Fatalf("ExportOpenCodeAgents: %v", err)
	}

	cfg := readJSON(t, path)

	if got := cfg["model"]; got != "anthropic/claude-sonnet-4-5" {
		t.Errorf("SECURITY: model was destroyed, got %v", got)
	}
	if got := cfg["default_agent"]; got != "build" {
		t.Errorf("default_agent is a user preference and must not be claimed, got %v", got)
	}
	if perms, ok := cfg["permissions"].([]any); !ok || len(perms) != 1 {
		t.Errorf("SECURITY: permissions were destroyed, got %v", cfg["permissions"])
	}
	servers, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp block missing: %v", cfg["mcp"])
	}
	if _, ok := servers["other-server"]; !ok {
		t.Errorf("SECURITY: pre-existing MCP server was destroyed, got %v", servers)
	}
	if _, ok := servers["a4c"]; !ok {
		t.Errorf("a4c MCP server was not added, got %v", servers)
	}
}

// TestExportOpenCodeAgents_ClaimsDefaultAgentWhenUnset: a project with no
// chosen agent gets the a4c default.
func TestExportOpenCodeAgents_ClaimsDefaultAgentWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"model": "opencode-go/mimo-v2.5"}`), 0644); err != nil {
		t.Fatalf("seed opencode.json: %v", err)
	}

	if err := a4c.ExportOpenCodeAgents(dir); err != nil {
		t.Fatalf("ExportOpenCodeAgents: %v", err)
	}

	cfg := readJSON(t, path)
	if got := cfg["default_agent"]; got != "a4c" {
		t.Errorf("expected default_agent to be claimed when unset, got %v", got)
	}
	if got := cfg["model"]; got != "opencode-go/mimo-v2.5" {
		t.Errorf("model was destroyed, got %v", got)
	}
}

// TestExportOpenCodeAgents_RefusesInvalidJSON: an existing but unparseable
// config must fail loudly rather than be silently replaced.
func TestExportOpenCodeAgents_RefusesInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	broken := `{ this is not json `
	if err := os.WriteFile(path, []byte(broken), 0644); err != nil {
		t.Fatalf("seed opencode.json: %v", err)
	}

	if err := a4c.ExportOpenCodeAgents(dir); err == nil {
		t.Fatalf("expected an error for an unparseable existing config")
	}

	data, _ := os.ReadFile(path)
	if string(data) != broken {
		t.Errorf("SECURITY: invalid config was overwritten:\n%s", string(data))
	}
}

// TestExportCopilotAgents_PreservesExistingMcpServers: .mcp.json follows the
// same merge rule.
func TestExportCopilotAgents_PreservesExistingMcpServers(t *testing.T) {
	dir := t.TempDir()
	// ExportCopilotAgents also merges the global ~/.copilot/mcp-config.json;
	// redirect HOME so the test never touches the developer's real config.
	t.Setenv("HOME", filepath.Join(dir, "home"))
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"docs": {"command": "npx", "args": ["-y", "docs-mcp"]}}}`), 0644); err != nil {
		t.Fatalf("seed .mcp.json: %v", err)
	}

	if err := a4c.ExportCopilotAgents(dir); err != nil {
		t.Fatalf("ExportCopilotAgents: %v", err)
	}

	cfg := readJSON(t, path)
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers block missing: %v", cfg["mcpServers"])
	}
	if _, ok := servers["docs"]; !ok {
		t.Errorf("SECURITY: pre-existing MCP server was destroyed, got %v", servers)
	}
	if _, ok := servers["a4c"]; !ok {
		t.Errorf("a4c MCP server was not added, got %v", servers)
	}
}

// TestExport_Idempotent: re-running install must not accumulate duplicate
// entries, since init now runs it on every invocation.
func TestExport_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))

	for i := 0; i < 3; i++ {
		if err := a4c.ExportOpenCodeAgents(dir); err != nil {
			t.Fatalf("ExportOpenCodeAgents run %d: %v", i, err)
		}
		if err := a4c.ExportCopilotAgents(dir); err != nil {
			t.Fatalf("ExportCopilotAgents run %d: %v", i, err)
		}
	}

	oc := readJSON(t, filepath.Join(dir, "opencode.json"))
	servers, ok := oc["mcp"].(map[string]any)
	if !ok || len(servers) != 1 {
		t.Errorf("expected exactly one OpenCode MCP server after 3 runs, got %v", oc["mcp"])
	}
	mcp := readJSON(t, filepath.Join(dir, ".mcp.json"))
	mcpServers, ok := mcp["mcpServers"].(map[string]any)
	if !ok || len(mcpServers) != 1 {
		t.Errorf("expected exactly one .mcp.json server after 3 runs, got %v", mcp["mcpServers"])
	}
}

// TestExportAntigravity_WritesToDiscoveredPath: agy discovers project skills at
// .agents/skills/<name>/SKILL.md. Writing anywhere else leaves a file that
// exists but that the host never sees, which is indistinguishable from the
// install having failed.
func TestExportAntigravity_WritesToDiscoveredPath(t *testing.T) {
	dir := t.TempDir()

	if err := a4c.ExportAntigravityCustomization(dir); err != nil {
		t.Fatalf("ExportAntigravityCustomization: %v", err)
	}

	want := filepath.Join(dir, ".agents", "skills", "a4c", "SKILL.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("skill not written to the path agy scans: %v", err)
	}

	// The legacy location must not be written: agy does not scan it, and a
	// second copy risks confusing an operator about which one is live.
	legacy := filepath.Join(dir, ".gemini", "antigravity-cli", "skills", "a4c", "SKILL.md")
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("skill written to the legacy path agy does not scan: %s", legacy)
	}
}

// TestPrimaryPromptsDocumentExecutionModes: every host's primary a4c prompt
// must describe the three execution strategies, or an operator has no way to
// select one. This regressed once for Antigravity, whose skill documented
// roles, durable state, and knowledge but never mentioned the modes — so the
// host had no idea they existed.
func TestPrimaryPromptsDocumentExecutionModes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home")) // ExportCopilotAgents also merges global config

	if err := a4c.ExportOpenCodeAgents(dir); err != nil {
		t.Fatalf("ExportOpenCodeAgents: %v", err)
	}
	if err := a4c.ExportCopilotAgents(dir); err != nil {
		t.Fatalf("ExportCopilotAgents: %v", err)
	}
	if err := a4c.ExportAntigravityCustomization(dir); err != nil {
		t.Fatalf("ExportAntigravityCustomization: %v", err)
	}

	surfaces := []string{
		filepath.Join(dir, ".opencode", "agents", "a4c.md"),
		filepath.Join(dir, ".github", "agents", "a4c.agent.md"),
		filepath.Join(dir, ".agents", "skills", "a4c", "SKILL.md"),
	}
	for _, path := range surfaces {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := strings.ToLower(string(data))
		for _, mode := range []string{"direct", "reviewed", "team"} {
			if !strings.Contains(body, mode) {
				t.Errorf("%s does not document execution mode %q", path, mode)
			}
		}
		// The operator must also learn the human acceptance boundary.
		if !strings.Contains(body, "task accept") {
			t.Errorf("%s does not mention the human acceptance command", path)
		}
	}
}
