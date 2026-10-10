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
	structDir := filepath.Join(dotDir, "structural")

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
	structStore, err := a4c.NewDurableStructuralStore(structDir)
	if err != nil {
		t.Fatalf("create structural store: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr, structStore)
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
	structDir := filepath.Join(dotDir, "structural")

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
	structStore, err := a4c.NewDurableStructuralStore(structDir)
	if err != nil {
		t.Fatalf("create structural store: %v", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr, structStore)
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
		"governed_read_file":     false,
		"governed_apply_patch":   false,
		"governed_git_inspect":   false,
		"governed_run_tests":                 false,
		"governed_structural_search_symbols": false,
		"governed_structural_context_for":   false,
		"governed_structural_callers":        false,
	}

	prohibitedTools := map[string]bool{
		"team_accept":    true,
		"team_integrate": true,
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
		"governed_read_file":     true,
		"governed_apply_patch":   true,
		"governed_run_tests":                 true,
		"governed_structural_search_symbols": true,
		"governed_structural_context_for":   true,
		"governed_structural_callers":        true,
	}

	prohibited := []string{
		"team_review_result", "governed_git_inspect", "team_delegate",
		"team_list", "team_feedback", "team_accept", "team_integrate",
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
		"team_status":          true,
		"team_result":          true,
		"team_diff":            true,
		"team_checkpoint":      true,
		"team_review_result":   true,
		"governed_read_file":                 true,
		"governed_git_inspect":               true,
		"governed_structural_search_symbols": true,
		"governed_structural_context_for":   true,
		"governed_structural_callers":        true,
	}

	prohibited := []string{
		"governed_apply_patch", "governed_run_tests", "team_delegate",
		"team_complete_activity", "team_accept", "team_integrate",
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
		"governed_read_file":                 true,
		"governed_git_inspect":               true,
		"governed_structural_search_symbols": true,
		"governed_structural_context_for":   true,
		"governed_structural_callers":        true,
	}

	prohibited := []string{
		"governed_apply_patch", "governed_run_tests", "team_accept", "team_integrate",
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
		TaskID:     ref.TaskID,
		Comments:   "Approved by operator CLI",
		AcceptedBy: "human_cli",
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
				"task_id":   beginResult.TaskID,
				"reviewer":  "charlie",
				"verdict":   "approved",
				"comments":  "Clean implementation and non-breaking",
				"findings":  "Zero regressions; clean style",
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
		TaskID:     beginResult.TaskID,
		Comments:   "Approved for merge by human engineer via CLI",
		AcceptedBy: "human_cli",
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

func TestMCPServer_GovernedTools(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. Begin a task to create worktree
	beginRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_begin_task",
			Arguments: map[string]any{
				"objective": "Test governed execution tools",
				"strategy":  "team",
			},
		},
	})
	if err != nil || beginRes.IsError {
		t.Fatalf("team_begin_task failed: %v (%+v)", err, beginRes)
	}
	var bRes a4c.BeginTaskResult
	_ = json.Unmarshal([]byte(beginRes.Content[0].(mcp.TextContent).Text), &bRes)

	// Assign activity to Bob (coder) so mutating authority is granted
	_, err = h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_assign_activity",
			Arguments: map[string]any{
				"task_id":   bRes.TaskID,
				"agent":     "bob",
				"role":      "coder",
				"objective": "Implement test file",
			},
		},
	})
	if err != nil {
		t.Fatalf("assign activity failed: %v", err)
	}

	// 2. governed_read_file on README.md
	readRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "governed_read_file",
			Arguments: map[string]any{
				"task_id": bRes.TaskID,
				"path":    "README.md",
			},
		},
	})
	if err != nil || readRes.IsError {
		t.Fatalf("governed_read_file failed: %v (%+v)", err, readRes)
	}

	// 3. governed_git_inspect status
	gitRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "governed_git_inspect",
			Arguments: map[string]any{
				"task_id":    bRes.TaskID,
				"subcommand": "status",
			},
		},
	})
	if err != nil || gitRes.IsError {
		t.Fatalf("governed_git_inspect failed: %v (%+v)", err, gitRes)
	}

	// 4. governed_apply_patch as Bob
	patchText := `diff --git a/sample.txt b/sample.txt
new file mode 100644
--- /dev/null
+++ b/sample.txt
@@ -0,0 +1,2 @@
+hello from governed patch
+verified by yawr
`
	patchRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "governed_apply_patch",
			Arguments: map[string]any{
				"task_id":         bRes.TaskID,
				"patch":           patchText,
				"idempotency_key": "patch-key-1",
			},
		},
	})
	if err != nil || patchRes.IsError {
		t.Fatalf("governed_apply_patch failed: %v (%+v)", err, patchRes)
	}

	// 5. Verify replay of the identical patch
	patchReplayRes, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "governed_apply_patch",
			Arguments: map[string]any{
				"task_id":         bRes.TaskID,
				"patch":           patchText,
				"idempotency_key": "patch-key-1",
			},
		},
	})
	if err != nil || patchReplayRes.IsError {
		t.Fatalf("governed_apply_patch replay failed: %v (%+v)", err, patchReplayRes)
	}
	var repOut map[string]any
	_ = json.Unmarshal([]byte(patchReplayRes.Content[0].(mcp.TextContent).Text), &repOut)
	if repOut["replayed"] != true {
		t.Errorf("expected replayed=true on identical key retry, got %+v", repOut)
	}
}

func TestMCPServer_GovernedStructuralTools(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// 1. Without index present, should return informative error over MCP (not crash)
	res, err := h.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "governed_structural_search_symbols",
			Arguments: map[string]any{
				"query": "something",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error when codetopo index is missing, got: %+v", res)
	}
}

func TestMCPServer_AblationOptions(t *testing.T) {
	h := setupMCPHarness(t)
	defer h.client.Close()

	ctx := context.Background()

	// Helper to get registered tool names for given options
	toolsForOpts := func(opts a4cmcp.ServerOptions) map[string]bool {
		srv := a4cmcp.NewServerWithOptions(h.service, opts)
		cli, err := client.NewInProcessClient(srv.MCPServer())
		if err != nil {
			t.Fatalf("create in-process client: %v", err)
		}
		defer cli.Close()
		if err := cli.Start(ctx); err != nil {
			t.Fatalf("start client: %v", err)
		}
		initReq := mcp.InitializeRequest{
			Params: mcp.InitializeParams{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo:      mcp.Implementation{Name: "ablation-tester", Version: "1.0.0"},
			},
		}
		if _, err := cli.Initialize(ctx, initReq); err != nil {
			t.Fatalf("init client: %v", err)
		}
		toolsRes, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		res := make(map[string]bool)
		for _, tool := range toolsRes.Tools {
			res[tool.Name] = true
		}
		return res
	}

	// 1. Minimal A4C: No YAWR, No CodeTopo
	minTools := toolsForOpts(a4cmcp.ServerOptions{EnableYAWR: false, EnableCodeTopo: false})
	if !minTools["team_begin_task"] || !minTools["team_status"] {
		t.Errorf("expected team coordination tools in minimal A4C")
	}
	if minTools["governed_apply_patch"] || minTools["governed_run_tests"] {
		t.Errorf("expected NO YAWR tools when EnableYAWR=false")
	}
	if minTools["governed_structural_search_symbols"] {
		t.Errorf("expected NO CodeTopo tools when EnableCodeTopo=false")
	}

	// 2. A4C + YAWR (no CodeTopo)
	yawrTools := toolsForOpts(a4cmcp.ServerOptions{EnableYAWR: true, EnableCodeTopo: false})
	if !yawrTools["governed_apply_patch"] || !yawrTools["governed_run_tests"] {
		t.Errorf("expected YAWR tools when EnableYAWR=true")
	}
	if yawrTools["governed_structural_search_symbols"] {
		t.Errorf("expected NO CodeTopo tools when EnableCodeTopo=false")
	}

	// 3. A4C + CodeTopo (no YAWR)
	ctTools := toolsForOpts(a4cmcp.ServerOptions{EnableYAWR: false, EnableCodeTopo: true})
	if ctTools["governed_apply_patch"] || ctTools["governed_run_tests"] {
		t.Errorf("expected NO YAWR tools when EnableYAWR=false")
	}
	if !ctTools["governed_structural_search_symbols"] {
		t.Errorf("expected CodeTopo tools when EnableCodeTopo=true")
	}

	// 4. Current A4C (both enabled)
	fullTools := toolsForOpts(a4cmcp.ServerOptions{EnableYAWR: true, EnableCodeTopo: true})
	if !fullTools["governed_apply_patch"] || !fullTools["governed_structural_search_symbols"] {
		t.Errorf("expected both YAWR and CodeTopo tools when both enabled")
	}
}


