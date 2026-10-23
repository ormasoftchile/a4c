package a4c

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The full opt-out contract: an opted-out workspace is silent everywhere —
// no directory in team_context, no writes from claims, no accountability
// warnings at acceptance — while the enabled default is byte-for-byte the
// behavior that existed before the opt-out.
func TestKnowledgeOptOut_SilentEverywhere(t *testing.T) {
	h := setupKBHarness(t)
	ctx := context.Background()

	if err := SetKnowledgeEnabled(h.repoDir, false); err != nil {
		t.Fatalf("SetKnowledgeEnabled: %v", err)
	}

	// 1. team_context exposes no knowledge directory: agents have nothing to
	// record into and are never told a path exists.
	teamCtx, err := h.svc.GetTeamContext(ctx)
	if err != nil {
		t.Fatalf("GetTeamContext: %v", err)
	}
	if teamCtx.KnowledgeDir != "" {
		t.Fatalf("context exposes knowledge dir %q for an opted-out workspace", teamCtx.KnowledgeDir)
	}

	// 2. A significant claim with inline content is refused with an explicit
	// outcome — and no file lands anywhere.
	taskID := h.beginDirect(t, "implement feature under opt-out")
	res, err := h.svc.CompleteActivity(ctx, CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder",
		Summary: "implemented", Success: true,
		Knowledge: &KnowledgeClaim{
			Significant: true,
			FileName:    "findings/optout-should-not-write.md",
			Content:     "# this document must never exist\n",
		},
	})
	if err != nil {
		t.Fatalf("CompleteActivity: %v", err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "disabled" {
		t.Fatalf("knowledge outcome = %+v, want status 'disabled'", res.Knowledge)
	}
	if !strings.Contains(res.Knowledge.Reason, "disabled") {
		t.Fatalf("refusal must explain itself, got reason %q", res.Knowledge.Reason)
	}
	if _, err := os.Stat(filepath.Join(h.kbDir, "findings", "optout-should-not-write.md")); !os.IsNotExist(err) {
		t.Fatalf("opt-out leaked a knowledge file to disk (err=%v)", err)
	}

	// 3. A path-claiming agent (which wrote outside, before opting out) is
	// not verified into legitimacy either: the claim is still 'disabled'.
	res2, err := h.svc.CompleteActivity(ctx, CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder",
		Summary: "second completion with paths claim", Success: true,
		Knowledge: &KnowledgeClaim{Significant: true, Paths: []string{"findings/whatever.md"}},
	})
	if err != nil {
		t.Fatalf("CompleteActivity #2: %v", err)
	}
	if res2.Knowledge == nil || res2.Knowledge.Status != "disabled" {
		t.Fatalf("path claim outcome = %+v, want 'disabled'", res2.Knowledge)
	}

	// 4. Coverage reports the opt-out instead of nagging about a missing
	// assessment, and acceptance carries zero knowledge warnings.
	details, err := h.svc.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	cov := details.KnowledgeCoverage
	if cov == nil || !cov.Disabled {
		t.Fatalf("coverage = %+v, want Disabled=true", cov)
	}
	if w := knowledgeCoverageWarnings(cov); len(w) != 0 {
		t.Fatalf("opted-out workspace still warns at acceptance: %v", w)
	}
}

// Re-enabling restores the exact prior behavior: directory exposed, claims
// processed, warnings active again.
func TestKnowledgeOptOut_ReEnableRestoresCapture(t *testing.T) {
	h := setupKBHarness(t)
	ctx := context.Background()

	if err := SetKnowledgeEnabled(h.repoDir, false); err != nil {
		t.Fatal(err)
	}
	if err := SetKnowledgeEnabled(h.repoDir, true); err != nil {
		t.Fatal(err)
	}

	teamCtx, err := h.svc.GetTeamContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if teamCtx.KnowledgeDir == "" {
		t.Fatal("re-enabled workspace must expose its knowledge dir again")
	}
	if teamCtx.KnowledgeDir != h.kbDir {
		t.Fatalf("context dir = %q, want %q", teamCtx.KnowledgeDir, h.kbDir)
	}

	taskID := h.beginDirect(t, "attest after re-enable")
	res, err := h.svc.CompleteActivity(ctx, CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder",
		Summary: "routine fix", Success: true,
		Knowledge: &KnowledgeClaim{Significant: false, Reason: "routine"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Knowledge == nil || res.Knowledge.Status != "assessed" {
		t.Fatalf("outcome = %+v, want 'assessed' (the pre-opt-out default)", res.Knowledge)
	}
}

// The default path — no config file at all — must be untouched: claims flow,
// documents are written, context exposes the directory. This is the regression
// guard against the opt-out accidentally inverting the default.
func TestKnowledgeOptOut_DefaultBehaviorUnchanged(t *testing.T) {
	h := setupKBHarness(t)
	ctx := context.Background()

	teamCtx, err := h.svc.GetTeamContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if teamCtx.KnowledgeDir != h.kbDir {
		t.Fatalf("default context dir = %q, want %q", teamCtx.KnowledgeDir, h.kbDir)
	}

	taskID := h.beginDirect(t, "capture under default")
	_, err = h.svc.CompleteActivity(ctx, CompleteActivityRequest{
		TaskID: taskID, ActivityID: taskID, Actor: "coder",
		Summary: "found something real", Success: true,
		Knowledge: &KnowledgeClaim{
			Significant: true,
			FileName:    "findings/default-capture-still-works.md",
			Content:     "# default capture\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	written := filepath.Join(h.kbDir, "findings", "default-capture-still-works.md")
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("default capture did not write: %v", err)
	}
	if err := os.Remove(written); err != nil {
		t.Fatal(err)
	}
}

// Generated instructions must agree with runtime behavior: an opted-out
// workspace's agent/skill files never tell the agent to record knowledge, and
// an enabled workspace's files are byte-for-byte unchanged (the section stays).
func TestKnowledgeOptOut_GeneratedInstructionsMatchConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))

	// Enabled (default): all three surfaces carry the knowledge section.
	exportAll := func() map[string]string {
		t.Helper()
		if err := ExportCopilotAgents(dir); err != nil {
			t.Fatalf("ExportCopilotAgents: %v", err)
		}
		if err := ExportOpenCodeAgents(dir); err != nil {
			t.Fatalf("ExportOpenCodeAgents: %v", err)
		}
		if err := ExportAntigravityCustomization(dir); err != nil {
			t.Fatalf("ExportAntigravityCustomization: %v", err)
		}
		surfaces := map[string]string{
			"copilot":         filepath.Join(dir, ".github", "agents", "a4c.agent.md"),
			"copilot-strict":  filepath.Join(dir, ".github", "agents", "a4c-strict.agent.md"),
			"opencode":        filepath.Join(dir, ".opencode", "agents", "a4c.md"),
			"opencode-strict": filepath.Join(dir, ".opencode", "agents", "a4c-strict.md"),
			"skill":           filepath.Join(dir, ".agents", "skills", "a4c", "SKILL.md"),
		}
		out := map[string]string{}
		for name, path := range surfaces {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			out[name] = string(data)
		}
		return out
	}

	enabled := exportAll()
	for name, body := range enabled {
		if !strings.Contains(body, "Engineering Knowledge") {
			t.Errorf("enabled workspace: %s lost its knowledge section", name)
		}
	}

	// Opted out: every surface loses the section, and nothing else.
	if err := SetKnowledgeEnabled(dir, false); err != nil {
		t.Fatal(err)
	}
	disabled := exportAll()
	for name, body := range disabled {
		if strings.Contains(body, "Engineering Knowledge") {
			t.Errorf("opted-out workspace: %s still instructs knowledge capture", name)
		}
		if !strings.Contains(body, "team_complete_activity") {
			t.Errorf("opted-out workspace: %s lost core protocol instructions", name)
		}
		if !strings.Contains(body, "team_begin_task") {
			t.Errorf("opted-out workspace: %s lost begin protocol instructions", name)
		}
		// The only difference from the enabled version is the removed section.
		enabledBody := enabled[name]
		if len(body) >= len(enabledBody) {
			t.Errorf("%s: disabled body (%d bytes) should be smaller than enabled (%d)", name, len(body), len(enabledBody))
		}
	}

	// Re-enable and regenerate: byte-for-byte back to the enabled originals.
	if err := SetKnowledgeEnabled(dir, true); err != nil {
		t.Fatal(err)
	}
	again := exportAll()
	for name, body := range again {
		if body != enabled[name] {
			t.Errorf("%s: re-enabled output differs from the enabled original", name)
		}
	}
}

// Doctor must report the opt-out as an intentional state, not a missing setup.
func TestDoctorReportsKnowledgeOptOut(t *testing.T) {
	dir := t.TempDir()
	runKBGit(t, dir, "init")
	runKBGit(t, dir, "config", "user.name", "T")
	runKBGit(t, dir, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runKBGit(t, dir, "add", "README.md")
	runKBGit(t, dir, "commit", "-m", "init")

	if err := SetKnowledgeEnabled(dir, false); err != nil {
		t.Fatal(err)
	}
	report := RunDoctor(dir)
	found := false
	for _, c := range report.Checks {
		if c.Name == "Knowledge Base" {
			found = true
			if !c.Passed {
				t.Errorf("opt-out must not fail the doctor check: %+v", c)
			}
			if !strings.Contains(c.Detail, "disabled") {
				t.Errorf("doctor detail must say capture is disabled, got %q", c.Detail)
			}
		}
	}
	if !found {
		t.Fatal("doctor did not report the Knowledge Base check")
	}
}
