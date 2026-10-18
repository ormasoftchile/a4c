package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	rosterDir := filepath.Join(dotDir, "roster")
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
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("create roster: %v", err)
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
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr)
	return createMCPHarnessWithRole(t, repoDir, svc, store, "")
}

func setupMCPHarnessWithRole(t *testing.T, role string) *mcpHarness {
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
	rosterDir := filepath.Join(dotDir, "roster")
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
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		t.Fatalf("create roster: %v", err)
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
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr)
	return createMCPHarnessWithRole(t, repoDir, svc, store, role)
}

func createMCPHarnessWithRole(t *testing.T, repoDir string, svc a4c.TeamService, store *a4c.DurableActivityStore, role string) *mcpHarness {
	srv := a4cmcp.NewServerWithRole(svc, role)

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
		"team_delegate":          false,
		"team_list":              false,
		"team_status":            false,
		"team_result":            false,
		"team_diff":              false,
		"team_feedback":          false,
		"team_context":           false,
		"team_begin_task":        false,
		"team_assign_activity":   false,
		"team_checkpoint":        false,
		"team_complete_activity": false,
		"team_review_result":     false,
		"team_apply_patch":       false,
	}

	prohibitedTools := map[string]bool{
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

func TestMCPServer_RoleDiscovery_Coder(t *testing.T) {
	h := setupMCPHarnessWithRole(t, "coder")
	defer h.client.Close()

	ctx := context.Background()
	toolsRes, err := h.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	allowed := map[string]bool{
		"team_status":            true,
		"team_diff":              true,
		"team_checkpoint":        true,
		"team_complete_activity": true,
		"team_apply_patch":       true,
	}

	prohibited := []string{
		"team_review_result", "team_delegate", "team_list", "team_feedback",
		"team_context", "team_begin_task", "team_assign_activity",
		"team_accept", "team_integrate",
		"governed_read_file", "governed_apply_patch", "governed_git_inspect", "governed_run_tests",
		"governed_structural_search_symbols", "governed_structural_context_for", "governed_structural_callers",
	}

	toolMap := make(map[string]bool)
	for _, tool := range toolsRes.Tools {
		toolMap[tool.Name] = true
		if !allowed[tool.Name] {
			t.Errorf("VIOLATION: coder sees unpermitted tool %q", tool.Name)
		}
	}

	for name := range allowed {
		if !toolMap[name] {
			t.Errorf("coder missing expected tool %q", name)
		}
	}

	for _, name := range prohibited {
		if toolMap[name] {
			t.Errorf("VIOLATION: coder exposed prohibited tool %q", name)
		}
	}
}

func TestMCPServer_RoleDiscovery_Reviewer(t *testing.T) {
	h := setupMCPHarnessWithRole(t, "reviewer")
	defer h.client.Close()

	ctx := context.Background()
	toolsRes, err := h.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	allowed := map[string]bool{
		"team_status":        true,
		"team_result":        true,
		"team_diff":          true,
		"team_checkpoint":    true,
		"team_review_result": true,
	}

	prohibited := []string{
		"team_delegate", "team_complete_activity", "team_list", "team_feedback",
		"team_context", "team_begin_task", "team_assign_activity",
		"team_accept", "team_integrate", "team_apply_patch",
		"governed_read_file", "governed_apply_patch", "governed_git_inspect", "governed_run_tests",
		"governed_structural_search_symbols", "governed_structural_context_for", "governed_structural_callers",
	}

	toolMap := make(map[string]bool)
	for _, tool := range toolsRes.Tools {
		toolMap[tool.Name] = true
		if !allowed[tool.Name] {
			t.Errorf("VIOLATION: reviewer sees unpermitted tool %q", tool.Name)
		}
	}

	for name := range allowed {
		if !toolMap[name] {
			t.Errorf("reviewer missing expected tool %q", name)
		}
	}

	for _, name := range prohibited {
		if toolMap[name] {
			t.Errorf("VIOLATION: reviewer exposed prohibited tool %q", name)
		}
	}
}

func TestMCPServer_RoleDiscovery_Lead(t *testing.T) {
	h := setupMCPHarnessWithRole(t, "lead")
	defer h.client.Close()

	ctx := context.Background()
	toolsRes, err := h.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	allowed := map[string]bool{
		"team_delegate":          true,
		"team_list":              true,
		"team_status":            true,
		"team_result":            true,
		"team_diff":              true,
		"team_feedback":          true,
		"team_context":           true,
		"team_begin_task":        true,
		"team_assign_activity":   true,
		"team_checkpoint":        true,
		"team_complete_activity": true,
	}

	prohibited := []string{
		"team_review_result", "team_accept", "team_integrate", "team_apply_patch",
		"governed_read_file", "governed_apply_patch", "governed_git_inspect", "governed_run_tests",
		"governed_structural_search_symbols", "governed_structural_context_for", "governed_structural_callers",
	}

	toolMap := make(map[string]bool)
	for _, tool := range toolsRes.Tools {
		toolMap[tool.Name] = true
		if !allowed[tool.Name] {
			t.Errorf("VIOLATION: lead sees unpermitted tool %q", tool.Name)
		}
	}

	for name := range allowed {
		if !toolMap[name] {
			t.Errorf("lead missing expected tool %q", name)
		}
	}

	for _, name := range prohibited {
		if toolMap[name] {
			t.Errorf("VIOLATION: lead exposed prohibited tool %q", name)
		}
	}
}

func TestMCPServer_DelegateStatusAndList(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. team_delegate
	callReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_delegate",
			Arguments: map[string]any{
				"objective":     "Implement fast JSON encoder",
				"budget_tokens": float64(30000),
				"strategy":      "team",
			},
		},
	}
	res, err := h.client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool team_delegate failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("team_delegate returned error result: %+v", res)
	}

	var taskRef a4c.TaskRef
	textContent := res.Content[0].(mcp.TextContent).Text
	if err := json.Unmarshal([]byte(textContent), &taskRef); err != nil {
		t.Fatalf("failed to unmarshal taskRef: %v\nOutput: %s", err, textContent)
	}
	if taskRef.TaskID == "" || taskRef.State != a4c.TaskStateQueued {
		t.Fatalf("unexpected taskRef: %+v", taskRef)
	}

	// 2. team_status
	statusReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_status",
			Arguments: map[string]any{
				"task_id": taskRef.TaskID,
			},
		},
	}
	statusRes, err := h.client.CallTool(ctx, statusReq)
	if err != nil {
		t.Fatalf("CallTool team_status failed: %v", err)
	}
	if statusRes.IsError {
		t.Fatalf("team_status returned error: %+v", statusRes)
	}

	var details a4c.TaskDetails
	statusText := statusRes.Content[0].(mcp.TextContent).Text
	if err := json.Unmarshal([]byte(statusText), &details); err != nil {
		t.Fatalf("failed to unmarshal details: %v", err)
	}
	if details.TaskID != taskRef.TaskID || details.Objective != "Implement fast JSON encoder" {
		t.Fatalf("unexpected details: %+v", details)
	}

	// 3. team_list
	listReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_list",
			Arguments: map[string]any{},
		},
	}
	listRes, err := h.client.CallTool(ctx, listReq)
	if err != nil {
		t.Fatalf("CallTool team_list failed: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("team_list returned error: %+v", listRes)
	}

	var summaries []a4c.TaskSummary
	listText := listRes.Content[0].(mcp.TextContent).Text
	if err := json.Unmarshal([]byte(listText), &summaries); err != nil {
		t.Fatalf("failed to unmarshal summaries: %v", err)
	}
	if len(summaries) != 1 || summaries[0].TaskID != taskRef.TaskID {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}
}

func TestMCPServer_AcceptAndIntegrateLifecycle(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// Delegate direct task
	delReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_delegate",
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

func TestMCPServer_HostNativeCoordinationFlow(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. team_context
	ctxRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_context",
			Arguments: map[string]any{},
		},
	})
	if err != nil || ctxRes.IsError {
		t.Fatalf("team_context failed: %v (%+v)", err, ctxRes)
	}
	var envCtx a4c.TeamContextResult
	_ = json.Unmarshal([]byte(ctxRes.Content[0].(mcp.TextContent).Text), &envCtx)
	if len(envCtx.TeamMembers) < 3 {
		t.Fatalf("expected at least 3 team members, got %d", len(envCtx.TeamMembers))
	}

	// 2. team_begin_task
	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective":   "Implement host-native math library",
				"coordinator": "alice",
				"runtime":     "copilot",
			},
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

	// 3. team_assign_activity (Bob Coder)
	assignRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_assign_activity",
			Arguments: map[string]any{
				"task_id":   beginResult.TaskID,
				"agent":     "bob",
				"role":      "coder",
				"objective": "Add Add(a, b) function and tests",
			},
		},
	})
	if err != nil || assignRes.IsError {
		t.Fatalf("team_assign_activity failed: %v (%+v)", err, assignRes)
	}
	var actRef a4c.ActivityRef
	_ = json.Unmarshal([]byte(assignRes.Content[0].(mcp.TextContent).Text), &actRef)
	if actRef.ActivityID == "" || actRef.Agent != "bob" {
		t.Fatalf("invalid activity ref: %+v", actRef)
	}

	// 4. team_checkpoint
	chkRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_checkpoint",
			Arguments: map[string]any{
				"task_id":     beginResult.TaskID,
				"activity_id": actRef.ActivityID,
				"actor":       "bob",
				"summary":     "Designed function signatures; implementing tests",
				"assumptions": "Pure Go with no external dependencies",
				"findings":    "Repo is using Go 1.22",
				"next_steps":  "Run go test",
			},
		},
	})
	if err != nil || chkRes.IsError {
		t.Fatalf("team_checkpoint failed: %v (%+v)", err, chkRes)
	}

	// Simulate Bob modifying files in the worktree
	mathFile := filepath.Join(beginResult.WorktreePath, "math.go")
	_ = os.WriteFile(mathFile, []byte("package main\n\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	runGit(t, beginResult.WorktreePath, "add", "math.go")
	runGit(t, beginResult.WorktreePath, "commit", "-m", "feat: add Add function")

	// 5. team_complete_activity
	compRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_complete_activity",
			Arguments: map[string]any{
				"task_id":             beginResult.TaskID,
				"activity_id":         actRef.ActivityID,
				"actor":               "bob",
				"summary":             "Implemented Add(a, b int) in math.go",
				"files_modified":      "math.go",
				"validation_evidence": "All unit tests pass",
				"success":             true,
			},
		},
	})
	if err != nil || compRes.IsError {
		t.Fatalf("team_complete_activity failed: %v (%+v)", err, compRes)
	}

	// 6. team_review_result (Charlie Reviewer)
	revRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_review_result",
			Arguments: map[string]any{
				"task_id":  beginResult.TaskID,
				"reviewer": "charlie",
				"verdict":  "approved",
				"comments": "Clean implementation and non-breaking",
				"findings": "Zero regressions; clean style",
			},
		},
	})
	if err != nil || revRes.IsError {
		t.Fatalf("team_review_result failed: %v (%+v)", err, revRes)
	}
	var revResult a4c.HostReviewResult
	_ = json.Unmarshal([]byte(revRes.Content[0].(mcp.TextContent).Text), &revResult)
	if revResult.LifecycleState != a4c.TaskStateAwaitingHumanReview {
		t.Fatalf("expected state %s, got %s", a4c.TaskStateAwaitingHumanReview, revResult.LifecycleState)
	}

	// 7. team_accept & team_integrate
	// Verify that MCP call (e.g. AI agent trying to self-approve) is rejected
	unauthRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_accept",
			Arguments: map[string]any{
				"taskID":   beginResult.TaskID,
				"task_id":  beginResult.TaskID,
				"comments": "Agent trying to self-approve",
			},
		},
	})
	if err == nil && (unauthRes == nil || !unauthRes.IsError) {
		t.Fatalf("expected unauthenticated team_accept to fail closed, got %+v", unauthRes)
	}

	// Verify that MCP call to team_integrate is also rejected
	unauthIntRes, intErr := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_integrate",
			Arguments: map[string]any{
				"task_id": beginResult.TaskID,
			},
		},
	})
	if intErr == nil && (unauthIntRes == nil || !unauthIntRes.IsError) {
		t.Fatalf("expected unauthenticated team_integrate to fail closed, got %+v", unauthIntRes)
	}

	// Now perform authenticated human CLI acceptance
	_, err = h.service.AcceptTask(ctx, a4c.AcceptTaskRequest{
		TaskID:        beginResult.TaskID,
		Comments:      "Approved for merge by human engineer via CLI",
		AcceptedBy:    "human_cli",
		IsInteractive: true,
	})
	if err != nil {
		t.Fatalf("human CLI AcceptTask failed: %v", err)
	}

	// Now perform authenticated human CLI integration
	_, err = h.service.IntegrateTask(ctx, a4c.IntegrateTaskRequest{
		TaskID: beginResult.TaskID,
	})
	if err != nil {
		t.Fatalf("human CLI IntegrateTask failed: %v", err)
	}

	// Verify math.go now exists in root repo checkout!
	if _, err := os.Stat(filepath.Join(h.repoDir, "math.go")); os.IsNotExist(err) {
		t.Fatalf("math.go was not integrated into base checkout")
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
