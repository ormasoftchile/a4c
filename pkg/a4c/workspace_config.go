package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceConfigFile is the durable, human-editable per-workspace settings
// file. It lives in the workspace's own .a4c state directory, beside the task
// records it governs, so the opt-out travels with the workspace checkout and
// never with the source repository.
const workspaceConfigFile = "config.json"

// WorkspaceConfig holds durable per-workspace settings. Every field is a
// default a request can override; none of it is a security boundary.
type WorkspaceConfig struct {
	// Knowledge configures knowledge capture for this workspace.
	Knowledge *KnowledgeSettings `json:"knowledge,omitempty"`
}

// KnowledgeSettings configures the knowledge capture opt-out.
type KnowledgeSettings struct {
	// Enabled is tri-state: nil (absent) means enabled — the default for a
	// workspace that never opted out — and false opts the workspace out of
	// producing knowledge entirely. It is a pointer so an explicit `true`
	// survives a round trip through JSON as distinct from "never configured".
	Enabled *bool `json:"enabled,omitempty"`
}

// workspaceConfigPath resolves the settings file for a workspace directory.
func workspaceConfigPath(workspaceDir string) string {
	if strings.TrimSpace(workspaceDir) == "" {
		workspaceDir = "."
	}
	return filepath.Join(workspaceDir, ".a4c", workspaceConfigFile)
}

// LoadWorkspaceConfig reads the workspace settings. A missing file is not an
// error: it means "all defaults", the same as a fresh workspace.
func LoadWorkspaceConfig(workspaceDir string) (WorkspaceConfig, error) {
	var cfg WorkspaceConfig
	data, err := os.ReadFile(workspaceConfigPath(workspaceDir))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("workspace config: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return WorkspaceConfig{}, fmt.Errorf("workspace config %s is not valid JSON: %w", workspaceConfigPath(workspaceDir), err)
	}
	return cfg, nil
}

// KnowledgeEnabled reports whether knowledge capture is on for workspaceDir.
//
// Default: enabled. `a4c init --no-knowledge` writes knowledge.enabled=false
// and capture stops everywhere — instructions, context, writes, warnings.
//
// A config file that exists but cannot be parsed resolves to DISABLED: an
// operator who wrote the file meant to express intent, and an opted-out
// workspace must not resume capture because its settings got corrupted.
// `a4c status knowledge` surfaces the parse error rather than hiding it.
func KnowledgeEnabled(workspaceDir string) bool {
	cfg, err := LoadWorkspaceConfig(workspaceDir)
	if err != nil {
		return false
	}
	if cfg.Knowledge == nil || cfg.Knowledge.Enabled == nil {
		return true
	}
	return *cfg.Knowledge.Enabled
}

// SetKnowledgeEnabled persists the knowledge setting, preserving every other
// key the settings file already carries. Only an explicit call writes: a plain
// re-run of `a4c init` never creates the file and never flips an existing
// opt-out back on.
func SetKnowledgeEnabled(workspaceDir string, enabled bool) error {
	path := workspaceConfigPath(workspaceDir)
	raw := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("workspace config %s is not valid JSON; fix or remove it before changing knowledge settings: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("workspace config: %w", err)
	}
	know, _ := raw["knowledge"].(map[string]any)
	if know == nil {
		know = map[string]any{}
	}
	know["enabled"] = enabled
	raw["knowledge"] = know

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("workspace config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("workspace config: %w", err)
	}
	return os.Rename(tmp, path)
}
