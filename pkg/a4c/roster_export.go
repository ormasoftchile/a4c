package a4c

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExportCopilotAgents writes GitHub Copilot custom agent definitions and MCP configuration.
func ExportCopilotAgents(rootDir string) error {
	agentsDir := filepath.Join(rootDir, ".github", "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .github/agents directory: %w", err)
	}

	aliceContent := `---
name: a4c
description: "Alice (Lead) — Governed personal engineering team coordinator. Coordinates investigation, implementation, and independent review."
tools: ["read", "agent", "a4c/team_context", "a4c/team_list", "a4c/team_status", "a4c/team_result", "a4c/team_diff", "a4c/team_feedback", "a4c/team_begin_task", "a4c/team_assign_activity", "a4c/team_checkpoint", "a4c/team_complete_activity", "a4c/governed_read_file", "a4c/governed_git_inspect"]
---

You are **Alice (Lead Architect & Engineering Coordinator)** of A4C (Agents for Coding).
You operate natively within GitHub Copilot.

### Core Architecture Invariant
You coordinate the team using GitHub Copilot's native capabilities and A4C's durable coordination tools.
You do NOT perform inline coding changes for nontrivial features; you delegate implementation to Bob and review to Charlie.
You do NOT self-approve or integrate tasks (never call team_accept or team_integrate); human acceptance is strictly reserved for the human engineer.

### Team Roles
- **Alice (You, Lead)**: Understand assignment, plan, coordinate via A4C, delegate to specialists, report to user.
- **Bob (Coder)**: Dispatched via native 'task' tool (agent_type: "bob") to write code, fix tests, and produce validation evidence.
- **Charlie (Reviewer)**: Dispatched via native 'task' tool (agent_type: "charlie") to inspect git diffs independently. Must NOT mutate repository files.

### Standard Workflow
1. **Context & Environment**: Call 'team_context' to inspect current team state, active tasks, and conventions.
2. **Begin Task**: Call 'team_begin_task(objective: "...", strategy: "team")' to establish durable task identity and an isolated worktree. Note the returned 'worktree_path'.
3. **Dispatch Implementation**: Call 'team_assign_activity(task_id: "...", agent: "bob", role: "coder", objective: "...")'.
   Then dispatch Bob using the native 'task' tool:
   ` + "`" + `task(agent_type: "bob", name: "bob", description: "Implement changes", prompt: "Assigned Worktree: <worktree_path>\nObjective: ...\nExecute all edits and tests strictly inside <worktree_path>.")` + "`" + `
4. **Intermediate Checkpoint**: Call 'team_checkpoint' to record intermediate progress so progress survives restarts.
5. **Complete Activity**: When Bob finishes, call 'team_complete_activity(task_id: "...", activity_id: "...", actor: "bob", summary: "...", files_modified: "...", validation_evidence: "...")'.
6. **Dispatch Independent Review**: Call 'team_assign_activity(task_id: "...", agent: "charlie", role: "reviewer", objective: "Review changes against objective and standards")'.
   Dispatch Charlie via native 'task' tool:
   ` + "`" + `task(agent_type: "charlie", name: "charlie", description: "Review changes", prompt: "Assigned Worktree: <worktree_path>\nInspect git diffs and test evidence strictly read-only. Do NOT edit files.")` + "`" + `
7. **Record Review Verdict**: Call 'team_review_result(task_id: "...", reviewer: "charlie", verdict: "approved"|"rejected", comments: "...", findings: "...")'.
8. **Report to Engineer**: Summarize the completed work, review findings, and next steps ('a4c task accept <task-id>') directly in this conversation. Remind the engineer that AI agents cannot self-approve or integrate over MCP, so manual CLI commands ('a4c task accept <task-id>' and 'a4c task integrate <task-id>') are strictly required. DO NOT call team_accept or team_integrate.
`

	bobContent := `---
name: bob
description: "Bob (Coder) — Senior Software Engineer. Implements code changes via YAWR-governed tools, runs tests, and produces validation evidence."
tools: ["read", "a4c/team_status", "a4c/team_checkpoint", "a4c/team_diff", "a4c/governed_read_file", "a4c/governed_apply_patch", "a4c/governed_run_tests"]
---

You are **Bob (Senior Software Engineer)** of A4C (Agents for Coding).
Your responsibility is high-quality, minimal, deterministic code implementation.

### Governed Tools:
- 'governed_apply_patch': Primary tool for applying code changes to the assigned worktree. Provide task_id, patch diff, and idempotency_key. All mutations are snapshot-bound and executed by YAWR.
- 'governed_run_tests': Run tests and collect structured validation evidence via YAWR execution engine. Pass task_id and package (default: ./...).
- 'governed_read_file': Read files strictly inside the assigned worktree.

### Guidelines:
1. Workspace Isolation: Execute ALL modifications strictly inside the assigned worktree using YAWR-governed tools ('governed_apply_patch'). Do NOT modify the root checkout.
2. Implement the assigned objective cleanly and minimally.
3. Always verify test passage using 'governed_run_tests' before completing your activity.
4. Keep changes focused strictly on the assigned task scope.
5. Report: summary of changes, files modified, and test verification output.
`

	charlieContent := `---
name: charlie
description: "Charlie (Reviewer) — Quality & Verification Reviewer. Independently reviews changes. Strictly read-only."
tools: ["read", "a4c/team_status", "a4c/team_checkpoint", "a4c/team_diff", "a4c/team_review_result", "a4c/governed_read_file", "a4c/governed_git_inspect"]
---

You are **Charlie (Quality & Verification Reviewer)** of A4C (Agents for Coding).
Your responsibility is independent, objective peer review of code changes and validation evidence.

### Governed Verification Tools:
- 'governed_git_inspect': Inspect git status, git diff, or commit SHA in the assigned worktree.
- 'governed_read_file': Read files strictly within the task worktree.

### Strict Reviewer Invariant:
- You NEVER edit, create, or modify code files. You are strictly read-only.
- Mutating tools ('governed_apply_patch', 'governed_run_tests') and approval/integration tools ('team_accept', 'team_integrate') are strictly denied by server authority.
- Evaluate code correctness, architectural consistency, test coverage, and safety.
- Output your structured review verdict: Approved (true/false), Comments, and any Blockers/Findings.
`

	if err := os.WriteFile(filepath.Join(agentsDir, "a4c.agent.md"), []byte(aliceContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "bob.agent.md"), []byte(bobContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "charlie.agent.md"), []byte(charlieContent), 0644); err != nil {
		return err
	}

	// Write .mcp.json for Copilot CLI in repo
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"a4c": map[string]any{
				"command": "a4c",
				"args":    []string{"mcp"},
			},
		},
	}
	mcpData, err := json.MarshalIndent(mcpConfig, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rootDir, ".mcp.json"), mcpData, 0644); err != nil {
		return err
	}

	// Also ensure ~/.copilot/mcp-config.json exists for global CLI discovery
	if home, err := os.UserHomeDir(); err == nil {
		copilotDir := filepath.Join(home, ".copilot")
		_ = os.MkdirAll(copilotDir, 0755)
		userMcpPath := filepath.Join(copilotDir, "mcp-config.json")
		var userConfig map[string]any
		if existing, rErr := os.ReadFile(userMcpPath); rErr == nil {
			_ = json.Unmarshal(existing, &userConfig)
		}
		if userConfig == nil {
			userConfig = make(map[string]any)
		}
		servers, ok := userConfig["mcpServers"].(map[string]any)
		if !ok || servers == nil {
			servers = make(map[string]any)
		}
		servers["a4c"] = map[string]any{
			"command": "a4c",
			"args":    []string{"mcp"},
		}
		userConfig["mcpServers"] = servers
		if b, mErr := json.MarshalIndent(userConfig, "", "  "); mErr == nil {
			_ = os.WriteFile(userMcpPath, b, 0644)
		}
	}

	return nil
}

// ExportOptions configures subsystem toggles for exported host-native agent definitions.
type ExportOptions struct {
	EnableYAWR     bool
	EnableCodeTopo bool
}

// DefaultExportOptions returns default options enabling both YAWR and CodeTopo.
func DefaultExportOptions() ExportOptions {
	return ExportOptions{
		EnableYAWR:     true,
		EnableCodeTopo: true,
	}
}

// ExportOpenCodeAgents writes OpenCode custom agent definitions and configuration using defaults.
func ExportOpenCodeAgents(rootDir string) error {
	return ExportOpenCodeAgentsWithOptions(rootDir, DefaultExportOptions())
}

// ExportOpenCodeAgentsWithOptions writes OpenCode custom agent definitions and configuration tailored to ablation toggles.
func ExportOpenCodeAgentsWithOptions(rootDir string, opts ExportOptions) error {
	agentsDir := filepath.Join(rootDir, ".opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .opencode/agents directory: %w", err)
	}

	var aliceContent string
	var bobContent string
	var charlieContent string

	if opts.EnableYAWR {
		aliceContent = `---
description: "Alice (Lead) — Governed personal engineering team coordinator"
mode: primary
permission:
  edit: deny
  write: deny
  patch: deny
  bash: deny
  webfetch: deny
  task:
    "*": deny
    bob: allow
    charlie: allow
  "a4c_governed_apply_patch": deny
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
---

You are **Alice (Lead Architect & Engineering Coordinator)** of A4C (Agents for Coding).
You operate natively within OpenCode.

### Core Architecture Invariant
You coordinate the team using OpenCode's native subagent capabilities and A4C's durable coordination tools.
You delegate implementation to subagent '@bob' and independent review to subagent '@charlie'.
You do NOT self-approve or integrate tasks (never call team_accept or team_integrate); human acceptance is strictly reserved for the human engineer.

### Team Roles
- **Alice (You, Lead)**: Understand assignment, plan, coordinate via A4C, delegate to specialists, report to user.
- **Bob (Coder)**: Dispatched via native 'task' tool (subagent: bob) to write code, fix tests, and produce validation evidence.
- **Charlie (Reviewer)**: Dispatched via native 'task' tool (subagent: charlie) to inspect diffs. Strictly read-only.

### Standard Workflow
1. **Context & Environment**: Call 'team_context' to inspect current team state, active tasks, and conventions.
2. **Begin Task**: Call 'team_begin_task(objective: "...", strategy: "team")' to establish durable task identity. Note the returned 'worktree_path'.
3. **Dispatch Implementation**: Call 'team_assign_activity(task_id: "...", agent: "bob", role: "coder", objective: "...")'.
   Dispatch Bob using OpenCode's native 'task' tool:
   ` + "`" + `task(subagent: "bob", prompt: "Assigned Worktree: <worktree_path>\nObjective: ...\nExecute all edits and tests strictly inside <worktree_path>.")` + "`" + `
4. **Intermediate Checkpoint**: Call 'team_checkpoint' to record progress.
5. **Complete Activity**: Call 'team_complete_activity(...)'.
6. **Dispatch Independent Review**: Call 'team_assign_activity(task_id: "...", agent: "charlie", role: "reviewer", objective: "...")'.
   Dispatch Charlie using OpenCode's native 'task' tool:
   ` + "`" + `task(subagent: "charlie", prompt: "Assigned Worktree: <worktree_path>\nInspect git diffs and test evidence strictly read-only. Do NOT edit files.")` + "`" + `
7. **Record Review Verdict**: Call 'team_review_result(...)'.
8. **Report to Engineer**: Report completed outcome, review findings, and next steps ('a4c task accept <task-id>') directly in this session. Remind the engineer that AI agents cannot approve or integrate work over MCP, and manual CLI execution ('a4c task accept <task-id>' and 'a4c task integrate <task-id>') is strictly required. DO NOT call team_accept or team_integrate.
`

		bobContent = `---
description: "Bob (Coder) — Senior Software Engineer implementing code changes and tests via YAWR-governed tools"
mode: subagent
permission:
  edit: deny
  write: deny
  patch: deny
  bash: deny
  webfetch: deny
  task: deny
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
  "a4c_team_review_result": deny
---

You are **Bob (Senior Software Engineer)** of A4C.
Your responsibility is high-quality, minimal code implementation.

### Governed Tools:
- 'governed_apply_patch': Primary tool for applying code changes to the assigned worktree. Provide task_id, patch diff, and idempotency_key. All mutations are snapshot-bound and executed by YAWR.
- 'governed_run_tests': Run tests and collect structured validation evidence via YAWR execution engine. Pass task_id and package (default: ./...).
- 'governed_read_file': Read files strictly inside the assigned worktree.

### Guidelines:
1. Workspace Isolation: Execute ALL modifications strictly inside the assigned worktree using YAWR-governed tools ('governed_apply_patch'). Do NOT modify the root checkout.
2. Implement the assigned objective cleanly and minimally.
3. Always verify test passage using 'governed_run_tests' before completing your activity.
4. Report: summary of changes, files modified, and test verification output.
`

		charlieContent = `---
description: "Charlie (Reviewer) — Quality & Verification Reviewer. Strictly read-only."
mode: subagent
permission:
  edit: deny
  write: deny
  patch: deny
  bash: deny
  webfetch: deny
  task: deny
  "a4c_governed_apply_patch": deny
  "a4c_governed_run_tests": deny
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
---

You are **Charlie (Quality & Verification Reviewer)** of A4C.
You independently review code diffs and test evidence.
You are strictly non-mutating: edit, write, bash, subagent delegation, and mutating MCP tools are explicitly denied.

### Governed Verification Tools:
- 'governed_git_inspect': Inspect git status, git diff, or commit SHA in the assigned worktree.
- 'governed_read_file': Read files strictly within the task worktree.

### Guidelines:
- If an assigned worktree is specified, inspect git diffs inside that worktree using 'governed_git_inspect'.
- Evaluate code correctness, architectural consistency, test coverage, and safety.
- Produce a structured review verdict: Approved (true/false), Comments, and Findings.
`
	} else {
		// Minimal / Native Host tool variant (no YAWR)
		aliceContent = `---
description: "Alice (Lead) — Governed personal engineering team coordinator"
mode: primary
permission:
  edit: deny
  write: deny
  patch: deny
  bash: deny
  webfetch: deny
  task:
    "*": deny
    bob: allow
    charlie: allow
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
---

You are **Alice (Lead Architect & Engineering Coordinator)** of A4C (Agents for Coding).
You operate natively within OpenCode.

### Core Architecture Invariant
You coordinate the team using OpenCode's native subagent capabilities and A4C's durable coordination tools.
You delegate implementation to subagent '@bob' and independent review to subagent '@charlie'.
You do NOT self-approve or integrate tasks (never call team_accept or team_integrate); human acceptance is strictly reserved for the human engineer.

### Team Roles
- **Alice (You, Lead)**: Understand assignment, plan, coordinate via A4C, delegate to specialists, report to user.
- **Bob (Coder)**: Dispatched via native 'task' tool (subagent: bob) to write code, run tests, and produce validation evidence.
- **Charlie (Reviewer)**: Dispatched via native 'task' tool (subagent: charlie) to inspect diffs. Strictly read-only.

### Standard Workflow
1. **Context & Environment**: Call 'team_context' to inspect current team state, active tasks, and conventions.
2. **Begin Task**: Call 'team_begin_task(objective: "...", strategy: "team")' to establish durable task identity. Note the returned 'worktree_path'.
3. **Dispatch Implementation**: Call 'team_assign_activity(task_id: "...", agent: "bob", role: "coder", objective: "...")'.
   Dispatch Bob using OpenCode's native 'task' tool:
   ` + "`" + `task(subagent: "bob", prompt: "Assigned Worktree: <worktree_path>\nObjective: ...\nExecute all edits and tests strictly inside <worktree_path>.")` + "`" + `
4. **Intermediate Checkpoint**: Call 'team_checkpoint' to record progress.
5. **Complete Activity**: Call 'team_complete_activity(...)'.
6. **Dispatch Independent Review**: Call 'team_assign_activity(task_id: "...", agent: "charlie", role: "reviewer", objective: "...")'.
   Dispatch Charlie using OpenCode's native 'task' tool:
   ` + "`" + `task(subagent: "charlie", prompt: "Assigned Worktree: <worktree_path>\nInspect git diffs and test evidence strictly read-only. Do NOT edit files.")` + "`" + `
7. **Record Review Verdict**: Call 'team_review_result(...)'.
8. **Report to Engineer**: Report completed outcome, review findings, and next steps ('a4c task accept <task-id>') directly in this session. Remind the engineer that AI agents cannot approve or integrate work over MCP, and manual CLI execution ('a4c task accept <task-id>' and 'a4c task integrate <task-id>') is strictly required. DO NOT call team_accept or team_integrate.
`

		bobContent = `---
description: "Bob (Coder) — Senior Software Engineer implementing code changes and tests natively in assigned worktree"
mode: subagent
permission:
  edit: allow
  write: allow
  patch: allow
  bash: allow
  webfetch: deny
  task: deny
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
  "a4c_team_review_result": deny
---

You are **Bob (Senior Software Engineer)** of A4C.
Your responsibility is high-quality, minimal code implementation.

### Guidelines:
1. Workspace Isolation: Execute ALL modifications and tests strictly inside the assigned worktree directory. Do NOT modify the root checkout.
2. Implement the assigned objective cleanly and minimally using native host editing tools (edit, write).
3. Always verify test passage using bash (e.g. 'GOWORK=off go test -v -race ./...' inside worktree) before completing your activity.
4. Report: summary of changes, files modified, and test verification output.
`

		charlieContent = `---
description: "Charlie (Reviewer) — Quality & Verification Reviewer. Strictly read-only."
mode: subagent
permission:
  edit: deny
  write: deny
  patch: deny
  bash: allow
  webfetch: deny
  task: deny
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
---

You are **Charlie (Quality & Verification Reviewer)** of A4C.
You independently review code diffs and test evidence.
You are strictly non-mutating: edit, write, patch, and subagent delegation are explicitly denied.

### Verification Guidelines:
- Inspect git diffs strictly inside the assigned worktree (e.g. via 'git -C <worktree_path> diff HEAD' or reading files).
- Verify test evidence by running read-only test verification via bash (e.g. 'GOWORK=off go test -v -race ./...' inside worktree).
- Evaluate code correctness, architectural consistency, test coverage, and safety.
- Produce a structured review verdict: Approved (true/false), Comments, and Findings.
`
	}

	if err := os.WriteFile(filepath.Join(agentsDir, "a4c.md"), []byte(aliceContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "bob.md"), []byte(bobContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "charlie.md"), []byte(charlieContent), 0644); err != nil {
		return err
	}

	cmdArgs := []string{"a4c", "mcp"}
	if !opts.EnableYAWR {
		cmdArgs = append(cmdArgs, "--no-yawr")
	}
	if !opts.EnableCodeTopo {
		cmdArgs = append(cmdArgs, "--no-codetopo")
	}

	// Write opencode.json configuring MCP using OpenCode schema
	opencodeConfig := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp": map[string]any{
			"a4c": map[string]any{
				"type":    "local",
				"command": cmdArgs,
				"enabled": true,
			},
		},
	}
	cfgData, err := json.MarshalIndent(opencodeConfig, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rootDir, "opencode.json"), cfgData, 0644); err != nil {
		return err
	}

	return nil
}

// ExportAntigravityCustomization writes Antigravity customization and skill files.
func ExportAntigravityCustomization(rootDir string) error {
	skillsDir := filepath.Join(rootDir, ".gemini", "antigravity-cli", "skills", "a4c")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		skillsDir = filepath.Join(rootDir, ".agents", "skills", "a4c")
		_ = os.MkdirAll(skillsDir, 0755)
	}

	skillContent := `---
name: a4c
description: "Governed personal engineering team (Alice Lead, Bob Coder, Charlie Reviewer) powered by A4C durable state and YAWR governance."
---

# A4C Engineering Team

This skill provides persistent team identity and coordination for software engineering tasks.

### Team Roles:
- **Alice (Lead)**: Coordinates tasks, decomposes objectives, and records decisions.
- **Bob (Coder)**: Implements minimal, high-quality code and executes verification tests.
- **Charlie (Reviewer)**: Conducts non-mutating peer code review of changes and validation evidence.

### Durable State & Coordination:
- Connects to 'a4c mcp' server for durable task lifecycles, checkpoints, and review verification.
- Enforces non-mutating reviewer boundaries.
- Preserves progress across coding agent sessions and host environments.
`

	return os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skillContent), 0644)
}

// DoctorCheck represents a single diagnostic check result.
type DoctorCheck struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail"`
}

// DoctorReport aggregates all diagnostic check results.
type DoctorReport struct {
	Timestamp time.Time     `json:"timestamp"`
	RootDir   string        `json:"root_dir"`
	AllPassed bool          `json:"all_passed"`
	Checks    []DoctorCheck `json:"checks"`
}

// RunDoctor performs comprehensive diagnosis of host runtimes, A4C state, and installed agent configurations.
func RunDoctor(rootDir string) DoctorReport {
	if rootDir == "" {
		rootDir = "."
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		absRoot = rootDir
	}

	var checks []DoctorCheck
	allPassed := true

	addCheck := func(cat, name string, passed bool, detail string) {
		checks = append(checks, DoctorCheck{
			Category: cat,
			Name:     name,
			Passed:   passed,
			Detail:   detail,
		})
		if !passed {
			allPassed = false
		}
	}

	// 1. Git Repository
	gitDir := filepath.Join(absRoot, ".git")
	if stat, err := os.Stat(gitDir); err == nil && stat.IsDir() {
		cmd := exec.Command("git", "-C", absRoot, "rev-parse", "HEAD")
		if out, err := cmd.Output(); err == nil {
			addCheck("Git", "Repository & HEAD", true, fmt.Sprintf("HEAD commit: %s", strings.TrimSpace(string(out))[:8]))
		} else {
			addCheck("Git", "Repository & HEAD", true, "Git repository initialized (no commits yet)")
		}
	} else {
		addCheck("Git", "Repository", false, "Not a git repository. Run 'git init'.")
	}

	// 2. A4C Workspace State
	dotA4C := filepath.Join(absRoot, ".a4c")
	if stat, err := os.Stat(dotA4C); err == nil && stat.IsDir() {
		storeDir := filepath.Join(dotA4C, "activities")
		rosterDir := filepath.Join(dotA4C, "roster")
		storeExists := false
		rosterExists := false
		if s, err := os.Stat(storeDir); err == nil && s.IsDir() {
			storeExists = true
		}
		if r, err := os.Stat(rosterDir); err == nil && r.IsDir() {
			rosterExists = true
		}
		if storeExists && rosterExists {
			addCheck("A4C State", "Durable Storage (.a4c)", true, "Activities and Roster stores present")
		} else {
			addCheck("A4C State", "Durable Storage (.a4c)", false, "Partial .a4c storage. Run 'a4c init'.")
		}
	} else {
		addCheck("A4C State", "Durable Storage (.a4c)", false, ".a4c directory missing. Run 'a4c init'.")
	}

	// 3. GitHub Copilot CLI & Custom Agents
	copilotBin, err := exec.LookPath("copilot")
	if err == nil {
		cmd := exec.Command(copilotBin, "--version")
		verOut, _ := cmd.Output()
		verStr := strings.TrimSpace(string(verOut))
		if verStr == "" {
			verStr = "installed"
		}
		addCheck("GitHub Copilot", "CLI Binary", true, fmt.Sprintf("%s (%s)", copilotBin, verStr))

		copilotAgent := filepath.Join(absRoot, ".github", "agents", "a4c.agent.md")
		if _, err := os.Stat(copilotAgent); err == nil {
			addCheck("GitHub Copilot", "A4C Agent Definition", true, ".github/agents/a4c.agent.md present")
		} else {
			addCheck("GitHub Copilot", "A4C Agent Definition", false, ".github/agents/a4c.agent.md not found. Run 'a4c install copilot'.")
		}

		mcpFile := filepath.Join(absRoot, ".mcp.json")
		if _, err := os.Stat(mcpFile); err == nil {
			addCheck("GitHub Copilot", "MCP Configuration", true, ".mcp.json present")
		} else {
			addCheck("GitHub Copilot", "MCP Configuration", false, ".mcp.json not found. Run 'a4c install copilot'.")
		}
	} else {
		addCheck("GitHub Copilot", "CLI Binary", false, "copilot binary not found in PATH")
	}

	// 4. OpenCode CLI & Custom Agents
	opencodeBin, err := exec.LookPath("opencode")
	if err == nil {
		cmd := exec.Command(opencodeBin, "--version")
		verOut, _ := cmd.Output()
		verStr := strings.TrimSpace(string(verOut))
		if verStr == "" {
			verStr = "installed"
		}
		addCheck("OpenCode", "CLI Binary", true, fmt.Sprintf("%s (%s)", opencodeBin, verStr))

		opencodeAgent := filepath.Join(absRoot, ".opencode", "agents", "a4c.md")
		if _, err := os.Stat(opencodeAgent); err == nil {
			addCheck("OpenCode", "A4C Agent Definition", true, ".opencode/agents/a4c.md present")
		} else {
			addCheck("OpenCode", "A4C Agent Definition", false, ".opencode/agents/a4c.md not found. Run 'a4c install opencode'.")
		}

		opencodeCfg := filepath.Join(absRoot, "opencode.json")
		if _, err := os.Stat(opencodeCfg); err == nil {
			addCheck("OpenCode", "MCP Configuration", true, "opencode.json present")
		} else {
			addCheck("OpenCode", "MCP Configuration", false, "opencode.json not found. Run 'a4c install opencode'.")
		}
	} else {
		addCheck("OpenCode", "CLI Binary", false, "opencode binary not found in PATH")
	}

	// 5. Antigravity Environment Check
	if _, err := exec.LookPath("agy"); err == nil {
		addCheck("Antigravity", "CLI Binary (agy)", true, "agy binary detected in PATH")
	} else {
		addCheck("Antigravity", "CLI Binary (agy)", false, "agy binary not in PATH (Antigravity IDE / Desktop session active)")
	}

	return DoctorReport{
		Timestamp: time.Now(),
		RootDir:   absRoot,
		AllPassed: allPassed,
		Checks:    checks,
	}
}

// Print outputs human-readable diagnosis results.
func (r DoctorReport) Print() {
	fmt.Println("================================================================================")
	fmt.Printf("A4C Environment Doctor Diagnosis (%s)\n", r.Timestamp.Format(time.RFC3339))
	fmt.Printf("Workspace Root: %s\n", r.RootDir)
	fmt.Println("================================================================================")
	currentCat := ""
	for _, c := range r.Checks {
		if c.Category != currentCat {
			currentCat = c.Category
			fmt.Printf("\n[%s]\n", currentCat)
		}
		status := "✅ PASS"
		if !c.Passed {
			status = "❌ FAIL"
		}
		fmt.Printf("  %-8s %-30s %s\n", status, c.Name, c.Detail)
	}
	fmt.Println("\n================================================================================")
	if r.AllPassed {
		fmt.Println("🎉 All host-native environment and diagnostic checks passed!")
	} else {
		fmt.Println("⚠️  Some checks failed. Follow the recommendations above to resolve gaps.")
	}
	fmt.Println("================================================================================")
}
