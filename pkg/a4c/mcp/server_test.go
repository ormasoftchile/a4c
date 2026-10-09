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
		"team_delegate":  false,
		"team_list":      false,
		"team_status":    false,
		"team_result":    false,
		"team_diff":      false,
		"team_feedback":  false,
		"team_accept":    false,
		"team_integrate": false,
	}

	for _, tool := range toolsRes.Tools {
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected MCP tool %q was not registered", name)
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

	// Attempt integrate before acceptance -> must fail
	intReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_integrate",
			Arguments: map[string]any{
				"task_id": ref.TaskID,
			},
		},
	}
	intRes, _ := h.client.CallTool(ctx, intReq)
	if !intRes.IsError {
		t.Fatalf("expected error integrating unaccepted task, got success")
	}

	// Complete activity to make it reviewable
	_ = h.store.Complete(ref.TaskID, a4c.ActivityResult{
		ID:     ref.TaskID,
		Status: a4c.StatusCompleted,
	})

	// Call team_accept
	accReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_accept",
			Arguments: map[string]any{
				"task_id":  ref.TaskID,
				"comments": "Approved via MCP",
			},
		},
	}
	accRes, err := h.client.CallTool(ctx, accReq)
	if err != nil || accRes.IsError {
		t.Fatalf("team_accept failed: %v (%+v)", err, accRes)
	}

	var accResult a4c.AcceptResult
	_ = json.Unmarshal([]byte(accRes.Content[0].(mcp.TextContent).Text), &accResult)
	if accResult.TaskID != ref.TaskID || accResult.AcceptedSnapshot == "" {
		t.Fatalf("unexpected accResult: %+v", accResult)
	}

	// Call team_integrate
	intRes2, err := h.client.CallTool(ctx, intReq)
	if err != nil || intRes2.IsError {
		t.Fatalf("team_integrate failed: %v (%+v)", err, intRes2)
	}

	var intResult a4c.IntegrateResult
	_ = json.Unmarshal([]byte(intRes2.Content[0].(mcp.TextContent).Text), &intResult)
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
