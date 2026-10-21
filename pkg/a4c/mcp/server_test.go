package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c"
	a4cmcp "github.com/ormasoftchile/a4c/pkg/a4c/mcp"
)

type mcpHarness struct {
	repoDir string
	service a4c.TeamService
	store   *a4c.DurableActivityStore
	server  *a4cmcp.Server
	client  *client.Client
}

func setupMCPHarness(t *testing.T) *mcpHarness {
	t.Helper()
	repoDir := t.TempDir()

	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "MCP Tester")
	runGit(t, repoDir, "config", "user.email", "mcp@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# MCP Test Repo\n"), 0644)
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "init")

	dotDir := filepath.Join(repoDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	lifecycleStore, err := a4c.NewDurableLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatalf("create lifecycle store: %v", err)
	}
	memory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	journal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	wtMgr, err := a4c.NewWorktreeManager(repoDir, wtDir)
	if err != nil {
		t.Fatalf("create worktrees: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_memory",
	})
	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, gov, memory, journal, wtMgr)
	return createMCPHarness(t, repoDir, svc, store)
}



func createMCPHarness(t *testing.T, repoDir string, svc a4c.TeamService, store *a4c.DurableActivityStore) *mcpHarness {
	srv := a4cmcp.NewServer(svc)

	cli, err := client.NewInProcessClient(srv.MCPServer())
	if err != nil {
		t.Fatalf("create in-process client: %v", err)
	}

	ctx := context.Background()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}

	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "copilot-test-client",
				Version: "1.0.0",
			},
		},
	}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize client: %v", err)
	}

	return &mcpHarness{
		repoDir: repoDir,
		service: svc,
		store:   store,
		server:  srv,
		client:  cli,
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed in %s: %s (%v)", args, dir, string(out), err)
	}
}

func TestMCPServer_ToolDiscovery(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()
	toolsRes, err := h.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	expectedTools := map[string]bool{
		"team_list":              false,
		"team_status":            false,
		"team_result":            false,
		"team_diff":              false,
		"team_feedback":          false,
		"team_context":           false,
		"team_begin_task":        false,
		"team_checkpoint":        false,
		"team_complete_activity": false,
		"team_apply_patch":       false,
		"team_resume":            false,
	}

	prohibitedTools := map[string]bool{
		"team_delegate":                      true,
		"team_assign_activity":               true,
		"team_review_result":                 true,
		"team_accept":                        true,
		"team_integrate":                     true,
		"governed_read_file":                 true,
		"governed_apply_patch":               true,
		"governed_git_inspect":               true,
		"governed_run_tests":                 true,
		"governed_structural_search_symbols": true,
		"governed_structural_context_for":    true,
		"governed_structural_callers":        true,
	}

	for _, tool := range toolsRes.Tools {
		if prohibitedTools[tool.Name] {
			t.Errorf("VIOLATION: human CLI boundary tool %q was exposed in MCP tool discovery", tool.Name)
		}
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected MCP tool %q was not registered", name)
		}
	}

	// Verify that calling human-only tools over MCP fails closed
	accRes, accErr := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "team_accept", Arguments: map[string]any{"task_id": "dummy"}},
	})
	if accErr == nil && (accRes == nil || !accRes.IsError) {
		t.Errorf("VIOLATION: calling team_accept over MCP succeeded instead of failing closed")
	}

	intRes, intErr := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "team_integrate", Arguments: map[string]any{"task_id": "dummy"}},
	})
	if intErr == nil && (intRes == nil || !intRes.IsError) {
		t.Errorf("VIOLATION: calling team_integrate over MCP succeeded instead of failing closed")
	}
}









func TestMCPServer_AcceptAndIntegrateLifecycle(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// Delegate direct task
	delReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective": "Add utils package",
				"strategy":  "direct",
			},
		},
	}
	delRes, err := h.client.CallTool(ctx, delReq)
	if err != nil || delRes.IsError {
		t.Fatalf("team_delegate failed: %v (%+v)", err, delRes)
	}
	var ref a4c.TaskRef
	_ = json.Unmarshal([]byte(delRes.Content[0].(mcp.TextContent).Text), &ref)

	// Attempt integrate via MCP -> must fail closed (unauthorized over MCP)
	intReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_integrate",
			Arguments: map[string]any{
				"task_id": ref.TaskID,
			},
		},
	}
	intRes, intErr := h.client.CallTool(ctx, intReq)
	if intErr == nil && (intRes == nil || !intRes.IsError) {
		t.Fatalf("expected error integrating unaccepted task, got success")
	}

	// Complete activity to make it reviewable
	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{
		ID:     ref.TaskID,
		Status: a4c.StatusCompleted,
	})

	// Call team_accept via MCP -> MUST FAIL CLOSED (Milestone 16 authorization hardening)
	accReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_accept",
			Arguments: map[string]any{
				"task_id":  ref.TaskID,
				"comments": "Agent attempting self-approval",
			},
		},
	}
	accRes, accErr := h.client.CallTool(ctx, accReq)
	if accErr == nil && (accRes == nil || !accRes.IsError) {
		t.Fatalf("expected team_accept via MCP to fail closed with unauthorized, but succeeded")
	}

	// Now perform authenticated human CLI acceptance
	accResult, err := h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:        ref.TaskID,
		Comments:      "Approved by operator CLI",
		AcceptedBy:    "human_cli",
		IsInteractive: true,
	})
	if err != nil {
		t.Fatalf("human CLI AcceptTask failed: %v", err)
	}
	if accResult.TaskID != ref.TaskID || accResult.AcceptedSnapshot == "" {
		t.Fatalf("unexpected accResult: %+v", accResult)
	}

	// Call team_integrate via MCP -> MUST FAIL CLOSED (AI agents cannot integrate over MCP)
	intResMCP, intMCPErr := h.client.CallTool(ctx, intReq)
	if intMCPErr == nil && (intResMCP == nil || !intResMCP.IsError) {
		t.Fatalf("expected team_integrate via MCP to fail closed with unauthorized, but succeeded")
	}

	// Execute authenticated human CLI integration
	intResult, err := h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID: ref.TaskID,
	})
	if err != nil {
		t.Fatalf("human CLI IntegrateTask failed: %v", err)
	}
	if intResult.TaskID != ref.TaskID {
		t.Fatalf("unexpected intResult: %+v", intResult)
	}

	// Verify status is now integrated
	statReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_status",
			Arguments: map[string]any{
				"task_id": ref.TaskID,
			},
		},
	}
	statRes, _ := h.client.CallTool(ctx, statReq)
	var det a4c.TaskDetails
	_ = json.Unmarshal([]byte(statRes.Content[0].(mcp.TextContent).Text), &det)
	if det.State != a4c.TaskStateIntegrated {
		t.Fatalf("expected state integrated, got %s", det.State)
	}
}



// TestMCPServer_ApplyPatch_EndToEnd drives the governed write through the MCP
// tool itself: begin a task to provision its worktree, apply a file, and
// confirm the bytes land inside that worktree. Traversal must fail closed.
func TestMCPServer_ApplyPatch_EndToEnd(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. Provision a task and its isolated worktree.
	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective":     "Governed write end-to-end",
				"budget_tokens": float64(50000),
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool team_begin_task failed: %v", err)
	}
	if beginRes.IsError {
		t.Fatalf("team_begin_task returned error: %+v", beginRes)
	}
	var begun a4c.BeginTaskResult
	if err := json.Unmarshal([]byte(beginRes.Content[0].(mcp.TextContent).Text), &begun); err != nil {
		t.Fatalf("unmarshal begin result: %v", err)
	}
	if begun.TaskID == "" || begun.WorktreePath == "" {
		t.Fatalf("expected task and worktree, got %+v", begun)
	}

	// 2. Apply a governed write into that worktree.
	const content = "package governed\n\nvar E2E = true\n"
	applyRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_apply_patch",
			Arguments: map[string]any{
				"task_id": begun.TaskID,
				"path":    "governed/e2e.go",
				"content": content,
				"agent":   "coder",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool team_apply_patch failed: %v", err)
	}
	if applyRes.IsError {
		t.Fatalf("team_apply_patch returned error: %+v", applyRes)
	}
	var applied a4c.ApplyFileResult
	if err := json.Unmarshal([]byte(applyRes.Content[0].(mcp.TextContent).Text), &applied); err != nil {
		t.Fatalf("unmarshal apply result: %v\noutput: %s", err, applyRes.Content[0].(mcp.TextContent).Text)
	}
	if applied.TaskID != begun.TaskID {
		t.Errorf("expected task %s, got %s", begun.TaskID, applied.TaskID)
	}

	// 3. The file must exist inside the worktree, not the main checkout.
	target := filepath.Join(begun.WorktreePath, "governed", "e2e.go")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("governed write did not land in the worktree: %v", err)
	}
	if string(got) != content {
		t.Errorf("content mismatch: %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(h.repoDir, "governed", "e2e.go")); err == nil {
		t.Errorf("SECURITY: governed write landed in the main checkout instead of the worktree")
	}

	// 4. Traversal must fail closed through the tool boundary.
	escRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_apply_patch",
			Arguments: map[string]any{
				"task_id": begun.TaskID,
				"path":    "../escaped.go",
				"content": "evil",
				"agent":   "coder",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool team_apply_patch (traversal) failed: %v", err)
	}
	if !escRes.IsError {
		t.Errorf("expected traversal to fail closed through MCP")
	}
}

// TestMCPServer_Resume_DetectsPriorWork reproduces the reported failure: a task
// makes progress, the session dies, and a NEW host asks to resume. Work lives in
// the task worktree, not the main checkout, so an agent that only inspects the
// checkout sees an empty repository. team_resume must return the same worktree
// and report the work already there.
func TestMCPServer_Resume_DetectsPriorWork(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. Begin a task; note the worktree it provisions.
	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective":     "Build a command line calculator",
				"budget_tokens": float64(50000),
			},
		},
	})
	if err != nil {
		t.Fatalf("team_begin_task: %v", err)
	}
	var begun a4c.BeginTaskResult
	if err := json.Unmarshal([]byte(beginRes.Content[0].(mcp.TextContent).Text), &begun); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 2. Progress happens in the worktree — invisible from the main checkout.
	prior := "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
	applyRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_apply_patch",
			Arguments: map[string]any{
				"task_id": begun.TaskID,
				"path":    "src/calc.go",
				"content": prior,
				"agent":   "coder",
			},
		},
	})
	if err != nil || applyRes.IsError {
		t.Fatalf("team_apply_patch: %v %+v", err, applyRes)
	}

	// 3. A new host resumes WITHOUT naming the task — the reported scenario.
	resumeRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "team_resume", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("team_resume: %v", err)
	}
	if resumeRes.IsError {
		t.Fatalf("team_resume returned error: %+v", resumeRes)
	}

	var chk a4c.TaskCheckpoint
	if err := json.Unmarshal([]byte(resumeRes.Content[0].(mcp.TextContent).Text), &chk); err != nil {
		t.Fatalf("unmarshal checkpoint: %v\n%s", err, resumeRes.Content[0].(mcp.TextContent).Text)
	}

	if chk.TaskID != begun.TaskID {
		t.Errorf("resumed %q, want the existing task %q", chk.TaskID, begun.TaskID)
	}
	if chk.WorktreePath != begun.WorktreePath {
		t.Errorf("resumed into a DIFFERENT worktree:\n  got  %s\n  want %s\nprior work would be invisible",
			chk.WorktreePath, begun.WorktreePath)
	}

	// The checkpoint must surface the prior work, not just the path.
	found := false
	for _, f := range append(chk.NewFiles, chk.ModifiedFiles...) {
		if strings.Contains(f, "src/calc.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("checkpoint did not report the prior work; got new=%v modified=%v",
			chk.NewFiles, chk.ModifiedFiles)
	}
}

// TestMCPServer_DirectGovernanceFlow exercises the simplified lifecycle:
// begin -> governed write -> complete -> diff, with retired orchestration
// tools failing closed instead of being advertised.
func TestMCPServer_DirectGovernanceFlow(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	ctxRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "team_context", Arguments: map[string]any{}},
	})
	if err != nil || ctxRes.IsError {
		t.Fatalf("team_context failed: %v (%+v)", err, ctxRes)
	}
	var envCtx a4c.TeamContextResult
	_ = json.Unmarshal([]byte(ctxRes.Content[0].(mcp.TextContent).Text), &envCtx)
	if envCtx.WorkspaceRoot == "" {
		t.Fatalf("expected workspace root in context, got %+v", envCtx)
	}

	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_begin_task",
			Arguments: map[string]any{"objective": "Add an Add(a,b) function", "strategy": "direct"},
		},
	})
	if err != nil || beginRes.IsError {
		t.Fatalf("team_begin_task failed: %v (%+v)", err, beginRes)
	}
	var beginResult a4c.BeginTaskResult
	_ = json.Unmarshal([]byte(beginRes.Content[0].(mcp.TextContent).Text), &beginResult)
	if beginResult.TaskID == "" || beginResult.WorktreePath == "" {
		t.Fatalf("invalid begin task result: %+v", beginResult)
	}

	// Retired strategies fail explicitly instead of being reinterpreted.
	legacyRes, legacyErr := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_begin_task",
			Arguments: map[string]any{"objective": "x", "strategy": "team"},
		},
	})
	if legacyErr == nil && (legacyRes == nil || !legacyRes.IsError) {
		t.Fatalf("retired strategy 'team' was accepted instead of failing explicitly")
	}

	// Retired orchestration tools are not exposed.
	for _, retired := range []string{"team_delegate", "team_assign_activity", "team_review_result"} {
		res, callErr := h.client.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: retired, Arguments: map[string]any{}},
		})
		if callErr == nil && (res == nil || !res.IsError) {
			t.Errorf("retired tool %q was still callable", retired)
		}
	}

	patchRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_apply_patch",
			Arguments: map[string]any{
				"task_id": beginResult.TaskID,
				"path":    "add.go",
				"content": "package add\n\nfunc Add(a, b int) int { return a + b }\n",
			},
		},
	})
	if err != nil || patchRes.IsError {
		t.Fatalf("team_apply_patch failed: %v (%+v)", err, patchRes)
	}

	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":     beginResult.TaskID,
				"activity_id": beginResult.TaskID,
				"actor":       "host",
				"success":     true,
				"summary":     "implemented Add with tests; go test ./... green",
			},
		},
	})
	if err != nil || compRes.IsError {
		t.Fatalf("team_complete_activity failed: %v (%+v)", err, compRes)
	}

	diffRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_diff",
			Arguments: map[string]any{"task_id": beginResult.TaskID},
		},
	})
	if err != nil || diffRes.IsError {
		t.Fatalf("team_diff failed: %v (%+v)", err, diffRes)
	}
	if !strings.Contains(diffRes.Content[0].(mcp.TextContent).Text, "add.go") {
		t.Fatalf("expected add.go in task diff, got %s", diffRes.Content[0].(mcp.TextContent).Text)
	}
}
