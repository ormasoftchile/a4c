package a4c

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type TestRunner interface {
	RunTests(ctx context.Context, workspaceDir string) (exitCode int, output string, err error)
}

type NativeGoTestRunner struct{}

func (r *NativeGoTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "go", "test", "-short", "./...")
	cmd.Dir = workspaceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), string(out), nil
		}
		return -1, string(out), err
	}
	return 0, string(out), nil
}

// goPackageResult matches a per-package `go test` result line:
// "ok   \tpkg\t0.1s", "FAIL\tpkg\t0.1s", or "?   \tpkg\t[no test files]".
var goPackageResult = regexp.MustCompile(`(?m)^(ok|FAIL|\?)\s`)

// goTestCase matches a verbose per-test line: "--- PASS: TestFoo (0.00s)".
var goTestCase = regexp.MustCompile(`(?m)^\s*--- (PASS|FAIL|SKIP): `)

// AnalyzeGoTestOutput reports how many tests a `go test` invocation actually
// executed and whether the run was vacuous. Vacuous means the command ran but
// exercised no tests, so it proves nothing about a change and must never be
// treated as VALID (it would convert "no tests ran" into a false pass).
//
// Detection is deliberately conservative so custom/non-Go runners that emit an
// opaque but non-empty "PASS" message are NOT misclassified as vacuous. A run
// is vacuous only when every Go package reported "[no test files]" or the
// command produced no output at all.
func AnalyzeGoTestOutput(output string) (testsExecuted int, vacuous bool) {
	// Prefer precise per-test counts when verbose lines are present.
	if n := len(goTestCase.FindAllString(output, -1)); n > 0 {
		return n, false
	}
	// Count packages that actually ran tests (ok/FAIL) vs "[no test files]" (?).
	ran, noTestFiles := 0, 0
	for _, m := range goPackageResult.FindAllStringSubmatch(output, -1) {
		switch m[1] {
		case "ok", "FAIL":
			ran++
		case "?":
			noTestFiles++
		}
	}
	if ran > 0 {
		return ran, false
	}
	// No package ran tests. Vacuous only for an explicit `go test` "no test
	// files" result or completely empty output -- not for opaque custom output.
	if noTestFiles > 0 {
		return 0, true
	}
	if strings.TrimSpace(output) == "" {
		return 0, true
	}
	// Non-empty output with no Go structure: non-vacuous but uncounted.
	return 0, false
}

// WriteDurableTestLog persists full test output to a durable, reviewer-readable
// artifact under <workspace>/.a4c/evidence/ and returns its relative path.
// The full log is what a reviewer inspects; only a summary is embedded in
// prompts. Returns "" when the log cannot be written.
func WriteDurableTestLog(workspaceDir, evidenceID, output string) string {
	if workspaceDir == "" || evidenceID == "" {
		return ""
	}
	dir := filepath.Join(workspaceDir, ".a4c", "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	safeID := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, evidenceID)
	path := filepath.Join(dir, safeID+".log")
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		return ""
	}
	return filepath.Join(".a4c", "evidence", safeID+".log")
}

type GovernedExecutionCoordinator struct {
	Store       *DurableActivityStore
	Agent       *ProgrammerAgent
	Snapshotter *WorkspaceSnapshotter
	TestRunner  TestRunner
	MaxRepair   int
}

func NewGovernedCoordinator(store *DurableActivityStore, agent *ProgrammerAgent, runner TestRunner) *GovernedExecutionCoordinator {
	return &GovernedExecutionCoordinator{
		Store:       store,
		Agent:       agent,
		Snapshotter: NewWorkspaceSnapshotter(),
		TestRunner:  runner,
		MaxRepair:   3,
	}
}

// ExecuteNext processes one pending activity through the native verification lifecycle.
func (c *GovernedExecutionCoordinator) ExecuteNext(ctx context.Context) (*ActivityResult, error) {
	req, err := c.Store.ClaimNext()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, nil // No pending work
	}

	// 1. Execute agent activity (patching, reading code)
	res, err := c.Agent.ExecuteActivity(ctx, *req)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return res, err
	}

	// 2. Compute pre-test workspace snapshot
	beforeSnapshot, err := c.Snapshotter.ComputeSnapshot(c.Agent.WorkspaceDir)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	// 3. Execute native tests
	exitCode, testOut, err := c.TestRunner.RunTests(ctx, c.Agent.WorkspaceDir)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	// 4. Compute post-test workspace snapshot (confirm zero concurrent edits)
	afterSnapshot, err := c.Snapshotter.ComputeSnapshot(c.Agent.WorkspaceDir)
	if err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	if err := c.Snapshotter.ValidateIntegrity(beforeSnapshot, afterSnapshot); err != nil {
		_ = c.Store.Fail(req.ID, err)
		return nil, err
	}

	// 5. If tests failed, enter bounded repair loop
	if exitCode != 0 {
		repaired := false
		repairAttempts := 0

		for repairAttempts < c.MaxRepair {
			repairAttempts++
			repairReq := ActivityRequest{
				ID:        fmt.Sprintf("%s-repair-%d", req.ID, repairAttempts),
				ParentID:  req.ID,
				AgentID:   c.Agent.ID,
				Objective: "Fix failing tests based on compiler/test output",
				Inputs: map[string]any{
					"error_output": testOut,
					"attempt":      repairAttempts,
				},
				Budget: Budget{
					Allocated: req.Budget.Available(),
				},
				CreatedAt: time.Now(),
			}

			_, rErr := c.Agent.ExecuteActivity(ctx, repairReq)
			if rErr != nil {
				break
			}

			// Re-test natively! Never trust agent self-report.
			preRepair, _ := c.Snapshotter.ComputeSnapshot(c.Agent.WorkspaceDir)
			exitCode, testOut, _ = c.TestRunner.RunTests(ctx, c.Agent.WorkspaceDir)
			postRepair, _ := c.Snapshotter.ComputeSnapshot(c.Agent.WorkspaceDir)
			if err := c.Snapshotter.ValidateIntegrity(preRepair, postRepair); err != nil {
				break
			}

			if exitCode == 0 {
				repaired = true
				res.Outputs["repaired"] = true
				res.Outputs["repair_attempts"] = repairAttempts
				afterSnapshot = postRepair
				break
			}
		}

		if !repaired {
			failErr := fmt.Errorf("tests failed with exit code %d after %d repair attempts: %s", exitCode, repairAttempts, testOut)
			_ = c.Store.Fail(req.ID, failErr)
			return nil, failErr
		}
	}

	now := time.Now()
	evidence := ValidationEvidence{
		EvidenceID:          fmt.Sprintf("evi-%s-%d", req.ID, now.UnixNano()),
		ActivityID:          req.ID,
		WorkspaceDir:        c.Agent.WorkspaceDir,
		WorkspaceSnapshotID: afterSnapshot,
		Command:             []string{"go", "test", "-short", "./..."},
		ExitCode:            0,
		Valid:               true,
		StartedAt:           now,
		CompletedAt:         now,
	}
	res.EvidenceIDs = append(res.EvidenceIDs, evidence.EvidenceID)
	res.Outputs["snapshot_id"] = afterSnapshot
	res.Outputs["validation_evidence"] = evidence

	// 7. Commit completed result to durable store
	if err := c.Store.Complete(req.ID, *res); err != nil {
		return nil, err
	}

	return res, nil
}
