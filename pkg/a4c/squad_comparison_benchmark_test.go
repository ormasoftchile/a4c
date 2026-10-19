package a4c_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSquadComparison_LiveBenchmark runs a controlled, real-execution token
// comparison of A4C against bradygaster/squad on an identical coding task,
// using GitHub Copilot CLI as the shared host and one pinned model for every
// agent invocation where the product supports pinning.
//
// It is deliberately gated behind an explicit opt-in env var because it
// spends real model tokens and takes many minutes:
//
//	A4C_BENCH_SQUAD_LIVE=1                       required to run
//	A4C_BENCH_SQUAD_CLI=/path/to/cli-entry.js    required; squad CLI built from source
//	A4C_BENCH_OUT=/path/to/results-dir           optional; raw records land here
//	A4C_BENCH_ONLY=a4c|squad                     optional; run only one product's legs
//	A4C_BENCH_A4C_STRATEGY=direct|reviewed|team  optional; A4C strategy (default team)
//
// Example:
//
//	A4C_BENCH_SQUAD_LIVE=1 \
//	A4C_BENCH_SQUAD_CLI=/tmp/squad/packages/squad-cli/dist/cli-entry.js \
//	A4C_BENCH_OUT=/Volumes/Projects/a4c/benchmarks/squad-comparison/results/run1 \
//	go test -run TestSquadComparison_LiveBenchmark -v -timeout 2h ./pkg/a4c
//
// Measurement model: GitHub Copilot CLI persists host-reported per-model-call
// usage for every session (including task-tool subagents, recorded with
// initiator='sub-agent') in ~/.copilot/session-store.db. Both products run on
// this same host, so one measurement source covers both. A4C's own
// ExecutionMetrics (parsed from the SDK usage stream) are saved alongside as a
// cross-check. Tokens are never estimated from text lengths.
//
// Phases are measured separately: "cold setup" (product init, no task) and
// "coding execution" (the assigned engineering task).
func TestSquadComparison_LiveBenchmark(t *testing.T) {
	if os.Getenv("A4C_BENCH_SQUAD_LIVE") != "1" {
		t.Skip("set A4C_BENCH_SQUAD_LIVE=1 to run the live A4C vs Squad benchmark")
	}
	squadCLI := os.Getenv("A4C_BENCH_SQUAD_CLI")
	if squadCLI == "" {
		t.Fatal("A4C_BENCH_SQUAD_CLI must point at squad's dist/cli-entry.js")
	}
	if _, err := os.Stat(squadCLI); err != nil {
		t.Fatalf("squad CLI not found at %s: %v", squadCLI, err)
	}

	outDir := os.Getenv("A4C_BENCH_OUT")
	if outDir == "" {
		var err error
		outDir, err = os.MkdirTemp("", "a4c-squad-bench-")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("results directory: %s", outDir)

	// ---- preflight ---------------------------------------------------------
	a4cBin := filepath.Join(outDir, "a4c"+a4cExeSuffix())
	build := exec.Command("go", "build", "-o", a4cBin, "../../cmd/a4c")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build a4c: %v\n%s", err, out)
	}
	for _, tool := range []string{"git", "node", "copilot", "sqlite3", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("required tool %q not on PATH: %v", tool, err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	usageDB := filepath.Join(home, ".copilot", "session-store.db")
	if _, err := os.Stat(usageDB); err != nil {
		t.Fatalf("copilot session store not found at %s: %v", usageDB, err)
	}
	copilotVersion := benchRunCmd(t, "", "copilot", "--version")

	// ---- scaffold identical fixture repos ---------------------------------
	a4cStrategy := os.Getenv("A4C_BENCH_A4C_STRATEGY")
	if a4cStrategy == "" {
		a4cStrategy = "team"
	}
	only := os.Getenv("A4C_BENCH_ONLY") // "", "a4c", or "squad"
	testdata := "testdata/squad-comparison"
	a4cDir := filepath.Join(outDir, "a4c-run")
	squadDir := filepath.Join(outDir, "squad-run")
	scaffoldFixtureRepo(t, filepath.Join(testdata, "fixture"), a4cDir)
	baseCommitA := gitOutput(t, a4cDir, "rev-parse", "HEAD")
	baseTreeA := gitOutput(t, a4cDir, "rev-parse", "HEAD^{tree}")
	if only != "a4c" {
		scaffoldFixtureRepo(t, filepath.Join(testdata, "fixture"), squadDir)
		if baseTreeS := gitOutput(t, squadDir, "rev-parse", "HEAD^{tree}"); baseTreeA != baseTreeS {
			t.Fatalf("fixture repos are not identical: tree %s vs %s", baseTreeA, baseTreeS)
		}
	}
	taskText, err := os.ReadFile(filepath.Join(testdata, "task.txt"))
	if err != nil {
		t.Fatal(err)
	}
	taskDesc := strings.TrimSpace(string(taskText))

	run := &benchRun{
		BaseCommit:   baseCommitA,
		BaseTree:     baseTreeA,
		Task:         taskDesc,
		CopilotVer:   strings.TrimSpace(copilotVersion),
		UsageDB:      usageDB,
		OutDir:       outDir,
		SquadCLI:     squadCLI,
		WorkDirA4C:   a4cDir,
		WorkDirSquad: squadDir,
		A4CBin:       a4cBin,
		BudgetTokens: 500000,
		Steps:        30,
		Model:        "gpt-5.5",
		A4CStrategy:  a4cStrategy,
	}

	// ---- PHASE 1: cold setup (no model calls expected) --------------------
	t.Log("phase 1a: A4C cold setup (a4c init)")
	wm := run.usageWatermark(t)
	start := time.Now()
	a4cInitOut := benchRunCmdIn(t, a4cDir, nil, a4cBin, "init", ".")
	run.SetupA4C = phaseResult{
		WallSeconds: time.Since(start).Seconds(),
		Log:         a4cInitOut,
		Usage:       run.collectUsage(t, wm, "", ""),
	}

	if only != "a4c" {
		t.Log("phase 1b: Squad cold setup (squad init --preset default)")
		wm = run.usageWatermark(t)
		start = time.Now()
		squadInitOut := benchRunCmdIn(t, squadDir, nil, "node", squadCLI, "init", "--preset", "default")
		run.SetupSquad = phaseResult{
			WallSeconds: time.Since(start).Seconds(),
			Log:         squadInitOut,
			Usage:       run.collectUsage(t, wm, "", ""),
		}
		// Squad's official model-pinning mechanism (Layer 0 in its own
		// model-selection reference): defaultModel in .squad/config.json pins the
		// coordinator to instruct the task tool to use this model for every spawn.
		pinSquadModelConfig(t, squadDir, run.Model)
		// Pin the squad_state MCP server to the same squad build under test
		// (init writes `npx -y @bradygaster/squad-cli@insider`, an unpinned
		// channel). This is version pinning, not a behavior change.
		pinSquadMCP(t, squadDir, squadCLI)
	}

	// ---- PHASE 2: coding execution ----------------------------------------
	t.Logf("phase 2a: A4C coding execution (a4c task start, %s strategy)", a4cStrategy)
	wm = run.usageWatermark(t)
	start = time.Now()
	a4cOut := benchRunCmdIn(t, a4cDir, envNoAutoUpdate(), a4cBin, "task", "start", taskDesc,
		strconv.Itoa(run.BudgetTokens),
		"--strategy", a4cStrategy,
		"--runtime", "copilot",
		"--model", run.Model,
		"--steps", strconv.Itoa(run.Steps))
	run.CodingA4C = phaseResult{
		WallSeconds: time.Since(start).Seconds(),
		Log:         a4cOut,
	}
	run.A4CTaskID = parseTaskID(a4cOut)
	t.Logf("a4c task id: %s", run.A4CTaskID)
	run.CodingA4C.Usage = run.collectUsage(t, wm, a4cDir, "a4c-review-"+run.A4CTaskID)

	if only != "a4c" {
		// Sequential execution with a cooldown avoids shared-rate-limit interference.
		t.Log("cooldown 10s before Squad run")
		time.Sleep(10 * time.Second)

		t.Log("phase 2b: Squad coding execution (copilot --agent squad -p)")
		wm = run.usageWatermark(t)
		squadUsageFile := filepath.Join(outDir, "squad-usage-output.json")
		start = time.Now()
		squadOut := benchRunCmdIn(t, squadDir, envNoAutoUpdate(), "copilot",
			"--agent", "squad",
			"--model", run.Model,
			"--yolo",
			"--additional-mcp-config", "@"+filepath.Join(squadDir, ".mcp.json"),
			"--usage-output-file", squadUsageFile,
			"-p", taskDesc)
		run.CodingSquad = phaseResult{
			WallSeconds: time.Since(start).Seconds(),
			Log:         squadOut,
			Usage:       run.collectUsage(t, wm, squadDir, ""),
		}
		rawSquadUsage, _ := os.ReadFile(squadUsageFile)
		run.SquadUsageOutput = string(rawSquadUsage)

		// Squad's own ledger view (may be empty on the copilot -p path).
		squadCostOut := benchRunCmdIn(t, squadDir, nil, "node", squadCLI, "cost", "--all")
		_ = os.WriteFile(filepath.Join(outDir, "squad-cost.txt"), []byte(squadCostOut), 0o644)
		run.SquadCostCmd = squadCostOut
	}

	// A4C's own view of the task (host-reported metrics per activity).
	if run.A4CTaskID != "" {
		inspectOut := benchRunCmdIn(t, a4cDir, nil, a4cBin, "task", "inspect", run.A4CTaskID, "--json")
		_ = os.WriteFile(filepath.Join(outDir, "a4c-task-inspect.json"), []byte(inspectOut), 0o644)
		run.A4CInspect = inspectOut
	}

	// ---- PHASE 3: independent verification --------------------------------
	t.Log("phase 3: independent acceptance verification")
	acceptSrc, err := os.ReadFile(filepath.Join(testdata, "acceptance_sched_test.go"))
	if err != nil {
		t.Fatal(err)
	}

	// A4C final state lives in the task worktree, not the base checkout.
	a4cWorktree := filepath.Join(a4cDir, ".a4c", "worktrees", run.A4CTaskID, "tree")
	a4cVerifyDir := a4cWorktree
	if _, err := os.Stat(a4cWorktree); err != nil {
		t.Errorf("a4c worktree not found at %s; verifying base checkout instead", a4cWorktree)
		a4cVerifyDir = a4cDir
	}
	run.A4CWorktree = a4cVerifyDir

	// Capture final diffs BEFORE injecting acceptance tests.
	gitOutput(t, a4cVerifyDir, "add", "-A")
	run.A4CDiff = gitOutput(t, a4cVerifyDir, "diff", "--cached", run.BaseCommit)
	_ = os.WriteFile(filepath.Join(outDir, "a4c-final-diff.patch"), []byte(run.A4CDiff), 0o644)
	if only != "a4c" {
		// On the Squad side, distinguish product scaffolding (.squad/, .github/,
		// .copilot/, .mcp.json, .gitignore) from task-relevant changes.
		squadAll := gitStagedDiff(t, squadDir, run.BaseCommit)
		_ = os.WriteFile(filepath.Join(outDir, "squad-final-diff-all.patch"), []byte(squadAll), 0o644)
		run.SquadDiff = gitFilteredDiff(t, squadDir, run.BaseCommit, func(path string) bool {
			return !strings.HasPrefix(path, ".squad/") &&
				!strings.HasPrefix(path, ".github/") &&
				!strings.HasPrefix(path, ".copilot/") &&
				!strings.HasPrefix(path, ".vscode/") &&
				path != ".mcp.json" && path != ".gitignore" && path != ".gitattributes"
		})
		_ = os.WriteFile(filepath.Join(outDir, "squad-final-diff-taskfiles.patch"), []byte(run.SquadDiff), 0o644)
	}

	if err := os.WriteFile(filepath.Join(a4cVerifyDir, "sched_acceptance_test.go"), acceptSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	a4cAccOut, a4cAccOK := runGoTest(t, a4cVerifyDir)
	run.A4CAcceptance = accResult{Passed: a4cAccOK, AllTestsPassed: strings.Contains(a4cAccOut, "ok  "), Output: a4cAccOut}
	_ = os.WriteFile(filepath.Join(outDir, "a4c-acceptance.log"), []byte(a4cAccOut), 0o644)
	if only != "a4c" {
		if err := os.WriteFile(filepath.Join(squadDir, "sched_acceptance_test.go"), acceptSrc, 0o644); err != nil {
			t.Fatal(err)
		}
		squadAccOut, squadAccOK := runGoTest(t, squadDir)
		run.SquadAcceptance = accResult{Passed: squadAccOK, AllTestsPassed: strings.Contains(squadAccOut, "ok  "), Output: squadAccOut}
		_ = os.WriteFile(filepath.Join(outDir, "squad-acceptance.log"), []byte(squadAccOut), 0o644)
	}

	// ---- persist raw records + summary ------------------------------------
	run.writeArtifacts(t)

	summary := run.buildSummary()
	summaryJSON, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(outDir, "comparison.json"), append(summaryJSON, '\n'), 0o644)
	report := run.buildReport(summary)
	_ = os.WriteFile(filepath.Join(outDir, "comparison.md"), []byte(report), 0o644)
	t.Log("\n" + report)
}

// ---------------------------------------------------------------------------
// data model
// ---------------------------------------------------------------------------

type benchRun struct {
	BaseCommit   string `json:"base_commit"`
	BaseTree     string `json:"base_tree"`
	Task         string `json:"task"`
	CopilotVer   string `json:"copilot_version"`
	Model        string `json:"model"`
	A4CStrategy  string `json:"a4c_strategy"`
	BudgetTokens int    `json:"a4c_budget_tokens"`
	Steps        int    `json:"a4c_max_steps"`

	UsageDB      string `json:"-"`
	OutDir       string `json:"-"`
	SquadCLI     string `json:"squad_cli"`
	WorkDirA4C   string `json:"-"`
	WorkDirSquad string `json:"-"`
	A4CBin       string `json:"-"`

	A4CTaskID        string      `json:"a4c_task_id"`
	A4CWorktree      string      `json:"-"`
	SetupA4C         phaseResult `json:"setup_a4c"`
	SetupSquad       phaseResult `json:"setup_squad"`
	CodingA4C        phaseResult `json:"coding_a4c"`
	CodingSquad      phaseResult `json:"coding_squad"`
	A4CInspect       string      `json:"-"`
	SquadCostCmd     string      `json:"squad_cost_output"`
	SquadUsageOutput string      `json:"squad_usage_output_file"`

	A4CDiff         string    `json:"-"`
	SquadDiff       string    `json:"-"`
	A4CAcceptance   accResult `json:"a4c_acceptance"`
	SquadAcceptance accResult `json:"squad_acceptance"`
}

type phaseResult struct {
	WallSeconds float64   `json:"wall_seconds"`
	Log         string    `json:"product_output,omitempty"`
	Usage       *usageAgg `json:"usage"`
}

type accResult struct {
	Passed         bool   `json:"harness_verdict_pass"`
	AllTestsPassed bool   `json:"package_ok_line_present"`
	Output         string `json:"output,omitempty"`
}

type usageEvent struct {
	ID                       int64  `json:"id"`
	SessionID                string `json:"session_id"`
	CWD                      string `json:"cwd"`
	Model                    string `json:"model"`
	Initiator                string `json:"initiator"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadTokens          int64  `json:"cache_read_tokens"`
	CacheWriteTokens         int64  `json:"cache_write_tokens"`
	ReasoningTokens          int64  `json:"reasoning_tokens"`
	TotalNanoAIU             int64  `json:"total_nano_aiu"`
	DurationMS               int64  `json:"duration_ms"`
	TurnIndex                *int64 `json:"turn_index"`
	AgentID                  string `json:"agent_id"`
	ParentToolCallID         string `json:"parent_tool_call_id"`
	FinishReason             string `json:"finish_reason"`
	ModelCallInitiatorDetail string `json:"-"`
	TokenDetailsJSON         string `json:"token_details_json"`
	CreatedAt                string `json:"created_at"`
}

type usageAgg struct {
	Events        int            `json:"model_calls"`
	Sessions      int            `json:"sessions"`
	Models        map[string]int `json:"models_by_calls"`
	Initiators    map[string]int `json:"calls_by_initiator"`
	UncachedInput int64          `json:"uncached_input_tokens"`
	CacheRead     int64          `json:"cache_read_tokens"`
	CacheWrite    int64          `json:"cache_write_tokens"`
	Output        int64          `json:"output_tokens"`
	Reasoning     int64          `json:"reasoning_tokens"`
	TotalInput    int64          `json:"total_input_tokens_incl_cache"`
	TotalOutput   int64          `json:"total_output_tokens"`
	CostAICredits float64        `json:"reported_cost_ai_credits"`
	DurationMS    int64          `json:"sum_model_call_duration_ms"`
	SubAgentCalls int            `json:"sub_agent_calls"`
	EventsRaw     []usageEvent   `json:"-"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func envNoAutoUpdate() []string {
	return append(os.Environ(), "COPILOT_AUTO_UPDATE=false")
}

func benchRunCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	return benchRunCmdIn(t, dir, nil, name, args...)
}

func benchRunCmdIn(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("command failed: %s %v\n%s", name, args, out)
		// Benchmark data collection continues; product failure is a result,
		// not a harness failure, except where callers check output content.
		return fmt.Sprintf("[command error: %v]\n%s", err, out)
	}
	return string(out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func scaffoldFixtureRepo(t *testing.T, fixtureDir, dest string) {
	t.Helper()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(fixtureDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitOutput(t, dest, "init", "-q")
	gitOutput(t, dest, "config", "user.email", "bench@example.com")
	gitOutput(t, dest, "config", "user.name", "Benchmark Harness")
	gitOutput(t, dest, "add", "-A")
	gitOutput(t, dest, "commit", "-q", "-m", "initial")
}

func parseTaskID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Task started: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Task started: "))
		}
	}
	return ""
}

func (r *benchRun) usageWatermark(t *testing.T) int64 {
	t.Helper()
	out := sqliteQuery(t, r.UsageDB, "SELECT COALESCE(MAX(id),0) AS w FROM assistant_usage_events;")
	var rows []struct {
		W int64 `json:"w"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("parse watermark %q: %v", out, err)
	}
	return rows[0].W
}

// collectUsage reads host-reported usage for all sessions and subagents whose
// cwd is the product's working directory (A4C sessions additionally include
// the disposable review workspace created under the OS temp dir).
func (r *benchRun) collectUsage(t *testing.T, watermark int64, cwdPrefix, contains string) *usageAgg {
	t.Helper()
	q := fmt.Sprintf(`SELECT u.id, u.session_id, s.cwd, u.model, u.initiator,
 u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_write_tokens,
 u.reasoning_tokens, u.total_nano_aiu, u.duration_ms, u.turn_index, u.agent_id,
 u.parent_tool_call_id, u.finish_reason, u.token_details_json, u.created_at
 FROM assistant_usage_events u JOIN sessions s ON s.id = u.session_id
 WHERE u.id > %d ORDER BY u.id;`, watermark)
	out := sqliteQuery(t, r.UsageDB, q)
	var events []usageEvent
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		t.Fatalf("parse usage events: %v\n%s", err, out)
	}
	prefix := ""
	if cwdPrefix != "" {
		if resolved, err := filepath.EvalSymlinks(cwdPrefix); err == nil {
			prefix = resolved
		} else {
			prefix = cwdPrefix
		}
	}
	kept := make([]usageEvent, 0, len(events))
	for _, e := range events {
		cwd := e.CWD
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = resolved
		}
		switch {
		case prefix != "" && (cwd == prefix || strings.HasPrefix(cwd, prefix+string(os.PathSeparator))):
			kept = append(kept, e)
		case contains != "" && strings.Contains(cwd, contains):
			kept = append(kept, e)
		}
	}
	agg := &usageAgg{
		Models:     map[string]int{},
		Initiators: map[string]int{},
		EventsRaw:  kept,
	}
	sessions := map[string]bool{}
	for _, e := range kept {
		agg.Events++
		sessions[e.SessionID] = true
		agg.Models[e.Model]++
		agg.Initiators[e.Initiator]++
		if e.Initiator == "sub-agent" {
			agg.SubAgentCalls++
		}
		agg.CacheRead += e.CacheReadTokens
		agg.CacheWrite += e.CacheWriteTokens
		agg.Reasoning += e.ReasoningTokens
		agg.TotalInput += e.InputTokens // host semantics: input incl. cache read/write
		agg.TotalOutput += e.OutputTokens
		agg.Output += e.OutputTokens
		agg.CostAICredits += float64(e.TotalNanoAIU) / 1e9
		agg.DurationMS += e.DurationMS
		// Uncached input: prefer the host's own token_details breakdown,
		// fall back to subtraction (input - cache_read - cache_write).
		uncached, ok := uncachedFromDetails(e.TokenDetailsJSON)
		if !ok {
			uncached = e.InputTokens - e.CacheReadTokens - e.CacheWriteTokens
			if uncached < 0 {
				uncached = 0
			}
		}
		agg.UncachedInput += uncached
	}
	agg.Sessions = len(sessions)
	return agg
}

func uncachedFromDetails(detailsJSON string) (int64, bool) {
	if detailsJSON == "" {
		return 0, false
	}
	var entries []struct {
		TokenType  string `json:"tokenType"`
		TokenCount int64  `json:"tokenCount"`
	}
	if err := json.Unmarshal([]byte(detailsJSON), &entries); err != nil {
		return 0, false
	}
	var total int64
	found := false
	for _, e := range entries {
		if e.TokenType == "input" {
			total += e.TokenCount
			found = true
		}
	}
	return total, found
}

func sqliteQuery(t *testing.T, dbPath, query string) string {
	t.Helper()
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sqlite3 query failed: %v\nquery: %s", err, query)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "[]"
	}
	return trimmed
}

func pinSquadModelConfig(t *testing.T, dir, model string) {
	t.Helper()
	cfgPath := filepath.Join(dir, ".squad", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read squad config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse squad config: %v", err)
	}
	cfg["defaultModel"] = model
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func pinSquadMCP(t *testing.T, dir, squadCLI string) {
	t.Helper()
	mcpPath := filepath.Join(dir, ".mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse .mcp.json: %v", err)
	}
	if srv, ok := cfg.MCPServers["squad_state"]; ok {
		srv.Command = "node"
		srv.Args = []string{squadCLI, "state-mcp"}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcpPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitStagedDiff(t *testing.T, dir, base string) string {
	t.Helper()
	gitOutput(t, dir, "add", "-A")
	return gitOutput(t, dir, "diff", "--cached", base)
}

// gitFilteredDiff returns the staged diff limited to files accepted by keep.
func gitFilteredDiff(t *testing.T, dir, base string, keep func(path string) bool) string {
	t.Helper()
	gitOutput(t, dir, "add", "-A")
	nameOnly := gitOutput(t, dir, "diff", "--cached", "--name-only", base)
	var paths []string
	for _, p := range strings.Split(nameOnly, "\n") {
		p = strings.TrimSpace(p)
		if p != "" && keep(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return "(no task-relevant file changes)"
	}
	args := append([]string{"diff", "--cached", base, "--"}, paths...)
	return gitOutput(t, dir, args...)
}

func runGoTest(t *testing.T, dir string) (string, bool) {
	t.Helper()
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func (r *benchRun) writeArtifacts(t *testing.T) {
	t.Helper()
	for _, p := range []struct {
		name string
		res  *phaseResult
	}{
		{"a4c-setup", &r.SetupA4C},
		{"squad-setup", &r.SetupSquad},
		{"a4c-coding", &r.CodingA4C},
		{"squad-coding", &r.CodingSquad},
	} {
		_ = os.WriteFile(filepath.Join(r.OutDir, p.name+".log"), []byte(p.res.Log), 0o644)
		if p.res.Usage != nil {
			raw, _ := json.MarshalIndent(p.res.Usage.EventsRaw, "", "  ")
			_ = os.WriteFile(filepath.Join(r.OutDir, p.name+"-usage-events.json"), append(raw, '\n'), 0o644)
			aggCopy, _ := json.MarshalIndent(p.res.Usage, "", "  ")
			_ = os.WriteFile(filepath.Join(r.OutDir, p.name+"-usage-agg.json"), append(aggCopy, '\n'), 0o644)
		}
	}
	meta, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile(filepath.Join(r.OutDir, "run-metadata.json"), append(meta, '\n'), 0o644)
}

// ---------------------------------------------------------------------------
// summary + report
// ---------------------------------------------------------------------------

type comparisonSummary struct {
	Task     string    `json:"task"`
	Model    string    `json:"model"`
	Host     string    `json:"host"`
	SquadCLI string    `json:"squad_cli"`
	A4CCLI   string    `json:"a4c_cli"`
	BaseTree string    `json:"identical_base_tree"`
	Setup    sidePhase `json:"setup"`
	A4C      sideTask  `json:"a4c"`
	Squad    sideTask  `json:"squad"`
}

type sidePhase struct {
	WallSeconds float64   `json:"wall_seconds"`
	Usage       *usageAgg `json:"usage"`
}

type sideTask struct {
	WallSeconds      float64   `json:"wall_seconds"`
	Usage            *usageAgg `json:"usage"`
	Acceptance       accResult `json:"acceptance"`
	DiffBytes        int       `json:"diff_bytes_task_files"`
	ModelTurns       int       `json:"model_turns"`
	SubAgentSessions int       `json:"sub_agent_model_calls"`
}

func (r *benchRun) buildSummary() comparisonSummary {
	s := comparisonSummary{
		Task:     r.Task,
		Model:    r.Model,
		Host:     "GitHub Copilot CLI " + r.CopilotVer,
		SquadCLI: r.SquadCLI,
		A4CCLI:   "github.com/ormasoftchile/a4c (built from workspace)",
		BaseTree: r.BaseTree,
		Setup: sidePhase{
			WallSeconds: r.SetupA4C.WallSeconds + r.SetupSquad.WallSeconds,
			Usage:       combineUsage(r.SetupA4C.Usage, r.SetupSquad.Usage),
		},
		A4C: sideTask{
			WallSeconds:      r.CodingA4C.WallSeconds,
			Usage:            r.CodingA4C.Usage,
			Acceptance:       r.A4CAcceptance,
			DiffBytes:        len(r.A4CDiff),
			ModelTurns:       eventsOf(r.CodingA4C.Usage),
			SubAgentSessions: subCallsOf(r.CodingA4C.Usage),
		},
		Squad: sideTask{
			WallSeconds:      r.CodingSquad.WallSeconds,
			Usage:            r.CodingSquad.Usage,
			Acceptance:       r.SquadAcceptance,
			DiffBytes:        len(r.SquadDiff),
			ModelTurns:       eventsOf(r.CodingSquad.Usage),
			SubAgentSessions: subCallsOf(r.CodingSquad.Usage),
		},
	}
	return s
}

func eventsOf(u *usageAgg) int {
	if u == nil {
		return 0
	}
	return u.Events
}

func subCallsOf(u *usageAgg) int {
	if u == nil {
		return 0
	}
	return u.SubAgentCalls
}

func combineUsage(a, b *usageAgg) *usageAgg {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := &usageAgg{
		Models:     map[string]int{},
		Initiators: map[string]int{},
	}
	for _, u := range []*usageAgg{a, b} {
		out.Events += u.Events
		out.Sessions += u.Sessions
		out.UncachedInput += u.UncachedInput
		out.CacheRead += u.CacheRead
		out.CacheWrite += u.CacheWrite
		out.Output += u.Output
		out.Reasoning += u.Reasoning
		out.TotalInput += u.TotalInput
		out.TotalOutput += u.TotalOutput
		out.CostAICredits += u.CostAICredits
		out.DurationMS += u.DurationMS
		out.SubAgentCalls += u.SubAgentCalls
		for k, v := range u.Models {
			out.Models[k] += v
		}
		for k, v := range u.Initiators {
			out.Initiators[k] += v
		}
	}
	return out
}

func fmtU(u *usageAgg) string {
	if u == nil {
		return "unavailable"
	}
	return fmt.Sprintf("in(uncached)=%d cache_read=%d cache_write=%d out=%d reasoning=%d sessions=%d calls=%d",
		u.UncachedInput, u.CacheRead, u.CacheWrite, u.Output, u.Reasoning, u.Sessions, u.Events)
}

func usageModels(u *usageAgg) map[string]int {
	if u == nil {
		return nil
	}
	return u.Models
}

func usageInitiators(u *usageAgg) map[string]int {
	if u == nil {
		return nil
	}
	return u.Initiators
}

func (r *benchRun) buildReport(s comparisonSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# A4C vs Squad — live token benchmark\n\n")
	fmt.Fprintf(&b, "- Task: interval algebra in Go (`sched` package), identical fixture tree `%s`\n", r.BaseTree[:12])
	fmt.Fprintf(&b, "- Host: %s (COPILOT_AUTO_UPDATE=false for both)\n", r.CopilotVer)
	fmt.Fprintf(&b, "- Model pin: %s (A4C --model; Squad --model + .squad/config.json defaultModel Layer-0)\n", r.Model)
	fmt.Fprintf(&b, "- Squad CLI: %s\n", r.SquadCLI)
	fmt.Fprintf(&b, "- A4C strategy: %s, budget %d tokens, max %d steps\n", r.A4CStrategy, r.BudgetTokens, r.Steps)
	fmt.Fprintf(&b, "- Squad invocation: copilot --agent squad --yolo --additional-mcp-config @.mcp.json -p\n")
	fmt.Fprintf(&b, "- Measurement: ~/.copilot/session-store.db assistant_usage_events (host-reported), filtered by working directory\n\n")

	fmt.Fprintf(&b, "## Setup phase (cold)\n\n")
	fmt.Fprintf(&b, "- A4C `a4c init`: %.1fs — %s\n", r.SetupA4C.WallSeconds, fmtU(r.SetupA4C.Usage))
	fmt.Fprintf(&b, "- Squad `squad init --preset default`: %.1fs — %s\n", r.SetupSquad.WallSeconds, fmtU(r.SetupSquad.Usage))

	fmt.Fprintf(&b, "\n## Coding phase\n\n")
	fmt.Fprintf(&b, "- A4C: %.1fs — %s\n", r.CodingA4C.WallSeconds, fmtU(r.CodingA4C.Usage))
	fmt.Fprintf(&b, "- Squad: %.1fs — %s\n", r.CodingSquad.WallSeconds, fmtU(r.CodingSquad.Usage))

	fmt.Fprintf(&b, "\n## Models observed (verification)\n\n")
	fmt.Fprintf(&b, "- A4C events by model: %v\n", usageModels(r.CodingA4C.Usage))
	fmt.Fprintf(&b, "- Squad events by model: %v\n", usageModels(r.CodingSquad.Usage))
	fmt.Fprintf(&b, "- A4C events by initiator: %v\n", usageInitiators(r.CodingA4C.Usage))
	fmt.Fprintf(&b, "- Squad events by initiator: %v\n", usageInitiators(r.CodingSquad.Usage))

	fmt.Fprintf(&b, "\n## Independent acceptance verification\n\n")
	fmt.Fprintf(&b, "- A4C: pass=%v\n- Squad: pass=%v\n", r.A4CAcceptance.AllTestsPassed, r.SquadAcceptance.AllTestsPassed)
	return b.String()
}
