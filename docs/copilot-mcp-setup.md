# GitHub Copilot & MCP Configuration Guide for A4C

This guide explains how to connect GitHub Copilot (and other MCP-compatible clients like Claude Desktop or Cursor) to your local A4C persistent engineering team.

---

## 1. Overview

A4C runs as a standard Model Context Protocol (MCP) server over standard input/output (`stdio`):

```bash
a4c mcp [--dir /path/to/repo]
```

When connected, GitHub Copilot gains access to 8 high-level engineering team tools:

1. **`team_delegate`**: Assign an objective to Alice (Lead), Bob (Coder), and Charlie (Reviewer).
2. **`team_list`**: List active and historical team assignments with their live states.
3. **`team_status`**: Inspect a task's full execution trail, current active agent, budget, and metrics.
4. **`team_result`**: Inspect the finished output, reviewer verdict, comments, and evidence.
5. **`team_diff`**: View isolated Git worktree modifications before approving changes.
6. **`team_feedback`**: Provide review feedback and request revisions from the coder.
7. **`team_accept`**: Record explicit human approval of an exact snapshot (*never modifies your working checkout*).
8. **`team_integrate`**: Safely merge approved changes into your base checkout (*fails closed if tree is dirty or snapshot diverged*).

---

## 2. GitHub Copilot Configuration (VS Code)

To enable A4C in GitHub Copilot inside VS Code, add the server to your VS Code MCP settings:

### Configuration File
- macOS: `~/Library/Application Support/Code/User/globalStorage/github.copilot/mcp.json` or in your workspace `.vscode/settings.json`.

```json
{
  "mcpServers": {
    "a4c": {
      "command": "/usr/local/bin/a4c",
      "args": ["mcp", "--dir", "${workspaceFolder}"],
      "env": {}
    }
  }
}
```

*Note: Replace `/usr/local/bin/a4c` with the absolute path to your compiled `a4c` binary.*

---

## 3. Claude Desktop Configuration

For Claude Desktop, edit `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "a4c": {
      "command": "/path/to/a4c/a4c",
      "args": ["mcp", "--dir", "/Users/cristianormazabal/Projects/your-project"]
    }
  }
}
```

---

## 4. Running the Background Worker Host

A4C enforces a single-writer workspace lock. While Copilot or the CLI submit and inspect tasks via MCP/CLI, a durable worker host executes the activities in the background:

```bash
# Option A: Run continuous daemon in a terminal or systemd service
a4c host --poll-ms 250

# Option B: Run a bounded number of execution steps for testing
a4c host --steps 10
```

If you submit a task without a running worker host, the task remains safely `queued` in durable storage until a worker starts.

---

## 5. Conversational Team Workflows

With MCP configured, you can operate your engineering team conversationally:

### Step 1: Assign a Feature or Bugfix
> **User**: "Alice, investigate our JSON parser and add support for streaming chunk decoding. Coordinate with Bob and Charlie."
>
> **Copilot**: *Invokes `team_delegate(objective="Add streaming chunk decoding to JSON parser", strategy="team")`*
>
> **Copilot**: "Task `task-179155...` has been enqueued under the full team strategy. Alice is leading, Bob will implement in an isolated worktree branch, and Charlie will review."

### Step 2: Check Status
> **User**: "What's the status of the JSON parser task?"
>
> **Copilot**: *Invokes `team_status(task_id="task-179155...")`*
>
> **Copilot**: "Bob has completed the implementation. Charlie has independently verified the changes and approved them. The task is currently `awaiting_human_review`."

### Step 3: Inspect Diff
> **User**: "Show me the diff."
>
> **Copilot**: *Invokes `team_diff(task_id="task-179155...")`*
>
> **Copilot**: "Modified 2 files (+45 lines / -10 lines). Here are the changes..."

### Step 4: Request Revisions (Optional)
> **User**: "Tell Bob to add a benchmark test for 1MB streams."
>
> **Copilot**: *Invokes `team_feedback(task_id="task-179155...", feedback="Please add a benchmark test for 1MB streams")`*
>
> **Copilot**: "Feedback submitted. Task reopened under `revision_requested`. Bob has been assigned the revision, and Charlie will re-review once completed."

### Step 5: Approve & Integrate
> **User**: "Looks great. Accept the task and integrate it."
>
> **Copilot**: *Invokes `team_accept(task_id="task-179155...", comments="Approved by Christian")`*
>
> **Copilot**: "Recorded human approval for snapshot `a1b2c3d`. Working tree remains untouched."
>
> **Copilot**: *Invokes `team_integrate(task_id="task-179155...")`*
>
> **Copilot**: "Snapshot verified against approved commit. Base checkout verified clean. Changes integrated via commit `e4f5g6h`!"

---

## 6. CLI Equivalence

Every operation available via MCP can be executed identically via CLI:

| MCP Tool | CLI Command Equivalent |
|---|---|
| `team_delegate` | `a4c task submit <objective> [budget] [--strategy ...]` |
| `team_list` | `a4c task list [--state ...] [--runtime ...]` |
| `team_status` | `a4c task inspect <task-id> [--json]` |
| `team_result` | `a4c task history <task-id>` |
| `team_diff` | `a4c task diff <task-id>` |
| `team_feedback` | `a4c task feedback <task-id> <feedback>` |
| `team_accept` | `a4c task accept <task-id> [comments]` |
| `team_integrate` | `a4c task integrate <task-id>` |
