package a4c_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c"
	a4cmcp "github.com/ormasoftchile/a4c/pkg/a4c/mcp"
)

func TestMilestone14_CLI_MCP_Parity(t *testing.T) {
	repoDir := t.TempDir()

	// Initialize git repo
	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Parity Tester")
	runGitCmd(t, repoDir, "config", "user.email", "parity@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Parity Test\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "init")

	// Initialize A4C workspace via CLI
	a4cBin := findA4CBinary(t)

	cmdInit := exec.Command(a4cBin, "init", repoDir)
	if out, err := cmdInit.CombinedOutput(); err != nil {
		t.Fatalf("a4c init failed: %s (%v)", string(out), err)
	}

	// 1. Submit task via CLI
	cmdSubmit := exec.Command(a4cBin, "task", "submit", "Implement parity feature", "35000", "--strategy", "direct")
	cmdSubmit.Dir = repoDir
	outSubmit, err := cmdSubmit.CombinedOutput()
	if err != nil {
		t.Fatalf("a4c task submit failed: %s (%v)", string(outSubmit), err)
	}

	// Extract Task ID from CLI output
	var taskID string
	for _, line := range strings.Split(string(outSubmit), "\n") {
		if strings.HasPrefix(line, "Task enqueued: ") {
			taskID = strings.TrimSpace(strings.TrimPrefix(line, "Task enqueued: "))
			break
		}
	}
	if taskID == "" {
		t.Fatalf("could not extract taskID from CLI output: %s", string(outSubmit))
	}

	// 2. Read state via MCP server
	dotDir := filepath.Join(repoDir, ".a4c")
	store, _ := a4c.NewDurableActivityStore(filepath.Join(dotDir, "activities"))
	lifecycleStore, _ := a4c.NewDurableLifecycleStore(filepath.Join(dotDir, "lifecycle"))
	roster, _ := a4c.DefaultTeamRoster(filepath.Join(dotDir, "roster"))
	memory, _ := a4c.NewDurableMemoryStore(filepath.Join(dotDir, "memory"))
	journal, _ := a4c.NewDurableIntentJournal(filepath.Join(dotDir, "journal"))
	wtMgr, _ := a4c.NewWorktreeManager(repoDir, filepath.Join(dotDir, "worktrees"))

	gov := a4c.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch", "governed_submit_review"})
	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &a4c.NativeGoTestRunner{}, nil)
	coord.SetWorktreeManager(wtMgr)

	svc := a4c.NewTeamService(repoDir, store, lifecycleStore, coord, roster, memory, journal, wtMgr)
	mcpServer := a4cmcp.NewServer(svc)

	cli, err := client.NewInProcessClient(mcpServer.MCPServer())
	if err != nil {
		t.Fatalf("create MCP client: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	_ = cli.Start(ctx)
	_, _ = cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "parity-client", Version: "1.0"},
		},
	})

	// Inspect task via MCP team_status
	mcpStatusRes, err := cli.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_status",
			Arguments: map[string]any{"task_id": taskID},
		},
	})
	if err != nil || mcpStatusRes.IsError {
		t.Fatalf("MCP team_status failed: %v (%+v)", err, mcpStatusRes)
	}

	var mcpDetails a4c.TaskDetails
	_ = json.Unmarshal([]byte(mcpStatusRes.Content[0].(mcp.TextContent).Text), &mcpDetails)
	if mcpDetails.TaskID != taskID {
		t.Fatalf("MCP taskID mismatch: %s != %s", mcpDetails.TaskID, taskID)
	}
	if mcpDetails.State != a4c.TaskStateQueued {
		t.Fatalf("MCP state mismatch: %s != queued", mcpDetails.State)
	}

	// 3. Complete activity in durable store
	_ = store.Complete(taskID, a4c.ActivityResult{
		ID:     taskID,
		Status: a4c.StatusCompleted,
	})

	// Inspect via CLI --json
	cmdInspect := exec.Command(a4cBin, "task", "inspect", taskID, "--json")
	cmdInspect.Dir = repoDir
	outInspect, err := cmdInspect.CombinedOutput()
	if err != nil {
		t.Fatalf("a4c task inspect failed: %s (%v)", string(outInspect), err)
	}
	var cliHistory a4c.TaskHistoryReport
	if err := json.Unmarshal(outInspect, &cliHistory); err != nil {
		t.Fatalf("unmarshal CLI inspect output: %v", err)
	}
	if cliHistory.TaskID != taskID {
		t.Fatalf("CLI inspect taskID mismatch: %s != %s", cliHistory.TaskID, taskID)
	}

	// 4. Accept via MCP must fail closed (Milestone 16 security hardening)
	mcpAcceptRes, accErr := cli.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "team_accept",
			Arguments: map[string]any{
				"task_id":  taskID,
				"comments": "Signed off via MCP in parity test",
			},
		},
	})
	if accErr == nil && (mcpAcceptRes == nil || !mcpAcceptRes.IsError) {
		t.Fatalf("expected MCP team_accept to fail closed with unauthorized error, got success")
	}

	// Accept via CLI
	cmdAccept := exec.Command(a4cBin, "task", "accept", taskID, "--comments", "Signed off via CLI")
	cmdAccept.Dir = repoDir
	outAccept, err := cmdAccept.CombinedOutput()
	if err != nil {
		t.Fatalf("a4c task accept failed: %s (%v)", string(outAccept), err)
	}

	// 5. Integrate via CLI
	cmdIntegrate := exec.Command(a4cBin, "task", "integrate", taskID)
	cmdIntegrate.Dir = repoDir
	outIntegrate, err := cmdIntegrate.CombinedOutput()
	if err != nil {
		t.Fatalf("a4c task integrate failed: %s (%v)", string(outIntegrate), err)
	}

	// 6. Verify final state via both CLI and MCP
	mcpStatusFinal, err := cli.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_status",
			Arguments: map[string]any{"task_id": taskID},
		},
	})
	if err != nil || mcpStatusFinal.IsError {
		t.Fatalf("final MCP team_status failed: %v", err)
	}

	var mcpFinalDetails a4c.TaskDetails
	_ = json.Unmarshal([]byte(mcpStatusFinal.Content[0].(mcp.TextContent).Text), &mcpFinalDetails)
	if mcpFinalDetails.State != a4c.TaskStateIntegrated {
		t.Fatalf("expected integrated state in MCP, got %s", mcpFinalDetails.State)
	}
	if mcpFinalDetails.LifecycleFacts == nil || !mcpFinalDetails.LifecycleFacts.Integrated {
		t.Fatalf("expected lifecycle facts integrated=true")
	}
}

func TestMilestone14_WorkerHost_ConcurrencyAndLocking(t *testing.T) {
	repoDir := t.TempDir()

	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Lock Tester")
	runGitCmd(t, repoDir, "config", "user.email", "lock@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Lock Test\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "init")

	dotDir := filepath.Join(repoDir, ".a4c")
	store, _ := a4c.NewDurableActivityStore(filepath.Join(dotDir, "activities"))
	roster, _ := a4c.DefaultTeamRoster(filepath.Join(dotDir, "roster"))
	memory, _ := a4c.NewDurableMemoryStore(filepath.Join(dotDir, "memory"))
	journal, _ := a4c.NewDurableIntentJournal(filepath.Join(dotDir, "journal"))
	gov := a4c.NewPolicyGovernor([]string{"governed_read_file", "governed_apply_patch"})

	fake := a4c.NewFakeProvider()
	fake.SetSessionResponseFunc(func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
		return &a4c.AgentMessage{Role: "assistant", Content: "Done"}, nil
	})

	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &workerMockTestRunner{}, fake)

	cfg := a4c.WorkerHostConfig{
		WorkspaceDir: repoDir,
		PollInterval: 25 * time.Millisecond,
		MaxSteps:     0, // Daemon mode
	}
	host := a4c.NewWorkerHost(cfg, coord, store, journal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = host.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Second worker host attempting to execute concurrently must be rejected
	coord2 := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, &workerMockTestRunner{}, fake)
	host2 := a4c.NewWorkerHost(cfg, coord2, store, journal)
	err2 := host2.Run(context.Background())
	if !errors.Is(err2, a4c.ErrWorkspaceLocked) {
		t.Fatalf("expected ErrWorkspaceLocked for second host, got %v", err2)
	}

	// While host is running, enqueuing tasks into store must succeed
	actReq := a4c.ActivityRequest{
		ID:        "bg-task-1",
		AgentID:   "coder",
		Objective: "Background processed job",
		Budget:    a4c.Budget{Allocated: 10000},
		CreatedAt: time.Now(),
	}
	if err := store.Enqueue(actReq); err != nil {
		t.Fatalf("enqueue while host running failed: %v", err)
	}

	// Wait for host to claim and complete activity
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		rec, err := store.Get("bg-task-1")
		if err == nil && rec.Status == a4c.StatusCompleted {
			break
		}
	}

	rec, _ := store.Get("bg-task-1")
	if rec.Status != a4c.StatusCompleted {
		t.Fatalf("expected activity completed by running host, got %s", rec.Status)
	}

	cancel()
	wg.Wait()
}

func TestMilestone14_MCP_Stdio_Subprocess(t *testing.T) {
	repoDir := t.TempDir()

	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.name", "Stdio Tester")
	runGitCmd(t, repoDir, "config", "user.email", "stdio@example.com")
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Stdio Test\n"), 0644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "init")

	a4cBin := findA4CBinary(t)

	cmdInit := exec.Command(a4cBin, "init", repoDir)
	if out, err := cmdInit.CombinedOutput(); err != nil {
		t.Fatalf("a4c init failed: %s (%v)", string(out), err)
	}

	// Launch a4c mcp as stdio MCP client
	stdioClient, err := client.NewStdioMCPClient(a4cBin, nil, "mcp", "--dir", repoDir)
	if err != nil {
		t.Fatalf("NewStdioMCPClient failed: %v", err)
	}
	defer stdioClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := stdioClient.Start(ctx); err != nil {
		t.Fatalf("stdio client start failed: %v", err)
	}

	initRes, err := stdioClient.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "stdio-test-client", Version: "1.0"},
		},
	})
	if err != nil {
		t.Fatalf("stdio initialize failed: %v", err)
	}
	if initRes.ServerInfo.Name != "a4c-team" {
		t.Fatalf("unexpected server name: %s", initRes.ServerInfo.Name)
	}

	// Call team_list
	listRes, err := stdioClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "team_list",
			Arguments: map[string]any{},
		},
	})
	if err != nil || listRes.IsError {
		t.Fatalf("stdio call team_list failed: %v (%+v)", err, listRes)
	}
}

func findA4CBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../a4c",
		"../a4c",
		"./a4c",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err == nil {
			info, err := os.Stat(abs)
			if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return abs
			}
		}
	}

	tmpBin := filepath.Join(t.TempDir(), "a4c")
	cmd := exec.Command("go", "build", "-o", tmpBin, "github.com/ormasoftchile/a4c/cmd/a4c")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build a4c binary for test: %s (%v)", string(out), err)
	}
	return tmpBin
}
