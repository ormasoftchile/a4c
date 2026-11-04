package a4c

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InitiativePrerequisite describes the readiness of a single tool or workspace asset.
type InitiativePrerequisite struct {
	Name        string `json:"name"`
	Category    string `json:"category"` // "Host Tool" or "Workspace Config"
	Required    bool   `json:"required"`
	Satisfied   bool   `json:"satisfied"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// InitiativePreflightReport aggregates all prerequisite checks for running initiatives.
type InitiativePreflightReport struct {
	AllSatisfied    bool                     `json:"all_satisfied"`
	MissingBinaries []InitiativePrerequisite `json:"missing_binaries,omitempty"`
	MissingConfigs  []InitiativePrerequisite `json:"missing_configs,omitempty"`
	Checks          []InitiativePrerequisite `json:"checks"`
}

// CheckInitiativePrerequisites performs an audit of all external CLI dependencies
// and project configuration files required for Squad, Spec-Kit, and A4C initiative execution.
func CheckInitiativePrerequisites(workDir string) InitiativePreflightReport {
	if workDir == "" {
		workDir = "."
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		absWork = workDir
	}

	var checks []InitiativePrerequisite
	var missingBinaries []InitiativePrerequisite
	var missingConfigs []InitiativePrerequisite
	allSatisfied := true

	addCheck := func(name, category string, satisfied bool, detail, rem string) {
		item := InitiativePrerequisite{
			Name:        name,
			Category:    category,
			Required:    true,
			Satisfied:   satisfied,
			Detail:      detail,
			Remediation: rem,
		}
		checks = append(checks, item)
		if !satisfied {
			allSatisfied = false
			if category == "Host Tool" {
				missingBinaries = append(missingBinaries, item)
			} else {
				missingConfigs = append(missingConfigs, item)
			}
		}
	}

	// 1. Git binary & repository
	gitBin, err := exec.LookPath("git")
	if err == nil {
		addCheck("git", "Host Tool", true, fmt.Sprintf("binary at %s", gitBin), "")
		cmd := exec.Command(gitBin, "-C", absWork, "rev-parse", "--is-inside-work-tree")
		if err := cmd.Run(); err == nil {
			addCheck("Git Repository", "Workspace Config", true, "valid git working tree", "")
		} else {
			addCheck("Git Repository", "Workspace Config", false, "directory is not inside a git repository", "Run 'git init' to initialize version control.")
		}
	} else {
		addCheck("git", "Host Tool", false, "git binary not found in PATH", "Install Git from https://git-scm.com or your system package manager.")
		addCheck("Git Repository", "Workspace Config", false, "cannot verify repository without git binary", "Install Git first.")
	}

	// 2. GitHub Copilot CLI & agent wiring
	copilotBin, err := exec.LookPath("copilot")
	if err == nil {
		cmd := exec.Command(copilotBin, "--version")
		verOut, _ := cmd.Output()
		verStr := strings.TrimSpace(string(verOut))
		if verStr == "" {
			verStr = "installed"
		}
		addCheck("copilot", "Host Tool", true, fmt.Sprintf("binary at %s (%s)", copilotBin, verStr), "")
	} else {
		addCheck("copilot", "Host Tool", false, "copilot binary not found in PATH", "Install Copilot CLI: npm install -g @github/copilot (then 'copilot auth')")
	}

	a4cAgent := filepath.Join(absWork, ".github", "agents", "a4c.agent.md")
	mcpFile := filepath.Join(absWork, ".mcp.json")
	_, errAgent := os.Stat(a4cAgent)
	_, errMcp := os.Stat(mcpFile)
	if errAgent == nil && errMcp == nil {
		addCheck("Copilot A4C Wiring", "Workspace Config", true, ".github/agents/a4c.agent.md and .mcp.json present", "")
	} else {
		addCheck("Copilot A4C Wiring", "Workspace Config", false, "Copilot A4C custom agent or .mcp.json missing", "Run 'a4c install copilot' to wire A4C MCP server and agent definition.")
	}

	// 3. Squad CLI & team roles
	squadBin, err := exec.LookPath("squad")
	if err == nil {
		cmd := exec.Command(squadBin, "--version")
		verOut, _ := cmd.Output()
		verStr := strings.TrimSpace(string(verOut))
		if verStr == "" {
			verStr = "installed"
		}
		addCheck("squad", "Host Tool", true, fmt.Sprintf("binary at %s (%s)", squadBin, verStr), "")
	} else {
		addCheck("squad", "Host Tool", false, "squad binary not found in PATH", "Install Squad CLI: npm install -g @bradygaster/squad-cli")
	}

	squadDir := filepath.Join(absWork, ".squad")
	squadAgent := filepath.Join(absWork, ".github", "agents", "squad.agent.md")
	_, errSquadDir := os.Stat(squadDir)
	_, errSquadAgent := os.Stat(squadAgent)
	if errSquadDir == nil && errSquadAgent == nil {
		addCheck("Squad Roles & Agent", "Workspace Config", true, ".squad/ and .github/agents/squad.agent.md present", "")
	} else {
		addCheck("Squad Roles & Agent", "Workspace Config", false, ".squad/ or .github/agents/squad.agent.md missing", "Run 'squad init --roles' in the repository root.")
	}

	// 4. Spec-Kit CLI & templates
	specifyBin, err := exec.LookPath("specify")
	if err == nil {
		cmd := exec.Command(specifyBin, "--version")
		verOut, _ := cmd.Output()
		verStr := strings.TrimSpace(string(verOut))
		if verStr == "" {
			verStr = "installed"
		}
		addCheck("specify", "Host Tool", true, fmt.Sprintf("binary at %s (%s)", specifyBin, verStr), "")
	} else {
		addCheck("specify", "Host Tool", false, "specify binary not found in PATH", "Install Spec-Kit: brew install specify (or: uv tool install specify-cli)")
	}

	specifyDir := filepath.Join(absWork, ".specify")
	if stat, err := os.Stat(specifyDir); err == nil && stat.IsDir() {
		addCheck("Spec-Kit Templates", "Workspace Config", true, ".specify/ present", "")
	} else {
		addCheck("Spec-Kit Templates", "Workspace Config", false, ".specify/ directory missing", "Run 'specify init --here --force --non-interactive --integration copilot'.")
	}

	return InitiativePreflightReport{
		AllSatisfied:    allSatisfied,
		MissingBinaries: missingBinaries,
		MissingConfigs:  missingConfigs,
		Checks:          checks,
	}
}

// AutoScaffoldInitiative initializes any missing repository-level configuration files
// (Copilot agent wiring, Squad roles, Spec-Kit templates) if their respective CLI
// binaries are installed on the system.
func AutoScaffoldInitiative(workDir string) ([]string, error) {
	if workDir == "" {
		workDir = "."
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		absWork = workDir
	}

	var actions []string

	// 1. A4C base state and Copilot wiring
	a4cAgent := filepath.Join(absWork, ".github", "agents", "a4c.agent.md")
	mcpFile := filepath.Join(absWork, ".mcp.json")
	if _, errAgent := os.Stat(a4cAgent); os.IsNotExist(errAgent) || func() bool { _, errM := os.Stat(mcpFile); return os.IsNotExist(errM) }() {
		if err := ExportCopilotAgents(absWork); err != nil {
			return actions, fmt.Errorf("scaffold copilot agents: %w", err)
		}
		actions = append(actions, "Installed Copilot custom agent and MCP (.github/agents/a4c.agent.md, .mcp.json)")
	}

	// 2. Squad roles and agent definition
	squadDir := filepath.Join(absWork, ".squad")
	squadAgent := filepath.Join(absWork, ".github", "agents", "squad.agent.md")
	if _, errSquad := os.Stat(squadDir); os.IsNotExist(errSquad) || func() bool { _, errA := os.Stat(squadAgent); return os.IsNotExist(errA) }() {
		if squadBin, err := exec.LookPath("squad"); err == nil {
			cmd := exec.Command(squadBin, "init", "--roles")
			cmd.Dir = absWork
			cmd.Stdin = strings.NewReader("y\n") // Provide non-interactive confirmation for Copilot autonomous team member
			if out, err := cmd.CombinedOutput(); err != nil {
				return actions, fmt.Errorf("squad init --roles: %w (%s)", err, strings.TrimSpace(string(out)))
			}
			actions = append(actions, "Initialized Squad team roles and agent (.squad/, .github/agents/squad.agent.md)")
		}
	}

	// 3. Spec-Kit templates
	specifyDir := filepath.Join(absWork, ".specify")
	if _, errSpecify := os.Stat(specifyDir); os.IsNotExist(errSpecify) {
		if specifyBin, err := exec.LookPath("specify"); err == nil {
			cmd := exec.Command(specifyBin, "init", "--here", "--force", "--non-interactive", "--integration", "copilot")
			cmd.Dir = absWork
			if out, err := cmd.CombinedOutput(); err != nil {
				return actions, fmt.Errorf("specify init: %w (%s)", err, strings.TrimSpace(string(out)))
			}
			actions = append(actions, "Initialized Spec-Kit templates and workflow (.specify/)")
		}
	}

	return actions, nil
}

// Print outputs a formatted preflight diagnostic report to stdout.
func (r InitiativePreflightReport) Print() {
	fmt.Println("================================================================================")
	fmt.Println("A4C Initiative Preflight Diagnostic")
	fmt.Println("================================================================================")

	currentCat := ""
	for _, c := range r.Checks {
		if c.Category != currentCat {
			currentCat = c.Category
			fmt.Printf("\n[%s]\n", currentCat)
		}
		status := "✅ PASS"
		if !c.Satisfied {
			status = "❌ FAIL"
		}
		fmt.Printf("  %-8s %-26s %s\n", status, c.Name, c.Detail)
		if !c.Satisfied && c.Remediation != "" {
			fmt.Printf("           -> Action: %s\n", c.Remediation)
		}
	}

	fmt.Println("\n================================================================================")
	if r.AllSatisfied {
		fmt.Println("🎉 All initiative dependencies and workspace configs are satisfied!")
	} else {
		if len(r.MissingBinaries) > 0 {
			fmt.Println("❌ Missing host CLI dependencies. Install the missing tools listed above.")
		}
		if len(r.MissingConfigs) > 0 {
			fmt.Println("⚠️  Missing workspace configurations. Run 'a4c initiative preflight --fix' to auto-scaffold.")
		}
	}
	fmt.Println("================================================================================")
}
