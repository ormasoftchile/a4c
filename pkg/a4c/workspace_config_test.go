package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestWorkspaceConfig_Roundtrip: a missing file means "all defaults", and a
// saved value reads back.
func TestWorkspaceConfig_Roundtrip(t *testing.T) {
	dir := t.TempDir()

	cfg, err := a4c.LoadWorkspaceConfig(dir)
	if err != nil {
		t.Fatalf("load with no file: %v", err)
	}
	if cfg.DefaultStrategy != "" {
		t.Errorf("expected empty default with no config file, got %q", cfg.DefaultStrategy)
	}

	cfg.DefaultStrategy = a4c.StrategyReviewed
	if err := a4c.SaveWorkspaceConfig(dir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := a4c.LoadWorkspaceConfig(dir)
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if got.DefaultStrategy != a4c.StrategyReviewed {
		t.Errorf("expected %q, got %q", a4c.StrategyReviewed, got.DefaultStrategy)
	}
}

// TestEffectiveStrategy: an explicit request always wins over the workspace
// default, and the final fallback is direct.
func TestEffectiveStrategy(t *testing.T) {
	dir := t.TempDir()
	if err := a4c.SaveWorkspaceConfig(dir, a4c.WorkspaceConfig{DefaultStrategy: a4c.StrategyTeam}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := a4c.EffectiveStrategy(a4c.StrategyDirect, dir); got != a4c.StrategyDirect {
		t.Errorf("explicit request must win: got %q, want %q", got, a4c.StrategyDirect)
	}
	if got := a4c.EffectiveStrategy("", dir); got != a4c.StrategyTeam {
		t.Errorf("workspace default should apply when no request: got %q, want %q", got, a4c.StrategyTeam)
	}

	// A workspace with no config falls back to direct.
	if got := a4c.EffectiveStrategy("", t.TempDir()); got != a4c.StrategyDirect {
		t.Errorf("expected direct fallback, got %q", got)
	}

	// An unreadable config must not break resolution.
	broken := t.TempDir()
	if err := os.MkdirAll(filepath.Join(broken, ".a4c"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".a4c", "config.json"), []byte("{ not json "), 0o644); err != nil {
		t.Fatalf("write broken config: %v", err)
	}
	if got := a4c.EffectiveStrategy("", broken); got != a4c.StrategyDirect {
		t.Errorf("expected direct fallback on unreadable config, got %q", got)
	}
}

// TestParseExecutionStrategy: the CLI surface validates its input.
func TestParseExecutionStrategy(t *testing.T) {
	for _, in := range []string{"direct", "Reviewed", " TEAM ", "team"} {
		if _, err := a4c.ParseExecutionStrategy(in); err != nil {
			t.Errorf("ParseExecutionStrategy(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "fast", "waterfall"} {
		if _, err := a4c.ParseExecutionStrategy(in); err == nil {
			t.Errorf("ParseExecutionStrategy(%q) should have failed", in)
		}
	}
}

// TestBeginTask_AppliesWorkspaceDefault: the MCP/CLI entry point must honor the
// workspace default, which is the whole point — a host that never mentions a
// mode still produces tasks in the intended one.
func TestBeginTask_AppliesWorkspaceDefault(t *testing.T) {
	dir := t.TempDir()
	if err := a4c.SaveWorkspaceConfig(dir, a4c.WorkspaceConfig{DefaultStrategy: a4c.StrategyReviewed}); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc := a4c.NewTeamService(dir, nil, nil, nil, nil, nil, nil, nil)

	// No strategy named: the workspace default applies, and the result says so.
	res, err := svc.BeginTask(context.Background(), a4c.BeginTaskRequest{Objective: "mode inheritance"})
	if err != nil {
		t.Fatalf("BeginTask: %v", err)
	}
	if res.Strategy != a4c.StrategyReviewed {
		t.Errorf("workspace default not applied: got %q, want %q", res.Strategy, a4c.StrategyReviewed)
	}

	// Explicitly named mode still overrides it.
	res, err = svc.BeginTask(context.Background(), a4c.BeginTaskRequest{
		Objective: "explicit mode",
		Strategy:  a4c.StrategyTeam,
	})
	if err != nil {
		t.Fatalf("BeginTask(explicit): %v", err)
	}
	if res.Strategy != a4c.StrategyTeam {
		t.Errorf("explicit strategy overridden by default: got %q, want %q", res.Strategy, a4c.StrategyTeam)
	}
}
