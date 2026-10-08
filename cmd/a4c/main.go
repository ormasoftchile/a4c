package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ormasoftchile/a4c/pkg/a4c"
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
  team list                             List persistent team members and permissions
  team add <id> <name> <role> <charter> Register a new persistent team member
  task submit <objective> [budget]      Submit a feature request or task to the team
  status                                Display active activities and evidence
  run [--real-copilot] [--steps N]      Run governed collaboration loop
  decisions list                        List questions awaiting human engineer guidance
  decisions resolve <id> <resolution>   Resolve decision and resume paused task
  memory list                           List verified architectural conventions
  demo [--real-copilot]                 Run full self-contained demonstration

Flags:
  --real-copilot                        Connect to official GitHub Copilot CLI via Go SDK
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

	case "team":
		runTeam(args)

	case "task":
		runTask(args)

	case "status":
		runStatus(args)

	case "run":
		runLoop(args)

	case "decisions":
		runDecisions(args)

	case "memory":
		runMemory(args)

	case "demo":
		runDemo(args)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q. Run 'a4c help' for usage.\n", cmd)
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

	dirs := []string{dotDir, storeDir, rosterDir, memDir, journalDir}
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

func runTask(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: a4c task submit <objective> [budget_tokens]")
		return
	}

	_, storeDir, _, _, _ := getPaths(".")
	store, err := a4c.NewDurableActivityStore(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open activity store: %v\n", err)
		os.Exit(1)
	}

	subCmd := args[0]
	if subCmd == "submit" {
		if len(args) < 2 {
			fmt.Println("Usage: a4c task submit <objective> [budget_tokens]")
			return
		}
		objective := args[1]
		budgetTokens := 50000.0
		if len(args) >= 3 {
			if b, err := strconv.ParseFloat(args[2], 64); err == nil {
				budgetTokens = b
			}
		}

		req := a4c.ActivityRequest{
			ID:        fmt.Sprintf("task-%d", os.Getpid()),
			AgentID:   "lead",
			Objective: objective,
			Budget:    a4c.Budget{Allocated: budgetTokens},
		}

		if err := store.Enqueue(req); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to enqueue task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task enqueued: %s\nAssigned to: lead\nObjective: %s\nBudget: %.0f tokens\n",
			req.ID, req.Objective, req.Budget.Allocated)
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

func runLoop(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	realCopilot := fs.Bool("real-copilot", false, "Use official GitHub Copilot CLI")
	maxSteps := fs.Int("steps", 10, "Maximum sequential steps")
	workDir := fs.String("dir", ".", "Workspace directory")
	_ = fs.Parse(args)

	_, storeDir, rosterDir, memDir, journalDir := getPaths(*workDir)

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
	runner := &a4c.NativeGoTestRunner{}

	gov := a4c.NewPolicyGovernor([]string{
		"governed_read_file", "governed_apply_patch", "governed_propose_activity",
		"governed_submit_review", "governed_request_human_decision", "governed_propose_memory",
	})
	for _, m := range roster.ListMembers() {
		gov.SetAgentTools(m.ID, m.AllowedTools)
	}

	var provider a4c.AgentProvider
	if *realCopilot {
		fmt.Println("Connecting to REAL GitHub Copilot via official Go SDK...")
		cp, err := a4c.NewCopilotProvider("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to connect to Copilot: %v\n", err)
			os.Exit(1)
		}
		defer cp.Close()
		provider = cp
	} else {
		fmt.Println("Running with simulated test provider...")
		provider = a4c.NewFakeProvider()
	}

	coord := a4c.NewTeamCoordinator(*workDir, store, roster, memory, gov, journal, runner, provider)

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
	})
	for _, m := range roster.ListMembers() {
		gov.SetAgentTools(m.ID, m.AllowedTools)
	}
	runner := &a4c.NativeGoTestRunner{}

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
