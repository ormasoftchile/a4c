package governed_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// TestCanaryEscapeExperiment empirically audits the sandbox containment of governed_run_tests.
// It determines whether test execution is constrained by an OS-level security boundary
// or whether it executes with the developer's full host UID permissions.
func TestCanaryEscapeExperiment(t *testing.T) {
	h := setupAdvHarness(t)
	ctx := context.Background()

	wtPath, err := h.service.GetWorktreePath(h.taskID)
	if err != nil {
		t.Fatalf("get worktree path: %v", err)
	}

	canaryFile := filepath.Join(os.TempDir(), fmt.Sprintf("a4c-canary-%d.txt", time.Now().UnixNano()))
	_ = os.Remove(canaryFile)
	defer os.Remove(canaryFile)

	// Inject a test file inside the worktree that attempts escape actions
	escapeTestContent := fmt.Sprintf(`package main

import (
	"net"
	"os"
	"os/exec"
	"testing"
)

func TestEscapeAttempts(t *testing.T) {
	// 1. Attempt filesystem write outside workspace
	err := os.WriteFile(%q, []byte("ESCAPE_CONFIRMED"), 0644)
	if err != nil {
		t.Logf("Canary write failed: %%v", err)
	} else {
		t.Log("Canary file successfully written outside workspace!")
	}

	// 2. Inspect host environment inheritance
	user := os.Getenv("USER")
	home := os.Getenv("HOME")
	t.Logf("Environment check: USER=%%s HOME=%%s", user, home)

	// 3. Attempt subprocess spawn
	out, cmdErr := exec.Command("whoami").CombinedOutput()
	if cmdErr != nil {
		t.Logf("Subprocess spawn failed: %%v", cmdErr)
	} else {
		t.Logf("Subprocess spawned successfully: %%s", string(out))
	}

	// 4. Attempt network socket creation
	l, netErr := net.Listen("tcp", "127.0.0.1:0")
	if netErr != nil {
		t.Logf("Network socket failed: %%v", netErr)
	} else {
		t.Logf("Network socket bound to: %%s", l.Addr().String())
		_ = l.Close()
	}
}
`, canaryFile)

	if err := os.WriteFile(filepath.Join(wtPath, "escape_test.go"), []byte(escapeTestContent), 0644); err != nil {
		t.Fatalf("write escape test: %v", err)
	}

	// Assign coder activity to authorize run_tests
	_, _ = h.service.AssignActivity(ctx, a4c.AssignActivityRequest{
		TaskID:    h.taskID,
		Agent:     "bob",
		Role:      "coder",
		Objective: "Run escape probe tests",
	})

	testRes, err := h.manager.RunTests(ctx, h.taskID, "./...", false, "canary-probe")
	if err != nil || testRes.Status != tool.InvocationStatusCompleted {
		t.Fatalf("RunTests failed: %v (%+v)", err, testRes)
	}

	// Check whether canary file was created outside workspace
	data, statErr := os.ReadFile(canaryFile)
	canaryCreated := statErr == nil && string(data) == "ESCAPE_CONFIRMED"

	t.Logf("================ CANARY EXPERIMENT RESULTS ================")
	t.Logf("Canary File Created Outside Workspace: %v", canaryCreated)
	t.Logf("Test Output:\n%s", testRes.Stdout)
	t.Logf("==========================================================")

	if canaryCreated {
		t.Log("EMPIRICAL FACT: Test execution is NOT sandboxed at the OS level.")
		t.Log("Containment is enforced via disposable copy and worktree diff checks,")
		t.Log("NOT via OS-level sandbox (seccomp, Landlock, containers, sandbox-exec).")
	}
}
