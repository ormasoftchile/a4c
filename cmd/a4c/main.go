package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	a4cmcp "github.com/ormasoftchile/a4c/pkg/a4c/mcp"
	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
)

const banner = `
   ▄████████     ▄████████   ▄████████ 
  ███    ███    ███    ███  ███    ███ 
  ███    ███    ███    █▀   ███    █▀  
  ███    ███   ▄███▄▄▄      ███        
▀███████████  ▀▀███▀▀▀    ▀███████████ 
  ███    ███  ▀███████████         ███ 
  ███    ███    ███    ███   ▄█    ███ 
  ███    █▀     ███    ███ ▄████████▀  
                ███    ███             
Agents for Coding — Governed Local AI Engineering Team
`

func printUsage() {
	fmt.Print(banner)
	fmt.Print(`Usage: a4c <command> [options]

Commands:
  init [path]                           Initialize an a4c workspace (.a4c directory)
  install <copilot|opencode|all>        Install host-native agent definitions (.github/agents, .opencode/agents)
  doctor [--dir path] [--json]          Diagnose environment, host agents, and durable stores
  team list                             List persistent team members and permissions
  task submit <obj> [budget] [--strategy s] [--specialist s] [--runtime r] [--model m] Submit a task (default: direct)
  task start <obj> [budget] [--strategy s] [--specialist s] [--runtime r] [--model m] [--steps N] Submit and immediately execute task
  task run [task-id] [--steps N] [--runtime r] [--model m] Execute pending task activities
  task resume <task-id> [--runtime r] [--model m] [--specialist s] [--run] Resume interrupted task on another host
  task adopt <obj> [--notes text] [--runtime r] [--model m] [--strategy s] [--in-place] [--run] Adopt external work
  task escalate <task-id> <reviewed|team> [--reviewer r] Escalate task to reviewed or team collaboration
  task checkpoint <task-id> [--json]    Capture authoritative task checkpoint
  task list [--state s] [--status s] [--agent a] [--runtime r] Query tasks or activities
  task history <task-id>                Display full audit history and DAG for a task
  task inspect <task-id> [--json]       Inspect a task; output machine-readable JSON with --json
  task diff <task-id> [--json]          Display worktree review diffs against base branch
  task feedback <task-id> <feedback>    Provide review guidance and request revisions
  task accept <task-id> [comments]      Record explicit human approval of reviewed snapshot
  task integrate <task-id>              Integrate approved task into base checkout
  task reject <task-id> <reason>        Reject task
  host [--steps N] [--poll-ms MS]       Run durable background worker host
  mcp                                   Run Model Context Protocol (MCP) server over stdio
  status                                Display active activities and evidence
  run [--runtime r] [--model m] [--real-copilot] [--steps N] Run governed collaboration loop
  decisions list                        List questions awaiting human engineer guidance
  decisions resolve <id> <resolution>   Resolve decision and resume paused task
  memory list                           List verified architectural conventions
  knowledge <enable|disable|status|...> Manage the optional A4C Knowledge module
  demo [--real-copilot]                 Run full self-contained demonstration

Flags:
  --real-copilot                        Connect to official GitHub Copilot CLI via Go SDK
  --strategy direct|reviewed|team       Execution strategy (default: direct)
  --steps N                             Maximum sequential steps to execute (default: 10)
  --dir path                            Workspace root directory (default: current directory)
`)

}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "help", "-h", "--help":
		printUsage()

	case "init":
		runInit(args)

	case "install":
		runInstall(args)

	case "doctor":
		runDoctor(args)

	case "team":
		runTeam(args)

	case "task":
		runTask(args)

	case "adopt":
		runTask(append([]string{"adopt"}, args...))

	case "host":
		runHost(args)

	case "mcp":
		runMCP(args)

	case "status":
		runStatus(args)

	case "run":
		runLoop(args)

	case "decisions":
		runDecisions(args)

	case "memory":
		runMemory(args)

	case "knowledge":
		runKnowledge(args)

	case "demo":
		runDemo(args)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q. Run 'a4c help' for usage.\n", cmd)
		os.Exit(1)
	}
}

func runInstall(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: a4c install <copilot|opencode|antigravity|all> [--dir <path>]")
		return
	}
	target := args[0]
	workDir := "."
	for i := 1; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		}
	}

	switch target {
	case "copilot":
		if err := a4c.ExportCopilotAgents(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install Copilot agents: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Installed GitHub Copilot agents (.github/agents/a4c.agent.md, bob.agent.md, charlie.agent.md, .mcp.json) in %s\n", workDir)

	case "opencode":
		if err := a4c.ExportOpenCodeAgents(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install OpenCode agents: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Installed OpenCode agents (.opencode/agents/a4c.md, bob.md, charlie.md, opencode.json) in %s\n", workDir)

	case "antigravity":
		if err := a4c.ExportAntigravityCustomization(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install Antigravity customization: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Installed Antigravity skill customization in %s\n", workDir)

	case "all":
		_ = a4c.ExportCopilotAgents(workDir)
		_ = a4c.ExportOpenCodeAgents(workDir)
		_ = a4c.ExportAntigravityCustomization(workDir)
		fmt.Printf("✅ Installed all host-native agent definitions (Copilot, OpenCode, Antigravity) in %s\n", workDir)

	default:
		fmt.Printf("Unknown install target %q. Available targets: copilot, opencode, antigravity, all\n", target)
		os.Exit(1)
	}
}

func runDoctor(args []string) {
	workDir := "."
	jsonOutput := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		} else if args[i] == "--json" {
			jsonOutput = true
		}
	}

	rep := a4c.RunDoctor(workDir)
	if jsonOutput {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		rep.Print()
	}
	if !rep.AllPassed {
		os.Exit(1)
	}
}

func getPaths(workDir string) (string, string, string, string, string) {
	if workDir == "" {
		workDir = "."
	}
	dotDir := filepath.Join(workDir, ".a4c")
	storeDir := filepath.Join(dotDir, "activities")
	rosterDir := filepath.Join(dotDir, "roster")
	memDir := filepath.Join(dotDir, "memory")
	journalDir := filepath.Join(dotDir, "journal")
	return dotDir, storeDir, rosterDir, memDir, journalDir
}

func runInit(args []string) {
	target := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
	}

	dotDir, storeDir, rosterDir, memDir, journalDir := getPaths(target)

	dirs := []string{
		dotDir, storeDir, rosterDir, memDir, journalDir,
		filepath.Join(dotDir, "worktrees"),
		filepath.Join(dotDir, "knowledge"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create directory %s: %v\n", d, err)
			os.Exit(1)
		}
	}

	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize roster: %v\n", err)
		os.Exit(1)
	}

	_, _ = a4c.NewDurableActivityStore(storeDir)
	_, _ = a4c.NewDurableMemoryStore(memDir)
	_, _ = a4c.NewDurableIntentJournal(journalDir)

	fmt.Printf("Initialized a4c workspace in %s\n", target)
	fmt.Printf("Persistent team active with %d members:\n", len(roster.ListMembers()))
	for _, m := range roster.ListMembers() {
		fmt.Printf("  - %s (%s)\n", m.Name, m.Role)
	}
}

func runTeam(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: a4c team [list | add <id> <name> <role> <charter>]")
		return
	}

	_, _, rosterDir, _, _ := getPaths(".")
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load team roster: %v (run 'a4c init' first)\n", err)
		os.Exit(1)
	}

	subCmd := args[0]
	switch subCmd {
	case "list":
		fmt.Printf("Persistent Team Roster (%d members):\n", len(roster.ListMembers()))
		for _, m := range roster.ListMembers() {
			fmt.Printf("\n[%s] %s\n", m.ID, m.Name)
			fmt.Printf("  Role:         %s\n", m.Role)
			fmt.Printf("  Specialties:  %v\n", m.Specialties)
			fmt.Printf("  Allowed Tools:%v\n", m.AllowedTools)
			fmt.Printf("  Charter:      %s\n", m.Charter)
		}

	case "add":
		if len(args) < 5 {
			fmt.Println("Usage: a4c team add <id> <name> <role> <charter>")
			return
		}
		ident := a4c.AgentIdentity{
			ID:           args[1],
			Name:         args[2],
			Role:         args[3],
			Charter:      args[4],
			AllowedTools: []string{"governed_read_file"},
		}
		if err := roster.RegisterMember(ident); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register member: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Registered team member %s (%s)\n", ident.Name, ident.ID)
	}
}

func buildService(workDir string) (*a4c.TeamServiceImpl, error) {
	if workDir == "" {
		workDir = "."
	}
	dotDir, storeDir, rosterDir, memDir, journalDir := getPaths(workDir)
	lifecycleDir := filepath.Join(dotDir, "lifecycle")
	wtDir := filepath.Join(dotDir, "worktrees")

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open activity store: %w", err)
	}
	lifecycleStore, err := a4c.NewDurableLifecycleStore(lifecycleDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open lifecycle store: %w", err)
	}
	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open roster: %w", err)
	}
	memStore, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open memory store: %w", err)
	}
	journal, err := a4c.NewDurableIntentJournal(journalDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open journal: %w", err)
	}
	wtMgr, err := a4c.NewWorktreeManager(workDir, wtDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open worktree manager: %w", err)
	}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		"knowledge_record", "knowledge_query",
	})
	coord := a4c.NewTeamCoordinator(workDir, store, roster, memStore, gov, journal, a4c.NewProjectTestRunner(), nil)
	coord.SetWorktreeManager(wtMgr)

	// Wire the optional A4C Knowledge service (ErrDisabled is expected when off).
	if ks, kerr := knowledge.NewService(workDir, func(activityID string) bool {
		rec, err := store.Get(activityID)
		return err == nil && rec != nil && rec.Status == a4c.StatusClaimed
	}); kerr == nil {
		coord.SetKnowledge(ks)
	} else if !errors.Is(kerr, knowledge.ErrDisabled) {
		fmt.Fprintf(os.Stderr, "knowledge disabled: %v\n", kerr)
	}

	return a4c.NewTeamService(workDir, store, lifecycleStore, coord, roster, memStore, journal, wtMgr), nil
}

func specialistOrFallback(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func configureProviderForCoordinator(coord *a4c.TeamCoordinator, runtime string, model string, realCopilot bool, opencodeEndpoint string) (func(), error) {
	var cleanupFuncs []func()
	cleanup := func() {
		for _, f := range cleanupFuncs {
			f()
		}
	}

	// 1. Auto-discover and register all available hosts into coord.Providers for seamless cross-host routing
	// OpenCode
	endpoint := opencodeEndpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:4096"
	}
	op := a4c.NewOpenCodeProvider(endpoint, model, nil)
	cleanupFuncs = append(cleanupFuncs, func() { op.Close() })
	coord.RegisterProvider("opencode", op)

	// GitHub Copilot
	if cp, err := a4c.NewCopilotProvider(""); err == nil {
		cleanupFuncs = append(cleanupFuncs, func() { cp.Close() })
		coord.RegisterProvider("copilot", cp)
	}

	// Antigravity CLI
	if agy, err := a4c.NewAntigravityProvider(model); err == nil {
		coord.RegisterProvider("antigravity", agy)
		coord.RegisterProvider("agy", agy)
	}

	// In-memory fake provider
	fake := a4c.NewFakeProvider()
	coord.RegisterProvider("fake", fake)

	// 2. Select default provider based on runtime parameter
	switch runtime {
	case "copilot":
		if cp, ok := coord.Providers["copilot"]; ok {
			coord.SetDefaultProvider(cp)
		} else {
			return cleanup, fmt.Errorf("copilot provider requested but unavailable")
		}
	case "opencode":
		coord.SetDefaultProvider(op)
	case "antigravity", "agy":
		if agy, ok := coord.Providers["antigravity"]; ok {
			coord.SetDefaultProvider(agy)
		} else {
			return cleanup, fmt.Errorf("antigravity provider requested but agy binary not found")
		}
	case "fake":
		coord.SetDefaultProvider(fake)
	case "":
		if realCopilot {
			if cp, ok := coord.Providers["copilot"]; ok {
				coord.SetDefaultProvider(cp)
			} else {
				coord.SetDefaultProvider(op)
			}
		} else if _, ok := coord.Providers["opencode"]; ok {
			coord.SetDefaultProvider(op)
		} else {
			coord.SetDefaultProvider(fake)
		}
	default:
		return cleanup, fmt.Errorf("unknown runtime provider %q (expected copilot, opencode, antigravity, or fake)", runtime)
	}

	return cleanup, nil
}

func runTask(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: a4c task <submit|start|run|resume|escalate|checkpoint|list|history|inspect|diff|feedback|accept|integrate|reject> [options]")
		return
	}

	workDir := "."
	var cleanArgs []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		} else {
			cleanArgs = append(cleanArgs, args[i])
		}
	}
	args = cleanArgs
	if len(args) == 0 {
		fmt.Println("Usage: a4c task <submit|list|history|inspect|diff|feedback|accept|integrate|reject> [options]")
		return
	}

	ctx := context.Background()
	svc, err := buildService(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init': %v\n", err)
		os.Exit(1)
	}

	subCmd := args[0]
	switch subCmd {
	case "submit":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task submit <objective> [budget_tokens] [--strategy direct|reviewed|team] [--specialist <agent>] [--runtime copilot|opencode] [--model <model>]")
			return
		}
		objective := args[1]
		budgetTokens := 50000.0
		strategy := a4c.StrategyDirect
		runtime := ""
		model := ""
		specialist := ""

		reviewerRuntime := ""
		reviewerModel := ""
		agentRuntimes := make(map[string]string)

		for i := 2; i < len(args); i++ {
			if args[i] == "--strategy" && i+1 < len(args) {
				strategy = a4c.ExecutionStrategy(args[i+1])
				i++
			} else if args[i] == "--runtime" && i+1 < len(args) {
				runtime = args[i+1]
				i++
			} else if args[i] == "--model" && i+1 < len(args) {
				model = args[i+1]
				i++
			} else if args[i] == "--reviewer-runtime" && i+1 < len(args) {
				reviewerRuntime = args[i+1]
				i++
			} else if args[i] == "--reviewer-model" && i+1 < len(args) {
				reviewerModel = args[i+1]
				i++
			} else if args[i] == "--agent-runtimes" && i+1 < len(args) {
				for _, p := range strings.Split(args[i+1], ",") {
					parts := strings.SplitN(p, "=", 2)
					if len(parts) == 2 {
						agentRuntimes[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
					}
				}
				i++
			} else if args[i] == "--specialist" && i+1 < len(args) {
				specialist = args[i+1]
				i++
			} else if b, err := strconv.ParseFloat(args[i], 64); err == nil {
				budgetTokens = b
			}
		}

		submitInputs := make(map[string]any)
		if reviewerRuntime != "" {
			submitInputs["reviewer_runtime"] = reviewerRuntime
		}
		if reviewerModel != "" {
			submitInputs["reviewer_model"] = reviewerModel
		}
		if len(agentRuntimes) > 0 {
			submitInputs["agent_runtimes"] = agentRuntimes
		}

		ref, err := svc.SubmitTask(ctx, a4c.SubmitTaskRequest{
			Objective:    objective,
			BudgetTokens: budgetTokens,
			Strategy:     strategy,
			Specialist:   specialist,
			Runtime:      runtime,
			Model:        model,
			Inputs:       submitInputs,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to enqueue task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task enqueued: %s\nStrategy: %s\nState: %s\nObjective: %s\nBudget: %.0f tokens\n",
			ref.TaskID, strategy, ref.State, objective, budgetTokens)

	case "start":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task start <objective> [budget_tokens] [--strategy direct|reviewed|team] [--specialist <agent>] [--runtime copilot|opencode|antigravity] [--model <model>] [--reviewer-runtime <r>] [--reviewer-model <m>] [--agent-runtimes a1=r1,a2=r2] [--steps N]")
			return
		}
		objective := args[1]
		budgetTokens := 50000.0
		strategy := a4c.StrategyDirect
		runtime := ""
		model := ""
		reviewerRuntime := ""
		reviewerModel := ""
		startAgentRuntimes := make(map[string]string)
		specialist := ""
		steps := 10
		realCopilot := false
		opencodeEndpoint := ""

		for i := 2; i < len(args); i++ {
			if args[i] == "--strategy" && i+1 < len(args) {
				strategy = a4c.ExecutionStrategy(args[i+1])
				i++
			} else if args[i] == "--runtime" && i+1 < len(args) {
				runtime = args[i+1]
				i++
			} else if args[i] == "--model" && i+1 < len(args) {
				model = args[i+1]
				i++
			} else if args[i] == "--reviewer-runtime" && i+1 < len(args) {
				reviewerRuntime = args[i+1]
				i++
			} else if args[i] == "--reviewer-model" && i+1 < len(args) {
				reviewerModel = args[i+1]
				i++
			} else if args[i] == "--agent-runtimes" && i+1 < len(args) {
				for _, p := range strings.Split(args[i+1], ",") {
					parts := strings.SplitN(p, "=", 2)
					if len(parts) == 2 {
						startAgentRuntimes[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
					}
				}
				i++
			} else if args[i] == "--specialist" && i+1 < len(args) {
				specialist = args[i+1]
				i++
			} else if args[i] == "--steps" && i+1 < len(args) {
				if s, err := strconv.Atoi(args[i+1]); err == nil {
					steps = s
				}
				i++
			} else if args[i] == "--real-copilot" {
				realCopilot = true
			} else if args[i] == "--opencode-endpoint" && i+1 < len(args) {
				opencodeEndpoint = args[i+1]
				i++
			} else if b, err := strconv.ParseFloat(args[i], 64); err == nil {
				budgetTokens = b
			}
		}

		startInputs := make(map[string]any)
		if reviewerRuntime != "" {
			startInputs["reviewer_runtime"] = reviewerRuntime
		}
		if reviewerModel != "" {
			startInputs["reviewer_model"] = reviewerModel
		}
		if len(startAgentRuntimes) > 0 {
			startInputs["agent_runtimes"] = startAgentRuntimes
		}

		ref, err := svc.SubmitTask(ctx, a4c.SubmitTaskRequest{
			Objective:    objective,
			BudgetTokens: budgetTokens,
			Strategy:     strategy,
			Specialist:   specialist,
			Runtime:      runtime,
			Model:        model,
			Inputs:       startInputs,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to submit task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task started: %s\nStrategy: %s\nObjective: %s\nExecuting with %s (max %d steps)...\n",
			ref.TaskID, strategy, objective, specialistOrFallback(specialist, "coder"), steps)

		cleanup, err := configureProviderForCoordinator(svc.Coordinator(), runtime, model, realCopilot, opencodeEndpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Provider initialization warning: %v\n", err)
		}
		if cleanup != nil {
			defer cleanup()
		}

		results, err := svc.ExecuteTask(ctx, ref.TaskID, steps)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Task execution encountered error: %v\n", err)
		}

		fmt.Printf("\nExecuted %d activities for task %s:\n", len(results), ref.TaskID)
		for _, res := range results {
			agentID := ""
			if rec, err := svc.Store().Get(res.ID); err == nil && rec != nil {
				agentID = rec.Request.AgentID
			}
			fmt.Printf("  -> [%s] Agent: %-10s Status: %-12s Consumed: %.0f tokens\n",
				res.ID, agentID, res.Status, res.Consumed.Consumed)
		}

		taskDetails, err := svc.GetTask(ctx, ref.TaskID)
		if err == nil {
			fmt.Printf("\nCurrent State: %s\nWorktree: %s (Branch: %s)\n",
				taskDetails.State, taskDetails.WorktreePath, taskDetails.WorktreeBranch)
			if taskDetails.State == a4c.TaskStateAwaitingHumanReview {
				fmt.Printf("Changes ready for inspection. Run 'a4c task diff %s' or 'a4c task accept %s'.\n", ref.TaskID, ref.TaskID)
			}
		}

	case "run":
		taskID := ""
		steps := 10
		runtime := ""
		model := ""
		realCopilot := false
		opencodeEndpoint := ""

		for i := 1; i < len(args); i++ {
			if args[i] == "--steps" && i+1 < len(args) {
				if s, err := strconv.Atoi(args[i+1]); err == nil {
					steps = s
				}
				i++
			} else if args[i] == "--runtime" && i+1 < len(args) {
				runtime = args[i+1]
				i++
			} else if args[i] == "--model" && i+1 < len(args) {
				model = args[i+1]
				i++
			} else if args[i] == "--real-copilot" {
				realCopilot = true
			} else if args[i] == "--opencode-endpoint" && i+1 < len(args) {
				opencodeEndpoint = args[i+1]
				i++
			} else if !strings.HasPrefix(args[i], "--") && taskID == "" {
				taskID = args[i]
			}
		}

		cleanup, err := configureProviderForCoordinator(svc.Coordinator(), runtime, model, realCopilot, opencodeEndpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Provider initialization warning: %v\n", err)
		}
		if cleanup != nil {
			defer cleanup()
		}

		var results []*a4c.ActivityResult
		if taskID != "" {
			results, err = svc.ExecuteTask(ctx, taskID, steps)
		} else {
			for s := 0; s < steps; s++ {
				if !svc.Store().HasClaimable() {
					break
				}
				res, stepErr := svc.Coordinator().Step(ctx)
				if stepErr != nil {
					err = stepErr
					break
				}
				if res == nil {
					break
				}
				results = append(results, res)
			}
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "Execution encountered error: %v\n", err)
		}

		fmt.Printf("Executed %d activities:\n", len(results))
		for _, res := range results {
			agentID := ""
			if rec, err := svc.Store().Get(res.ID); err == nil && rec != nil {
				agentID = rec.Request.AgentID
			}
			fmt.Printf("  -> [%s] Agent: %-10s Status: %-12s Consumed: %.0f tokens\n",
				res.ID, agentID, res.Status, res.Consumed.Consumed)
		}

	case "resume":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task resume <task-id> [--runtime <r>] [--model <m>] [--specialist <s>] [--instructions <text>] [--run] [--steps N]")
			return
		}
		taskID := args[1]
		runtime := ""
		model := ""
		specialist := ""
		instructions := ""
		runImmediately := false
		steps := 10
		realCopilot := false
		opencodeEndpoint := ""

		for i := 2; i < len(args); i++ {
			if args[i] == "--runtime" && i+1 < len(args) {
				runtime = args[i+1]
				i++
			} else if args[i] == "--model" && i+1 < len(args) {
				model = args[i+1]
				i++
			} else if args[i] == "--specialist" && i+1 < len(args) {
				specialist = args[i+1]
				i++
			} else if args[i] == "--instructions" && i+1 < len(args) {
				instructions = args[i+1]
				i++
			} else if args[i] == "--run" {
				runImmediately = true
			} else if args[i] == "--real-copilot" {
				realCopilot = true
			} else if args[i] == "--opencode-endpoint" && i+1 < len(args) {
				opencodeEndpoint = args[i+1]
				i++
			} else if args[i] == "--steps" && i+1 < len(args) {
				if s, err := strconv.Atoi(args[i+1]); err == nil {
					steps = s
				}
				i++
			}
		}

		chkPreview, _ := svc.CaptureCheckpoint(ctx, taskID)
		if runtime == "" && chkPreview != nil {
			alternatives, _ := svc.DiscoverAlternativeHosts(ctx, chkPreview.SourceRuntime)
			if len(alternatives) > 0 {
				fmt.Printf("A4C Task: %s\n\n", taskID)
				if chkPreview.SourceRuntime != "" {
					fmt.Printf("Host: %s\n", chkPreview.SourceRuntime)
				}
				stateDesc := "Interrupted"
				fmt.Printf("Status: %s\n\n", stateDesc)
				if chkPreview.FailureReason != "" {
					fmt.Printf("Reason: %s\n", chkPreview.FailureReason)
					if chkPreview.InterruptionMessage != "" {
						fmt.Printf("Details: %s\n", chkPreview.InterruptionMessage)
					}
					fmt.Println()
				}
				fmt.Printf("Engineering state preserved:\n")
				fmt.Printf("  Worktree: intact\n")
				fmt.Printf("  Modified files: %d\n", len(chkPreview.ModifiedFiles))
				fmt.Printf("  New files: %d\n", len(chkPreview.NewFiles))
				fmt.Printf("  Completed obligations: %d\n", len(chkPreview.CompletedWork))
				fmt.Printf("  Remaining obligations: %d\n", len(chkPreview.RemainingObligations))

				fmt.Printf("\nAvailable execution hosts:\n")
				for idx, alt := range alternatives {
					fmt.Printf("  %d. %s\n", idx+1, alt.DisplayName)
				}

				// If interactive TTY, prompt user for selection
				stat, statErr := os.Stdin.Stat()
				if statErr == nil && (stat.Mode()&os.ModeCharDevice) != 0 {
					fmt.Print("\nChoose a host to continue: ")
					var input string
					fmt.Scanln(&input)
					input = strings.TrimSpace(input)
					if idx, err := strconv.Atoi(input); err == nil && idx >= 1 && idx <= len(alternatives) {
						runtime = alternatives[idx-1].HostID
					} else if input != "" {
						runtime = input
					}
				}
				if runtime == "" {
					fmt.Printf("\nChoose a host by running: a4c task resume %s --runtime <host> --run\n", taskID)
					return
				}
			}
		}

		chk, err := svc.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{
			TaskID:       taskID,
			Runtime:      runtime,
			Model:        model,
			Specialist:   specialist,
			Instructions: instructions,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to resume task %s: %v\n", taskID, err)
			os.Exit(1)
		}

		fmt.Printf("Task %s resumed successfully.\n", taskID)
		fmt.Printf("  Worktree:        %s (Branch: %s)\n", chk.WorktreePath, chk.WorktreeBranch)
		fmt.Printf("  Active Activity: %s (Specialist: %s)\n", chk.ActiveActivityID, chk.SpecialistID)
		if chk.TargetRuntime != "" {
			fmt.Printf("  Target Runtime:  %s (Model: %s)\n", chk.TargetRuntime, chk.Model)
		}
		if chk.GitStatusSummary != "" {
			fmt.Printf("  Git Status:\n    %s\n", strings.ReplaceAll(chk.GitStatusSummary, "\n", "\n    "))
		}
		if len(chk.CompletedWork) > 0 {
			fmt.Printf("  Completed Work:  %d items\n", len(chk.CompletedWork))
		}
		if len(chk.RemainingObligations) > 0 {
			fmt.Printf("  Remaining:       %d obligations\n", len(chk.RemainingObligations))
		}

		if runImmediately {
			fmt.Printf("\nExecuting resumed task %s...\n", taskID)
			cleanup, cErr := configureProviderForCoordinator(svc.Coordinator(), runtime, model, realCopilot, opencodeEndpoint)
			if cErr != nil {
				fmt.Fprintf(os.Stderr, "Provider warning: %v\n", cErr)
			}
			if cleanup != nil {
				defer cleanup()
			}
			results, xErr := svc.ExecuteTask(ctx, taskID, steps)
			if xErr != nil {
				fmt.Fprintf(os.Stderr, "Execution warning: %v\n", xErr)
			}
			fmt.Printf("Executed %d activities upon resumption.\n", len(results))
		}

	case "adopt":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task adopt <objective> [--notes <text>] [--runtime <r>] [--model <m>] [--strategy <s>] [--in-place] [--run] [--steps N]")
			return
		}
		objective := args[1]
		notes := ""
		runtime := ""
		model := ""
		strategy := a4c.StrategyDirect
		inPlace := false
		runImmediately := false
		steps := 10
		realCopilot := false
		opencodeEndpoint := ""

		for i := 2; i < len(args); i++ {
			if args[i] == "--notes" && i+1 < len(args) {
				notes = args[i+1]
				i++
			} else if args[i] == "--runtime" && i+1 < len(args) {
				runtime = args[i+1]
				i++
			} else if args[i] == "--model" && i+1 < len(args) {
				model = args[i+1]
				i++
			} else if args[i] == "--strategy" && i+1 < len(args) {
				strategy = a4c.ExecutionStrategy(args[i+1])
				i++
			} else if args[i] == "--in-place" {
				inPlace = true
			} else if args[i] == "--run" {
				runImmediately = true
			} else if args[i] == "--real-copilot" {
				realCopilot = true
			} else if args[i] == "--opencode-endpoint" && i+1 < len(args) {
				opencodeEndpoint = args[i+1]
				i++
			} else if args[i] == "--steps" && i+1 < len(args) {
				if s, err := strconv.Atoi(args[i+1]); err == nil {
					steps = s
				}
				i++
			}
		}

		res, err := svc.AdoptExistingWork(ctx, a4c.AdoptWorkRequest{
			Objective:    objective,
			HandoffNotes: notes,
			Runtime:      runtime,
			Model:        model,
			Strategy:     strategy,
			InPlace:      inPlace,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to adopt existing work: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Adopted existing engineering work into Task %s\n", res.TaskID)
		fmt.Printf("  Branch:          %s\n", res.Branch)
		fmt.Printf("  Workspace:       %s\n", res.WorkspacePath)
		fmt.Printf("  Preserved state: %d modified files, %d new files\n", len(res.ModifiedFiles), len(res.NewFiles))
		for _, m := range res.ModifiedFiles {
			fmt.Printf("    * [MODIFIED]: %s\n", m)
		}
		for _, n := range res.NewFiles {
			fmt.Printf("    * [NEW FILE]: %s\n", n)
		}
		if runtime != "" {
			fmt.Printf("  Target host:     %s (Model: %s)\n", runtime, model)
		}

		if runImmediately {
			fmt.Printf("\nExecuting adopted task %s...\n", res.TaskID)
			cleanup, cErr := configureProviderForCoordinator(svc.Coordinator(), runtime, model, realCopilot, opencodeEndpoint)
			if cErr != nil {
				fmt.Fprintf(os.Stderr, "Provider warning: %v\n", cErr)
			}
			if cleanup != nil {
				defer cleanup()
			}
			execResults, xErr := svc.ExecuteTask(ctx, res.TaskID, steps)
			if xErr != nil {
				fmt.Fprintf(os.Stderr, "Execution warning: %v\n", xErr)
			}
			fmt.Printf("Executed %d activities for adopted task %s.\n", len(execResults), res.TaskID)
		}

	case "escalate":
		if len(args) < 3 {
			fmt.Println("Usage: a4c task escalate <task-id> <reviewed|team> [--reviewer <agent>]")
			return
		}
		taskID := args[1]
		newStrategy := a4c.ExecutionStrategy(args[2])
		reviewer := ""

		for i := 3; i < len(args); i++ {
			if args[i] == "--reviewer" && i+1 < len(args) {
				reviewer = args[i+1]
				i++
			}
		}

		ref, err := svc.EscalateTask(ctx, a4c.EscalateTaskRequest{
			TaskID:      taskID,
			NewStrategy: newStrategy,
			Reviewer:    reviewer,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to escalate task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task %s escalated to strategy %q. Current State: %s\n", ref.TaskID, newStrategy, ref.State)

	case "checkpoint":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task checkpoint <task-id> [--json]")
			return
		}
		taskID := args[1]
		chk, err := svc.CaptureCheckpoint(ctx, taskID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to capture checkpoint for task %s: %v\n", taskID, err)
			os.Exit(1)
		}
		if len(args) > 2 && args[2] == "--json" {
			b, _ := json.MarshalIndent(chk, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Printf("Checkpoint for Task %s:\n", chk.TaskID)
			fmt.Printf("  Branch:      %s\n", chk.WorktreeBranch)
			fmt.Printf("  Worktree:    %s\n", chk.WorktreePath)
			fmt.Printf("  Specialist:  %s (%s)\n", chk.SpecialistID, chk.SpecialistRole)
			if chk.GitStatusSummary != "" {
				fmt.Printf("  Git Status:\n    %s\n", strings.ReplaceAll(chk.GitStatusSummary, "\n", "\n    "))
			} else {
				fmt.Printf("  Git Status:  clean\n")
			}
			fmt.Printf("  Completed Work (%d):\n", len(chk.CompletedWork))
			for _, w := range chk.CompletedWork {
				fmt.Printf("    * %s\n", w)
			}
			fmt.Printf("  Remaining Obligations (%d):\n", len(chk.RemainingObligations))
			for _, o := range chk.RemainingObligations {
				fmt.Printf("    - %s\n", o)
			}
		}

	case "list":
		var stateFilter a4c.TaskLifecycleState
		var runtimeFilter string
		for i := 1; i < len(args); i++ {
			if (args[i] == "--state" || args[i] == "--status") && i+1 < len(args) {
				stateFilter = a4c.TaskLifecycleState(args[i+1])
				i++
			} else if args[i] == "--runtime" && i+1 < len(args) {
				runtimeFilter = args[i+1]
				i++
			}
		}
		tasks, err := svc.ListTasks(ctx, a4c.TaskFilter{
			State:   stateFilter,
			Runtime: runtimeFilter,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to list tasks: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Tasks (%d matches):\n", len(tasks))
		for _, t := range tasks {
			fmt.Printf("[%s] State: %-22s Agent: %-8s Tokens: %-6d Objective: %s\n",
				t.TaskID, t.State, t.ActiveAgent, t.TotalTokens, t.Objective)
		}

	case "history":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task history <task-id>")
			return
		}
		taskID := args[1]
		dotDir, storeDir, _, memDir, journalDir := getPaths(workDir)
		store, _ := a4c.NewDurableActivityStore(storeDir)
		journal, _ := a4c.NewDurableIntentJournal(journalDir)
		memStore, _ := a4c.NewDurableMemoryStore(memDir)
		wtMgr, _ := a4c.NewWorktreeManager(workDir, filepath.Join(dotDir, "worktrees"))

		report, err := a4c.BuildTaskHistory(taskID, store, journal, wtMgr, memStore)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error building task history: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(report.FormatHumanReadable())

	case "inspect":
		jsonOutput := false
		taskID := ""
		for _, arg := range args[1:] {
			if arg == "--json" {
				jsonOutput = true
			} else if !strings.HasPrefix(arg, "--") && taskID == "" {
				taskID = arg
			}
		}
		if taskID == "" {
			fmt.Println("Usage: a4c task inspect <task-id> [--json]")
			return
		}

		dotDir, storeDir, _, memDir, journalDir := getPaths(workDir)
		store, _ := a4c.NewDurableActivityStore(storeDir)
		journal, _ := a4c.NewDurableIntentJournal(journalDir)
		memStore, _ := a4c.NewDurableMemoryStore(memDir)
		wtMgr, _ := a4c.NewWorktreeManager(workDir, filepath.Join(dotDir, "worktrees"))

		report, err := a4c.BuildTaskHistory(taskID, store, journal, wtMgr, memStore)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error inspecting task: %v\n", err)
			os.Exit(1)
		}

		if jsonOutput {
			output, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error encoding task report as JSON: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(output))
			return
		}

		fmt.Print(report.FormatInspect(workDir))

	case "diff":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task diff <task-id> [--json]")
			return
		}
		taskID := args[1]
		jsonOutput := len(args) > 2 && args[2] == "--json"
		diffRes, err := svc.GetTaskDiff(ctx, taskID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get diff for task %s: %v\n", taskID, err)
			os.Exit(1)
		}
		if jsonOutput {
			out, _ := json.MarshalIndent(diffRes, "", "  ")
			fmt.Println(string(out))
			return
		}
		fmt.Printf("Diff for Task %s (Snapshot: %s, Base: %s):\n", diffRes.TaskID, diffRes.SnapshotID, diffRes.BaseBranch)
		fmt.Printf("Files Changed: %d | +%d / -%d\n", diffRes.TotalFiles, diffRes.TotalAdds, diffRes.TotalDels)
		for _, f := range diffRes.Files {
			fmt.Printf("  - %-30s [%s] (+%d/-%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
		}
		if diffRes.RawDiff != "" {
			fmt.Println("\n" + diffRes.RawDiff)
		}

	case "feedback":
		if len(args) < 3 {
			fmt.Println("Usage: a4c task feedback <task-id> <feedback text> [--agent <agent>]")
			return
		}
		taskID := args[1]
		feedbackText := args[2]
		targetAgent := "coder"
		for i := 3; i < len(args); i++ {
			if args[i] == "--agent" && i+1 < len(args) {
				targetAgent = args[i+1]
				i++
			}
		}
		ref, err := svc.SubmitFeedback(ctx, a4c.FeedbackRequest{
			TaskID:      taskID,
			Feedback:    feedbackText,
			TargetAgent: targetAgent,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to submit feedback: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Feedback submitted for task %s. State updated to: %s\n", ref.TaskID, ref.State)

	case "accept":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task accept <task-id> [comments] [--token <auth-token>]")
			return
		}
		taskID := args[1]
		comments := ""
		token := ""
		for i := 2; i < len(args); i++ {
			if (args[i] == "--token" || args[i] == "-t") && i+1 < len(args) {
				token = args[i+1]
				i++
			} else {
				if comments != "" {
					comments += " "
				}
				comments += args[i]
			}
		}
		stat, statErr := os.Stdin.Stat()
		isInteractive := statErr == nil && (stat.Mode()&os.ModeCharDevice) != 0

		res, err := svc.AcceptTask(ctx, a4c.AcceptTaskRequest{
			TaskID:            taskID,
			Comments:          comments,
			AcceptedBy:        "human_cli",
			ConfirmationToken: token,
			IsInteractive:     isInteractive,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to accept task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task %s accepted successfully at snapshot %s (auth: %s).\nWorking checkout untouched. Run 'a4c task integrate %s' to apply changes.\n",
			res.TaskID, res.AcceptedSnapshot, res.AuthorizationMethod, res.TaskID)

	case "integrate":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task integrate <task-id>")
			return
		}
		taskID := args[1]
		res, err := svc.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: taskID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to integrate task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task %s integrated successfully.\nCommit: %s | Files modified: %d\n",
			res.TaskID, res.IntegratedCommit, res.FilesChanged)

	case "reject":
		if len(args) < 3 {
			fmt.Println("Usage: a4c task reject <task-id> <reason>")
			return
		}
		taskID := args[1]
		reason := strings.Join(args[2:], " ")
		res, err := svc.RejectTask(ctx, a4c.RejectTaskRequest{
			TaskID: taskID,
			Reason: reason,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to reject task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task %s rejected at %s. Reason: %s\n", res.TaskID, res.RejectedAt.Format(time.RFC3339), res.Reason)

	default:
		fmt.Printf("Unknown task command %q. Usage: a4c task <submit|start|run|resume|adopt|escalate|checkpoint|list|history|inspect|diff|feedback|accept|integrate|reject>\n", subCmd)
	}
}

func runHost(args []string) {
	if len(args) > 0 && (args[0] == "capabilities" || args[0] == "list") {
		jsonOutput := false
		for _, a := range args[1:] {
			if a == "--json" {
				jsonOutput = true
			}
		}
		rep := a4c.RunDoctor(".")
		if jsonOutput {
			b, _ := json.MarshalIndent(rep.Hosts, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Println("Universal Host Capabilities & Limitations:")
			for _, h := range rep.Hosts {
				avail := "Available"
				if !h.Available {
					avail = "Unavailable"
				}
				fmt.Printf("\n• %s (%s) — %s\n", h.DisplayName, h.HostID, avail)
				fmt.Printf("  Default Model:        %s\n", h.DefaultModel)
				fmt.Printf("  Direct Mode:          %t\n", h.SessionInvocation)
				fmt.Printf("  Reviewed Mode:        %t\n", h.ToolRestrictions)
				fmt.Printf("  Cross-Host Resume:    %t\n", h.SessionResumption)
				fmt.Printf("  Structured Telemetry: %t\n", h.StructuredTelemetry)
				fmt.Printf("  Native Subagents:     %t\n", h.NativeSubagents)
				if len(h.Limitations) > 0 {
					fmt.Printf("  Limitations:          %s\n", strings.Join(h.Limitations, "; "))
				}
			}
		}
		return
	}

	fs := flag.NewFlagSet("host", flag.ExitOnError)
	workDir := fs.String("dir", ".", "Workspace directory")
	steps := fs.Int("steps", 0, "Max sequential steps (0 = daemon mode)")
	pollMs := fs.Int("poll-ms", 250, "Poll interval in milliseconds")
	_ = fs.Parse(args)

	svc, err := buildService(*workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize A4C service: %v\n", err)
		os.Exit(1)
	}

	cfg := a4c.WorkerHostConfig{
		WorkspaceDir: *workDir,
		PollInterval: time.Duration(*pollMs) * time.Millisecond,
		MaxSteps:     *steps,
	}
	host := a4c.NewWorkerHost(cfg, svc.Coordinator(), svc.Store(), svc.Journal())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived shutdown signal. Stopping worker host...")
		host.Stop()
		cancel()
	}()

	fmt.Printf("A4C Worker Host started (workspace: %s, steps: %d, poll: %dms)\n", *workDir, *steps, *pollMs)
	if err := host.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "Worker host error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("A4C Worker Host stopped cleanly.")
}

func runMCP(args []string) {
	workDir := "."
	role := ""
	token := ""

	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		} else if (args[i] == "--role" || args[i] == "--agent") && i+1 < len(args) {
			role = args[i+1]
			i++
		} else if args[i] == "--context-token" && i+1 < len(args) {
			token = args[i+1]
			i++
		}
	}
	if token == "" {
		token = os.Getenv("A4C_KNOWLEDGE_TOKEN")
	}
	svc, err := buildService(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize A4C service: %v\n", err)
		os.Exit(1)
	}
	srv := a4cmcp.NewServerWithRole(svc, role)

	// Wire A4C Knowledge when enabled. ErrDisabled is expected and silent.
	if ks, kerr := knowledge.NewService(workDir, func(activityID string) bool {
		rec, err := svc.Store().Get(activityID)
		return err == nil && rec != nil && rec.Status == a4c.StatusClaimed
	}); kerr == nil {
		srv.RegisterKnowledge(ks, token)
	} else if !errors.Is(kerr, knowledge.ErrDisabled) {
		fmt.Fprintf(os.Stderr, "knowledge disabled: %v\n", kerr)
	}

	if err := srv.ServeStdio(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server exited with error: %v\n", err)
		os.Exit(1)
	}
}




func runStatus(args []string) {
	_, storeDir, rosterDir, memDir, _ := getPaths(".")
	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init'.\n")
		return
	}

	roster, _ := a4c.DefaultTeamRoster(rosterDir)
	memory, _ := a4c.NewDurableMemoryStore(memDir)

	fmt.Println("================================================================================")
	fmt.Println("a4c Workspace Status")
	fmt.Println("================================================================================")
	fmt.Printf("Persistent Team: %d agents\n", len(roster.ListMembers()))
	fmt.Printf("Completed Activities: %d\n", store.CompletedCount())
	fmt.Printf("Claimable Pending Activities: %t\n", store.HasClaimable())
	fmt.Printf("Awaiting Human Decisions: %d\n", len(store.ListPendingDecisions()))
	fmt.Printf("Verified Architectural Conventions: %d\n", len(memory.ListVerified()))
}

func runDecisions(args []string) {
	_, storeDir, _, _, _ := getPaths(".")
	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init'.\n")
		return
	}

	if len(args) == 0 || args[0] == "list" {
		pending := store.ListPendingDecisions()
		if len(pending) == 0 {
			fmt.Println("No decisions currently awaiting human guidance.")
			return
		}
		fmt.Printf("Pending Human Decisions (%d):\n", len(pending))
		for _, dec := range pending {
			fmt.Printf("\n[ID: %s] Escalate by: %s\n", dec.DecisionID, dec.ProposedBy)
			fmt.Printf("Question: %s\n", dec.Question)
			fmt.Printf("Context:  %v\n", dec.Context)
		}
		return
	}

	if args[0] == "resolve" {
		if len(args) < 3 {
			fmt.Println("Usage: a4c decisions resolve <decision_id> <resolution_text> [--approve | --reject | --reassign <agent>]")
			return
		}
		id := args[1]
		resolution := args[2]
		approve := true
		reassignTo := ""

		for i := 3; i < len(args); i++ {
			if args[i] == "--reject" {
				approve = false
			} else if args[i] == "--reassign" && i+1 < len(args) {
				reassignTo = args[i+1]
				i++
			}
		}

		if err := store.ResolveHumanDecision(id, resolution, approve, reassignTo); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to resolve decision: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Decision %s resolved (Approve=%t, Reassign=%s)\n", id, approve, reassignTo)
	}
}

func runMemory(args []string) {
	_, _, _, memDir, _ := getPaths(".")
	memory, err := a4c.NewDurableMemoryStore(memDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init'.\n")
		return
	}

	verified := memory.ListVerified()
	if len(verified) == 0 {
		fmt.Println("No verified architectural conventions recorded yet.")
		return
	}

	fmt.Printf("Verified Organizational Memory (%d records):\n", len(verified))
	for _, m := range verified {
		fmt.Printf("- [%s (rev %d, verified by %s)]: %s\n", m.Key, m.Revision, m.Verifier, m.Content)
	}
}

// runKnowledge implements the `a4c knowledge` subcommand surface for the
// optional A4C Knowledge module (enable/disable/status/list/query/show).
func runKnowledge(args []string) {
	workDir := "."
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		}
	}
	if len(args) == 0 {
		fmt.Println("Usage: a4c knowledge <enable|disable|status|list|query|show> [--dir path]")
		return
	}
	sub := args[0]
	cfg, _ := knowledge.ReadConfig(workDir)

	switch sub {
	case "enable":
		cfg.Enabled = true
		if cfg.Project == "" {
			cfg.Project = filepath.Base(workDir)
		}
		if err := knowledge.WriteConfig(workDir, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "enable failed: %v\n", err)
			return
		}
		// Warm the store so the DB and secret exist.
		if ks, err := knowledge.NewService(workDir, knowledge.ActivityIsActive); err == nil {
			_ = ks.Close()
		}
		fmt.Printf("A4C Knowledge enabled for project %q (db: %s)\n", cfg.Project, filepath.Join(workDir, ".a4c", "knowledge.db"))

	case "disable":
		cfg.Enabled = false
		if err := knowledge.WriteConfig(workDir, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "disable failed: %v\n", err)
			return
		}
		fmt.Println("A4C Knowledge disabled.")

	case "status":
		if !cfg.Enabled {
			fmt.Println("A4C Knowledge: disabled (no .a4c/knowledge.json with enabled=true)")
			return
		}
		ks, err := knowledge.NewService(workDir, knowledge.ActivityIsActive)
		if err != nil {
			fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
			return
		}
		defer ks.Close()
		byKind, bySupport, byStatus, total, path, err := ks.Status()
		if err != nil {
			fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
			return
		}
		fmt.Printf("A4C Knowledge: enabled\n  project: %s\n  db: %s\n  records: %d\n", cfg.Project, path, total)
		fmt.Printf("  by kind: %v\n  by support: %v\n  by status: %v\n", byKind, bySupport, byStatus)

	case "list", "query", "show":
		if !cfg.Enabled {
			fmt.Println("A4C Knowledge: disabled")
			return
		}
		ks, err := knowledge.NewService(workDir, knowledge.ActivityIsActive)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n", sub, err)
			return
		}
		defer ks.Close()
		switch sub {
		case "list", "query":
			text := ""
			if sub == "query" && len(args) > 1 && !strings.HasPrefix(args[1], "--") {
				text = args[1]
			}
			res, err := ks.Query(knowledge.QueryInput{Text: text, Limit: 50})
			if err != nil {
				fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
				return
			}
			if res.Count == 0 {
				fmt.Println("No knowledge records.")
				return
			}
			for _, r := range res.Records {
				fmt.Printf("- [%s %s/%s] %s\n", r.Kind, r.Support, r.Status, r.Statement)
			}
		case "show":
			if len(args) < 2 {
				fmt.Println("Usage: a4c knowledge show <id>")
				return
			}
			rec, err := ks.Store().Get(args[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "show failed: %v\n", err)
				return
			}
			b, _ := json.MarshalIndent(rec, "", "  ")
			fmt.Println(string(b))
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown knowledge subcommand %q. Usage: a4c knowledge <enable|disable|status|list|query|show>\n", sub)
	}
}

func runLoop(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	realCopilot := fs.Bool("real-copilot", false, "Use official GitHub Copilot CLI")
	runtimeFlag := fs.String("runtime", "", "Agent execution runtime (copilot, opencode, fake)")
	modelFlag := fs.String("model", "", "Model identifier (e.g. opencode-go/mimo-v2.5, gpt-4o)")
	opencodeEndpoint := fs.String("opencode-endpoint", "", "OpenCode daemon HTTP endpoint (default auto-spawn or http://127.0.0.1:4096)")
	maxSteps := fs.Int("steps", 10, "Maximum sequential steps")
	workDir := fs.String("dir", ".", "Workspace directory")
	_ = fs.Parse(args)

	dotDir, storeDir, rosterDir, memDir, journalDir := getPaths(*workDir)

	roster, err := a4c.DefaultTeamRoster(rosterDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load team roster: %v (run 'a4c init' first)\n", err)
		os.Exit(1)
	}

	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load store: %v\n", err)
		os.Exit(1)
	}

	memory, _ := a4c.NewDurableMemoryStore(memDir)
	journal, _ := a4c.NewDurableIntentJournal(journalDir)
	runner := a4c.NewProjectTestRunner()

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		"knowledge_record", "knowledge_query",
	})
	for _, m := range roster.ListMembers() {
		gov.SetAgentTools(m.ID, m.AllowedTools)
	}

	var provider a4c.AgentProvider
	var opProvider *a4c.OpenCodeProvider
	var cpProvider *a4c.CopilotProvider

	if *realCopilot || *runtimeFlag == "copilot" {
		fmt.Println("Connecting to REAL GitHub Copilot via official Go SDK...")
		cp, err := a4c.NewCopilotProvider("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to connect to Copilot: %v\n", err)
			os.Exit(1)
		}
		defer cp.Close()
		provider = cp
		cpProvider = cp
	} else if *runtimeFlag == "opencode" {
		fmt.Println("Connecting to OpenCode runtime...")
		var mgr *a4c.OpenCodeServerManager
		endpoint := *opencodeEndpoint
		if endpoint == "" {
			var err error
			mgr, err = a4c.NewManagedOpenCodeServer(context.Background(), 0)
			if err != nil {
				endpoint = "http://127.0.0.1:4096"
			} else {
				endpoint = mgr.Endpoint()
				defer mgr.Stop()
			}
		}
		op := a4c.NewOpenCodeProvider(endpoint, *modelFlag, mgr)
		defer op.Close()
		provider = op
		opProvider = op
	} else {
		fmt.Println("Running with simulated test provider...")
		provider = a4c.NewFakeProvider()
	}

	coord := a4c.NewTeamCoordinator(*workDir, store, roster, memory, gov, journal, runner, provider)
	if opProvider != nil {
		coord.RegisterProvider("opencode", opProvider)
	}
	if cpProvider != nil {
		coord.RegisterProvider("copilot", cpProvider)
	}
	if wtMgr, err := a4c.NewWorktreeManager(*workDir, filepath.Join(dotDir, "worktrees")); err == nil {
		coord.SetWorktreeManager(wtMgr)
	}

	fmt.Printf("Starting governed team loop (max steps: %d)...\n", *maxSteps)
	ctx := context.Background()
	results, err := coord.RunLoop(ctx, *maxSteps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Execution failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Completed %d steps.\n", len(results))
	for _, res := range results {
		fmt.Printf("  -> Activity: %s | Status: %s | Consumed: %.0f tokens\n",
			res.ID, res.Status, res.Consumed.Consumed)
	}

	pendingDecs := coord.ListPendingDecisions()
	if len(pendingDecs) > 0 {
		fmt.Printf("\n[PAUSED]: %d decision(s) awaiting human review. Run 'a4c decisions list' to inspect.\n", len(pendingDecs))
	}
}

func runDemo(args []string) {
	useReal := len(args) > 0 && args[0] == "--real-copilot"

	baseDir, err := os.MkdirTemp("", "a4c-demo-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(baseDir)

	repoDir := filepath.Join(baseDir, "workspace")
	dotDir, storeDir, rosterDir, memDir, journalDir := getPaths(repoDir)
	_ = os.MkdirAll(repoDir, 0755)
	_ = os.MkdirAll(dotDir, 0755)

	// Scaffold broken calc.go
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/calc\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn 0 // Bug\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "calc_test.go"), []byte("package calc\n\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fatal(\"err\") } }\n"), 0644)

	roster, _ := a4c.DefaultTeamRoster(rosterDir)
	store, _ := a4c.NewDurableActivityStore(storeDir)
	memory, _ := a4c.NewDurableMemoryStore(memDir)
	journal, _ := a4c.NewDurableIntentJournal(journalDir)
	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
		"knowledge_record", "knowledge_query",
	})
	for _, m := range roster.ListMembers() {
		gov.SetAgentTools(m.ID, m.AllowedTools)
	}
	runner := a4c.NewProjectTestRunner()

	var provider a4c.AgentProvider
	if useReal {
		cp, err := a4c.NewCopilotProvider("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to initialize Copilot: %v\n", err)
			os.Exit(1)
		}
		defer cp.Close()
		provider = cp
	} else {
		fake := a4c.NewFakeProvider()
		fake.SessionResponseFunc = func(ctx context.Context, session *a4c.FakeSession, prompt string) (*a4c.AgentMessage, error) {
			switch session.Config().AgentID {
			case "lead":
				if session.Config().OnProposeActivity != nil {
					session.Config().OnProposeActivity(a4c.ActivityProposal{
						ProposalID:      "subtask-fix-add",
						ProposedAgentID: "coder",
						Objective:       "Fix Add(a, b int) int in calc.go to return a + b",
						RequestedBudget: a4c.Budget{Allocated: 20000},
					})
					session.Config().OnProposeActivity(a4c.ActivityProposal{
						ProposalID:      "subtask-review",
						ProposedAgentID: "reviewer",
						Objective:       "Verify Add(a, b) fix and test evidence",
						RequestedBudget: a4c.Budget{Allocated: 10000},
						DependsOn:       []string{"subtask-fix-add"},
					})
				}
				return &a4c.AgentMessage{Content: "Delegated implementation and review"}, nil
			case "coder":
				_ = os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"), 0644)
				if session.Config().OnProposeMemory != nil {
					session.Config().OnProposeMemory("pure_functions", "Calculator functions must be pure and free of side effects")
				}
				return &a4c.AgentMessage{Content: "Applied patch to calc.go"}, nil
			case "reviewer":
				if session.Config().OnSubmitReview != nil {
					session.Config().OnSubmitReview(a4c.ReviewResult{
						Approved: true,
						Comments: "Approved. All tests pass with exit code 0.",
					})
				}
				return &a4c.AgentMessage{Content: "Review approved"}, nil
			}
			return &a4c.AgentMessage{Content: "Done"}, nil
		}
		provider = fake
	}

	coord := a4c.NewTeamCoordinator(repoDir, store, roster, memory, gov, journal, runner, provider)

	fmt.Println("Submitting demo task: Fix failing tests in calc.go...")
	_, _ = coord.SubmitTask("Fix failing tests in calc.go", a4c.Budget{Allocated: 50000}, nil)

	ctx := context.Background()
	results, err := coord.RunLoop(ctx, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Demo failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Completed %d collaboration activities.\n", len(results))
	exitCode, out, _ := runner.RunTests(ctx, repoDir)
	fmt.Printf("Native tests status: ExitCode=%d (Output: %s)\n", exitCode, strings.TrimSpace(out))
	fmt.Printf("Demo completed successfully!\n")
}
