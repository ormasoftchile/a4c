package a4c

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Default: a workspace that never opted out has capture enabled and carries
// no config file at all.
func TestWorkspaceConfig_DefaultEnabledWithoutFile(t *testing.T) {
	dir := t.TempDir()
	if !KnowledgeEnabled(dir) {
		t.Fatal("fresh workspace: knowledge must be enabled by default")
	}
	if _, err := os.Stat(workspaceConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("reading defaults must not create a config file (err=%v)", err)
	}
}

func TestWorkspaceConfig_OptOutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SetKnowledgeEnabled(dir, false); err != nil {
		t.Fatalf("SetKnowledgeEnabled: %v", err)
	}
	if KnowledgeEnabled(dir) {
		t.Fatal("opt-out must disable capture")
	}
	// A plain re-read must not resurrect the default.
	if !KnowledgeEnabled(dir) {
		// expected
	} else {
		t.Fatal("opt-out must be durable across reads")
	}
	if err := SetKnowledgeEnabled(dir, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !KnowledgeEnabled(dir) {
		t.Fatal("--knowledge must re-enable capture")
	}
}

// Only the explicit flag writes: ordinary init must never create or flip the
// setting — otherwise a re-run would silently undo an opt-out.
func TestWorkspaceConfig_SetPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".a4c"), 0755); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{"unrelated": {"key": 42}}`)
	if err := os.WriteFile(workspaceConfigPath(dir), existing, 0644); err != nil {
		t.Fatal(err)
	}
	if err := SetKnowledgeEnabled(dir, false); err != nil {
		t.Fatalf("SetKnowledgeEnabled: %v", err)
	}
	data, err := os.ReadFile(workspaceConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"key": 42`) {
		t.Fatalf("unrelated config keys were destroyed:\n%s", data)
	}
	if KnowledgeEnabled(dir) {
		t.Fatal("opt-out lost after merge write")
	}
}

// An explicit `true` is distinct from "never configured" and survives JSON.
func TestWorkspaceConfig_ExplicitTrueSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SetKnowledgeEnabled(dir, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWorkspaceConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Knowledge == nil || cfg.Knowledge.Enabled == nil || !*cfg.Knowledge.Enabled {
		t.Fatalf("explicit true must round-trip, got %+v", cfg.Knowledge)
	}
}

// Fail toward the operator's expressible intent: a settings file that exists
// but cannot be parsed disables capture rather than silently resuming it.
func TestWorkspaceConfig_CorruptFileDisablesCapture(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".a4c"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceConfigPath(dir), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if KnowledgeEnabled(dir) {
		t.Fatal("corrupt config must not silently resume capture")
	}
	if _, err := LoadWorkspaceConfig(dir); err == nil {
		t.Fatal("corrupt config must surface an error to reporting surfaces")
	}
	// And the operator must be able to recover by fixing the file.
	if err := SetKnowledgeEnabled(dir, false); err == nil {
		t.Fatal("writing over a corrupt file must be refused, not silently overwritten")
	}
	if err := os.WriteFile(workspaceConfigPath(dir), []byte(`{"knowledge":{"enabled":false}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if KnowledgeEnabled(dir) {
		t.Fatal("fixed config must take effect")
	}
}

// SetKnowledgeEnabled must never clobber a corrupt file it cannot read —
// that would destroy whatever intent the file still carried.
func TestWorkspaceConfig_MalformedConfigRefusesWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".a4c"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceConfigPath(dir), []byte("{broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := SetKnowledgeEnabled(dir, true); err == nil {
		t.Fatal("expected refusal to write over corrupt config")
	}
	data, _ := os.ReadFile(workspaceConfigPath(dir))
	if string(data) != "{broken" {
		t.Fatalf("corrupt file was modified: %q", data)
	}
}

// stripKnowledgeSection removes exactly the knowledge block and nothing else.
func TestStripKnowledgeSection(t *testing.T) {
	withBlock := `You are A4C.

### Resuming Existing Work
Always resume first.

### Engineering Knowledge (optional, never ceremonial)
- Read the knowledge dir.
- Record meaningful discoveries.

### Tool invocation (OpenCode)
Call the tools directly.
`
	got := stripKnowledgeSection(withBlock)
	if strings.Contains(got, "Engineering Knowledge") {
		t.Errorf("knowledge heading survived:\n%s", got)
	}
	if strings.Contains(got, "Record meaningful discoveries") {
		t.Errorf("knowledge body survived:\n%s", got)
	}
	if !strings.Contains(got, "### Resuming Existing Work") {
		t.Errorf("unrelated section before was lost:\n%s", got)
	}
	if !strings.Contains(got, "### Tool invocation (OpenCode)") {
		t.Errorf("unrelated section after was lost:\n%s", got)
	}

	// Section last in the document (copilot/antigravity shapes).
	tailHeavy := "charter line\n\n### Engineering Knowledge (optional, never ceremonial)\n- record things.\n"
	gotTail := stripKnowledgeSection(tailHeavy)
	if strings.Contains(gotTail, "Engineering Knowledge") {
		t.Errorf("tail heading survived: %q", gotTail)
	}
	if !strings.HasPrefix(gotTail, "charter line") {
		t.Errorf("charter text damaged: %q", gotTail)
	}
	if !strings.HasSuffix(gotTail, "\n") {
		t.Errorf("document must end with newline: %q", gotTail)
	}

	// A document without the block is untouched byte-for-byte.
	noBlock := "just a charter\nwith no knowledge section\n"
	if out := stripKnowledgeSection(noBlock); out != noBlock {
		t.Errorf("document without knowledge block changed: %q", out)
	}
}
