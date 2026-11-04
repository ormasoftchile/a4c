package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func runInitiative(args []string) {
	if len(args) == 0 {
		printInitiativeUsage()
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
		printInitiativeUsage()
		return
	}

	subcmd := args[0]
	subArgs := args[1:]

	// Preflight can be executed in an uninitialized workspace to diagnose readiness.
	if subcmd == "preflight" {
		runPreflight(workDir, subArgs)
		return
	}

	// For start: run preflight first, ensuring missing binaries halt immediately,
	// and missing repo configurations are auto-scaffolded seamlessly.
	if subcmd == "start" {
		report := a4c.CheckInitiativePrerequisites(workDir)
		if len(report.MissingBinaries) > 0 {
			fmt.Fprintln(os.Stderr, "❌ Cannot start initiative: required CLI dependencies are missing from PATH:")
			for _, m := range report.MissingBinaries {
				fmt.Fprintf(os.Stderr, "  • %-10s %s\n", m.Name, m.Detail)
				if m.Remediation != "" {
					fmt.Fprintf(os.Stderr, "    -> Install: %s\n", m.Remediation)
				}
			}
			os.Exit(1)
		}

		// Ensure A4C base directories exist if not already initialized
		dotDir := resolveDotDir(workDir)
		if _, err := os.Stat(filepath.Join(dotDir, "activities")); os.IsNotExist(err) {
			fmt.Println("⚡ Initializing A4C workspace state...")
			runInit([]string{workDir})
		}

		if len(report.MissingConfigs) > 0 {
			fmt.Println("⚡ Auto-scaffolding missing workspace configuration for Squad & Spec-Kit...")
			actions, err := a4c.AutoScaffoldInitiative(workDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Auto-scaffolding failed: %v\n", err)
				os.Exit(1)
			}
			for _, act := range actions {
				fmt.Printf("  ✔ %s\n", act)
			}
		}
	}

	svc, err := buildService(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init': %v\n", err)
		os.Exit(1)
	}

	runner, err := a4c.NewCopilotProcessRunner()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Copilot runner unavailable: %v\n", err)
		os.Exit(1)
	}

	initSvc := a4c.NewInitiativeService(workDir, svc, runner)
	ctx := context.Background()

	switch subcmd {
	case "start":
		if len(subArgs) < 2 {
			fmt.Println("Usage: a4c initiative start <title> <objective>")
			return
		}
		title := subArgs[0]
		objective := strings.Join(subArgs[1:], " ")

		fmt.Printf("🚀 Initializing initiative %q...\n", title)
		init, err := initSvc.CreateInitiative(title, objective)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create initiative: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Initiative created: %s (status: %s)\n", init.ID, init.Status)

		fmt.Println("\n🔍 Phase 1: Running Squad Adversarial Discovery...")
		if _, err := initSvc.ExecuteDiscovery(ctx, init.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Discovery failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Discovery complete. Scope written to .a4c/initiatives/%s/scope.md\n", init.ID)

		fmt.Println("\n📐 Phase 2: Running Squad Architecture & Stage DAG Planning...")
		dag, err := initSvc.ExecutePlanning(ctx, init.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Planning failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("✅ Stage DAG generated (%d stages):\n", len(dag.Stages))
		for _, st := range dag.Stages {
			deps := "none"
			if len(st.Dependencies) > 0 {
				deps = strings.Join(st.Dependencies, ", ")
			}
			fmt.Printf("  • [%s] %s (deps: %s)\n", st.ID, st.Title, deps)
		}

		fmt.Printf("\nInitiative ready. To start the first stage, run:\n  a4c initiative stage start %s %s\n", init.ID, dag.Stages[0].ID)

	case "plan":
		var id string
		if len(subArgs) > 0 {
			id = subArgs[0]
		} else {
			entries, err := os.ReadDir(workDir + "/.a4c/initiatives")
			if err != nil || len(entries) == 0 {
				fmt.Println("No initiatives found. Create one with 'a4c initiative start'.")
				return
			}
			id = entries[len(entries)-1].Name()
		}

		fmt.Printf("📐 Running / resolving Squad Architecture & Stage DAG Planning for %s...\n", id)
		dag, err := initSvc.ExecutePlanning(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Planning failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("✅ Stage DAG generated (%d stages):\n", len(dag.Stages))
		for _, st := range dag.Stages {
			deps := "none"
			if len(st.GetDependencies()) > 0 {
				deps = strings.Join(st.GetDependencies(), ", ")
			}
			fmt.Printf("  • [%s] %s (deps: %s)\n", st.ID, st.Title, deps)
		}
		fmt.Printf("\nInitiative ready. To start the first stage, run:\n  a4c initiative stage start %s %s\n", id, dag.Stages[0].ID)

	case "status":
		var id string
		if len(subArgs) > 0 {
			id = subArgs[0]
		} else {
			// Find latest initiative
			entries, err := os.ReadDir(workDir + "/.a4c/initiatives")
			if err != nil || len(entries) == 0 {
				fmt.Println("No initiatives found. Create one with 'a4c initiative start'.")
				return
			}
			id = entries[len(entries)-1].Name()
		}

		init, err := initSvc.GetInitiative(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load initiative %q: %v\n", id, err)
			os.Exit(1)
		}

		fmt.Printf("Initiative: %s (%s)\n", init.Title, init.ID)
		fmt.Printf("Objective:  %s\n", init.Objective)
		fmt.Printf("Status:     %s\n", init.Status)
		fmt.Println("Stages:")
		for _, st := range init.Stages {
			deps := "none"
			if len(st.Dependencies) > 0 {
				deps = strings.Join(st.Dependencies, ", ")
			}
			fmt.Printf("  • [%s] %-25s status: %-12s deps: %s\n", st.ID, st.Title, st.Status, deps)
			if st.WorktreePath != "" {
				fmt.Printf("    worktree: %s\n", st.WorktreePath)
			}
		}

	case "stage":
		if len(subArgs) < 3 {
			fmt.Println("Usage: a4c initiative stage <start|review|integrate> <init-id> <stage-id>")
			return
		}
		action := subArgs[0]
		initID := subArgs[1]
		stageID := subArgs[2]

		switch action {
		case "start":
			fmt.Printf("⚙️ Starting stage %q for initiative %q...\n", stageID, initID)
			stage, err := initSvc.StartStage(ctx, initID, stageID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("✅ Stage started in worktree: %s\n", stage.WorktreePath)
			fmt.Println("Run Spec-Kit inside the worktree, or run review when ready:")
			fmt.Printf("  a4c initiative stage review %s %s\n", initID, stageID)

		case "review":
			fmt.Printf("🕵️ Running Squad Reviewer on stage %q...\n", stageID)
			rev, err := initSvc.ExecuteStageReview(ctx, initID, stageID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Verdict: %s\n", rev.Verdict)
			fmt.Printf("Summary: %s\n", rev.Summary)
			if len(rev.Violations) > 0 {
				fmt.Println("Violations:")
				for _, v := range rev.Violations {
					fmt.Printf("  - %s\n", v)
				}
			}
			if rev.Verdict == "PASS" {
				fmt.Printf("\nStage verified! To integrate into main, run:\n  a4c initiative stage integrate %s %s\n", initID, stageID)
			}

		case "integrate":
			fmt.Printf("🚢 Integrating verified stage %q into main...\n", stageID)
			err := initSvc.IntegrateStage(ctx, initID, stageID, "human-operator", "operator-token", true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Integration failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("✅ Stage %q successfully integrated into main!\n", stageID)

		default:
			fmt.Printf("Unknown stage action %q. Use start, review, or integrate.\n", action)
		}

	default:
		fmt.Printf("Unknown initiative command %q.\n", subcmd)
		printInitiativeUsage()
	}
}

func runPreflight(workDir string, args []string) {
	fix := false
	jsonOutput := false
	for _, a := range args {
		if a == "--fix" {
			fix = true
		} else if a == "--json" {
			jsonOutput = true
		}
	}

	if fix {
		fmt.Println("⚡ Attempting automatic scaffolding of missing initiative configurations...")
		actions, err := a4c.AutoScaffoldInitiative(workDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Scaffolding warning: %v\n", err)
		}
		for _, act := range actions {
			fmt.Printf("  ✔ %s\n", act)
		}
	}

	report := a4c.CheckInitiativePrerequisites(workDir)
	if jsonOutput {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
	} else {
		report.Print()
	}

	if !report.AllSatisfied {
		os.Exit(1)
	}
}

func printInitiativeUsage() {
	fmt.Print(`Usage: a4c initiative <command> [options]

Commands:
  preflight [--fix]                           Validate required CLI tools and workspace configuration
  start <title> <objective>                   Start an initiative, run Squad discovery and stage DAG planning
  status [init-id]                            Show initiative details, stage DAG, and progress
  stage start <init-id> <stage-id>            Allocate isolated worktree and start stage
  stage review <init-id> <stage-id>           Run Squad Reviewer against stage acceptance criteria
  stage integrate <init-id> <stage-id>        Human-gated safe integration of verified stage to main
`)
}
