package a4c

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireMake skips a preflight that executes a Makefile-based verification
// contract on hosts without make installed (commonly the case on Windows).
func requireMake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available on this host, skipping Makefile verification preflight: %v", err)
	}
}

// preflightRepo creates a committed git fixture.
func preflightRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
	run("add", "-A")
	run("commit", "-qm", "baseline")
}

// TestIntegrationPreflight_FullChain is the mandatory integration preflight
// (Section 10). It exercises the real runtime components end to end against a
// small cross-language fixture and replays the preserved benchmark failure:
//
//	coder completion -> project verification -> evidence persistence ->
//	reviewer workspace -> evidence inspection -> tool-boundary verdict ->
//	task transition, plus diff hygiene and .a4c isolation.
func TestIntegrationPreflight_FullChain(t *testing.T) {
	requireMake(t)
	// ---- Fixture: a C-style (Makefile) project that ALSO has a Go module, so
	// the trust rule (baseline-locked verification) is exercised. ----
	producer := t.TempDir()
	preflightRepo(t, producer, map[string]string{
		"Makefile":            "test:\n\t@echo '=== RUN TestCypher'\n\t@echo '--- PASS: TestCypher (0.00s)'\n\t@echo 'All tests passed'\n",
		"src.c":               "int main(){return 0;}\n",
		"tests/test_cypher.c": "void TestPropertyToProperty(void) {}\nvoid TestInequality(void) {}\n",
	})
	// Coder (adversarially) adds an uncommitted go.mod + dummy Go test.
	_ = os.WriteFile(filepath.Join(producer, "go.mod"), []byte("module evil\n\ngo 1.22\n"), 0o644)
	_ = os.WriteFile(filepath.Join(producer, "dummy_test.go"), []byte("package evil\n\nimport \"testing\"\n\nfunc TestDummy(t *testing.T) {}\n"), 0o644)

	// ---- 1. Project verification runs the CORRECT project test command ----
	runner := NewProjectTestRunner()
	res, err := runner.Verify(context.Background(), producer)
	if err != nil {
		t.Fatalf("preflight: verify failed: %v", err)
	}
	if res.Unavailable || len(res.Command) == 0 || res.Command[0] != "make" {
		t.Fatalf("PREFLIGHT FAIL: verifier did not run the configured C test command (got %v unavailable=%t)", res.Command, res.Unavailable)
	}
	if res.Vacuous || res.TestsExecuted == 0 {
		t.Fatalf("PREFLIGHT FAIL: verification vacuous or zero tests: %s", res.Output)
	}
	if res.SourceRevision == "" {
		t.Fatalf("PREFLIGHT FAIL: evidence does not bind to a source revision")
	}

	// ---- 2. Evidence corresponds to the candidate code + durable log ----
	evidenceID := "evi-preflight-1"
	logRel := WriteDurableTestLog(producer, evidenceID, res.Output)
	if logRel == "" {
		t.Fatalf("PREFLIGHT FAIL: durable log not written")
	}
	evidence := ValidationEvidence{
		EvidenceID: evidenceID, Command: res.Command, ExitCode: res.ExitCode,
		Valid: !res.Vacuous && !res.Unavailable, TestsExecuted: res.TestsExecuted,
		Vacuous: res.Vacuous, SourceRevision: res.SourceRevision,
		LogPath: logRel, WorkspaceDir: producer, OutputSummary: truncateSummary(res.Output, 200),
		WorkspaceSnapshotID: "preflight-snap", CompletedAt: time.Now(),
	}

	// ---- 3. Reviewer workspace: a disposable copy of the producer EXCLUDING
	// .a4c (mirrors createDisposableReviewWorkspace), on a separate filesystem.
	reviewer := t.TempDir()
	for _, rel := range []string{"src.c", "tests/test_cypher.c", "Makefile"} {
		data, rerr := os.ReadFile(filepath.Join(producer, rel))
		if rerr != nil {
			continue
		}
		dst := filepath.Join(reviewer, rel)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.WriteFile(dst, data, 0o644)
	}
	c := &TeamCoordinator{}
	bundle := c.materializeReviewEvidence(reviewer, &ActivityRequest{Objective: "obj"},
		"diff --git a/src.c b/src.c\n+int main(){return 0;}",
		[]string{"src.c", "tests/test_cypher.c"}, nil, nil, &evidence, "VALID")

	// ---- 4. Reviewer can ACTUALLY read the evidence log (historical failure) ----
	if bundle == nil || !bundle.LogAvailable {
		t.Fatalf("PREFLIGHT FAIL: reviewer cannot read the verification log (the historical defect). missing=%v", bundle)
	}
	logData, err := os.ReadFile(filepath.Join(reviewer, bundle.LogPath))
	if err != nil {
		t.Fatalf("PREFLIGHT FAIL: reviewer read of verification log failed: %v", err)
	}
	if !strings.Contains(string(logData), "TestCypher") {
		t.Fatalf("PREFLIGHT FAIL: reviewer log missing test output")
	}
	// Changed-files index lists test names so the reviewer need not page files.
	filesData, _ := os.ReadFile(filepath.Join(reviewer, bundle.FilesPath))
	if !strings.Contains(string(filesData), "TestPropertyToProperty") {
		t.Fatalf("PREFLIGHT FAIL: changed-files index does not list test names: %s", filesData)
	}

	// ---- 5. Tool boundary: verdict submission works after exploration cap ----
	enforcer := newToolBudgetEnforcer(2, nil)
	for i := 0; i < 2; i++ {
		if err := enforcer.check("governed_read_file"); err != nil {
			t.Fatalf("PREFLIGHT FAIL: exploration %d: %v", i, err)
		}
	}
	if err := enforcer.check("governed_read_file"); err == nil {
		t.Fatalf("PREFLIGHT FAIL: expected exploration cap to bind")
	}
	// Terminal verdict submission must still succeed.
	if err := enforcer.check("governed_submit_review"); err != nil {
		t.Fatalf("PREFLIGHT FAIL: submit_review blocked after exploration exhaustion: %v", err)
	}

	// ---- 6. Diff hygiene flags the planted unrelated go.mod ----
	warnings := FlagDiffHygiene([]string{"src.c", "tests/test_cypher.c", "go.mod", "dummy_test.go"}, "implement feature", false)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "go.mod") {
		t.Fatalf("PREFLIGHT FAIL: unrelated go.mod not flagged: %v", warnings)
	}

	// ---- 7. A4C operational state never enters the deliverable ----
	if isA4CInternal(logRel) == false {
		t.Fatalf("PREFLIGHT FAIL: durable log path should be A4C-internal")
	}
	if got := excludeA4CInternal([]string{"src.c", ".a4c/evidence/x.log"}); len(got) != 1 || got[0] != "src.c" {
		t.Fatalf("PREFLIGHT FAIL: .a4c not excluded from deliverable: %v", got)
	}

	t.Logf("PREFLIGHT PASS: command=%v tests=%d rev=%s reviewer_log=%s hygiene=%d",
		res.Command, res.TestsExecuted, res.SourceRevision[:8], bundle.LogPath, len(warnings))
}

// TestIntegrationPreflight_ReplayPreservedBenchmarkFailure replays the exact
// evidence from the failed 2026-10-15 reviewfix benchmark and confirms the
// original defects do not recur: the vacuous `go test` "[no test files]"
// evidence is classified as a verification gap (never a pass), and a dummy
// go.mod cannot flip a C contract.
func TestIntegrationPreflight_ReplayPreservedBenchmarkFailure(t *testing.T) {
	requireMake(t)
	// The preserved coder evidence was: `go test -short ./...` exit 0, output
	// "[no test files]", previously recorded valid=true (a false pass).
	preservedOutput := "?   \tgithub.com/DeusData/codebase-memory-mcp\t[no test files]\n"
	testsExecuted, vacuous := AnalyzeGoTestOutput(preservedOutput)
	if !vacuous || testsExecuted != 0 {
		t.Fatalf("REPLAY FAIL: preserved [no test files] evidence must be vacuous, got n=%d vac=%t", testsExecuted, vacuous)
	}

	// Replay the gaming: C repo baseline + coder-added go.mod must stay on C.
	repo := t.TempDir()
	preflightRepo(t, repo, map[string]string{
		"Makefile.cbm": "test:\n\t@echo 'All tests passed'\n",
		"cypher.c":     "int x;\n",
	})
	_ = os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module evil\n\ngo 1.22\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "go_harness_test.go"), []byte("package evil\n\nimport \"testing\"\n\nfunc TestRootGoHarness(t *testing.T) {}\n"), 0o644)
	runner := NewProjectTestRunner()
	res, err := runner.Verify(context.Background(), repo)
	if err != nil {
		t.Fatalf("REPLAY FAIL: verify: %v", err)
	}
	if len(res.Command) == 0 || res.Command[0] != "make" {
		t.Fatalf("REPLAY FAIL: coder go.mod/go_harness_test.go flipped C verification to %v", res.Command)
	}
}
