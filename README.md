# A4C: Agents for Coding — Your Host-Native Coding Agent

**A4C** is a coding agent you use **inside GitHub Copilot CLI, OpenCode, or Antigravity**. Select the A4C agent or skill in your host, describe the engineering task in chat, and let it implement and verify changes using that host's native tools.

The host owns the conversation, model, and editing tools. A4C adds isolated worktrees, durable task identity, verification evidence, independent review, and checkpoints that survive switching hosts.

The `a4c` executable provides setup, MCP coordination, human approval, and optional headless automation. **You do not need to launch every interactive task with `a4c task start` or run a separate `a4c host` worker.**

---

## 1. Use A4C as Your Coding Agent

### One-Time Setup

Build the A4C executable from this source checkout, using the Go version declared in `go.mod`:

```bash
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/a4c" ./cmd/a4c
export PATH="$HOME/.local/bin:$PATH"
```

Keep that directory on your shell's `PATH`. Then initialize the repository you want the agent to work on:

```bash
cd /path/to/your-project
a4c init .
a4c install all
```

The installer writes host agent definitions and MCP configuration. Review existing `.mcp.json` and `opencode.json` settings before installing into an already configured project. To install only one host, use `a4c install copilot`, `a4c install opencode`, or `a4c install antigravity`.

Restart an existing host session after installation and allow the local A4C MCP server to start when prompted.

### GitHub Copilot CLI

From your project directory:

```bash
copilot --agent a4c
```

Copilot loads `.github/agents/a4c.agent.md`. The installer registers the `a4c mcp` server in the project's `.mcp.json` and the CLI's `~/.copilot/mcp-config.json`. Give your task directly to the A4C agent in the Copilot conversation.

### OpenCode

From the same project directory:

```bash
opencode
```

The installer sets `default_agent` to `a4c` in `opencode.json`, so OpenCode loads `.opencode/agents/a4c.md` as a **primary agent** and starts new sessions as A4C, with the local A4C MCP server configured in the same file. Chat with A4C normally; you do not need to start an OpenCode headless server for this interactive workflow.

To pick the agent manually instead — or to switch back in an existing session — press `Shift+Tab` to cycle agents, press `Ctrl+X` then `A`, or run `/agents` and select `a4c`. For one-shot scripted runs, pass the agent to the `run` subcommand:

```bash
opencode run --agent a4c "your task description"
```

### Antigravity CLI or IDE

Antigravity uses the **`a4c` skill**. The installer writes `SKILL.md` under `.gemini/antigravity-cli/skills/a4c/`, with `.agents/skills/a4c/` as a fallback. If your host discovers project skills through `.agents/skills/`, place the installed skill at `.agents/skills/a4c/SKILL.md`.

The skill also needs the A4C MCP server connected. For the Antigravity CLI, register it from your project directory, then start an interactive session:

```bash
agy mcp add --type stdio a4c -- "$HOME/.local/bin/a4c" mcp --dir "$PWD"
agy
```

For the IDE, open the project and add the equivalent stdio server through its MCP server settings, replacing the absolute paths:

```json
{
  "mcpServers": {
    "a4c": {
      "command": "/absolute/path/to/a4c",
      "args": ["mcp", "--dir", "/absolute/path/to/your-project"]
    }
  }
}
```

In the agent conversation, invoke the installed skill:

```text
/a4c
```

You can also ask, "Use the a4c skill to implement this task." The skill supplies A4C's engineering workflow while Antigravity remains the coding host.

### Give A4C a Task in Chat

After selecting the agent or skill in any of the three hosts, try:

> Review this repository, identify a meaningful engineering improvement, propose a plan, and execute it using A4C's Reviewed strategy. Preserve the work and evidence so I can resume it from another host.

For a small change, ask for **Direct** mode. For independent read-only review, ask for **Reviewed** mode. Request **Team** mode when you want explicit specialist collaboration.

A4C records the task, works in its assigned isolated worktree, runs verification, and preserves evidence and checkpoints. It returns a task ID for inspection and continuation. Human acceptance and integration remain explicit CLI actions; the agent does not approve or merge its own work.

To continue in another host, open the same repository, select its A4C agent or skill, and ask:

> Resume A4C task `<task-id>` from its checkpoint. Inspect the saved changes and verification evidence before continuing.

The `.a4c/` directory contains local task state and evidence; it is not committed with the code. A different machine needs that state as well as the task branch, with saved workspace paths relocated if necessary.

---

## 2. Engineering Knowledge

A4C keeps a per-repository engineering knowledge base (KB) of plain Markdown files under your home directory:

```text
~/.a4c/<repo-name>/knowledge/
├── INDEX.md          # curated map (optional, hand-maintained)
├── findings/         # confirmed discoveries
├── experiments/      # trials, including negative results
├── decisions/        # architectural decisions and rationale
├── reviews/          # accumulated-knowledge assessments
├── iterations/       # iteration transitions within the repo
└── evidence/         # small copied artifacts (test logs, measurements)
```

No setup is required — the path is derived from the repository name (resolved correctly from linked worktrees). `a4c doctor` reports the resolved location. The KB lives entirely outside the source repository, so worktree cleanup, task rollback, and integration never touch it.

### When agents record knowledge

During a task, the agent consults the KB when prior learning helps, and records only meaningful discoveries — failed approaches, limitations, consequential findings — as new documents. Routine work records no documents. At `team_complete_activity`, the agent reports a knowledge disposition that A4C verifies against the filesystem:

- `{"significant": false, "reason": "No significant reusable knowledge"}` — normal for small changes; nothing is written.
- `{"significant": true, "paths": ["findings/…​.md"]}` — A4C confirms the files exist inside the KB.
- `{"significant": true, "file_name": "…", "content": "…"}` — inline fallback when the host cannot write outside its workspace; A4C writes the document atomically and refuses to overwrite existing files.

Published documents are never overwritten by A4C-managed writes; corrections create new documents that reference the earlier ones. The task-level assessment is recorded after review (the lead's synthesis may amend only the knowledge field of an already-completed root activity). Missing or pending assessments surface as warnings at `a4c task inspect` and `a4c task accept` — they never block acceptance of the engineering work.

### Reviewing and reusing knowledge

Inside any host, ask the A4C agent:

> Review what we've learned about this implementation. What has been established, what failed, what remains uncertain, and which architectural assumptions deserve reconsideration?

The agent writes a reviewable assessment under `knowledge/reviews/`. To weigh continuing vs. restarting:

> Evaluate whether we should continue, repair, or start a new implementation based on what we've learned.

To carry selected learning into a new repository's independent KB (the new repo gets its own `~/.a4c/<new-repo>/knowledge/`):

> Review what we learned in `<old-repo>` and propose what should be carried into this new implementation.

The agent proposes a transfer; after your approval it copies selected documents with origin notes. Nothing is shared or linked afterwards. OneDrive or another custom root can be considered later; today everything is local.

Users of earlier A4C releases with an existing `.a4c/knowledge.db` can migrate with `a4c knowledge export` (the legacy database is read-only and left untouched).

---

## 3. Fundamental Architecture

A4C establishes strict ownership boundaries between the coding host and the engineering organization:

### The Coding Host Owns:
- Model inference, context window management, and token generation.
- Model-specific configuration, credentials, and quotas.
- Native tool execution (`bash`, file editing, reading, patching).
- Native agent turns, streaming output, and interactive developer UX.

### A4C Owns:
- **Organizational Identity & Roles**: Specialists (`coder`, `reviewer`, `lead`, domain specialists) defined in a durable team roster.
- **Task Lifecycle & Intent**: Persistent task graphs, activities, and budget governance stored in `.a4c/`.
- **Workspace Isolation**: Clean Git worktree provisioning per task (`.a4c/worktrees/<task-id>/`), eliminating state pollution on the developer's main branch.
- **Validation Evidence**: Cryptographic workspace snapshots (`WorkspaceSnapshotter`) and deterministic test execution results (`go test ./...`), binding task completion to verified evidence.
- **Independent Verification**: Non-mutating review guarantees enforced natively or via snapshot mutation verification and automatic rollback.
- **Cross-Host Continuation**: Authoritative checkpoints enabling work started on one host to resume on another host without transcript replay.
- **Human Acceptance & Integration**: Explicit engineer sign-off (`a4c task accept`) and clean merge integration (`a4c task integrate`).

Worktree removal fails closed: Git inspection or removal errors are reported without
discarding recovery records or recursively deleting task directories. Forced removal
permits dirty worktrees, but does not bypass Git errors or locks. Evidence and other
task artifacts stored alongside the worktree are preserved after successful removal.
If Git removes the worktree but saving its released record fails, A4C reports that
partial completion and retains the in-memory record for recovery.

---

## 4. Execution Strategies

A4C supports three distinct execution strategies tailored to task complexity:

| Strategy | Flag | Execution Flow | Overhead | Ideal Use Case |
| :--- | :--- | :--- | :--- | :--- |
| **Direct Mode** | `--strategy direct` (Default) | Single specialist turn with worktree isolation and test evidence collection. Zero coordinators, zero automatic reviewers. | < 0.2s | Bugfixes, focused features, rapid iterations requiring native host speed. |
| **Reviewed Mode** | `--strategy reviewed` | Specialist implementer turn, followed by an independent read-only reviewer turn gating approval on passing test evidence. | Specialist + Reviewer | Security-sensitive changes, public API modifications, production code. |
| **Team Mode** | `--strategy team` | Lead engineer breaks down objective into delegated subtasks for specialists, followed by quality review. | Multi-turn | Complex multi-step architectural features requiring decomposition. |

---

## 5. Optional Headless Host Adapters

A4C also supports CLI-driven automation across the same hosts. The matrix below describes those **headless adapters**, not the interactive agent entry points above:

| Capability | GitHub Copilot CLI (`copilot`) | OpenCode (`opencode`) | Antigravity CLI (`antigravity` / `agy`) |
| :--- | :--- | :--- | :--- |
| **Execution Mode** | CLI automation SDK / stdio | API server (`opencode serve`) | Print non-interactive (`agy -p`) |
| **Worktree Binding** | ✅ Native worktree binding | ✅ Session `location` scoping | ✅ Working directory flag |
| **Context Injection** | ✅ Charter & Handoff prompt | ✅ Charter folded into session prompt | ✅ Handoff prompt injection |
| **Telemetry & Tokens**| ✅ Prompt, completion, total | ✅ Input, output, reasoning | ✅ Input, output, thinking, cache |
| **Review Isolation** | ✅ SDK tool definition filters | ✅ Session permission ruleset (`deny`) | ✅ Snapshot mutation check & rollback |
| **Model Selection** | ✅ Custom model per activity | ✅ Custom provider/model per activity | ✅ Custom model per activity |
| **Continuation** | ✅ Authoritative checkpoints | ✅ Authoritative checkpoints | ✅ Authoritative checkpoints |

The OpenCode adapter speaks the v2 HTTP API and manages its HTTP Basic authentication automatically: managed servers pin a random password through `OPENCODE_SERVER_PASSWORD`, and external servers are attached with the same environment variable.

---

## 6. CLI Automation & Administration

The commands below are optional automation and administrative interfaces, not a prerequisite for chatting with the A4C agent. `a4c doctor` reports environment readiness, including optional host adapters that may be unavailable when you are not using them.

### Direct Mode (Headless Automation)
Execute a focused task through a host's automation adapter:
```bash
# Direct task on OpenCode
a4c task start "Fix nil pointer dereference in cache.go" --runtime opencode --strategy direct

# Direct task on GitHub Copilot
a4c task start "Refactor options parser" --runtime copilot --strategy direct

# Direct task on Antigravity
a4c task start "Add table-driven unit tests" --runtime antigravity --strategy direct
```

### Reviewed Mode (Independent Quality Gate)
Execute implementation with independent, non-mutating review:
```bash
a4c task start "Add distributed rate limiter" \
  --strategy reviewed \
  --runtime opencode \
  --reviewer-runtime copilot
```

### Cross-Host Teams (Team Mode)
Run a mixed-host engineering team where different specialists execute on different hosts:
```bash
a4c task start "Implement robust health check endpoints" \
  --strategy team \
  --runtime copilot \
  --agent-runtimes coder=opencode,reviewer=copilot
```

### Cross-Host Continuation
Pause an in-flight task on one host and continue on another:
```bash
# Capture authoritative checkpoint
a4c task checkpoint task-1791671400473010000

# Resume task on another host without context loss
a4c task resume task-1791671400473010000 --runtime opencode --run
```

### Human Review & Integration
A4C maintains a strict human acceptance boundary:
```bash
# Inspect task audit trail and activity DAG
a4c task inspect task-1791671400473010000

# View review diff in isolated worktree against base branch
a4c task diff task-1791671400473010000

# Provide human review feedback (re-opens task for revision)
a4c task feedback task-1791671400473010000 "Please add benchmarks for concurrent access"

# Record explicit human approval of reviewed snapshot
a4c task accept task-1791671400473010000 "Approved for production"

# Cleanly integrate approved worktree branch into base repository
a4c task integrate task-1791671400473010000
```

---

## 7. Verification & Regression Protection

Run the hardened test suite:
```bash
# Run production hardening tests (capabilities, evidence resolution, mutation rollback)
go test -v -run TestProductionHardening ./pkg/a4c

# Run universal host and mixed routing tests
go test -v -run "TestHostContract|TestMixedHostRouting|TestCrossHostContinuation" ./pkg/a4c
```
