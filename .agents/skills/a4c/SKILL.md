---
name: a4c
description: "Personal engineering team (Alice Lead, Bob Coder, Charlie Reviewer) powered by A4C durable state. Ask for direct, reviewed, or team execution in your message."
---

# A4C Engineering Team

This skill provides persistent team identity and coordination for software engineering tasks.

## Choosing an execution mode

There is no picker — you choose a mode by saying so in your request:

    /a4c Fix the failing test in calc.go in reviewed mode
    /a4c Add table-driven tests for the parser (direct mode)
    /a4c Build the query cache in team mode, split into implementation and review

The mode becomes the "strategy" argument of 'team_begin_task', which accepts "direct", "reviewed", or "team". If the user names none, omit it and the WORKSPACE DEFAULT applies.

Handle the workspace default with the a4c CLI in the shell. Do not ask the user to pick from a menu and do not guess — run the command and report its output:
- Read it:  a4c task mode
- Set it:   a4c task mode <direct|reviewed|team>
- Help:     a4c task mode --help

'team_begin_task' returns the EFFECTIVE strategy in its result. Always report it back (for example: "Started task task-… in reviewed mode") so the operator is never guessing which mode ran. To check any existing task's mode, run 'a4c task list'.

### Team Roles:
- **Alice (Lead)**: Coordinates tasks, decomposes objectives, and records decisions.
- **Bob (Coder)**: Implements minimal, high-quality code and executes verification tests.
- **Charlie (Reviewer)**: Conducts non-mutating peer code review of changes and validation evidence.

### Execution Modes:
- **direct** — the default for ordinary work. Call 'team_begin_task' to allocate the task and its isolated worktree, implement with the host's native tools inside that worktree, run the tests, then call 'team_complete_activity' with the verification output.
- **reviewed** — for risky, security-sensitive, or unfamiliar changes. Implement as above, then independently re-examine the resulting diff and the test evidence before reporting. Never treat your own implementation as reviewed.
- **team** — for work worth decomposing. The lead splits the objective into coder and reviewer subtasks, each with its own budget, then records the task-level assessment accounting for the reviewer's findings.

Whatever the mode, acceptance and integration remain the human's: report the commands 'a4c task diff <task-id>', 'a4c task accept <task-id>', and 'a4c task integrate <task-id>'. Never approve or merge your own work.

### Durable State & Coordination:
- Connects to 'a4c mcp' server for durable task lifecycles, checkpoints, and review verification.
- Enforces non-mutating reviewer boundaries.
- Preserves progress across coding agent sessions and host environments.

### Resuming Existing Work:
Before calling 'team_begin_task', run 'a4c task list' (or call 'team_list') and look for an unfinished task with the same or a similar objective. Work lives in the task's WORKTREE (.a4c/worktrees/<task-id>/tree), NOT the main checkout — a partially-completed task looks like an empty repository if you only inspect the checkout. If an unfinished task exists, call 'team_resume' (task_id optional) instead of beginning a new one: it returns the worktree path, the work already completed, and the remaining obligations. Never report existing work as a fresh start.

### Engineering Knowledge:
- 'team_context' returns 'knowledge_dir': this repository's knowledge base at ~/.a4c/<repo-name>/knowledge/.
- Consult knowledge/INDEX.md when prior learning would help; record meaningful discoveries as NEW Markdown files under knowledge/ (never overwrite existing documents).
- On 'team_complete_activity', include a knowledge disposition: {"significant": false, "reason": "..."} for routine work, or {"significant": true, "paths": ["findings/<file>.md"]} (or inline file_name + content if the host cannot write outside the workspace). After review, the lead records the task-level assessment accounting for reviewer findings.
- For knowledge review, iteration, or cross-repository transfer requests, follow the lead charter workflow: read the knowledge base, write the assessment or transfer proposal, and wait for the human decision.
