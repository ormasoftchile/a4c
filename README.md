# A4C: Agents for Coding — Host-Independent Engineering Organization

**A4C** turns coding hosts into a **governed, persistent software engineering organization**.

Its tasks, specialists, worktrees, decisions, activity history, and engineering knowledge exist independently of GitHub Copilot CLI, OpenCode, Antigravity, or any future coding host.

A4C uses each host at its near-native execution speed without imposing an artificial agent runtime, coordinator overhead, or unnecessary inference loops.

---

## 1. Fundamental Architecture

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

---

## 2. Execution Strategies

A4C supports three distinct execution strategies tailored to task complexity:

| Strategy | Flag | Execution Flow | Overhead | Ideal Use Case |
| :--- | :--- | :--- | :--- | :--- |
| **Direct Mode** | `--strategy direct` (Default) | Single specialist turn with worktree isolation and test evidence collection. Zero coordinators, zero automatic reviewers. | < 0.2s | Bugfixes, focused features, rapid iterations requiring native host speed. |
| **Reviewed Mode** | `--strategy reviewed` | Specialist implementer turn, followed by an independent read-only reviewer turn gating approval on passing test evidence. | Specialist + Reviewer | Security-sensitive changes, public API modifications, production code. |
| **Team Mode** | `--strategy team` | Lead engineer breaks down objective into delegated subtasks for specialists, followed by quality review. | Multi-turn | Complex multi-step architectural features requiring decomposition. |

---

## 3. Supported Peer Hosts & Capability Matrix

A4C treats supported hosts as peers, discovering their capabilities truthfully and enforcing governance appropriate to each host:

| Capability | GitHub Copilot CLI (`copilot`) | OpenCode (`opencode`) | Antigravity CLI (`antigravity` / `agy`) |
| :--- | :--- | :--- | :--- |
| **Execution Mode** | CLI automation SDK / stdio | Headless HTTP Server (`4096`) | Print non-interactive (`agy -p`) |
| **Worktree Binding** | ✅ Native worktree binding | ✅ Directory scoping | ✅ Working directory flag |
| **Context Injection** | ✅ Charter & Handoff prompt | ✅ Session prompt & system rules | ✅ Handoff prompt injection |
| **Telemetry & Tokens**| ✅ Prompt, completion, total | ✅ Input, output, reasoning | ✅ Input, output, thinking, cache |
| **Review Isolation** | ✅ SDK tool definition filters | ✅ Server permission table (`deny`) | ✅ Snapshot mutation check & rollback |
| **Model Selection** | ✅ Custom model per activity | ✅ Custom provider/model per activity | ✅ Custom model per activity |
| **Continuation** | ✅ Authoritative checkpoints | ✅ Authoritative checkpoints | ✅ Authoritative checkpoints |

---

## 4. Quick Start & CLI Reference

### 1. Build and Initialize
```bash
# Build A4C CLI
go build -o ~/.local/bin/a4c ./cmd/a4c

# Initialize A4C repository (.a4c durable stores)
a4c init .

# Diagnose environment across all hosts
a4c doctor
```

### 2. Direct Mode (Near-Native Speed)
Execute a focused task directly on any host:
```bash
# Direct task on OpenCode
a4c task start "Fix nil pointer dereference in cache.go" --runtime opencode --strategy direct

# Direct task on GitHub Copilot
a4c task start "Refactor options parser" --runtime copilot --strategy direct

# Direct task on Antigravity
a4c task start "Add table-driven unit tests" --runtime antigravity --strategy direct
```

### 3. Reviewed Mode (Independent Quality Gate)
Execute implementation with independent, non-mutating review:
```bash
a4c task start "Add distributed rate limiter" \
  --strategy reviewed \
  --runtime opencode \
  --reviewer-runtime copilot
```

### 4. Cross-Host Teams (Team Mode)
Run a mixed-host engineering team where different specialists execute on different hosts:
```bash
a4c task start "Implement robust health check endpoints" \
  --strategy team \
  --runtime copilot \
  --agent-runtimes coder=opencode,reviewer=copilot
```

### 5. Cross-Host Continuation
Pause an in-flight task on one host and continue on another:
```bash
# Capture authoritative checkpoint
a4c task checkpoint task-1791671400473010000

# Resume task on another host without context loss
a4c task resume task-1791671400473010000 --runtime opencode --run
```

### 6. Human Review & Integration
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

## 5. Verification & Regression Protection

Run the hardened test suite:
```bash
# Run production hardening tests (capabilities, evidence resolution, mutation rollback)
go test -v -run TestProductionHardening ./pkg/a4c

# Run universal host and mixed routing tests
go test -v -run "TestHostContract|TestMixedHostRouting|TestCrossHostContinuation" ./pkg/a4c
```
