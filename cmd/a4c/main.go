package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ormasoftchile/a4c/pkg/a4c"
	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
	a4cmcp "github.com/ormasoftchile/a4c/pkg/a4c/mcp"
)

// ANSI colors for the A4C wordmark: bright white over bright red, with gray
// separators between the letters.
const (
	ansiReset = "\x1b[0m"
	ansiWhite = "\x1b[0;97m"
	ansiRed   = "\x1b[0;91m"
	ansiGray  = "\x1b[0;37m"
)

// bannerLegend is the line of text set beside each wordmark row. The first
// two rows spell the product line — Agent for Coding — Knowledge Building —
// and the third carries the release version, so the banner self-describes the
// binary the operator is actually running.
var bannerLegend = [3]string{
	"Agent for Coding",
	"Knowledge Building",
	"v. " + a4c.Version,
}

// bannerLegendGap separates the wordmark canvas from the legend text.
const bannerLegendGap = "  "

// bannerRows is the A4C wordmark: a compact three-row half-block rendering.
// Every row is padded to the same 11-column canvas so the legend stays
// aligned down the block; row two is one glyph narrower because the middle of
// the C is a single stem, which is what leaves its gap a column wider.
var bannerRows = [3]struct{ colored, plain string }{
	{
		colored: ansiWhite + "▀▀█" + ansiGray + " " + ansiWhite + "█ █" + ansiGray + "  " + ansiWhite + "█▀" + ansiReset,
		plain:   "▀▀█ █ █  █▀",
	},
	{
		colored: ansiRed + "█▀█" + ansiGray + " " + ansiRed + "▀▀█▀" + ansiGray + " " + ansiRed + "█" + ansiGray + " " + ansiReset,
		plain:   "█▀█ ▀▀█▀ █ ",
	},
	{
		colored: ansiRed + "▀▀▀" + ansiGray + " " + ansiWhite + "  " + ansiRed + "▀" + ansiGray + "  " + ansiRed + "▀▀" + ansiReset,
		plain:   "▀▀▀   ▀  ▀▀",
	},
}

// colorsEnabled reports whether ANSI color may be emitted: stdout must be an
// interactive terminal and NO_COLOR must be unset. This matches the
// ModeCharDevice checks used elsewhere in this file, so piped output, CI logs,
// and captured test output stay free of escape sequences.
func colorsEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	stat, err := os.Stdout.Stat()
	return err == nil && (stat.Mode()&os.ModeCharDevice) != 0
}

// banner renders the logo lockup: the wordmark with the expanded name set to
// its right, one legend line per wordmark row.
func banner() string {
	colors := colorsEnabled()
	var b strings.Builder
	b.WriteString("\n")
	for i, row := range bannerRows {
		glyphs := row.plain
		legend := bannerLegend[i]
		if colors {
			glyphs = row.colored
			legend = ansiGray + legend + ansiReset
		}
		b.WriteString(glyphs + bannerLegendGap + legend + "\n")
	}
	return b.String()
}

// versionLine is the single-line report of the running binary's release.
func versionLine() string {
	return "a4c " + a4c.Version
}

// printVersionBump reports the release version an integration landed, or
// nothing at all when the workspace carries no version file.
func printVersionBump(version string) {
	if version != "" {
		fmt.Printf("Version: %s\n", version)
	}
}

func printUsage() {
	fmt.Print(banner())
	fmt.Print("Usage: a4c <command> [options]\n\nCommands:\n")
	for _, c := range rootCommands {
		fmt.Printf("  %-9s %s\n", c.name, c.summary)
	}
	fmt.Print(`
Global options:
  --version             Print the a4c release version

Subcommands:
  init, install, and mcp are self-contained.
  task    submit start run resume adopt escalate checkpoint list history
          inspect diff feedback accept integrate reject decide worker loop
  status  doctor team decisions memory knowledge

Run 'a4c <command>' with no arguments for that command's own usage.
`)
}

// rootCommand is one entry in the root dispatch table. printUsage renders every
// entry here, so the documented surface and the dispatched surface cannot drift
// apart the way a hand-maintained switch allowed them to.
type rootCommand struct {
	name    string
	summary string
	run     func(args []string)
}

// rootCommands is the complete root surface: the lifecycle of the whole system
// (init, install, mcp) plus the two nouns an operator lives in (task, status).
// Everything else is a subcommand of one of these.
var rootCommands = []rootCommand{
	{name: "init", summary: "Initialize an a4c workspace and wire host integrations", run: runInit},
	{name: "install", summary: "Install host-native agent definitions (copilot|opencode|antigravity|all)", run: runInstall},
	{name: "task", summary: "Submit, execute, review, and integrate work", run: runTask},
	{name: "mcp", summary: "Run the Model Context Protocol (MCP) server over stdio", run: runMCP},
	{name: "status", summary: "Inspect environment, team, and workspace state", run: runStatus},
}

// removedCommands maps displaced root commands to their new invocation so an
// old name fails with an actionable redirect instead of a dead end.
var removedCommands = map[string]string{
	"doctor":    "a4c status doctor",
	"team":      "a4c status team",
	"host":      "a4c task worker",
	"run":       "a4c task loop",
	"decisions": "a4c status decisions",
	"memory":    "a4c status memory",
	"knowledge": "a4c status knowledge",
	"adopt":     "a4c task adopt",
}

func lookupRoot(name string) (rootCommand, bool) {
	for _, c := range rootCommands {
		if c.name == name {
			return c, true
		}
	}
	return rootCommand{}, false
}

// suggestCommand returns the closest root command name within a small edit
// distance, or "" when nothing is close enough to be worth suggesting.
func suggestCommand(name string) string {
	best, bestDist := "", 3
	for _, c := range rootCommands {
		if d := editDistance(name, c.name); d < bestDist {
			best, bestDist = c.name, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		copy(prev, curr)
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printUsage()
		return
	}

	// Version is a global flag, not a root command: it takes no arguments and
	// belongs to every invocation, so it is answered before dispatch instead
	// of occupying a slot in the root surface.
	if cmd == "version" || cmd == "--version" || cmd == "-version" {
		fmt.Println(versionLine())
		return
	}

	if c, ok := lookupRoot(cmd); ok {
		c.run(args)
		return
	}

	// Hard break for displaced commands: name the new location rather than
	// silently failing, and suggest a close match for plain typos.
	fmt.Fprintf(os.Stderr, "Unknown command %q.\n", cmd)
	if moved, ok := removedCommands[cmd]; ok {
		fmt.Fprintf(os.Stderr, "It moved: use %s\n", moved)
	} else if suggestion := suggestCommand(cmd); suggestion != "" {
		fmt.Fprintf(os.Stderr, "Did you mean %q?\n", suggestion)
	}
	fmt.Fprintln(os.Stderr, "Run 'a4c help' for usage.")
	os.Exit(1)
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
		if err := a4c.RegisterAntigravityMCP(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Antigravity skill installed, but MCP registration failed: %v\n", err)
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
	installHosts := true
	for _, a := range args {
		switch {
		case a == "--no-install":
			installHosts = false
		case !strings.HasPrefix(a, "-"):
			target = a
		}
	}

	dotDir, storeDir, _, memDir, journalDir := getPaths(target)

	dirs := []string{
		dotDir, storeDir, memDir, journalDir,
		filepath.Join(dotDir, "worktrees"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create directory %s: %v\n", d, err)
			os.Exit(1)
		}
	}

	_, _ = a4c.NewDurableActivityStore(storeDir)
	_, _ = a4c.NewDurableMemoryStore(memDir)
	_, _ = a4c.NewDurableIntentJournal(journalDir)

	fmt.Printf("Initialized a4c workspace in %s\n", target)

	if !installHosts {
		fmt.Println("Host installation skipped (--no-install). Run 'a4c install all' to wire hosts.")
		return
	}

	// Host wiring is part of setup, not a second step to remember. Installation
	// merges into existing configuration and is idempotent, so re-running init
	// is safe in an already-configured project.
	fmt.Println("Wiring host integrations (existing configuration is merged, never overwritten)...")
	installs := []struct {
		host string
		fn   func(string) error
	}{
		{"Copilot", a4c.ExportCopilotAgents},
		{"OpenCode", a4c.ExportOpenCodeAgents},
		{"Antigravity", a4c.ExportAntigravityCustomization},
	}
	installed := 0
	for _, in := range installs {
		if err := in.fn(target); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %s install failed: %v\n", in.host, err)
			continue
		}
		fmt.Printf("  %s: ok\n", in.host)
		installed++
	}

	if installed > 0 {
		fmt.Println("Start a host session from this directory (for example 'copilot', 'opencode', or 'agy').")
		fmt.Println("The host executes the work with its own tools; A4C governs the task lifecycle.")
		fmt.Println("Allow the local A4C MCP server to start when your host prompts for it.")
	}
}

// hostStartCommand describes how to launch one host from the initialized
// directory.
type hostStartCommand struct {
	host    string
	command string
	hint    string
}



func buildService(workDir string) (*a4c.TeamServiceImpl, error) {
	if workDir == "" {
		workDir = "."
	}
	dotDir, storeDir, _, memDir, journalDir := getPaths(workDir)
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
		"governed_read_file", "governed_apply_patch", "governed_propose_memory",
	})
	return a4c.NewTeamService(workDir, store, lifecycleStore, gov, memStore, journal, wtMgr), nil
}





// taskIDTakingSubcommands lists the task subcommands whose first positional
// argument is a task ID, with the states to prefer when the ID is omitted. A
// nil preference means "most recently updated task, whatever its state".
var taskIDTakingSubcommands = map[string][]a4c.TaskLifecycleState{
	"resume":     {a4c.TaskStateInterrupted, a4c.TaskStateRunning, a4c.TaskStateFailed, a4c.TaskStateQueued},
	"checkpoint": {a4c.TaskStateRunning, a4c.TaskStateInterrupted, a4c.TaskStateQueued, a4c.TaskStateFailed},
	"inspect":    nil,
	"history":    nil,
	"diff":       {a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision, a4c.TaskStateAccepted, a4c.TaskStateRunning, a4c.TaskStateInterrupted},
	"feedback":   {a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision, a4c.TaskStateRevisionRequested},
	"accept":     {a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision},
	"escalate":   {a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision, a4c.TaskStateFailed},
	"integrate":  {a4c.TaskStateAccepted},
	"reject":     {a4c.TaskStateAwaitingHumanReview, a4c.TaskStateAwaitingDecision},
}

// withImplicitTaskID inserts a resolved task ID when the operator omitted one.
// The notice goes to stderr so any machine-readable stdout stays parseable.
func withImplicitTaskID(svc *a4c.TeamServiceImpl, args []string) []string {
	if len(args) == 0 {
		return args
	}
	prefer, takesID := taskIDTakingSubcommands[args[0]]
	if !takesID {
		return args
	}
	// A positional ID was supplied.
	if len(args) > 1 {
		switch args[0] {
		case "feedback", "reject", "accept":
			// Free text follows the ID here, so anything not shaped like a task
			// ID is the text, not the ID.
			if strings.HasPrefix(args[1], "task-") {
				return args
			}
		default:
			if !strings.HasPrefix(args[1], "-") {
				return args
			}
		}
	}

	resolved, err := svc.ResolveTask("", prefer)
	if err != nil {
		// The command was well-formed; there is simply no task to apply it to.
		// Reporting the resolver's own reason beats falling through to usage.
		fmt.Fprintf(os.Stderr, "a4c task %s: %v\n", args[0], err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Using task %s (%s): %s\n", resolved.TaskID, resolved.State, resolved.Objective)

	out := make([]string, 0, len(args)+1)
	out = append(out, args[0], resolved.TaskID)
	out = append(out, args[1:]...)
	return out
}

// printDiffSummary shows what a task actually changed. Acceptance is a human
// sign-off, so the evidence must travel with the approval rather than live in a
// separate command the operator has to remember to run.
func printDiffSummary(d *a4c.DiffResult) {
	fmt.Printf("Files changed: %d | +%d / -%d\n", d.TotalFiles, d.TotalAdds, d.TotalDels)
	for _, f := range d.Files {
		fmt.Printf("  %-46s [%s] (+%d/-%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
	}
}

// taskHelpRequested reports whether the arguments request subcommand help.
func taskHelpRequested(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// flagLikeObjective reports whether a positional value looks like an option
// rather than a task objective. `a4c task start --help` used to create a task
// whose objective was "--help"; flag-like values must never become objectives.
func flagLikeObjective(v string) bool {
	return strings.HasPrefix(v, "-")
}

// taskUsageFor returns the usage text for a task subcommand.
func taskUsageFor(sub string) string {
	switch sub {
	case "submit":
		return "Usage: a4c task submit <objective> [budget_tokens] [--strategy direct]  (direct is the only execution model)"
	case "start":
		return "Usage: a4c task start <objective> [budget_tokens] [--strategy direct]  (creates the task and its isolated worktree; the host executes)"
	case "adopt":
		return "Usage: a4c task adopt <objective> [--notes <text>] [--in-place] [--strategy direct]"
	}
	return "Usage: a4c task <submit|start|run|resume|adopt|escalate|checkpoint|list|history|inspect|diff|feedback|accept|integrate|reject|decide|worker|loop|mode> [options]"
}

func runTask(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: a4c task <submit|start|run|resume|adopt|escalate|checkpoint|list|history|inspect|diff|feedback|accept|integrate|reject|decide|worker|loop|mode> [options]")
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
		fmt.Println("Usage: a4c task <submit|start|run|resume|adopt|escalate|checkpoint|list|history|inspect|diff|feedback|accept|integrate|reject|decide|worker|loop|mode> [options]")
		return
	}

	// Help is handled before any workspace access, task resolution, or task
	// creation: `--help` must never become a task objective and must never
	// trigger side effects (including implicit task-id execution).
	if taskHelpRequested(args[1:]) {
		fmt.Println(taskUsageFor(args[0]))
		return
	}

	ctx := context.Background()
	svc, err := buildService(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace not initialized. Run 'a4c init': %v\n", err)
		os.Exit(1)
	}

	// A task ID may be omitted: a4c resolves the task that most needs attention
	// in the relevant state, so the common case never means copy-pasting an
	// opaque identifier.
	args = withImplicitTaskID(svc, args)

	subCmd := args[0]
	switch subCmd {
	case "submit":
		if len(args) < 2 {
			fmt.Println(taskUsageFor("submit"))
			return
		}
		if flagLikeObjective(args[1]) {
			fmt.Fprintf(os.Stderr, "a4c task submit: refusing flag-like objective %q; options come after the objective (see 'a4c task submit --help')\n", args[1])
			return
		}
		objective := args[1]
		budgetTokens := 50000.0
		if len(args) > 2 {
			if b, err := strconv.ParseFloat(args[2], 64); err == nil {
				budgetTokens = b
				args = append(args[:2], args[3:]...)
			}
		}
		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--strategy" && i+1 < len(args):
				if args[i+1] != "direct" {
					fmt.Fprintf(os.Stderr, "strategy %q is retired: A4C no longer manages teams or AI reviewers; direct is the only execution model\n", args[i+1])
					return
				}
				i++
			default:
				fmt.Fprintf(os.Stderr, "unsupported argument %q (see 'a4c task submit --help')\n", args[i])
				return
			}
		}
		ref, err := svc.SubmitTask(ctx, a4c.SubmitTaskRequest{
			Objective:    objective,
			BudgetTokens: budgetTokens,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to submit task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task enqueued: %s\nState: %s\nObjective: %s\nBudget: %.0f tokens\n", ref.TaskID, ref.State, objective, budgetTokens)
	case "start":
		if len(args) < 2 {
			fmt.Println(taskUsageFor("start"))
			return
		}
		if flagLikeObjective(args[1]) {
			fmt.Fprintf(os.Stderr, "a4c task start: refusing flag-like objective %q; options come after the objective (see 'a4c task start --help')\n", args[1])
			return
		}
		objective := args[1]
		budgetTokens := 50000.0
		if len(args) > 2 {
			if b, err := strconv.ParseFloat(args[2], 64); err == nil {
				budgetTokens = b
				args = append(args[:2], args[3:]...)
			}
		}
		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--strategy" && i+1 < len(args):
				if args[i+1] != "direct" {
					fmt.Fprintf(os.Stderr, "strategy %q is retired: A4C no longer manages teams or AI reviewers; direct is the only execution model\n", args[i+1])
					return
				}
				i++
			default:
				fmt.Fprintf(os.Stderr, "unsupported argument %q (see 'a4c task start --help')\n", args[i])
				return
			}
		}
		res, err := svc.BeginTask(ctx, a4c.BeginTaskRequest{
			Objective:    objective,
			BudgetTokens: budgetTokens,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to begin task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task started: %s\nState: %s\nObjective: %s\nWorktree: %s\nBranch: %s\n",
			res.TaskID, res.State, objective, res.WorktreePath, res.WorktreeBranch)
		fmt.Println("\nExecute the work with the host's own tools inside the worktree, then report:")
		fmt.Printf("  a4c task inspect %s\n  a4c task accept %s     (human acceptance)\n  a4c task integrate %s\n", res.TaskID, res.TaskID, res.TaskID)
	case "run":
		fmt.Fprintf(os.Stderr, "a4c task run is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	case "resume":
		if len(args) < 2 {
			fmt.Println(taskUsageFor("resume"))
			return
		}
		taskID := args[1]
		instructions := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--notes" && i+1 < len(args) {
				instructions = args[i+1]
				i++
			}
		}
		chk, err := svc.ResumeTaskOnHost(ctx, a4c.ResumeTaskRequest{TaskID: taskID, Instructions: instructions})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to resume task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Task %s ready to continue.\n", chk.TaskID)
		fmt.Println("\nContinuation summary for the host:")
		fmt.Println(a4c.BuildContinuationPrompt(*chk))
	case "adopt":
		if len(args) < 2 {
			fmt.Println(taskUsageFor("adopt"))
			return
		}
		if flagLikeObjective(args[1]) {
			fmt.Fprintf(os.Stderr, "a4c task adopt: refusing flag-like objective %q; options come after the objective (see 'a4c task adopt --help')\n", args[1])
			return
		}
		objective := args[1]
		notes := ""
		inPlace := false
		for i := 2; i < len(args); i++ {
			if args[i] == "--notes" && i+1 < len(args) {
				notes = args[i+1]
				i++
			} else if args[i] == "--in-place" {
				inPlace = true
			} else if args[i] == "--strategy" && i+1 < len(args) {
				fmt.Fprintf(os.Stderr, "strategy %q is retired: A4C no longer manages teams or AI reviewers; direct is the only execution model\n", args[i+1])
				os.Exit(1)
			}
		}
		res, err := svc.AdoptExistingWork(ctx, a4c.AdoptWorkRequest{
			Objective:    objective,
			HandoffNotes: notes,
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
	case "escalate":
		fmt.Fprintf(os.Stderr, "a4c task escalate is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	case "checkpoint":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task checkpoint [task-id] [--json]")
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
			fmt.Printf("[%s] State: %-22s Mode: %-9s Agent: %-8s Tokens: %-6d Objective: %s\n",
				t.TaskID, t.State, t.Strategy, t.ActiveAgent, t.TotalTokens, t.Objective)
		}

	case "history":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task history [task-id]")
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
			fmt.Println("Usage: a4c task inspect [task-id] [--json]")
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
			fmt.Println("Usage: a4c task diff [task-id] [--json]")
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
		printDiffSummary(diffRes)
		if diffRes.RawDiff != "" {
			fmt.Println("\n" + diffRes.RawDiff)
		}

	case "feedback":
		if len(args) < 3 {
			fmt.Println("Usage: a4c task feedback [task-id] <feedback text> [--agent <agent>]")
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
			fmt.Println("Usage: a4c task accept [task-id] [comments] [--token <auth-token>] [--integrate]")
			return
		}
		taskID := args[1]
		comments := ""
		token := ""
		doIntegrate := false
		for i := 2; i < len(args); i++ {
			switch {
			case (args[i] == "--token" || args[i] == "-t") && i+1 < len(args):
				token = args[i+1]
				i++
			case args[i] == "--integrate":
				doIntegrate = true
			default:
				if comments != "" {
					comments += " "
				}
				comments += args[i]
			}
		}

		// Show what is about to be approved. Acceptance is a human sign-off
		// bound to a snapshot, and a snapshot hash is not evidence — so the
		// evidence travels with the approval rather than living in a separate
		// command the operator has to remember to run.
		if dres, derr := svc.GetTaskDiff(ctx, taskID); derr == nil {
			fmt.Printf("Approving %s against snapshot %s\n", taskID, dres.SnapshotID)
			printDiffSummary(dres)
			fmt.Println()
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
		fmt.Printf("Accepted %s at snapshot %s (auth: %s).\n", res.TaskID, res.AcceptedSnapshot, res.AuthorizationMethod)
		if res.VersionBumpTo != "" {
			fmt.Printf("Release rule: version will be bumped to %s when you run 'a4c task integrate'.\n", res.VersionBumpTo)
		}

		if !doIntegrate {
			fmt.Println("Working checkout untouched. Run 'a4c task integrate' to apply the changes.")
			return
		}

		ires, ierr := svc.IntegrateTask(ctx, a4c.IntegrateTaskRequest{TaskID: taskID})
		if ierr != nil {
			fmt.Fprintf(os.Stderr, "Accepted, but integration failed: %v\n", ierr)
			os.Exit(1)
		}
		fmt.Printf("Integrated %s. Commit: %s | Files modified: %d\n",
			ires.TaskID, ires.IntegratedCommit, ires.FilesChanged)
		printVersionBump(ires.Version)

	case "integrate":
		if len(args) < 2 {
			fmt.Println("Usage: a4c task integrate [task-id]")
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
		printVersionBump(res.Version)

	case "reject":
		if len(args) < 3 {
			fmt.Println("Usage: a4c task reject [task-id] <reason>")
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

	case "decide":
		fmt.Fprintf(os.Stderr, "a4c task decide is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	case "mode":
		fmt.Fprintf(os.Stderr, "a4c task mode is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	case "worker":
		fmt.Fprintf(os.Stderr, "a4c task worker is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	case "loop":
		fmt.Fprintf(os.Stderr, "a4c task loop is retired: A4C no longer runs agent execution.\nUse the host's own tools to do the work, then record the outcome with\n  a4c task checkpoint, a4c task accept, a4c task integrate\n")
	}
}

func runMCP(args []string) {
	workDir := "."

	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		}
	}
	svc, err := buildService(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize A4C service: %v\n", err)
		os.Exit(1)
	}
	// Announce the bound workspace on stderr (never stdout, which carries
	// JSON-RPC). Without this, a server pinned to the wrong directory looks
	// exactly like a healthy one — it just serves another project's tasks.
	if abs, absErr := filepath.Abs(workDir); absErr == nil {
		fmt.Fprintf(os.Stderr, "a4c mcp: serving %s\n", abs)
	}
	srv := a4cmcp.NewServer(svc)

	if err := srv.ServeStdio(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server exited with error: %v\n", err)
		os.Exit(1)
	}
}

// runStatus routes the read-only introspection commands. The bare form prints
// a one-screen overview; every subcommand reports on exactly one area and
// mutates nothing.
func runStatus(args []string) {
	switch {
	case len(args) == 0:
		runDoctor(nil)
	case args[0] == "doctor":
		runDoctor(args[1:])
	case args[0] == "memory":
		runMemory(args[1:])
	case args[0] == "knowledge":
		runKnowledge(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown status subcommand %q.\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: a4c status [doctor|memory|knowledge]")
		os.Exit(1)
	}
}

// runDecisions lists open questions awaiting the human engineer. Resolving one
// is a task mutation, so it lives at 'a4c task decide'.


// runMode reads or sets the workspace's default execution strategy — the mode
// applied to any task whose request names none. It is a durable preference, not
// a security setting, so a per-task value simply overrides it.


// runDecide resolves an open decision and resumes the paused task, so it is
// routed from 'a4c task decide' rather than from status.


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

// runKnowledge implements `a4c status knowledge`, the one-way migration of
// legacy SQLite knowledge records into the per-repository home-directory
// Markdown knowledge base. The legacy database is opened read-only and is
// never modified or deleted.
func runKnowledge(args []string) {
	workDir := "."
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			workDir = args[i+1]
			i++
		}
	}
	if len(args) == 0 || args[0] != "export" {
		fmt.Println("Usage: a4c status knowledge export [--dir path]")
		fmt.Println("Exports legacy .a4c/knowledge.db records to the home-directory knowledge base.")
		return
	}

	store, err := knowledge.OpenExport(workDir)
	if errors.Is(err, knowledge.ErrNoDatabase) {
		fmt.Println("No legacy knowledge database found; nothing to export.")
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
		return
	}
	defer store.Close()

	records, err := store.All()
	if err != nil {
		fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
		return
	}
	if len(records) == 0 {
		fmt.Printf("Legacy knowledge database %s contains no records; nothing to export.\n", store.Path())
		return
	}

	kbDir, err := a4c.ResolveKnowledgeDir(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export failed: cannot resolve knowledge directory: %v\n", err)
		return
	}

	exported, skipped := 0, 0
	for _, rec := range records {
		name := fmt.Sprintf("imported/%s.md", rec.ID)
		content := knowledge.RenderMarkdown(rec)
		if _, werr := a4c.WriteKnowledgeFile(kbDir, name, content); werr != nil {
			if errors.Is(werr, a4c.ErrKnowledgeExists) {
				skipped++
				continue
			}
			fmt.Fprintf(os.Stderr, "export: %s: %v\n", rec.ID, werr)
			continue
		}
		exported++
	}
	fmt.Printf("Exported %d legacy knowledge record(s) to %s (skipped %d already present).\n", exported, kbDir, skipped)
	fmt.Printf("Legacy database left untouched: %s\n", store.Path())
}


