# A4C — Milestone 15: Real Host-Native Team Execution & Cross-Host Continuity Report

**Date**: October 9, 2026  
**Status**: Completed & Verified  
**Target Environments**: GitHub Copilot CLI, OpenCode CLI, YAWR Governance Framework  
**Repository Tested**: `example.com/cache` (`experiments/disposable-cache-repo`)  
**Core Invariant**: Zero Hidden Inference — 100% of reasoning, subagent invocation, and tool execution performed natively by host engines.

---

## 1. Executive Summary

Milestone 15 successfully proves the fundamental execution model of **A4C as a Host-Native Engineering Team**. Previous milestones validated agent definitions, MCP schemas, and Go-level application services. This milestone establishes genuine, live end-to-end execution of a persistent engineering team across two completely distinct host environments: **GitHub Copilot CLI** and **OpenCode CLI**.

### Key Achievements

1. **Dual-Host Native Orchestration**:
   - **Phase A (Copilot)**: Alice launched natively under GitHub Copilot CLI (`copilot --agent a4c`). Alice reasoned over the task, created an isolated Git worktree, and dispatched native Copilot subagent **Bob** running `claude-opus-4.8`. Bob modified source files, verified test suites under Go's race detector, and committed snapshot `fea18cc6796cecc0ed3dc1e941b73bb97002f125` to the worktree branch.
   - **Phase B (OpenCode)**: Alice launched natively under OpenCode CLI (`opencode run --agent a4c`). Alice inspected A4C durable state via MCP, recovered Bob's completed checkpoint, and did **not** re-execute the implementation. Alice dispatched native OpenCode subagent **Charlie** running `mimo-v2.5`. Charlie independently inspected diffs, re-verified race tests, and approved the snapshot read-only without modifying a single file.
2. **Authentic Bug Resolution**:
   - The test was conducted on a real concurrency bug in `example.com/cache`: concurrent map writes (`fatal error: concurrent map writes`) and unsynchronized metrics.
   - The fix properly introduced `sync.RWMutex` protecting entry storage and atomic primitives (`sync/atomic`) for metrics.
   - Verification passed with `GOWORK=off go test -race -v ./...` in 1.38s with zero race detector warnings.
3. **Worktree & Checkout Isolation**:
   - Throughout both Copilot and OpenCode execution phases, the root repository checkout remained completely clean and untouched at commit `4a75ca517514d47330c26b75de0adfdd2b0d69ba`.
   - All code mutations were physically isolated in `.a4c/worktrees/task-1791565903972427000/tree`.
4. **Enforced Human Governance Boundary**:
   - AI models were strictly prohibited from self-approving or self-integrating.
   - A4C's MCP server enforced the human boundary: unauthenticated `team_accept` calls failed with `unauthorized: AI agents cannot approve their own work`.
   - A unique one-time human confirmation token (`human-auth-9e105a56`) was generated.
   - Human acceptance was recorded via CLI: `a4c task accept task-1791565903972427000 --token human-auth-9e105a56`.
   - Integration into the main branch was executed as a separate atomic step via `a4c task integrate task-1791565903972427000`, producing merge commit `20d6fed0bb9cef4c9b504da6b268b776e8e2d265`.
5. **Zero Hidden Inference**:
   - A4C ran strictly as an MCP service and CLI coordination layer.
   - Zero background LLM calls or internal SDK model loops were invoked by A4C. All AI model credits were consumed through the respective host accounts (Copilot subscription tokens and OpenCode local/daemon quota).

---

## 2. Experimental Setup

### 2.1 Disposable Repository Architecture

A clean, disposable repository was initialized at `/Users/cristianormazabal/Projects/yawr/experiments/disposable-cache-repo`:
- **Module**: `example.com/cache`
- **Initial Commit**: `4a75ca517514d47330c26b75de0adfdd2b0d69ba`
- **Defect Injected**:
  - `cache.go`: Unsynchronized map reads/writes across concurrent goroutines (`Cache.Set`, `Cache.Get`, `Cache.Delete`, `Cache.Len`).
  - `options.go`: Unsynchronized counters (`Stats.Hits`, `Stats.Misses`).
  - `cache_test.go`: `TestCache_ConcurrentAccess` spawns 16 concurrent reader/writer goroutines.
- **Baseline Race Test**:
  ```bash
  $ GOWORK=off go test -race -v ./...
  ==================
  WARNING: DATA RACE
  Write at 0x00c000108390 by goroutine 12:
    runtime.mapassign_faststr()
    example.com/cache.(*Cache).Set() cache.go:56
  Previous read at 0x00c000108390 by goroutine 11:
    runtime.mapaccess2_faststr()
    example.com/cache.(*Cache).Get() cache.go:37
  ==================
  fatal error: concurrent map writes
  FAIL    example.com/cache   0.412s
  ```

### 2.2 Host Environment Configurations

| Attribute | GitHub Copilot CLI | OpenCode CLI |
| :--- | :--- | :--- |
| **CLI Version** | `copilot` 0.0.428 (darwin-arm64) | `opencode` 1.1.28 (darwin-arm64) |
| **Configuration Path** | `~/.copilot/mcp-config.json` | `./opencode.json` |
| **MCP Declaration** | `"mcpServers": { "a4c": { "command": "a4c", "args": ["mcp"] } }` | `"mcp": { "a4c": { "type": "local", "command": ["a4c", "mcp"] } }` |
| **Agent Roster Path** | `.github/agents/{a4c,bob,charlie}.agent.md` | `.opencode/agents/{a4c,bob,charlie}.md` |
| **Primary Agent Model** | Claude 3.5 Sonnet / Claude Opus 4.8 | MIMO v2.5 |
| **Subagent Delegation Syntax** | Built-in `task(agent_type: "bob")` | Built-in `task(agent: "charlie")` |
| **Permission Flags Used** | `--allow-tool='shell'`<br/>`--allow-tool='write'`<br/>`--allow-tool='task'`<br/>`--allow-tool='a4c'`<br/>`--allow-all-paths` | `--auto` (subagent permissions constrained via frontmatter `permission: { edit: deny, write: deny }`) |

---

## 3. Phase A: Copilot-Native Team Execution

### 3.1 Invocation & Session Startup

Copilot CLI was launched targeting Alice:
```bash
copilot --agent a4c \
  --allow-tool='shell' \
  --allow-tool='write' \
  --allow-tool='task' \
  --allow-tool='a4c' \
  --allow-all-paths \
  -p "Alice, please fix the data races in this cache repository. Have Bob implement the fix in an isolated worktree and verify it passes go test -race. Report the result when ready for review."
```

### 3.2 Live Trace & Event Sequence

Full execution transcript was recorded in `.a4c/traces/copilot_phase_a.jsonl`:

1. **Context Discovery & Initialization**:
   - Alice received user instructions and invoked `a4c_team_context` to understand workspace state.
   - Alice called `a4c_team_init` to establish task `task-1791565903972427000`.
   - A4C allocated isolated worktree `.a4c/worktrees/task-1791565903972427000/tree` on branch `a4c/task-1791565903972427000`.
2. **Subagent Delegation to Bob**:
   - Alice formulated instructions and dispatched Copilot's native subagent tool:
     ```json
     {
       "tool": "task",
       "parameters": {
         "agent_type": "bob",
         "instruction": "Fix cache data races and missing synchronization in cache.go and options.go inside worktree .a4c/worktrees/task-1791565903972427000/tree. Verify with GOWORK=off go test -race -v ./... and commit."
       }
     }
     ```
   - Copilot emitted native subagent lifecycle event:
     - `eventType`: `subagent.started`
     - `agentName`: `bob`
     - `agentId`: `c70d0296-9945-4b4c-af43-a53f02545f4f`
     - `model`: `claude-opus-4.8`
3. **Bob's Implementation in Worktree**:
   - Bob inspected `cache.go` and `options.go` using `view` tools.
   - Bob used Copilot's `write` tool to update `cache.go`:
     - Added `sync.RWMutex` to `struct Cache`.
     - Added `c.mu.Lock()` / `defer c.mu.Unlock()` to `Get()`, `Set()`, and `Delete()`.
     - Added `c.mu.RLock()` / `defer c.mu.RUnlock()` to `Len()`.
     - Replaced non-atomic counter increments with `atomic.AddInt64(&c.stats.Hits, 1)` and `atomic.AddInt64(&c.stats.Misses, 1)`.
   - Bob updated `options.go`:
     - Converted `GetHits()` and `GetMisses()` to `atomic.LoadInt64`.
   - Bob executed shell validation inside the worktree:
     ```bash
     cd .a4c/worktrees/task-1791565903972427000/tree && GOWORK=off go test -race -v ./...
     ```
     Result: All 3 tests passed (`PASS: TestCache_ConcurrentAccess` in 0.01s).
   - Bob committed the fix directly to the worktree branch:
     ```bash
     git -C .a4c/worktrees/task-1791565903972427000/tree commit -am "Fix cache data races with RWMutex and atomic stats"
     ```
     Commit SHA: `fea18cc6796cecc0ed3dc1e941b73bb97002f125`.
4. **Checkpoint Recording**:
   - Bob reported completion back to Alice.
   - Alice invoked `a4c_team_checkpoint` via MCP with `snapshot_sha: fea18cc6796cecc0ed3dc1e941b73bb97002f125`.
   - Copilot session concluded with Alice reporting the completed implementation.

### 3.3 Root Checkout Verification

Post-Phase A inspection confirmed physical isolation:
```bash
$ git status --porcelain
(clean - 0 files modified)

$ git rev-parse HEAD
4a75ca517514d47330c26b75de0adfdd2b0d69ba

$ git rev-parse a4c/task-1791565903972427000
fea18cc6796cecc0ed3dc1e941b73bb97002f125
```
The primary working tree was completely unaffected by Bob's mutations.

---

## 4. Phase B: OpenCode-Native Continuation & Independent Review

### 4.1 Invocation & Session Startup

OpenCode CLI was launched to continue the task in the same repository:
```bash
opencode run \
  --dir /Users/cristianormazabal/Projects/yawr/experiments/disposable-cache-repo \
  --agent a4c \
  --auto \
  "Alice, inspect the ongoing task created previously. Do NOT redo Bob's coding work. Have Charlie perform an independent read-only review of Bob's snapshot and record the review result."
```

### 4.2 Live Trace & Event Sequence

Full execution transcript was captured in `.a4c/traces/opencode_phase_b.txt`:

1. **State Discovery & Recovery**:
   - OpenCode Alice called `a4c_team_context` and `a4c_team_status`.
   - Result: Alice recovered `task-1791565903972427000`, observed that Bob's activity was already `completed`, and identified snapshot SHA `fea18cc6796cecc0ed3dc1e941b73bb97002f125`.
   - **Crucial Behavior**: Alice did **not** dispatch Bob again and did **not** touch code files. She recognized that the implementation phase was done.
2. **Subagent Delegation to Charlie**:
   - Alice called OpenCode's native subagent tool:
     ```json
     {
       "tool": "task",
       "parameters": {
         "subagent": "charlie",
         "description": "Review snapshot fea18cc6796cecc0ed3dc1e941b73bb97002f125 in worktree .a4c/worktrees/task-1791565903972427000/tree. Verify RWMutex and atomic synchronization. Read-only."
       }
     }
     ```
   - OpenCode spawned native subagent **Charlie** running `mimo-v2.5`.
3. **Charlie's Read-Only Verification**:
   - Charlie inspected git diff between base commit `4a75ca5` and snapshot `fea18cc`.
   - Charlie ran fresh verification inside the worktree:
     ```bash
     GOWORK=off go test -race -count=1 -v ./...
     ```
     Result: All 3 tests passed in 1.359s.
   - Charlie produced structured review findings:
     - Synchronization: `sync.RWMutex` correctly serializes map mutations and reads.
     - Metrics: `sync/atomic` prevents race conditions on counters.
     - Minor Observation: Doc-comment bug annotations remain in comments (non-blocking).
     - File Modifications: 0 files modified.
   - Charlie approved the changes.
4. **Review Submission & State Transition**:
   - Alice received Charlie's approval and called `a4c_team_review_result`:
     ```json
     {
       "task_id": "task-1791565903972427000",
       "reviewer": "charlie",
       "verdict": "approved",
       "snapshot_sha": "fea18cc6796cecc0ed3dc1e941b73bb97002f125",
       "comments": "Independent code review by Charlie: synchronization verified, race test passes.",
       "findings": ["Stale BUG doc-comments (non-blocking)"]
     }
     ```
   - A4C's service verified the non-mutating reviewer invariant (worktree clean).
   - Task transitioned to `awaiting_human_review`.
   - A4C generated human confirmation token: `human-auth-9e105a56`.
5. **AI Self-Approval Guard**:
   - Alice stopped execution and reported the token and review findings to the human engineer:
     > *"Charlie completed his independent review and approved snapshot `fea18cc`... Next steps: Run `a4c task accept task-1791565903972427000 --token human-auth-9e105a56` via CLI."*

---

## 5. Phase C: Human Acceptance & Integration

### 5.1 Trust Boundary Enforcement

To verify that AI agents cannot bypass the governance gate, unauthenticated acceptance calls were evaluated:
- **Via MCP `team_accept` without token**:
  ```
  Result: Error: unauthorized: AI agents cannot approve their own work; valid human confirmation token required (or run 'a4c task accept <task-id>' via CLI)
  ```
- **Via MCP `team_integrate`**:
  ```
  Result: Error: service: task not accepted (task state is awaiting_human_review)
  ```

### 5.2 Human Approval via CLI

The human engineer explicitly approved the exact reviewed snapshot:
```bash
$ a4c task accept task-1791565903972427000 "Verified race conditions fixed and Charlie approved" --token human-auth-9e105a56
Task task-1791565903972427000 accepted successfully at snapshot fea18cc6796cecc0ed3dc1e941b73bb97002f125.
Working checkout untouched. Run 'a4c task integrate task-1791565903972427000' to apply changes.
```

Inspection after acceptance verified the two-step safety invariant:
```bash
$ git status
On branch main
nothing to commit, working tree clean

$ git rev-parse HEAD
4a75ca517514d47330c26b75de0adfdd2b0d69ba
```
Approval explicitly recorded human consent without altering the user's active working tree.

### 5.3 Task Integration via CLI

Integration was then executed as an independent operation:
```bash
$ a4c task integrate task-1791565903972427000
Task task-1791565903972427000 integrated successfully.
Commit: 20d6fed0bb9cef4c9b504da6b268b776e8e2d265 | Files modified: 0
```

Verification of repository history and test suite:
```bash
$ git log -n 3 --oneline
20d6fed (HEAD -> main) integrate(a4c): task task-1791565903972427000
fea18cc (a4c/task-1791565903972427000) Fix cache data races with RWMutex and atomic stats
4a75ca5 chore: ignore .gemini

$ GOWORK=off go test -race -v ./...
=== RUN   TestCache_BasicOperations
--- PASS: TestCache_BasicOperations (0.00s)
=== RUN   TestCache_Expiration
--- PASS: TestCache_Expiration (0.02s)
=== RUN   TestCache_ConcurrentAccess
--- PASS: TestCache_ConcurrentAccess (0.01s)
PASS
ok      example.com/cache   1.382s
```
The bug fix is now permanently integrated into `main`, verified with race detector passing.

---

## 6. Comprehensive Governance Assessment Matrix

Every governance invariant established across Milestones 1–14 is evaluated under the host-native execution model:

| Governance Invariant | Classification | Enforcing Mechanism | Verification Method | Failure Mode | Path to Elevate Assurance |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **1. Worktree Physical Isolation** | **Enforced** | Git worktree filesystem paths (`.a4c/worktrees/...`) | `git status` in root checkout before, during, and after Phase A/B | Model could ignore path instruction and edit root directly | Enforce via filesystem sandboxing (OS-level seatbelt/bwrap) restricting write paths to worktree directory. |
| **2. Non-Mutating Reviewer** | **Enforced & Detected** | **Enforced**: OpenCode frontmatter `permission: { edit: deny, write: deny }`.<br/>**Detected**: A4C `RecordReview` runs `git status --porcelain` on worktree; rejects if dirty. | Tested in `TestReviewerBoundaryEnforcement` (`pkg/a4c/reviewer_boundary_test.go`) | Host blocks tool call; if bypassed, A4C rejects review verdict with governance error. | For Copilot: declare explicit read-only subagent tool schema once Copilot CLI supports per-agent tool restrictions. |
| **3. Subagent Role Boundaries** (Alice delegates, Bob codes, Charlie reviews) | **Detected & Instruction Only** | Role prompts in `.github/agents/` and `.opencode/agents/`; A4C activity logs attribute actor. | Activity log inspection via `a4c task inspect` | Model attempts to perform work directly instead of delegating | Hard-filter MCP tool availability per subagent (e.g. only Alice can call `team_init`/`team_delegate`). |
| **4. Authentic Human Approval Gate** | **Enforced** | A4C MCP Server rejects unauthenticated `team_accept` calls; requires `HumanConfirmationToken` or direct CLI execution. | Tested via unauthorized MCP call simulation; CLI `--token` validation | If model calls `team_accept`, call fails closed with HTTP/JSON-RPC error | Fully enforced at service level. |
| **5. Two-Step Acceptance vs Integration** | **Enforced** | A4C state machine: `AcceptTask` only modifies lifecycle store; `IntegrateTask` performs Git commit/merge after SHA check. | Tested via `TestTeamService_AcceptTask_DoesNotModifyWorkingCheckout` | Calling integrate before accept returns `ErrTaskNotAccepted` | Fully enforced at service level. |
| **6. Snapshot Identity Verification** | **Enforced** | Git commit SHA comparison (`AcceptedSnapshot == currentSnapshot`) prior to merge. | Service test `ErrSnapshotMismatch` | Rejects merge if branch advanced after approval | Fully enforced at service level. |
| **7. Token Budget Governance** | **Instruction Only** | Injected prompt budgets; A4C stores activity budget requests. | Inspection of host token consumption | Host models manage their own context; A4C does not kill host sessions | Use host-native token limit flags (`--max-tokens`) where supported. |
| **8. YAWR Kernel Tool Governance** | **Not Supported (Host-Native)** | Host tools execute directly through host's shell/edit tools rather than YAWR kernel. | Code inspection of Copilot and OpenCode tool dispatch | Malicious shell command would execute with host process privileges | Expose YAWR kernel as an MCP tool proxy (`yawr_exec`) and deny host native shell tool. |

---

## 7. Host Environment Comparison & Findings

### 7.1 GitHub Copilot CLI vs. OpenCode CLI

| Dimension | GitHub Copilot CLI | OpenCode CLI |
| :--- | :--- | :--- |
| **Subagent Discovery** | `.github/agents/*.agent.md` | `.opencode/agents/*.md` |
| **Subagent Execution Syntax** | `task(agent_type: "<name>")` | `task(agent: "<name>")` or `task(subagent: "<name>")` |
| **Subagent Confinement** | Allows standalone invocation if permitted | Enforces `mode: subagent`; blocks standalone CLI execution (`! agent is a subagent`) |
| **Tool Permission Control** | Granular CLI flags (`--allow-tool='...'`) | Declarative YAML frontmatter (`permission: { ... }`) |
| **MCP Integration Schema** | VSCode standard `mcpServers: { ... }` | OpenCode schema `mcp: { <name>: { type: "local", ... } }` |
| **Context & Token Caching** | High prompt cache efficiency (~22.5k prompt tokens cached per turn) | Local model / daemon inference with direct streaming output |
| **Subagent Visibility** | Native `subagent.started` and `subagent.completed` event telemetry | Interactive step display with tool execution streaming |

### 7.2 Gotchas & Key Learnings

1. **Copilot CLI Tool Permission Naming**:
   Copilot CLI requires `--allow-tool='write'` for file writes, not `edit`. Supplying `--allow-tool=edit` caused Bob to be blocked on file modifications.
2. **OpenCode MCP Configuration Schema**:
   OpenCode rejects the standard VSCode `"mcpServers"` object in `opencode.json`. It requires `"mcp": { "a4c": { "type": "local", "command": ["a4c", "mcp"], "enabled": true } }`.
3. **Go Workspace Contamination**:
   When working in repositories nested inside parent Go workspaces (such as monorepos with `go.work`), tests in child modules fail unless `GOWORK=off` is explicitly passed.
4. **Subagent Execution Invariance in OpenCode**:
   Declaring `mode: subagent` in OpenCode agent frontmatter prevents accidental primary execution. When someone runs `opencode --agent charlie`, OpenCode warns and refuses to run Charlie as primary.

---

## 8. Conclusion & Recommendations

Milestone 15 conclusively proves the viability of A4C as a persistent host-native engineering team:
- **Zero Hidden Inference** is honored: Copilot and OpenCode executed 100% of the cognitive workload.
- **Cross-Host Continuity** is genuine: A task initialized in Copilot was resumed, reviewed, and finalized in OpenCode without losing state or repeating work.
- **Human Authority** is preserved: AI agents are programmatically barred from approving their own work, enforcing a strict verification boundary before any code reaches the primary branch.

### Recommended Next Steps for YAWR Integration

To graduate **YAWR Kernel Tool Governance** from *Not Supported* to *Enforced*:
1. Package YAWR's sandboxed tool execution engine as an MCP server (`yawr mcp`).
2. Configure both Copilot and OpenCode to route shell and filesystem operations through YAWR MCP tools (`yawr_shell`, `yawr_file_write`) while disabling native host tools (`shell`, `bash`, `write`).
3. This will unify host-native agent execution with YAWR's kernel-level capability governance.
