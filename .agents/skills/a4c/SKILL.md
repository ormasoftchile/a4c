---
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
