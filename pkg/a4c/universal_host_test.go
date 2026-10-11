package a4c_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func TestHostContract_CapabilityReporting(t *testing.T) {
	fake := a4c.NewFakeProvider()
	caps := fake.HostCapabilities()
	if caps.HostID != "fake" {
		t.Fatalf("expected host_id fake, got %s", caps.HostID)
	}
	if !caps.SessionInvocation || !caps.WorktreeBinding || !caps.ContextInjection {
		t.Fatalf("expected mandatory capabilities to be true on fake provider")
	}

	// Antigravity capabilities
	agy, err := a4c.NewAntigravityProvider("gemini-3.8-flash-low")
	if err != nil {
		t.Fatalf("failed to create Antigravity provider: %v", err)
	}
	agyCaps := agy.HostCapabilities()
	if agyCaps.HostID != "antigravity" {
		t.Fatalf("expected host_id antigravity, got %s", agyCaps.HostID)
	}
	if !agyCaps.SessionInvocation || !agyCaps.StructuredTelemetry || !agyCaps.SessionResumption {
		t.Fatalf("expected mandatory & resumption capabilities on antigravity provider")
	}
	if agyCaps.NativeSubagents {
		t.Fatalf("expected NativeSubagents to be false (accurately reported limitation)")
	}
	if len(agyCaps.Limitations) == 0 {
		t.Fatalf("expected transparent limitations recorded for antigravity")
	}

	// OpenCode capabilities
	op := a4c.NewOpenCodeProvider("http://127.0.0.1:4096", "opencode-go/mimo-v2.5", nil)
	opCaps := op.HostCapabilities()
	if opCaps.HostID != "opencode" {
		t.Fatalf("expected host_id opencode, got %s", opCaps.HostID)
	}
	if !opCaps.ToolRestrictions || !opCaps.StructuredTelemetry {
		t.Fatalf("expected opencode capabilities to be true")
	}

	// Registry listing
	reg := a4c.NewUniversalHostRegistry()
	reg.Register("fake", fake)
	reg.Register("antigravity", agy)
	reg.Register("opencode", op)

	allCaps := reg.ListCapabilities()
	if len(allCaps) != 3 {
		t.Fatalf("expected 3 registered hosts, got %d", len(allCaps))
	}
}

func TestMixedHostRouting_ActivityLevelHostSelection(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	// Setup fake providers representing 3 distinct hosts
	hostA := a4c.NewFakeProvider()
	hostB := a4c.NewFakeProvider()
	hostC := a4c.NewFakeProvider()

	var invocations []string

	hostA.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		invocations = append(invocations, "hostA:"+session.Config().AgentID)
		return &a4c.AgentMessage{Content: "Architecture design complete"}, nil
	}
	hostB.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		invocations = append(invocations, "hostB:"+session.Config().AgentID)
		wtPath := session.Config().WorkspaceDir
		if wtPath == "" {
			wtPath = h.repoDir
		}
		_ = os.WriteFile(filepath.Join(wtPath, "multi.go"), []byte("package main\nfunc Calc() int { return 42 }\n"), 0644)
		return &a4c.AgentMessage{Content: "Code implemented in multi.go"}, nil
	}
	hostC.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		invocations = append(invocations, "hostC:"+session.Config().AgentID)
		if session.Config().OnSubmitReview != nil {
			session.Config().OnSubmitReview(a4c.ReviewResult{
				Approved:  true,
				Comments:  "Verified multi.go implementation",
				Timestamp: time.Now(),
			})
		}
		return &a4c.AgentMessage{Content: "Review approved [REVIEW_APPROVED]"}, nil
	}

	h.coordinator.RegisterProvider("copilot", hostA)
	h.coordinator.RegisterProvider("opencode", hostB)
	h.coordinator.RegisterProvider("antigravity", hostC)

	// Submit a task with 3 activities configured with different runtimes
	rootReq := a4c.ActivityRequest{
		ID:        "mixed-task-root",
		AgentID:   "lead",
		Objective: "Design architecture on Copilot",
		Runtime:   "copilot",
		Budget:    a4c.Budget{Allocated: 20000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(rootReq)

	coderReq := a4c.ActivityRequest{
		ID:        "mixed-task-impl",
		ParentID:  "mixed-task-root",
		AgentID:   "coder",
		Objective: "Implement code on OpenCode",
		Runtime:   "opencode",
		DependsOn: []string{"mixed-task-root"},
		Budget:    a4c.Budget{Allocated: 30000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(coderReq)

	reviewerReq := a4c.ActivityRequest{
		ID:        "mixed-task-review",
		ParentID:  "mixed-task-root",
		AgentID:   "reviewer",
		Objective: "Review implementation on Antigravity",
		Runtime:   "antigravity",
		DependsOn: []string{"mixed-task-impl"},
		Budget:    a4c.Budget{Allocated: 20000},
		CreatedAt: time.Now(),
	}
	_ = h.store.Enqueue(reviewerReq)

	// Step through all 3 activities
	for i := 0; i < 3; i++ {
		res, err := h.coordinator.Step(ctx)
		if err != nil {
			t.Fatalf("Step %d failed: %v", i+1, err)
		}
		if res == nil {
			t.Fatalf("Step %d returned nil", i+1)
		}
	}

	if len(invocations) != 3 {
		t.Fatalf("expected 3 invocations across hosts, got %d: %v", len(invocations), invocations)
	}

	if invocations[0] != "hostA:lead" {
		t.Errorf("step 1 expected hostA:lead, got %s", invocations[0])
	}
	if invocations[1] != "hostB:coder" {
		t.Errorf("step 2 expected hostB:coder, got %s", invocations[1])
	}
	if invocations[2] != "hostC:reviewer" {
		t.Errorf("step 3 expected hostC:reviewer, got %s", invocations[2])
	}

	// Verify durable state preserves task and worktree integrity across hosts
	rec, err := h.store.Get("mixed-task-impl")
	if err != nil || rec.Status != a4c.StatusCompleted {
		t.Fatalf("expected impl activity to be completed")
	}
}

func TestCrossHostContinuation_CheckpointPreservation(t *testing.T) {
	h := setupInvisibleTeamHarness(t)
	ctx := context.Background()

	// 1. Start task on Host A (Copilot)
	ref, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Implement distributed lock",
		Strategy:  a4c.StrategyDirect,
		Runtime:   "copilot",
		Model:     "claude-opus-4.8",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Create partial change in worktree
	wt, _ := h.wtMgr.GetWorktree(ref.TaskID)
	wtPath := h.repoDir
	if wt != nil {
		wtPath = wt.WorktreePath
	}
	_ = os.WriteFile(filepath.Join(wtPath, "lock.go"), []byte("package main\n// partial implementation\n"), 0644)
	runGitCmd(t, wtPath, "add", "lock.go")

	// 2. Interrupt work and capture checkpoint
	chk, err := h.service.CaptureCheckpoint(ctx, ref.TaskID)
	if err != nil {
		t.Fatalf("CaptureCheckpoint failed: %v", err)
	}

	if chk.TaskID != ref.TaskID {
		t.Errorf("checkpoint TaskID mismatch: %s vs %s", chk.TaskID, ref.TaskID)
	}
	if chk.SourceRuntime != "copilot" {
		t.Errorf("expected source runtime copilot, got %s", chk.SourceRuntime)
	}
	if !strings.Contains(chk.GitStatusSummary, "lock.go") {
		t.Errorf("expected git status to capture partial lock.go, got %s", chk.GitStatusSummary)
	}

	// 3. Resume on Host B (OpenCode)
	resumedChk, err := h.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:       ref.TaskID,
		Runtime:      "opencode",
		Model:        "opencode-go/mimo-v2.5",
		Instructions: "Finish lock implementation and add tests",
	})
	if err != nil {
		t.Fatalf("ResumeTaskOnHost failed: %v", err)
	}

	if resumedChk.TargetRuntime != "opencode" {
		t.Errorf("expected target runtime opencode, got %s", resumedChk.TargetRuntime)
	}
	if resumedChk.Model != "opencode-go/mimo-v2.5" {
		t.Errorf("expected model mimo-v2.5, got %s", resumedChk.Model)
	}

	// 4. Verify continuation prompt is generated with checkpoint context and without chat transcript replay
	artifact := a4c.HandoffArtifact{
		Objective:          chk.Objective,
		Role:               "coder (specialist)",
		AgentID:            "coder",
		Strategy:           a4c.StrategyDirect,
		ActiveWorkspace:    wtPath,
		Checkpoint:         resumedChk,
	}
	prompt := h.coordinator.Handoff.BuildPrompt(artifact)

	if !strings.Contains(prompt, "CROSS-HOST CONTINUATION CHECKPOINT") {
		t.Errorf("expected continuation header in prompt")
	}
	if !strings.Contains(prompt, "lock.go") {
		t.Errorf("expected lock.go in continuation prompt")
	}
	if !strings.Contains(prompt, "Finish lock implementation") {
		t.Errorf("expected continuation instructions in prompt")
	}
}
