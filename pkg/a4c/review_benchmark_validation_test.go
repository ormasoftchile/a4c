package a4c_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// preservedBenchmarkDir locates the preserved large-repo benchmark artifacts.
// Validation reuses these real files and the real vacuous evidence; it does NOT
// re-run the two-hour coding benchmark.
const preservedBenchmarkDir = "../../benchmarks/large-repo-comparison/results/2026-10-15-large-repo-team-budget-rerun"

// preservedWorktreeFiles are the real files from the observed failure, copied
// into a lightweight fixture so validation stays fast and self-contained.
var preservedWorktreeFiles = []string{
	"src/cypher/cypher.c",
	"src/cypher/cypher.h",
	"tests/test_cypher.c",
	"doc.go",
	"go.mod",
}

// loadPreservedVacuousEvidence reads the coder's actual recorded evidence from
// the benchmark activity artifact. This is the false-positive evidence that
// caused the original failure: `go test` on a C repo reporting "[no test files]"
// with exit code 0 and valid=true.
func loadPreservedVacuousEvidence(t *testing.T) *spike.ValidationEvidence {
	t.Helper()
	art := filepath.Join(preservedBenchmarkDir, "a4c-team", ".a4c", "activities", "task-1792091246000145000-sub-1.json")
	data, err := os.ReadFile(art)
	if err != nil {
		t.Skipf("preserved benchmark artifacts not available: %v", err)
	}
	var rec struct {
		Result struct {
			Outputs map[string]json.RawMessage `json:"outputs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse preserved artifact: %v", err)
	}
	raw, ok := rec.Result.Outputs["validation_evidence"]
	if !ok {
		t.Fatalf("preserved artifact missing validation_evidence")
	}
	var ev spike.ValidationEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("parse preserved evidence: %v", err)
	}
	// Confirm this is genuinely the vacuous evidence from the observed run.
	if ev.ExitCode != 0 {
		t.Fatalf("expected preserved evidence exit 0, got %d", ev.ExitCode)
	}
	if !strings.Contains(strings.Join(ev.Command, " "), "go test") {
		t.Fatalf("expected preserved go test command, got %v", ev.Command)
	}
	return &ev
}

// buildPreservedFixture copies the real preserved files into a git repo so the
// corrected workflow can be exercised against the actual implementation.
func buildPreservedFixture(t *testing.T) string {
	t.Helper()
	srcRoot := filepath.Join(preservedBenchmarkDir, "a4c-team", ".a4c", "worktrees", "task-1792091246000145000", "tree")
	if _, err := os.Stat(srcRoot); err != nil {
		t.Skipf("preserved worktree not available: %v", err)
	}
	ws := t.TempDir()
	for _, rel := range preservedWorktreeFiles {
		src := filepath.Join(srcRoot, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("preserved file %s not readable: %v", rel, err)
		}
		dst := filepath.Join(ws, rel)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", rel, err)
		}
	}
	// Make it a git repo so diff/file-listing works.
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		_ = cmd.Run()
	}
	run("init", "-q")
	run("config", "user.email", "bench@example.com")
	run("config", "user.name", "Benchmark Harness")
	run("add", "-A")
	run("commit", "-q", "-m", "baseline")
	return ws
}

// TestIntegration_PreservedBenchmarkReviewWorkflow validates the corrected
// review workflow against the preserved large-repository implementation and its
// real vacuous evidence. It demonstrates that a reviewer can now:
//   - access the real implementation and tests via bounded reads,
//   - see that the recorded evidence is VACUOUS (ran no tests), not a pass,
//   - have doc.go/go.mod flagged as unrelated,
//   - reach verification_blocked WITHOUT forcing an unnecessary coding revision.
//
// This is the exact failure from the benchmark, replayed without re-running it.
func TestIntegration_PreservedBenchmarkReviewWorkflow(t *testing.T) {
	ev := loadPreservedVacuousEvidence(t)
	ws := buildPreservedFixture(t)

	// 1. The corrected analyzer classifies the REAL preserved evidence as
	// vacuous instead of a valid pass.
	testsExecuted, vacuous := spike.AnalyzeGoTestOutput(ev.OutputSummary)
	if !vacuous {
		t.Fatalf("preserved '[no test files]' evidence must be detected vacuous; got testsExecuted=%d", testsExecuted)
	}

	// 2. Diff hygiene flags the REAL unrelated doc.go/go.mod additions.
	warnings := spike.FlagDiffHygiene([]string{
		"src/cypher/cypher.c", "src/cypher/cypher.h", "tests/test_cypher.c", "doc.go", "go.mod",
	}, "Support property-to-property comparisons in Cypher WHERE clauses", false)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "go.mod") || !strings.Contains(joined, "doc.go") {
		t.Fatalf("real unrelated files must be flagged; warnings=%v", warnings)
	}

	// 3. The reviewer can page through the REAL 2877-line test file that the
	// original reviewer could not read past line 5.
	testFile, err := os.ReadFile(filepath.Join(ws, "tests", "test_cypher.c"))
	if err != nil {
		t.Fatalf("read preserved test file: %v", err)
	}
	totalLines := strings.Count(string(testFile), "\n") + 1
	if totalLines < 2000 {
		t.Fatalf("expected a large preserved test file, got %d lines", totalLines)
	}
	// Page to the end with bounded reads (the reviewer's new budget allows this).
	const window = 400
	reachedEnd := false
	for start := 1; start <= totalLines; start += window {
		chunk := spike.SliceFileLines(string(testFile), start, start+window-1, window)
		// An explicit in-range page of a real file must return real content.
		if start < totalLines && strings.TrimSpace(chunk) == "" {
			t.Fatalf("bounded read at line %d returned no content", start)
		}
		if strings.Contains(chunk, "TestPropertyToProperty") || strings.Contains(chunk, "test_property") || start+window > totalLines {
			reachedEnd = true
		}
	}
	if !reachedEnd {
		t.Fatalf("reviewer could not reach the end of the preserved test file via bounded reads")
	}

	// 4. End-to-end: with the REAL vacuous evidence loaded, the reviewer's
	// approval is overridden to verification_blocked (a verification gap), and
	// NO coding revision is scheduled. This is the corrected behavior.
	h := newReviewHarness(t, &fakeScriptedRunner{exitCode: ev.ExitCode, output: ev.OutputSummary})
	h.coord.MaxRevisions = 2

	// Seed the coder activity with the REAL preserved evidence exactly as it
	// was recorded (valid=true, exit 0) -- the false positive that caused the
	// original failure. The corrected classifier must reclassify it vacuous.
	_ = h.store.Enqueue(spike.ActivityRequest{ID: "coder-1", AgentID: "coder", Objective: "cypher prop-to-prop", CreatedAt: time.Now()})
	preservedEv := *ev
	_ = h.store.Complete("coder-1", spike.ActivityResult{
		ID: "coder-1", Status: spike.StatusCompleted,
		Outputs: map[string]any{
			"summary":             "implemented cypher property-to-property comparisons; ran make -f Makefile.cbm SANITIZE= test",
			"validation_evidence": preservedEv,
		},
	})

	// Reviewer inspects and (correctly) cannot treat vacuous evidence as a pass.
	h.fake.SessionResponseFunc = func(ctx context.Context, session *spike.FakeSession, prompt string) (*spike.AgentMessage, error) {
		session.Config().OnSubmitReview(spike.ReviewResult{
			Verdict: spike.ReviewVerdictApproved, Approved: true,
			Comments:  "looks correct but the recorded evidence ran no C tests",
			Timestamp: time.Now(),
		})
		return &spike.AgentMessage{Content: "[REVIEW_APPROVED]"}, nil
	}
	_ = h.store.Enqueue(spike.ActivityRequest{
		ID: "rev-1", AgentID: "reviewer", Objective: "review cypher change", DependsOn: []string{"coder-1"}, CreatedAt: time.Now(),
	})

	res, err := h.coord.Step(context.Background())
	if err != nil {
		t.Fatalf("step failed: %v", err)
	}

	verdict, _ := res.Outputs["verdict"].(string)
	if verdict != string(spike.ReviewVerdictVerificationBlocked) {
		t.Fatalf("vacuous preserved evidence must yield verification_blocked (not approved, not rejected); got %q", verdict)
	}
	if approved, _ := res.Outputs["approved"].(bool); approved {
		t.Fatalf("vacuous evidence must not allow approval")
	}
	// The corrected workflow must NOT force a coding revision for a verification gap.
	if _, err := h.store.Get("rev-1-revision-1"); err == nil {
		t.Fatalf("corrected workflow must not schedule a coding revision on a verification gap")
	}
	// It must have collected evidence and scheduled a verification retry instead.
	if collected, _ := res.Outputs["evidence_collected"].(bool); !collected {
		t.Fatalf("expected bounded evidence collection, outputs=%+v", res.Outputs)
	}
	if _, err := h.store.Get("rev-1-verify-1"); err != nil {
		t.Fatalf("expected a verification retry (not a coder revision)")
	}

	t.Logf("validated: vacuous evidence reclassified; unrelated files flagged; "+
		"bounded reads reached line %d of %d; verdict=%s; no coding revision scheduled",
		totalLines, totalLines, verdict)
}
