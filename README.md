# A4C: Agents for Coding - Your Host-Native Coding Agent

**A4C** is a coding agent you use **inside GitHub Copilot CLI, OpenCode, or Antigravity**. Select the A4C agent or skill in your host and give it work in chat. It does the engineering in an isolated worktree, verifies the result, and hands you the evidence.

The `a4c` executable provides setup, MCP governance, human approval, and safe integration. **You do not need to run it by hand** - the agent uses it through MCP while you approve the result.

A4C is a **governance and evidence layer**, not an agent framework: the native host owns coding execution (including any native subagents); A4C owns task identity, workspace isolation, durable evidence, human acceptance, and safe integration.

## 1. Use A4C as Your Coding Agent

### One-Time Setup

Build (or install) the `a4c` executable:

```bash
go build -o a4c ./cmd/a4c
```

Keep that directory on your shell's `PATH`. Then initialize the repository you want the agent to work on:

```bash
cd /path/to/your/repo
a4c init .        # or: a4c install all
```

`init` does both halves of setup in one step: it creates the `.a4c` state directory **and** wires the repo into Copilot, OpenCode, and Antigravity.

### GitHub Copilot CLI

```bash
copilot --agent a4c
```

Copilot loads `.github/agents/a4c.agent.md`. The installer registers the `a4c mcp` server in the project's `.mcp.json` and in `~/.copilot/mcp-config.json`. The charter names the host's real shell tools (`bash`, `powershell`) so verification can run.

### OpenCode

```bash
opencode
```

The installer sets `default_agent` to `a4c` in `opencode.json`, so OpenCode loads `.opencode/agents/a4c.md` as a **primary** agent. To switch manually, press `Shift+Tab`, or run:

```bash
opencode run --agent a4c "your task description"
```

### Antigravity CLI or IDE

Antigravity reaches a4c as a **skill**, not as a selectable agent. The installer writes `.agents/skills/a4c/SKILL.md` and registers the MCP server:

```bash
a4c install antigravity    # writes the skill and runs: agy mcp add a4c a4c mcp
```

In the agent conversation, invoke the skill with `/a4c`.

### Give A4C a Task in Chat

> Fix the flaky parser test in this repository and make the suite pass.

A4C begins the task, works in its assigned isolated worktree, verifies the result, records the outcome, and preserves evidence. It reports the human commands for acceptance. There is no execution mode to choose: one task is one governed engineering outcome, executed directly.

To continue in another host, open the same repository, select its A4C agent or skill, and ask:

> Resume the unfinished task and continue it.

You do not need to supply a task ID - it is resolved to the task that most needs attention and named back to you.

### What A4C does and does not do

- **The host executes.** Implementation, investigation, testing, and any native subagents are the host's own business. A4C does not orchestrate them and does not restrict them.
- **A4C governs.** Task identity, the isolated worktree, durable records, verification evidence, human acceptance, snapshot validation, and safe integration.
- **A human accepts.** AI agents never self-approve or integrate; `a4c task accept` and `a4c task integrate` are human operations.

## 2. Engineering Knowledge

A4C keeps an optional, durable engineering memory **outside** the source repository:

```
~/.a4c/<repo-name>/knowledge/
├── findings/        what was learned and why it matters
├── experiments/     what was tried and what happened
├── decisions/       consequential choices and their rationale
└── reviews/         reviewable assessments of the accumulated knowledge
```

During a task, the agent consults the knowledge base when prior learning helps, and records only meaningful discoveries - failed approaches, surprising limitations, non-obvious observations. Knowledge work is optional and never ceremonial.

Inside any host, ask the A4C agent:

> Review what we know and challenge the previous conclusions.

## 3. Fundamental Architecture

### The Coding Host Owns

- Coding agent execution, native subagents, and internal delegation.
- Tool selection, implementation planning, investigation, implementation, and verification.

### A4C Owns

- **Task identity and lifecycle** - one task is one governed engineering outcome.
- **Workspace isolation** - an isolated worktree per task; the checkout is never dirtied.
- **Durable records and evidence** - execution history, verification results, diffs, failure explanations.
- **Human acceptance and integration authorization** - deterministic gates, snapshot validation, safe integration into the repository.
- **Auditability and recovery** - checkpoints, continuation prompts, resumable tasks.

A4C does not reproduce the host's agent hierarchy. If a host uses native subagents, they are opaque to A4C unless their work is reported as evidence.

### Task Lifecycle

```
Task created -> Worktree allocated -> Host executes -> Result recorded
     (failed / interrupted is recorded honestly and stops here)
        -> Verification evidence -> Human acceptance
        -> Snapshot validation -> Safe integration
```

## 4. Strict Governance (opt-in)

By default, agents mutate files with the host's **native** `edit`/`write`/`patch` tools. For unattended or regulated runs, the installer also ships **strict** agent variants that deny native mutation and route every write through the governed, journaled `team_apply_patch` tool:

```bash
a4c install opencode   # writes a4c.md and a4c-strict.md
a4c install copilot    # writes a4c.agent.md and a4c-strict.agent.md
```

Select `a4c-strict` the same way you select `a4c` - `/agents` in OpenCode, `copilot --agent a4c-strict` in Copilot.

## 5. CLI Automation & Administration

The commands below are optional automation and administrative interfaces, not a prerequisite for chatting with the A4C agent.

### Command Surface

| Command | Purpose |
|---|---|
| `a4c init <dir>` | Initialize workspace + wire all hosts |
| `a4c install <copilot\|opencode\|antigravity\|all>` | Install host agents/skills and MCP registration |
| `a4c task <...>` | Task lifecycle (below) |
| `a4c mcp` | Run the MCP server a host connects to |
| `a4c status [doctor\|memory\|knowledge]` | Introspection |

### Task Lifecycle Commands

| Command | Purpose |
|---|---|
| `a4c task submit <objective> [budget_tokens]` | Create a governed task (record only) |
| `a4c task start <objective> [budget_tokens]` | Create a task and its isolated worktree |
| `a4c task adopt <objective> [--notes ...] [--in-place]` | Register pre-existing external work as a governed task |
| `a4c task resume [task-id] [--notes ...]` | Prepare continuation of an interrupted task |
| `a4c task checkpoint [task-id]` | Capture an authoritative checkpoint |
| `a4c task list / history / inspect / diff` | Read task state, audit trail, and changes |
| `a4c task feedback <text>` | Send work back for revision |
| `a4c task accept [task-id] [comments] [--integrate]` | **Human** approval against a validated snapshot |
| `a4c task integrate [task-id]` | **Human** integration of accepted work |
| `a4c task reject <reason>` | Reject the task |

Task IDs are optional: the task that most needs attention is resolved for you.

### Version

`a4c --version` (also `-version` or `version`) prints the release as a single parseable line:

```console
$ a4c --version
a4c 0.1.1
```

Approving a change carries a release rule: `a4c task accept` records a patch-level bump of the workspace version (`pkg/a4c/VERSION`) with the approval, and `a4c task integrate` lands it as its own `chore(release): bump version to X.Y.Z` commit alongside the approved change. A rejected or abandoned task never bumps the version, and a workspace without a version file is left alone.

### Human Review & Integration

```bash
a4c task diff                 # what you are approving
a4c task accept               # approve against a validated snapshot
a4c task accept --integrate   # approve and merge in one step
a4c task feedback "..."       # send it back for revision
a4c task inspect --json       # the full audit trail
a4c task integrate            # merge an already-approved task later
```

Integration requires human acceptance plus independently validated evidence; incomplete work, dirty base checkouts, and stale snapshots are refused.

## 6. Verification & Regression Protection

```bash
go build ./...
go test -short ./...
go test -race -short ./pkg/a4c/...
```

All testing is deterministic and offline.

## 7. Retired Capabilities

Earlier releases offered execution strategies (`reviewed`, `team`) with A4C-managed reviewers, delegated worker activities, and a team coordinator. Three stages of controlled benchmarking found no demonstrated correctness or efficiency advantage for that orchestration, and it has been **retired** as a deliberate architectural reduction:

- New tasks cannot select a strategy; requests naming `reviewed` or `team` fail explicitly.
- No delegation, reviewer scheduling, or coordinator execution exists in the product.
- Historical Team/Reviewed task records remain readable and immutable, and cannot bypass lifecycle validation.

Agent orchestration belongs to the native execution host; A4C governs engineering outcomes.
