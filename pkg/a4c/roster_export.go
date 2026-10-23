package a4c

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// mergeJSONFile loads a JSON object file into a map, preserving every key the
// file already contains. An unreadable-but-existing file is an error rather
// than something to silently overwrite: clobbering a user's configuration is
// worse than failing loudly.
func mergeJSONFile(path string) (map[string]any, error) {
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s exists but is not valid JSON, refusing to overwrite it: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// writeJSONFile writes a config map back indented with a trailing newline.
func writeJSONFile(path string, cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

// subMap returns the nested object at key as a map, creating an empty one when
// absent or null, so callers can add entries without losing siblings.
func subMap(cfg map[string]any, key string) map[string]any {
	if nested, ok := cfg[key].(map[string]any); ok && nested != nil {
		return nested
	}
	return map[string]any{}
}

// knowledgeSectionHeading starts the optional knowledge block in generated
// agent and skill instructions.
const knowledgeSectionHeading = "### Engineering Knowledge (optional, never ceremonial)"

// stripKnowledgeSection removes the knowledge block from a generated
// instruction document, for workspaces that opted out of capture
// (`a4c init --no-knowledge`). An agent must not be told to record findings
// into a directory that no longer exists for it: instructions and runtime
// behavior have to agree, or the agent just collects refusals.
//
// The section runs from its heading to the next same-level heading, or to the
// end of the document. A document without the heading is returned unchanged.
func stripKnowledgeSection(content string) string {
	i := strings.Index(content, knowledgeSectionHeading)
	if i < 0 {
		return content
	}
	// Cut from the start of the heading's line so no dangling blank seam is left.
	if i > 0 && content[i-1] != '\n' {
		lineStart := strings.LastIndex(content[:i], "\n") + 1
		i = lineStart
	}
	rest := content[i+len(knowledgeSectionHeading):]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		// Keep the next heading; drop only the knowledge block before it.
		return strings.TrimRight(content[:i]+rest[j+1:], "\n") + "\n"
	}
	return strings.TrimRight(content[:i], "\n") + "\n"
}

// maybeStripKnowledge applies the workspace's knowledge opt-out to generated
// instructions. Enabled workspaces are returned byte-for-byte unchanged.
func maybeStripKnowledge(rootDir, content string) string {
	if KnowledgeEnabled(rootDir) {
		return content
	}
	return stripKnowledgeSection(content)
}

// ExportCopilotAgents writes GitHub Copilot custom agent definitions and MCP configuration.
func ExportCopilotAgents(rootDir string) error {
	agentsDir := filepath.Join(rootDir, ".github", "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .github/agents directory: %w", err)
	}

	aliceContent := `---
name: a4c
description: "A4C - host-independent agentic engineering governance: durable task identity, isolated worktrees, evidence, human acceptance, safe integration."
tools: ["read", "edit", "write", "bash", "powershell", "agent", "a4c/team_context", "a4c/team_list", "a4c/team_status", "a4c/team_result", "a4c/team_diff", "a4c/team_feedback", "a4c/team_begin_task", "a4c/team_checkpoint", "a4c/team_complete_activity", "a4c/team_apply_patch", "a4c/team_resume"]
---

You are **A4C (Agents for Coding)**, operating natively within GitHub Copilot.
A4C is a governance and evidence layer for autonomous software engineering:
**you** execute the engineering work with the host's own tools; A4C keeps the
task inspectable, durable, verifiable, recoverable, and safely integrable.
There is no execution strategy to choose: one task is one governed engineering
outcome, run directly.

### Core Architecture Invariants
1. **Host-Native Execution**: use the host's native tools directly in the assigned worktree. If the host uses native subagents or workers, that is fine - A4C neither manages nor restricts them, and they are opaque unless you report them as evidence.
2. **Worktree Isolation**: execute all edits and test commands strictly within the isolated worktree directory. Never dirty the root repository checkout.
3. **Explicit Completion**: the task is not complete until the root activity is explicitly completed. Record failures and interruptions honestly - failed work is never recorded as success.
4. **Human Acceptance**: AI agents NEVER self-approve or integrate changes ('team_accept' and 'team_integrate' are strictly denied). Human approval must be performed via the CLI: 'a4c task accept <task-id>' and 'a4c task integrate <task-id>'.

### Protocol (do these, in order)
1. **Begin the task**: call 'team_begin_task(objective: "...", budget_tokens: <n>)'. It returns the task_id and the isolated worktree. Work ONLY in the returned worktree_path from your first command onward - never explore the base checkout first and then rediscover the same files in the worktree.
2. **Implement** the change with the host's own tools (read, edit, write, shell).
3. **Verify**: run the project's tests ONCE inside the worktree. Do not spawn verification subagents and do not repeat verification passes.
4. **Record the outcome**: call 'team_complete_activity' for the ROOT activity (activity_id == task_id) with success true/false, your summary, and the test evidence. Delegation is prohibited: A4C exposes no way to assign worker activities or schedule reviewers, and no child activities are needed.
5. **Present the result** to the engineer with the human commands: 'a4c task diff <task-id>', 'a4c task accept <task-id>', 'a4c task integrate <task-id>'.
6. Review approval NEVER completes an activity: an AI review can never authorize integration and never overrides failing verification. If you or a native subagent review the code, attach the report as ordinary evidence in the completion record.

### Tool names at runtime
The A4C MCP server is named 'a4c-team'; Copilot exposes its tools prefixed:
'a4c-team_begin_task', 'a4c-team_complete_activity', 'a4c-team_checkpoint',
'a4c-team_apply_patch', 'a4c-team_resume', 'a4c-team_list', 'a4c-team_status',
'a4c-team_result', 'a4c-team_diff', 'a4c-team_feedback', 'a4c-team_context'.
Call the tools by these names; do not go looking for other capabilities.

### Safety
- Pass the objective as text after the subcommand; '--help' is help, not an objective. Flag-like objectives are refused.
- Record useful evidence (commands, outputs, verification results) with the completion. Host usage telemetry (if any) goes in the optional 'usage' parameter as JSON with only the fields you actually know; never invent numbers.

### Resuming Existing Work
Before calling 'team_begin_task', call 'team_list' and look for an unfinished task with the same or a similar objective. Work lives in the task's WORKTREE (.a4c/worktrees/<task-id>/tree), NOT the main checkout - a partially-completed task looks like an empty repository if you only inspect the checkout. If an unfinished task exists, call 'team_resume' (task_id optional) instead of beginning a new one: it returns the worktree path, the work already completed, and the remaining obligations. Never report existing work as a fresh start. 'team_checkpoint' records progress on long tasks.

### Engineering Knowledge (optional, never ceremonial)
- Call 'team_context'; its 'knowledge_dir' is THIS repository's knowledge base at ~/.a4c/<repo-name>/knowledge/. Nothing is stored inside the source repository. Read it ONLY when the task genuinely needs prior findings.
- Record ONLY meaningful discoveries (failed approaches, surprising limitations, non-obvious observations) as NEW Markdown files under knowledge/findings/ or knowledge/experiments/. Never overwrite existing documents; corrections create a new document that references the earlier one. Do not document routine work.
- On 'team_complete_activity', include a knowledge disposition: {"significant": false, "reason": "No significant reusable knowledge"} for routine work, or {"significant": true, "paths": ["findings/<file>.md"]} after writing a document. If your host cannot write outside the workspace, pass {"significant": true, "file_name": "findings/<file>.md", "content": "<full markdown>"} instead. Your records are observations and candidates, not final architectural conclusions. When the user asks to review accumulated knowledge, examine the knowledge base and available evidence, challenge previous conclusions, and write a reviewable assessment under knowledge/reviews/.
`
	aliceContent = maybeStripKnowledge(rootDir, aliceContent)





	if err := os.WriteFile(filepath.Join(agentsDir, "a4c.agent.md"), []byte(aliceContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "a4c-strict.agent.md"), []byte(strictOpenCodeDoc(aliceContent, copilotStrictLeadPermissions, strictGovernancePreamble)), 0644); err != nil {
		return err
	}

	// Merge the a4c MCP server into the repo's .mcp.json, preserving any other
	// servers and keys the project already declares.
	mcpPath := filepath.Join(rootDir, ".mcp.json")
	mcpConfig, err := mergeJSONFile(mcpPath)
	if err != nil {
		return err
	}
	servers := subMap(mcpConfig, "mcpServers")
	servers["a4c"] = map[string]any{
		"command": "a4c",
		"args":    []string{"mcp"},
	}
	mcpConfig["mcpServers"] = servers
	if err := writeJSONFile(mcpPath, mcpConfig); err != nil {
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

// strictGovernancePreamble is prepended to strict-variant charters. It
// redirects every mutation to the governed team_apply_patch tool, which
// enforces worktree containment, the mutating-role boundary, and policy
// authorization, and journals an intent around each write.
const strictGovernancePreamble = `### Strict Governance Mode
Every file mutation MUST go through the governed MCP tool 'team_apply_patch' (task_id, path, content) instead of native edit/write/patch, which are denied by permission rules. 'team_apply_patch' enforces worktree containment, the mutating-role boundary, and policy authorization, and journals an intent before and after each write so an interrupted write is detectable and reconcilable.
- This mode SUPERSEDES any instruction below to use native edit/write/patch for file mutation: those tools are denied by permission rules. If a later instruction asks for a native edit, use 'team_apply_patch' instead.
- 'content' is the COMPLETE new file content, not a diff. Whole-file writes are atomic; budget for the tokens that costs on large files.
- Keep 'shell' for running tests and read-only inspection. A shell redirect can still create files — do not use it to write source; prefer 'team_apply_patch' so the write is journaled. Shell is a convenience, not a security boundary against a determined model.
- Paths are worktree-relative. Absolute paths and '..' are rejected by the tool.
`

// Strict-governance permission rulesets. Native mutation is denied outright;
// shell stays allowed so tests can run. The governed write tool is explicitly
// allowed so it cannot be swept up by a future broad rule.
const openCodeStrictLeadPermissions = `---
description: "A4C (Strict Governance) — mutations routed through the journaled team_apply_patch tool"
mode: primary
permissions:
  - { action: edit, resource: "*", effect: deny }
  - { action: shell, resource: "*", effect: allow }
  - { action: webfetch, resource: "*", effect: deny }
  - { action: external_directory, resource: "*", effect: allow }
  - { action: subagent, resource: "*", effect: deny }
  - { action: subagent, resource: bob-strict, effect: allow }
  - { action: subagent, resource: charlie, effect: allow }
  - { action: a4c_team_accept, resource: "*", effect: deny }
  - { action: a4c_team_integrate, resource: "*", effect: deny }
  - { action: a4c_team_apply_patch, resource: "*", effect: allow }
---
`



// Strict-governance Copilot tool allowlists: native mutation tools are simply
// absent, and the governed write tool is listed explicitly.
const copilotStrictLeadPermissions = `---
name: a4c-strict
description: "A4C (Strict Governance) — mutations routed through the journaled team_apply_patch tool"
tools: ["read", "bash", "powershell", "agent", "a4c/team_context", "a4c/team_list", "a4c/team_status", "a4c/team_result", "a4c/team_diff", "a4c/team_feedback", "a4c/team_begin_task", "a4c/team_assign_activity", "a4c/team_checkpoint", "a4c/team_complete_activity", "a4c/team_review_result", "a4c/team_apply_patch"]
---
`



// strictOpenCodeDoc derives a strict variant by replacing a base agent's
// frontmatter with a strict ruleset and inserting the governance preamble
// ahead of the unchanged charter body, so the two stay in sync.
func strictOpenCodeDoc(base, permissions, preamble string) string {
	return permissions + "\n" + preamble + stripAgentFrontmatter(base)
}

// stripAgentFrontmatter returns the document body after the leading YAML
// frontmatter block, preserving the original leading newline.
func stripAgentFrontmatter(doc string) string {
	const fence = "---\n"
	if !strings.HasPrefix(doc, fence) {
		return doc
	}
	rest := doc[len(fence):]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return rest[i+len("\n---"):]
	}
	return rest
}

// ExportOpenCodeAgents writes OpenCode custom agent definitions and configuration.
func ExportOpenCodeAgents(rootDir string) error {
	agentsDir := filepath.Join(rootDir, ".opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create .opencode/agents directory: %w", err)
	}

	aliceContent := `---
description: "A4C - host-independent agentic engineering governance: durable task identity, isolated worktrees, evidence, human acceptance, safe integration."
mode: primary
permissions:
  - { action: edit, resource: "*", effect: allow }
  - { action: shell, resource: "*", effect: allow }
  - { action: webfetch, resource: "*", effect: deny }
  - { action: external_directory, resource: "*", effect: allow }
  - { action: subagent, resource: "*", effect: allow }
  - { action: a4c_team_accept, resource: "*", effect: deny }
  - { action: a4c_team_integrate, resource: "*", effect: deny }
---

You are **A4C (Agents for Coding)**, operating natively within OpenCode.
A4C is a governance and evidence layer for autonomous software engineering:
**you** execute the engineering work with the host's own tools; A4C keeps the
task inspectable, durable, verifiable, recoverable, and safely integrable.
There is no execution strategy to choose: one task is one governed engineering
outcome, run directly.

### Core Architecture Invariants
1. **Host-Native Execution**: use the host's native tools directly in the assigned worktree. If the host uses native subagents or workers, that is fine - A4C neither manages nor restricts them, and they are opaque unless you report them as evidence.
2. **Worktree Isolation**: execute all edits and test commands strictly within the isolated worktree directory. Never dirty the root repository checkout.
3. **Explicit Completion**: the task is not complete until the root activity is explicitly completed. Record failures and interruptions honestly - failed work is never recorded as success.
4. **Human Acceptance**: AI agents NEVER self-approve or integrate changes ('team_accept' and 'team_integrate' are strictly denied). Human approval must be performed via the CLI: 'a4c task accept <task-id>' and 'a4c task integrate <task-id>'.

### Protocol (do these, in order)
1. **Begin the task**: call 'team_begin_task(objective: "...", budget_tokens: <n>)'. It returns the task_id and the isolated worktree. Work ONLY in the returned worktree_path from your first command onward - never explore the base checkout first and then rediscover the same files in the worktree.
2. **Implement** the change with the host's own tools (read, edit, write, shell).
3. **Verify**: run the project's tests ONCE inside the worktree. Do not spawn verification subagents and do not repeat verification passes.
4. **Record the outcome**: call 'team_complete_activity' for the ROOT activity (activity_id == task_id) with success true/false, your summary, and the test evidence. Delegation is prohibited: A4C exposes no way to assign worker activities or schedule reviewers, and no child activities are needed.
5. **Present the result** to the engineer with the human commands: 'a4c task diff <task-id>', 'a4c task accept <task-id>', 'a4c task integrate <task-id>'.
6. Review approval NEVER completes an activity: an AI review can never authorize integration and never overrides failing verification. If you or a native subagent review the code, attach the report as ordinary evidence in the completion record.

### Tool names at runtime
The A4C MCP server is named 'a4c-team'; Copilot exposes its tools prefixed:
'a4c-team_begin_task', 'a4c-team_complete_activity', 'a4c-team_checkpoint',
'a4c-team_apply_patch', 'a4c-team_resume', 'a4c-team_list', 'a4c-team_status',
'a4c-team_result', 'a4c-team_diff', 'a4c-team_feedback', 'a4c-team_context'.
Call the tools by these names; do not go looking for other capabilities.

### Safety
- Pass the objective as text after the subcommand; '--help' is help, not an objective. Flag-like objectives are refused.
- Record useful evidence (commands, outputs, verification results) with the completion. Host usage telemetry (if any) goes in the optional 'usage' parameter as JSON with only the fields you actually know; never invent numbers.

### Resuming Existing Work
Before calling 'team_begin_task', call 'team_list' and look for an unfinished task with the same or a similar objective. Work lives in the task's WORKTREE (.a4c/worktrees/<task-id>/tree), NOT the main checkout - a partially-completed task looks like an empty repository if you only inspect the checkout. If an unfinished task exists, call 'team_resume' (task_id optional) instead of beginning a new one: it returns the worktree path, the work already completed, and the remaining obligations. Never report existing work as a fresh start. 'team_checkpoint' records progress on long tasks.

### Engineering Knowledge (optional, never ceremonial)
- Call 'team_context'; its 'knowledge_dir' is THIS repository's knowledge base at ~/.a4c/<repo-name>/knowledge/. Nothing is stored inside the source repository. Read it ONLY when the task genuinely needs prior findings.
- Record ONLY meaningful discoveries (failed approaches, surprising limitations, non-obvious observations) as NEW Markdown files under knowledge/findings/ or knowledge/experiments/. Never overwrite existing documents; corrections create a new document that references the earlier one. Do not document routine work.
- On 'team_complete_activity', include a knowledge disposition: {"significant": false, "reason": "No significant reusable knowledge"} for routine work, or {"significant": true, "paths": ["findings/<file>.md"]} after writing a document. If your host cannot write outside the workspace, pass {"significant": true, "file_name": "findings/<file>.md", "content": "<full markdown>"} instead. Your records are observations and candidates, not final architectural conclusions. When the user asks to review accumulated knowledge, examine the knowledge base and available evidence, challenge previous conclusions, and write a reviewable assessment under knowledge/reviews/.

### Tool invocation (OpenCode)
A4C MCP tools live on the 'a4c' MCP server; call them through the code-execution tool exactly as: await tools.a4c.team_begin_task({...}), await tools.a4c.team_complete_activity({...}), await tools.a4c.team_list({...}), and likewise team_context, team_status, team_result, team_diff, team_feedback, team_checkpoint, team_resume, team_apply_patch. Call them directly - do not use tool search, do not run exploratory a4c CLI help commands, and never build your own MCP client.
`
	aliceContent = maybeStripKnowledge(rootDir, aliceContent)





	if err := os.WriteFile(filepath.Join(agentsDir, "a4c.md"), []byte(aliceContent), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "a4c-strict.md"), []byte(strictOpenCodeDoc(aliceContent, openCodeStrictLeadPermissions, strictGovernancePreamble)), 0644); err != nil {
		return err
	}

	cmdArgs := []string{"a4c", "mcp"}

	// Merge a4c into the project's opencode.json. Existing keys — other MCP
	// servers, model, permissions, plugins, formatter — are preserved; only
	// our own entries are set. default_agent is a user preference, so it is
	// only claimed when the project has not chosen one.
	cfgPath := filepath.Join(rootDir, "opencode.json")
	opencodeConfig, err := mergeJSONFile(cfgPath)
	if err != nil {
		return err
	}
	if _, ok := opencodeConfig["$schema"]; !ok {
		opencodeConfig["$schema"] = "https://opencode.ai/config.json"
	}
	if _, ok := opencodeConfig["default_agent"]; !ok {
		opencodeConfig["default_agent"] = "a4c"
	}
	ocServers := subMap(opencodeConfig, "mcp")
	ocServers["a4c"] = map[string]any{
		"type":    "local",
		"command": cmdArgs,
		"enabled": true,
	}
	opencodeConfig["mcp"] = ocServers
	if err := writeJSONFile(cfgPath, opencodeConfig); err != nil {
		return err
	}

	return nil
}

// ExportAntigravityCustomization writes Antigravity customization and skill files.
// ExportAntigravityCustomization writes the Antigravity skill file.
//
// The skill is written to `.agents/skills/<name>/SKILL.md`, which is the
// project-workspace path the agy binary documents for skill discovery
// ("{workspace}/.agents/skills/{skill_name}/SKILL.md"). Antigravity reaches
// a4c as a SKILL invoked with `/a4c`, not as a selectable agent — that is why
// it does not appear in agy's agent list.
func ExportAntigravityCustomization(rootDir string) error {
	skillsDir := filepath.Join(rootDir, ".agents", "skills", "a4c")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return fmt.Errorf("create antigravity skill directory: %w", err)
	}

	skillContent := `---
name: a4c
description: "A4C - host-independent agentic engineering governance: durable task identity, isolated worktrees, evidence, human acceptance, safe integration."
---

This skill provides the A4C engineering governance protocol: durable task
identity, isolated worktrees, evidence, human acceptance, and safe integration.
**You** execute the engineering work with Antigravity's own tools; A4C governs
the outcome. There is no execution strategy to choose: one task is one governed
engineering outcome, run directly.

### Protocol (do these, in order)
1. **Begin the task**: call 'team_begin_task(objective: "...", budget_tokens: <n>)' through the A4C MCP server. It returns the task_id and the isolated worktree. Work ONLY in the returned worktree_path from your first command onward - never explore the base checkout first and then rediscover the same files in the worktree.
2. **Implement** the change with the host's own tools (read, edit, write, shell). Host subagents are allowed and are opaque to A4C.
3. **Verify**: run the project's tests ONCE inside the worktree. Do not spawn verification subagents and do not repeat verification passes.
4. **Record the outcome**: call 'team_complete_activity' for the ROOT activity (activity_id == task_id) with success true/false, your summary, and the test evidence. Delegation is prohibited: A4C exposes no way to assign worker activities or schedule reviewers, and no child activities are needed. Record failures honestly - failed work is never recorded as success.
5. **Present the result** with 'a4c task diff <task-id>', 'a4c task accept <task-id>'.
6. Integrate ONLY through 'a4c task integrate <task-id>', run by the human. AI agents NEVER self-approve or integrate ('team_accept' and 'team_integrate' are denied). Review approval NEVER completes an activity: an AI review can never authorize integration and never overrides failing verification.

### MCP registration (Antigravity)
The A4C tools come from the local MCP server. Register it with:
'agy mcp add a4c a4c mcp'
'a4c init' wires only this skill; the MCP server is registered by
'a4c install antigravity' (which runs the command above). Call the A4C tools
exactly as the MCP server exposes them - never build your own MCP client and do
not run exploratory a4c CLI help commands.

### Safety
- Pass the objective as text after the subcommand; '--help' is help, not an objective. Flag-like objectives are refused.
- Preserve useful evidence (commands, outputs, verification results) in the completion record.

### Resuming and recovery
Before beginning, call 'team_list' for an unfinished task with the same or similar objective; work lives in the task's WORKTREE (.a4c/worktrees/<task-id>/tree), NOT the main checkout. If one exists, call 'team_resume' instead of beginning a new one. After an interruption, re-check durable state instead of replaying old work. 'team_checkpoint' records progress on long tasks.

### Engineering Knowledge (optional, never ceremonial)
- 'team_context' returns this repository's knowledge directory (~/.a4c/<repo-name>/knowledge/). Read it only when the task genuinely needs prior findings.
- Record ONLY meaningful discoveries (failed approaches, surprising limitations, non-obvious observations) as NEW Markdown files under knowledge/findings/ or knowledge/experiments/. Never overwrite existing documents. Do not document routine work.
- On 'team_complete_activity', include a knowledge disposition: {"significant": false, "reason": "No significant reusable knowledge"} for routine work, or {"significant": true, "paths": ["findings/<file>.md"]} after writing a document (use {"significant": true, "file_name": "findings/<file>.md", "content": "<full markdown>"} if your host cannot write outside the workspace). When the user asks to review accumulated knowledge, examine the knowledge base and available evidence, challenge previous conclusions, and write a reviewable assessment under knowledge/reviews/.
`
	skillContent = maybeStripKnowledge(rootDir, skillContent)

	return os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skillContent), 0644)
}

// RegisterAntigravityMCP registers the A4C MCP server with the Antigravity CLI
// so host-native sessions can call team_* tools instead of reverse-engineering
// A4C through CLI experiments. It is idempotent ("add or update") and
// best-effort: hosts without the agy CLI simply skip registration.
func RegisterAntigravityMCP() error {
	agyPath, err := exec.LookPath("agy")
	if err != nil {
		return nil // no Antigravity CLI on this host; skill install still applies
	}
	cmd := exec.Command(agyPath, "mcp", "add", "a4c", "a4c", "mcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("register a4c MCP server with antigravity (%s): %w (%s)", agyPath, err, strings.TrimSpace(string(out)))
	}
	return nil
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

	// 2b. Home-directory knowledge base (convention: ~/.a4c/<repo-name>/knowledge)
	if !KnowledgeEnabled(absRoot) {
		addCheck("Knowledge", "Knowledge Base", true, "capture disabled for this workspace (a4c init --knowledge to re-enable)")
	} else if kbDir, kerr := ResolveKnowledgeDir(absRoot); kerr == nil {
		detail := kbDir
		if st, serr := os.Stat(kbDir); serr == nil && st.IsDir() {
			detail += " (exists)"
		} else {
			detail += " (created on first write; no setup required)"
		}
		addCheck("Knowledge", "Knowledge Base", true, detail)
	} else {
		addCheck("Knowledge", "Knowledge Base", false, "cannot resolve knowledge directory: "+kerr.Error())
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
	// agy discovers project skills at .agents/skills/<name>/SKILL.md. Check the
	// discoverable path specifically: a skill written anywhere else is invisible
	// to the host even though the file exists.
	antigravitySkill := filepath.Join(absRoot, ".agents", "skills", "a4c", "SKILL.md")
	staleSkill := filepath.Join(absRoot, ".gemini", "antigravity-cli", "skills", "a4c", "SKILL.md")
	_, skillErr := os.Stat(antigravitySkill)
	_, staleErr := os.Stat(staleSkill)
	switch {
	case skillErr == nil && staleErr == nil:
		addCheck("Antigravity", "A4C Skill", true,
			".agents/skills/a4c/SKILL.md present (invoke with /a4c); a stale copy at .gemini/antigravity-cli/skills is not scanned by agy and can be removed")
	case skillErr == nil:
		addCheck("Antigravity", "A4C Skill", true, ".agents/skills/a4c/SKILL.md present (invoke with /a4c)")
	default:
		addCheck("Antigravity", "A4C Skill", false, ".agents/skills/a4c/SKILL.md not found. Run 'a4c install antigravity'.")
	}

	// 6. Host availability (binary presence only; hosts own their execution)
	hosts := probeHosts()

	return DoctorReport{
		Timestamp: time.Now(),
		RootDir:   absRoot,
		AllPassed: allPassed,
		Checks:    checks,
		Hosts:     hosts,
	}
}

// probeHosts reports which coding hosts are installed. A4C does not drive
// host sessions; availability is informational for the operator.
func probeHosts() []HostCapabilities {
	var out []HostCapabilities
	for _, h := range []struct{ id, name, bin string }{
		{"copilot", "GitHub Copilot CLI", "copilot"},
		{"opencode", "OpenCode", "opencode"},
		{"antigravity", "Antigravity (agy)", "agy"},
	} {
		caps := HostCapabilities{HostID: h.id, DisplayName: h.name}
		if p, err := exec.LookPath(h.bin); err == nil {
			caps.Available = true
			caps.Limitations = append(caps.Limitations, "binary: "+p)
		} else {
			caps.Limitations = append(caps.Limitations, "binary not found on PATH")
		}
		out = append(out, caps)
	}
	return out
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
