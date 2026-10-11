package a4c_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

type fakePassTestRunner struct{}

func (r *fakePassTestRunner) RunTests(ctx context.Context, workspaceDir string) (int, string, error) {
	return 0, "PASS: All tests passed", nil
}

// TestProviderFailureClassification tests accurate categorization of provider errors vs ordinary failures.
func TestProviderFailureClassification(t *testing.T) {
	testCases := []struct {
		name         string
		hostID       string
		err          error
		stderr       string
		expectedKind a4c.ProviderFailureKind
		expectError  bool
	}{
		{
			name:         "Quota exhaustion in stderr",
			hostID:       "antigravity",
			err:          errors.New("exit status 1"),
			stderr:       "ERROR: resource has been exhausted: quota limit reached for gemini-pro",
			expectedKind: a4c.ProviderFailureQuotaExhausted,
			expectError:  true,
		},
		{
			name:         "Rate limit 429",
			hostID:       "opencode",
			err:          errors.New("HTTP 429: too many requests"),
			stderr:       "rate limit exceeded, please retry after 60s",
			expectedKind: a4c.ProviderFailureRateLimited,
			expectError:  true,
		},
		{
			name:         "Auth invalid 401",
			hostID:       "copilot",
			err:          errors.New("HTTP 401 Unauthorized"),
			stderr:       "authentication failed: invalid GitHub token",
			expectedKind: a4c.ProviderFailureAuthInvalid,
			expectError:  true,
		},
		{
			name:         "Temporary outage 503",
			hostID:       "antigravity",
			err:          errors.New("server unavailable"),
			stderr:       "503 Service Unavailable: upstream connection reset",
			expectedKind: a4c.ProviderFailureTemporaryOutage,
			expectError:  true,
		},
		{
			name:         "Process killed / SIGTERM",
			hostID:       "copilot",
			err:          errors.New("signal: killed"),
			stderr:       "process received SIGKILL",
			expectedKind: a4c.ProviderFailureProcessInterrupted,
			expectError:  true,
		},
		{
			name:         "Sentinel ErrQuotaExhausted",
			hostID:       "antigravity",
			err:          a4c.ErrQuotaExhausted,
			stderr:       "",
			expectedKind: a4c.ProviderFailureQuotaExhausted,
			expectError:  true,
		},
		{
			name:         "Ordinary compiler error (NOT provider failure)",
			hostID:       "antigravity",
			err:          errors.New("exit status 2"),
			stderr:       "./main.go:42:10: syntax error: unexpected newline, expecting comma or )",
			expectedKind: "",
			expectError:  false,
		},
		{
			name:         "Ordinary test failure (NOT provider failure)",
			hostID:       "opencode",
			err:          errors.New("exit status 1"),
			stderr:       "--- FAIL: TestCalculation (0.01s)\n    calc_test.go:15: expected 42, got 40\nFAIL",
			expectedKind: "",
			expectError:  false,
		},
		{
			name:         "Internal budget limit reached (NOT provider failure)",
			hostID:       "copilot",
			err:          errors.New("token budget exhausted for subtask: max 50000"),
			stderr:       "",
			expectedKind: "",
			expectError:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pErr := a4c.ClassifyProviderError(tc.hostID, "default-model", tc.err, tc.stderr)
			if tc.expectError {
				if pErr == nil {
					t.Fatalf("expected provider error for %s, got nil", tc.name)
				}
				if pErr.Kind != tc.expectedKind {
					t.Fatalf("expected kind %q, got %q", tc.expectedKind, pErr.Kind)
				}
				if pErr.HostID != tc.hostID {
					t.Fatalf("expected host %q, got %q", tc.hostID, pErr.HostID)
				}
				// Verify AsProviderFailure unwraps cleanly
				unwrapped, ok := a4c.AsProviderFailure(pErr)
				if !ok || unwrapped == nil {
					t.Fatalf("AsProviderFailure failed to detect provider error")
				}
			} else {
				if pErr != nil {
					t.Fatalf("expected nil provider error for %s, got %+v", tc.name, pErr)
				}
			}
		})
	}
}

// TestContinuationPromptFormat verifies that BuildContinuationPrompt distinguishes already done vs still needed.
func TestContinuationPromptFormat(t *testing.T) {
	task := a4c.TaskCheckpoint{
		TaskID:              "task-test-01",
		Objective:           "Implement durable metrics collector",
		Strategy:            a4c.StrategyDirect,
		WorktreeBranch:      "a4c/task-test-01",
		SourceRuntime:       "antigravity",
		TargetRuntime:       "copilot",
		ModifiedFiles:       []string{"pkg/metrics/collector.go"},
		NewFiles:            []string{"pkg/metrics/types.go"},
		CompletedWork:       []string{"Implemented MetricPoint and MetricBatch structs"},
		AcceptedDecisions:   []string{"Use in-memory circular ring buffer"},
		InterruptionKind:    a4c.ProviderFailureQuotaExhausted,
		InterruptionMessage: "Quota window exhausted on antigravity",
		RemainingObligations: []string{"Complete collector.go flush loop and verify tests"},
		ContinuationInstructions: "Please finish collector.go and add tests in collector_test.go",
		ValidationEvidence: &a4c.ValidationEvidence{
			EvidenceID: "val-prior-001",
			Command:    []string{"go", "test", "-short", "./pkg/metrics/..."},
			ExitCode:   0,
			Valid:      true,
		},
	}

	prompt := a4c.BuildContinuationPrompt(task)

	// Check for key structural headers
	if !strings.Contains(prompt, "CROSS-HOST CONTINUATION CHECKPOINT") {
		t.Fatalf("missing continuation header in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Task Continuation Checkpoint") {
		t.Fatalf("missing task continuation label in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[ALREADY DONE]") {
		t.Fatalf("missing ALREADY DONE header in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[STILL NEEDED]") {
		t.Fatalf("missing STILL NEEDED header in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "pkg/metrics/collector.go") {
		t.Fatalf("missing modified file in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "pkg/metrics/types.go") {
		t.Fatalf("missing new file in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "val-prior-001") {
		t.Fatalf("missing prior evidence in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Please finish collector.go and add tests in collector_test.go") {
		t.Fatalf("missing continuation instructions in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Build incrementally upon the existing changes") {
		t.Fatalf("missing incremental continuation directive in prompt:\n%s", prompt)
	}
}

// TestScenarioA_ManagedTask_ProviderFailure verifies failover when a provider runs out of quota.
func TestScenarioA_ManagedTask_ProviderFailure(t *testing.T) {
	h := setupServiceTestHarness(t)
	h.coordinator.TestRunner = &fakePassTestRunner{}
	ctx := context.Background()

	// Register two hosts: Host A (which fails with quota exhaustion) and Host B (which succeeds)
	fakeA := a4c.NewFakeProvider()
	fakeA.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		// Simulate partial work followed by quota exhaustion
		return nil, &a4c.ProviderError{
			Kind:    a4c.ProviderFailureQuotaExhausted,
			HostID:  "antigravity",
			Model:   "gemini-pro",
			Message: "five-hour usage window exhausted",
		}
	})

	hostBExecuted := false
	fakeB := a4c.NewFakeProvider()
	fakeB.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		hostBExecuted = true
		return &a4c.AgentMessage{
			Role:      "assistant",
			Content:   "Implemented remaining logic and validated all tests.",
			Timestamp: time.Now(),
		}, nil
	})

	h.coordinator.RegisterProvider("antigravity", fakeA)
	h.coordinator.RegisterProvider("copilot", fakeB)

	// Step 1: Submit task targeted to Host A
	taskRes, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Add rate-limiting token bucket",
		Strategy:  a4c.StrategyDirect,
		Runtime:   "antigravity",
		Model:     "gemini-pro",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Step 2: Execute task on Host A -> should fail with ProviderError
	_, execErr := h.service.ExecuteTask(ctx, taskRes.TaskID, 5)
	if execErr == nil {
		t.Fatalf("expected provider failure on Host A, got nil")
	}

	provErr, isProv := a4c.AsProviderFailure(execErr)
	if !isProv {
		t.Fatalf("expected error to be ProviderError, got %v", execErr)
	}
	if provErr.Kind != a4c.ProviderFailureQuotaExhausted {
		t.Fatalf("expected QuotaExhausted, got %s", provErr.Kind)
	}

	// Step 3: Verify task state is interrupted, NOT failed or deleted
	taskDetails, err := h.service.GetTask(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskDetails.State != a4c.TaskStateInterrupted {
		t.Fatalf("expected task state %q, got %q", a4c.TaskStateInterrupted, taskDetails.State)
	}

	// Verify checkpoint captures interruption reason
	ckpt, err := h.service.CaptureCheckpoint(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("CaptureCheckpoint failed: %v", err)
	}
	if ckpt.InterruptionKind != a4c.ProviderFailureQuotaExhausted {
		t.Fatalf("expected checkpoint interruption kind %s, got %s", a4c.ProviderFailureQuotaExhausted, ckpt.InterruptionKind)
	}

	// Step 4: Discover alternative hosts
	alternatives, err := h.service.DiscoverAlternativeHosts(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("DiscoverAlternativeHosts failed: %v", err)
	}
	if len(alternatives) == 0 {
		t.Fatalf("expected at least 1 alternative host, got 0")
	}
	foundCopilot := false
	for _, alt := range alternatives {
		if alt.HostID == "copilot" {
			foundCopilot = true
		}
	}
	if !foundCopilot {
		t.Fatalf("expected copilot in alternative hosts: %+v", alternatives)
	}

	// Step 5: Resume task on Host B
	resumeRes, err := h.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:  taskRes.TaskID,
		Runtime: "copilot",
		Model:   "claude-3.5-sonnet",
	})
	if err != nil {
		t.Fatalf("ResumeTaskOnHost failed: %v", err)
	}
	if resumeRes.TargetRuntime != "copilot" {
		t.Fatalf("expected target runtime 'copilot', got %s", resumeRes.TargetRuntime)
	}

	// Step 6: Execute resumed task on Host B
	execResults, err := h.service.ExecuteTask(ctx, taskRes.TaskID, 5)
	if err != nil {
		t.Fatalf("ExecuteTask on Host B failed: %v", err)
	}
	if !hostBExecuted {
		t.Fatalf("Host B was not executed")
	}
	if len(execResults) == 0 {
		t.Fatalf("expected execution results, got 0")
	}

	// Verify task is now awaiting human review
	updatedTask, err := h.service.GetTask(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if updatedTask.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected task state awaiting review, got %s", updatedTask.State)
	}
}

// TestScenarioB_ExternalWork_Adoption verifies adopting ongoing external work into A4C.
func TestScenarioB_ExternalWork_Adoption(t *testing.T) {
	h := setupServiceTestHarness(t)
	h.coordinator.TestRunner = &fakePassTestRunner{}
	ctx := context.Background()

	// Simulate developer working directly in workspace before using A4C:
	// Create a new source file and a partially modified file
	pkgDir := filepath.Join(h.repoDir, "pkg", "auth")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	partialFile := filepath.Join(pkgDir, "session.go")
	initialContent := []byte("// Session manager\npackage auth\n\ntype Session struct { ID string }\n")
	if err := os.WriteFile(partialFile, initialContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Register Host B
	hostBExecuted := false
	fakeB := a4c.NewFakeProvider()
	fakeB.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		hostBExecuted = true
		return &a4c.AgentMessage{
			Role:      "assistant",
			Content:   "Completed session timeout validation and tests.",
			Timestamp: time.Now(),
		}, nil
	})
	h.coordinator.RegisterProvider("opencode", fakeB)

	// Step 1: Adopt existing work
	adoptRes, err := h.service.AdoptExistingWork(ctx, a4c.AdoptWorkRequest{
		Objective:    "Complete session manager with timeout logic",
		HandoffNotes: "Started implementing Session struct in pkg/auth/session.go. Need expiry checking.",
		Runtime:      "opencode",
		Model:        "deepseek-coder",
		InPlace:      true,
	})
	if err != nil {
		t.Fatalf("AdoptExistingWork failed: %v", err)
	}

	if adoptRes.TotalChanges == 0 {
		t.Fatalf("expected adopted changes, got 0")
	}
	if len(adoptRes.ModifiedFiles) == 0 && len(adoptRes.NewFiles) == 0 {
		t.Fatalf("expected adopted files, got none")
	}

	// Step 2: Verify existing file content was NOT overwritten or deleted
	content, err := os.ReadFile(partialFile)
	if err != nil {
		t.Fatalf("failed to read partial file: %v", err)
	}
	if string(content) != string(initialContent) {
		t.Fatalf("file content changed unexpectedly: expected %q, got %q", string(initialContent), string(content))
	}

	// Step 3: Execute adopted task
	_, execErr := h.service.ExecuteTask(ctx, adoptRes.TaskID, 5)
	if execErr != nil {
		t.Fatalf("ExecuteTask failed on adopted task: %v", execErr)
	}
	if !hostBExecuted {
		t.Fatalf("Host B was not executed for adopted task")
	}
}

// TestScenarioC_VoluntaryHostSwitch verifies switching hosts voluntarily without provider failure.
func TestScenarioC_VoluntaryHostSwitch(t *testing.T) {
	h := setupServiceTestHarness(t)
	h.coordinator.TestRunner = &fakePassTestRunner{}
	ctx := context.Background()

	hostAExecuted := false
	fakeA := a4c.NewFakeProvider()
	fakeA.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		hostAExecuted = true
		return &a4c.AgentMessage{
			Role:      "assistant",
			Content:   "Wrote scaffolding for cache manager.",
			Timestamp: time.Now(),
		}, nil
	})

	hostBExecuted := false
	fakeB := a4c.NewFakeProvider()
	fakeB.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		hostBExecuted = true
		return &a4c.AgentMessage{
			Role:      "assistant",
			Content:   "Completed independent review of cache manager. [REVIEW_APPROVED]",
			Timestamp: time.Now(),
		}, nil
	})

	h.coordinator.RegisterProvider("copilot", fakeA)
	h.coordinator.RegisterProvider("antigravity", fakeB)

	// Step 1: Submit task on Host A using StrategyReviewed
	taskRes, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Implement LRU cache manager",
		Strategy:  a4c.StrategyReviewed,
		Runtime:   "copilot",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	// Step 2: Run 1 step on Host A
	_, err = h.service.ExecuteTask(ctx, taskRes.TaskID, 1)
	if err != nil {
		t.Fatalf("ExecuteTask step 1 failed: %v", err)
	}
	if !hostAExecuted {
		t.Fatalf("Host A was not executed")
	}

	// Step 3: Voluntary switch to Host B
	resumeRes, err := h.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:  taskRes.TaskID,
		Runtime: "antigravity",
	})
	if err != nil {
		t.Fatalf("ResumeTaskOnHost failed: %v", err)
	}
	if resumeRes.TargetRuntime != "antigravity" {
		t.Fatalf("expected antigravity, got %s", resumeRes.TargetRuntime)
	}

	// Step 4: Run remaining steps on Host B
	_, err = h.service.ExecuteTask(ctx, taskRes.TaskID, 2)
	if err != nil {
		t.Fatalf("ExecuteTask on Host B failed: %v", err)
	}
	if !hostBExecuted {
		t.Fatalf("Host B was not executed")
	}

	// Step 5: Verify task audit history shows completion
	history, err := h.service.GetTask(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if history.State != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected task awaiting review, got %s", history.State)
	}
}

// TestProviderFailure_ExhaustedAlternatives_SafeSuspension verifies that when no alternatives exist,
// the task stays safely suspended and does not loop, fake completion, or destroy files.
func TestProviderFailure_ExhaustedAlternatives_SafeSuspension(t *testing.T) {
	h := setupServiceTestHarness(t)
	h.coordinator.TestRunner = &fakePassTestRunner{}
	ctx := context.Background()

	// Only 1 host registered, which exhausts quota
	fakeA := a4c.NewFakeProvider()
	fakeA.SetResponseFunc(func(ctx context.Context, prompt string) (*a4c.AgentMessage, error) {
		return nil, a4c.ErrQuotaExhausted
	})
	h.coordinator.RegisterProvider("copilot", fakeA)

	taskRes, err := h.service.SubmitTask(ctx, a4c.SubmitTaskRequest{
		Objective: "Refactor database connection pool",
		Strategy:  a4c.StrategyDirect,
		Runtime:   "copilot",
	})
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}

	_, execErr := h.service.ExecuteTask(ctx, taskRes.TaskID, 2)
	if execErr == nil {
		t.Fatalf("expected provider error, got nil")
	}

	// Check alternatives -> should be empty
	alts, err := h.service.DiscoverAlternativeHosts(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("DiscoverAlternativeHosts failed: %v", err)
	}
	if len(alts) != 0 {
		t.Fatalf("expected 0 alternatives, got %d", len(alts))
	}

	// Attempt resume without specifying a valid host -> should fail gracefully
	_, resumeErr := h.service.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
		TaskID:  taskRes.TaskID,
		Runtime: "non-existent-host",
	})
	if resumeErr == nil {
		t.Fatalf("expected resume on invalid host to error, got nil")
	}

	// Verify task remains safely suspended in Interrupted state
	taskDetails, err := h.service.GetTask(ctx, taskRes.TaskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskDetails.State != a4c.TaskStateInterrupted {
		t.Fatalf("expected TaskStateInterrupted, got %s", taskDetails.State)
	}
}
