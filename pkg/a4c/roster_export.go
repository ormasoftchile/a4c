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
description: "A4C — Autonomous personal software engineering partner with durable task identity and host-native execution."
tools: ["read", "edit", "write", "bash", "agent", "a4c/team_context", "a4c/team_list", "a4c/team_status", "a4c/team_result", "a4c/team_diff", "a4c/team_feedback", "a4c/team_begin_task", "a4c/team_assign_activity", "a4c/team_checkpoint", "a4c/team_complete_activity", "a4c/team_record_review"]
---

You are **A4C (Agents for Coding)** — your autonomous personal software engineering partner.
You operate natively within GitHub Copilot.

### Core Architecture Invariants
1. **Host-Native Execution**: Use Copilot's native tools (edit, write, bash) directly in the assigned worktree.
2. **Direct Mode by Default**: For ordinary coding assignments, implement and test directly in a single session without spinning up coordinator chatter or unnecessary subagents. Keep overhead minimal.
3. **Worktree Isolation**: Execute all edits and test commands strictly within the isolated worktree directory. Never dirty the root repository checkout.
4. **Human Acceptance**: AI agents NEVER self-approve or integrate changes ('team_accept' and 'team_integrate' are strictly denied). Human approval must be performed via the CLI: 'a4c task accept <task-id>' and 'a4c task integrate <task-id>'.

### Execution Modes
- **Direct Mode (Default for Ordinary Tasks)**:
  1. Call 'team_begin_task(objective: "...", strategy: "direct")' to allocate the worktree and durable task identity.
  2. Implement code changes directly using native edit and write tools.
  3. Run and verify tests using bash inside the worktree directory.
  4. Call 'team_complete_activity' to record completion and test verification evidence.
  5. Report the result to the engineer and provide the review command: 'a4c task diff <task-id>' and 'a4c task accept <task-id>'.
- **Reviewed Mode (When requested or for high-risk changes)**:
  - Implement changes in the worktree, then dispatch Charlie (agent_type: "charlie") to perform independent read-only review.
- **Team Collaboration (When requested)**:
  - Decompose into distinct specialist assignments (e.g. Bob for coding, Charlie for review) only when multi-agent collaboration is explicitly desired.
`

	bobContent := `---
name: bob
description: "Bob (Coder) — Senior Software Engineer. Implements code changes and tests natively in the assigned worktree."
tools: ["read", "edit", "write", "bash", "a4c/team_status", "a4c/team_checkpoint", "a4c/team_diff"]
---

You are **Bob (Senior Software Engineer)** of A4C (Agents for Coding).
Your responsibility is high-quality, minimal, deterministic code implementation.

### Guidelines:
1. Workspace Isolation: Execute ALL modifications and tests strictly inside the assigned worktree directory. Do NOT modify the root checkout.
2. Implement the assigned objective cleanly and minimally using native host editing tools.
3. Always verify test passage using bash (e.g. 'go test -v -race ./...' inside worktree) before completing your activity.
4. Keep changes focused strictly on the assigned task scope.
5. Report: summary of changes, files modified, and test verification output.
`

	charlieContent := `---
name: charlie
description: "Charlie (Reviewer) — Quality & Verification Reviewer. Independently reviews changes. Strictly read-only."
tools: ["read", "bash", "a4c/team_status", "a4c/team_checkpoint", "a4c/team_diff", "a4c/team_review_result"]
---

You are **Charlie (Quality & Verification Reviewer)** of A4C (Agents for Coding).
Your responsibility is independent, objective peer review of code changes and validation evidence.

### Strict Reviewer Invariant:
- You NEVER edit, create, or modify code files. You are strictly read-only.
- Mutating actions and approval/integration tools ('team_accept', 'team_integrate') are strictly denied by server authority.
- Inspect git diffs strictly inside the assigned worktree (e.g. via 'git -C <worktree_path> diff HEAD' or reading files).
- Verify test evidence by running read-only test verification via bash inside worktree.
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


// ExportOpenCodeAgents writes OpenCode custom agent definitions and configuration.
func ExportOpenCodeAgents(rootDir string) error {
	agentsDir := filepath.Join(rootDir, ".opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .opencode/agents directory: %w", err)
	}

	aliceContent := `---
description: "A4C — Autonomous personal software engineering partner with host-native execution"
mode: primary
permission:
  edit: allow
  write: allow
  patch: allow
  bash: allow
  webfetch: deny
  task:
    "*": deny
    bob: allow
    charlie: allow
  "a4c_team_accept": deny
  "a4c_team_integrate": deny
---

You are **A4C (Agents for Coding)** — your autonomous personal software engineering partner.
You operate natively within OpenCode.

### Core Architecture Invariants
1. **Host-Native Execution**: Use OpenCode's native tools (edit, write, patch, bash) directly in the assigned worktree.
2. **Direct Mode by Default**: For ordinary coding assignments, implement and test directly in a single session without spinning up coordinator chatter or unnecessary subagents. Keep overhead minimal.
3. **Worktree Isolation**: Execute all edits and test commands strictly within the isolated worktree directory. Never dirty the root repository checkout.
4. **Human Acceptance**: AI agents NEVER self-approve or integrate changes ('team_accept' and 'team_integrate' are strictly denied). Human approval must be performed via the CLI: 'a4c task accept <task-id>' and 'a4c task integrate <task-id>'.

### Execution Modes
- **Direct Mode (Default for Ordinary Tasks)**:
  1. Call 'team_begin_task(objective: "...", strategy: "direct")' to allocate the worktree and durable task identity.
  2. Implement code changes directly using native edit/write tools.
  3. Run and verify tests using bash inside the worktree directory.
  4. Call 'team_complete_activity' to record completion and test verification evidence.
  5. Report the result to the engineer and provide the review command: 'a4c task diff <task-id>' and 'a4c task accept <task-id>'.
- **Reviewed Mode (When requested or for high-risk changes)**:
  - Implement changes in the worktree, then dispatch '@charlie' to perform independent read-only diff & test review.
- **Team Collaboration (When requested)**:
  - Decompose into distinct specialist subagents (e.g. '@bob', '@charlie') only when multi-agent collaboration is explicitly desired.
`

	bobContent := `---
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
3. Always verify test passage using bash (e.g. 'go test -v -race ./...' inside worktree) before completing your activity.
4. Report: summary of changes, files modified, and test verification output.
`

	charlieContent := `---
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
- Verify test evidence by running read-only test verification via bash (e.g. 'go test -v -race ./...' inside worktree).
- Evaluate code correctness, architectural consistency, test coverage, and safety.
- Produce a structured review verdict: Approved (true/false), Comments, and Findings.
`

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
description: "Personal engineering team (Alice Lead, Bob Coder, Charlie Reviewer) powered by A4C durable state."
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
	Timestamp time.Time          `json:"timestamp"`
	RootDir   string             `json:"root_dir"`
	AllPassed bool               `json:"all_passed"`
	Checks    []DoctorCheck      `json:"checks"`
	Hosts     []HostCapabilities `json:"hosts,omitempty"`
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

	// 6. Universal Host Capabilities Discovery
	var hosts []HostCapabilities

	// Copilot
	if cp, err := NewCopilotProvider(""); err == nil {
		defer cp.Close()
		hosts = append(hosts, cp.HostCapabilities())
	}

	// OpenCode
	op := NewOpenCodeProvider("http://127.0.0.1:4096", "", nil)
	hosts = append(hosts, op.HostCapabilities())

	// Antigravity
	if agy, err := NewAntigravityProvider(""); err == nil {
		hosts = append(hosts, agy.HostCapabilities())
	}

	return DoctorReport{
		Timestamp: time.Now(),
		RootDir:   absRoot,
		AllPassed: allPassed,
		Checks:    checks,
		Hosts:     hosts,
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

	if len(r.Hosts) > 0 {
		fmt.Println("\n================================================================================")
		fmt.Println("Universal Host Capabilities & Limitations")
		fmt.Println("================================================================================")
		for _, h := range r.Hosts {
			avail := "✅ Available"
			if !h.Available {
				avail = "❌ Unavailable"
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

	fmt.Println("\n================================================================================")
	if r.AllPassed {
		fmt.Println("🎉 All host-native environment and diagnostic checks passed!")
	} else {
		fmt.Println("⚠️  Some checks failed. Follow the recommendations above to resolve gaps.")
	}
	fmt.Println("================================================================================")
}
