package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceConfigFile is the durable, human-editable per-workspace settings
// file. It lives beside the other a4c state so it survives host switches and
// travels with the repository checkout's .a4c directory.
const workspaceConfigFile = "config.json"

// WorkspaceConfig holds durable per-workspace settings. Everything here is a
// default that a per-request value can override — nothing in it is a security
// boundary.
type WorkspaceConfig struct {
	// DefaultStrategy is the execution strategy applied to a task whose
	// request names none. Empty means "direct".
	DefaultStrategy ExecutionStrategy `json:"default_strategy,omitempty"`
}

// workspaceConfigPath returns the config file path for a workspace directory.
func workspaceConfigPath(workspaceDir string) string {
	if strings.TrimSpace(workspaceDir) == "" {
		workspaceDir = "."
	}
	return filepath.Join(workspaceDir, ".a4c", workspaceConfigFile)
}

// LoadWorkspaceConfig reads the workspace settings. A missing file is not an
// error: it means "all defaults", which is the same as a fresh workspace.
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
		// A malformed config must not block work; report it and fall back to
		// defaults so the operator can fix the file without a service outage.
		return WorkspaceConfig{}, fmt.Errorf("workspace config %s is not valid JSON: %w", workspaceConfigPath(workspaceDir), err)
	}
	return cfg, nil
}

// SaveWorkspaceConfig writes the workspace settings, creating .a4c if needed.
func SaveWorkspaceConfig(workspaceDir string, cfg WorkspaceConfig) error {
	path := workspaceConfigPath(workspaceDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("workspace config: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ParseExecutionStrategy validates a strategy name.
func ParseExecutionStrategy(s string) (ExecutionStrategy, error) {
	switch ExecutionStrategy(strings.TrimSpace(strings.ToLower(s))) {
	case StrategyDirect:
		return StrategyDirect, nil
	case StrategyReviewed:
		return StrategyReviewed, nil
	case StrategyTeam:
		return StrategyTeam, nil
	default:
		return "", fmt.Errorf("unknown execution strategy %q (want direct, reviewed, or team)", s)
	}
}

// EffectiveStrategy resolves the strategy for a task: an explicit request wins,
// then the workspace default, then direct. Keeping this resolution in the
// service — rather than in each host's prompt — means a task gets the intended
// mode even when a host never mentions one.
func EffectiveStrategy(requested ExecutionStrategy, workspaceDir string) ExecutionStrategy {
	if requested != "" {
		return requested
	}
	if cfg, err := LoadWorkspaceConfig(workspaceDir); err == nil && cfg.DefaultStrategy != "" {
		return cfg.DefaultStrategy
	}
	return StrategyDirect
}
