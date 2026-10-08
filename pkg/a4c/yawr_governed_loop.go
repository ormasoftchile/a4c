package a4c

import (
	"context"
	"fmt"
	"os/exec"
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

type GovernedExecutionCoordinator struct {
	Store        *DurableActivityStore
	Agent        *ProgrammerAgent
	Snapshotter  *WorkspaceSnapshotter
	TestRunner   TestRunner
	MaxRepair    int
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

// ExecuteNext processes one pending activity through the full governed lifecycle.
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

	// 3. Execute native tests (Zero LLM hallucination)
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

	// 6. Record verified validation evidence
	evidence := ValidationEvidence{
		EvidenceID:          fmt.Sprintf("evi-%s-%d", req.ID, time.Now().Unix()),
		WorkspaceSnapshotID: afterSnapshot,
		Command:             []string{"go", "test", "./..."},
		ExitCode:            0,
		Valid:               true,
	}
	res.EvidenceIDs = append(res.EvidenceIDs, evidence.EvidenceID)
	res.Outputs["snapshot_id"] = afterSnapshot

	// 7. Commit completed result to durable store
	if err := c.Store.Complete(req.ID, *res); err != nil {
		return nil, err
	}

	return res, nil
}
