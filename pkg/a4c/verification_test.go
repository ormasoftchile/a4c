package a4c_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	spike "github.com/ormasoftchile/a4c/pkg/a4c"
)

// initGitRepo creates a git repo at dir with the given committed files.
func initGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "baseline")
}

// TestVerification_CRepoExecutesConfiguredCTests: a repository whose committed
// baseline is a Makefile project resolves to and runs the make test command.
func TestVerification_CRepoExecutesConfiguredCTests(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"Makefile": "test:\n\t@echo 'PASS: suite_cypher ok'\n\t@echo 'All tests passed'\n",
		"main.c":   "int main(){return 0;}\n",
	})
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Unavailable {
		t.Fatalf("verification should not be unavailable for a Makefile project")
	}
	if len(res.Command) == 0 || res.Command[0] != "make" {
		t.Fatalf("expected a make command, got %v", res.Command)
	}
	if res.ExitCode != 0 || res.Failed {
		t.Fatalf("expected passing make, got exit=%d failed=%t out=%s", res.ExitCode, res.Failed, res.Output)
	}
	if res.Vacuous {
		t.Fatalf("make test that echoed PASS must not be vacuous: %s", res.Output)
	}
	if res.TestsExecuted == 0 {
		t.Fatalf("expected tests to be counted as executed")
	}
}

// TestVerification_GoRepoExecutesGoTests preserves valid Go testing behavior.
func TestVerification_GoRepoExecutesGoTests(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"go.mod":    "module example.com/x\n\ngo 1.22\n",
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n",
	})
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(res.Command) == 0 || res.Command[0] != "go" {
		t.Fatalf("expected a go test command, got %v", res.Command)
	}
	if res.ExitCode != 0 {
		t.Fatalf("go test should pass, got exit=%d out=%s", res.ExitCode, res.Output)
	}
	if res.Vacuous {
		t.Fatalf("a Go repo with a real test must not be vacuous: %s", res.Output)
	}
}

// TestVerification_DummyGoModCannotFlipCVerification is the trust rule: a coder
// adding an uncommitted go.mod (and a dummy Go test) to a C repository must NOT
// change the configured C verification contract into a Go contract.
func TestVerification_DummyGoModCannotFlipCVerification(t *testing.T) {
	dir := t.TempDir()
	// Committed baseline is a Makefile (C-style) project.
	initGitRepo(t, dir, map[string]string{
		"Makefile": "test:\n\t@echo 'PASS: c suite ok'\n\t@echo 'All tests passed'\n",
		"src.c":    "int main(){return 0;}\n",
	})
	// Coder adds go.mod + a dummy Go test (uncommitted — not part of baseline).
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module evil\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dummy_test.go"), []byte("package evil\n\nimport \"testing\"\n\nfunc TestDummy(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(res.Command) == 0 || res.Command[0] != "make" {
		t.Fatalf("CODER GAMING DETECTED: adding go.mod flipped C verification to %v", res.Command)
	}
	if strings.Contains(strings.Join(res.Command, " "), "go test") {
		t.Fatalf("verification must remain the make contract, got %v", res.Command)
	}
}

// TestVerification_NoTestFilesIsNotAPass: `go test` reporting "[no test files]"
// is vacuous and must never be treated as a meaningful passing suite.
func TestVerification_NoTestFilesIsNotAPass(t *testing.T) {
	testsExecuted, vacuous := spike.AnalyzeGoTestOutput("?   \tgithub.com/x\t[no test files]\n")
	if !vacuous || testsExecuted != 0 {
		t.Fatalf("[no test files] must be vacuous with 0 tests, got n=%d vacuous=%t", testsExecuted, vacuous)
	}
}

// TestVerification_FailedTestsProduceFailureEvidence: a non-zero exit is a
// genuine failure, not vacuous.
func TestVerification_FailedTestsProduceFailureEvidence(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"Makefile": "test:\n\t@echo 'FAIL: suite_cypher not ok'\n\t@exit 1\n",
	})
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.Failed || res.ExitCode == 0 {
		t.Fatalf("expected a genuine failure, got exit=%d failed=%t", res.ExitCode, res.Failed)
	}
	if res.Vacuous {
		t.Fatalf("a failing run is not vacuous")
	}
}

// TestVerification_MissingConfigIsExplicit: a repo with no recognizable test
// contract yields an explicit unavailable result, never invented evidence.
func TestVerification_MissingConfigIsExplicit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{"readme.txt": "no build system\n"})
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify should not hard-error: %v", err)
	}
	if !res.Unavailable {
		t.Fatalf("expected unavailable for a repo with no test contract, got command=%v", res.Command)
	}
	if res.ExitCode != 0 || !res.Vacuous {
		t.Fatalf("unavailable must be vacuous with non-zero exit marker")
	}
}

// TestVerification_AuthoredConfigOverridesDetection: .a4c/verify.json is
// trusted configuration and takes precedence over detection.
func TestVerification_AuthoredConfigOverridesDetection(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"go.mod":    "module example.com/x\n\ngo 1.22\n", // detection would pick go
		"Makefile":  "mytest:\n\t@echo 'PASS: custom ok'\n\t@echo 'All tests passed'\n",
		"x_test.go": "package x\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n",
	})
	// Authored trusted config selects a custom make target.
	if err := os.MkdirAll(filepath.Join(dir, ".a4c"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"command":["make","mytest"],"kind":"make"}`
	if err := os.WriteFile(filepath.Join(dir, ".a4c", "verify.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(res.Command) < 2 || res.Command[1] != "mytest" {
		t.Fatalf("authored config must override detection, got %v", res.Command)
	}
}

// TestVerification_EvidenceBindsToSourceRevision: recorded verification
// identifies the git revision it validated.
func TestVerification_EvidenceBindsToSourceRevision(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"Makefile": "test:\n\t@echo 'PASS: ok'\n\t@echo 'All tests passed'\n",
	})
	r := spike.NewProjectTestRunner()
	res, err := r.Verify(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.SourceRevision == "" {
		t.Fatalf("verification must record the source revision it validated")
	}
	// HEAD sha
	out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(out)) != res.SourceRevision {
		t.Fatalf("source revision %q does not match HEAD %q", res.SourceRevision, strings.TrimSpace(string(out)))
	}
}

// TestVerification_UnavailableRunnerRunTestsContract: the legacy RunTests
// surface still works for the project runner.
func TestVerification_UnavailableRunnerRunTestsContract(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"Makefile": "test:\n\t@echo 'PASS: ok'\n\t@echo 'All tests passed'\n",
	})
	r := spike.NewProjectTestRunner()
	exit, out, err := r.RunTests(context.Background(), dir)
	if err != nil {
		t.Fatalf("RunTests: %v", err)
	}
	if exit != 0 {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if !strings.Contains(out, "PASS") {
		t.Fatalf("expected output to contain PASS, got %q", out)
	}
	_ = time.Now
}
