package knowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is the trusted workspace configuration at <workspace>/.a4c/knowledge.json.
// It is authored by a human or the harness before agents run, mirroring
// .a4c/verify.json. Absence means the module is disabled.
type Config struct {
	Enabled            bool   `json:"enabled"`
	Project            string `json:"project"`
	Investigation      string `json:"investigation,omitempty"`
	ContextTTLSeconds  int    `json:"context_ttl_seconds,omitempty"`
	MaxRecordsPerTask  int    `json:"max_records_per_task,omitempty"`
}

// DefaultContextTTLSeconds is used when the config omits a TTL.
const DefaultContextTTLSeconds = 14400

// TTL returns the configured token lifetime.
func (c Config) TTL() time.Duration {
	if c.ContextTTLSeconds <= 0 {
		return time.Duration(DefaultContextTTLSeconds) * time.Second
	}
	return time.Duration(c.ContextTTLSeconds) * time.Second
}

// ReadConfig reads <workspace>/.a4c/knowledge.json. A missing file is not an
// error: it returns a disabled config.
func ReadConfig(workspaceDir string) (Config, error) {
	var cfg Config
	path := filepath.Join(workspaceDir, ".a4c", "knowledge.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{Enabled: false}, nil
		}
		return cfg, fmt.Errorf("knowledge: read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("knowledge: parse config: %w", err)
	}
	return cfg, nil
}

// WriteConfig persists the configuration (used by `a4c knowledge enable|disable`).
func WriteConfig(workspaceDir string, cfg Config) error {
	dir := filepath.Join(workspaceDir, ".a4c")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("knowledge: create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "knowledge.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
