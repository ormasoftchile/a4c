# OpenCode Integration Contract & Architecture Specification

## 1. Executive Summary & Installed Version

This document defines the verified integration contract between **A4C (Agents for Coding)** and **OpenCode (version 1.18.35)**, establishing OpenCode as A4C's second supported agent execution runtime alongside GitHub Copilot.

- **OpenCode Executable:** `/opt/homebrew/bin/opencode` (Mach-O 64-bit arm64, Homebrew Cellar `1.18.35`).
- **Installed Version:** `1.18.35`.
- **Runtime Mode:** Headless HTTP REST/OpenAPI Server (`opencode serve`) bound to `127.0.0.1`.
- **OpenAPI Specification:** Available at `GET /doc` (478 KB, OpenAPI 3.1).
- **Client Protocol:** Pure Go typed HTTP client (`net/http`) without Node.js runtime dependencies.

---

## 2. Verified OpenCode HTTP API Contract

All endpoints and request/response payloads below were verified directly against a running OpenCode `1.18.35` server on macOS arm64.

### 2.1 Server Lifecycle & Health Check

- **Startup Command:** `opencode serve --port <port> --hostname 127.0.0.1`
- **Health Check Endpoint:** `GET /global/health`
  - Response: `HTTP 200 OK`
  - Payload:
    ```json
    {
      "healthy": true,
      "version": "1.18.35"
    }
    ```
- **Authentication:** Supported via Basic Auth (`OPENCODE_SERVER_USERNAME`, `OPENCODE_SERVER_PASSWORD`). For managed local servers, loopback binding (`127.0.0.1`) with dynamic high ports ensures local process isolation.

### 2.2 Session Management

- **Create Session:** `POST /session?directory=<path>`
  - Request Body:
    ```json
    {
      "title": "A4C Task task-1791495918441718000",
      "model": {
        "providerID": "opencode-go",
        "id": "mimo-v2.5"
      },
      "permission": [
        { "permission": "bash", "pattern": "*", "action": "deny" },
        { "permission": "write", "pattern": "*", "action": "deny" },
        { "permission": "edit", "pattern": "*", "action": "deny" },
        { "permission": "task", "pattern": "*", "action": "deny" }
      ]
    }
    ```
  - Response: `HTTP 200 OK`
    ```json
    {
      "id": "ses_ee26390b7ffeVVfXRh84rXeuKO",
      "slug": "neon-eagle",
      "directory": "/path/to/workspace",
      "cost": 0,
      "tokens": {
        "input": 0,
        "output": 0,
        "reasoning": 0,
        "cache": { "read": 0, "write": 0 }
      },
      "version": "1.18.35",
      "time": { "created": 1791498153801, "updated": 1791498153801 },
      "permission": [
        { "permission": "bash", "pattern": "*", "action": "deny" },
        { "permission": "edit", "pattern": "*", "action": "deny" },
        { "permission": "write", "pattern": "*", "action": "deny" },
        { "permission": "task", "pattern": "*", "action": "deny" }
      ]
    }
    ```
- **Inspect Session:** `GET /session/{sessionID}` (Returns cumulative tokens, cost, status).
- **List Sessions:** `GET /session` (Array of all sessions).
- **Session History:** `GET /session/{sessionID}/message` (Full message history, parts, token metrics).
- **Abort / Cancel:** `POST /session/{sessionID}/abort` (Returns `true`, immediately halts active model execution).

### 2.3 Message Exchange & Execution Modes

- **Synchronous Turn Submission:** `POST /session/{sessionID}/message`
  - Request Body:
    ```json
    {
      "system": "Optional system prompt override",
      "tools": {
        "bash": false,
        "write": false,
        "edit": false,
        "task": false,
        "webfetch": false,
        "websearch": false
      },
      "parts": [
        {
          "type": "text",
          "text": "Task objective or prompt"
        }
      ]
    }
    ```
  - Response: `HTTP 200 OK`
    ```json
    {
      "info": {
        "id": "msg_11d9c966d001a8xFF0bTV2aujk",
        "sessionID": "ses_ee26390b7ffeVVfXRh84rXeuKO",
        "role": "assistant",
        "modelID": "mimo-v2.5",
        "providerID": "opencode-go",
        "cost": 0.0000500752,
        "tokens": {
          "total": 5374,
          "input": 126,
          "output": 47,
          "reasoning": 17,
          "cache": { "write": 0, "read": 5184 }
        },
        "time": {
          "created": 1791498163821,
          "completed": 1791498167869
        },
        "finish": "stop"
      },
      "parts": [
        { "type": "step-start", "id": "prt_...", "snapshot": "..." },
        { "type": "reasoning", "text": "Model internal reasoning...", "time": { ... } },
        { "type": "text", "text": "Assistant output text...", "time": { ... } },
        {
          "type": "step-finish",
          "reason": "stop",
          "tokens": {
            "total": 5374,
            "input": 126,
            "output": 47,
            "reasoning": 17,
            "cache": { "write": 0, "read": 5184 }
          },
          "cost": 0.0000500752
        }
      ]
    }
    ```
- **Asynchronous Turn Submission:** `POST /session/{sessionID}/prompt_async` (Returns `HTTP 204 No Content`).
- **Streaming Event Subscription:** `GET /event` or `GET /api/session/{sessionID}/event` (Server-Sent Events: `text/event-stream`).

---

## 3. Tool Containment & YAWR Execution Authority (P0)

OpenCode is a full coding agent runtime with built-in execution tools. Unchecked execution poses severe governance risks (unmediated file overwrites, unapproved Git operations, arbitrary shell execution, runaway subagent spawning).

### 3.1 Exposed Tool Attack Surface

Verified native OpenCode tools (`GET /experimental/tool?provider=opencode-go&model=mimo-v2.5`):

| Tool ID | Purpose | Risk Level | Governance Action |
| :--- | :--- | :---: | :--- |
| `bash` | Shell command execution | **CRITICAL** | **Deny completely**. Never permitted. |
| `write` | Arbitrary file creation | **HIGH** | Controlled: denied for Reviewer; allowed only in isolated worktree for Coder. |
| `edit` | Arbitrary file editing | **HIGH** | Controlled: denied for Reviewer; allowed only in isolated worktree for Coder. |
| `task` | Spawning subagents | **HIGH** | **Deny completely**. A4C owns team delegation. |
| `webfetch` | URL content retrieval | **MEDIUM** | Deny by default; authorized only if capability permits. |
| `websearch` | Real-time web search | **MEDIUM** | Deny by default; authorized only if capability permits. |
| `read` | Bounded file reading | **LOW** | Authorized for Coder/Reviewer. |
| `glob` | File pattern matching | **LOW** | Authorized for Coder/Reviewer. |
| `grep` | Code search | **LOW** | Authorized for Coder/Reviewer. |

### 3.2 Dual-Layer Enforcement Mechanism

1. **Layer 1: Session-Level Permission Rules (`POST /session`)**
   - Ruleset:
     ```json
     [
       { "permission": "bash", "pattern": "*", "action": "deny" },
       { "permission": "task", "pattern": "*", "action": "deny" },
       { "permission": "write", "pattern": "*", "action": "deny" },
       { "permission": "edit", "pattern": "*", "action": "deny" }
     ]
     ```
   - **Empirically Proven Behavior:** Setting `action: "deny"` causes OpenCode to completely strip the tool definition from the LLM prompt. The LLM explicitly reports it has no access to shell or mutation capabilities.
2. **Layer 2: Per-Message Tool Toggles (`POST /session/{sessionID}/message`)**
   - In every turn payload, `tools: { "bash": false, "task": false, ... }` explicitly turns off unauthorized tools.
3. **Layer 3: Workspace Isolation (Option B)**
   - When a Coder activity executes, it runs inside an isolated Git worktree (`wt.WorktreePath`) managed by `WorktreeManager`.
   - The primary workspace remains immutable.
   - Mutations are captured via `git diff` on the worktree, validated via `NativeGoTestRunner`, stamped with cryptographic SHA-256 tree hashes, and submitted to the independent Reviewer.
   - Reviewer runs with `write: deny`, `edit: deny`, `bash: deny`, `task: deny` (strictly read-only).

---

## 4. Token Accounting & Economic Normalization

OpenCode exposes detailed token metrics that map directly to A4C's normalized `UsageInfo`:

| OpenCode Metric | A4C `UsageInfo` Field | Description |
| :--- | :--- | :--- |
| `tokens.input` | `PromptTokens` | Input / prompt tokens billed |
| `tokens.output` | `CompletionTokens` | Output / generation tokens billed |
| `tokens.reasoning` | `ReasoningTokens` | Provider thinking tokens (subset of completion/output) |
| `tokens.cache.read` | `CachedPromptTokens` | Cached prompt tokens read |
| `tokens.cache.write`| `CacheCreationTokens`| Prompt tokens written to cache |
| `tokens.total` | `TotalTokens` | Total chargeable tokens reported |
| `cost` | `ReportedCostUSD` | Monetary cost in USD reported by OpenCode |

### Duration Domain Separation

- `ProviderWaitDuration`: Measured from `POST /session/{sessionID}/message` request dispatch to HTTP response completion.
- `ToolDuration`: Measured from local tool/worktree operations.
- `ValidationDuration`: Measured from native test executions (`go test -short ./...`).
- `WallClockDuration`: Total activity lifecycle duration.

---

## 5. Explicit Milestone 12 Execution Order

To ensure methodical, leak-free implementation without regressions to existing Copilot and deterministic suites, the milestone follows this strict sequence:

```
[Phase 1: Foundation]
  1.1 Integration Contract & Architecture Spec (this document)
  1.2 Refine Provider Abstraction (pkg/a4c/provider.go, types.go)
       - Minimal, backward-compatible interface updates
       - Decouple Copilot-specific structures

[Phase 2: OpenCode Adapter Implementation]
  2.1 Managed & External OpenCode Server Lifecycle (pkg/a4c/opencode_server.go)
       - Process spawning on 127.0.0.1, dynamic port, health check polling, graceful termination
  2.2 OpenCode Client & Session Adapter (pkg/a4c/opencode_adapter.go)
       - Session creation, permission ruleset, tool toggles, synchronous & streaming turns
       - Token mapping & duration domain separation
       - Cancellation & session abortion

[Phase 3: YAWR Governance & Capability Scoping]
  3.1 Capability Profile to OpenCode Permission Mapping
       - Coder: Read/Glob/Grep allowed, Mutations isolated to Worktree, Bash denied, Task denied
       - Reviewer: Strictly read-only, all mutations denied, Bash denied, Task denied
  3.2 Budget Settlement & In-Flight Checks (ReconcileWithStore integration)

[Phase 4: CLI & Configuration Integration]
  4.1 CLI `--runtime` Flag in cmd/a4c (default: copilot; supports opencode)
  4.2 Configuration Support for OpenCode Endpoint & Model

[Phase 5: Deterministic Verification Suite]
  5.1 Implement Fake OpenCode HTTP Server & Test Suite (pkg/a4c/milestone12_test.go)
       - 20 required deterministic test scenarios

[Phase 6: Live Integration & Comparative Benchmark]
  6.1 Live OpenCode Tests against real OpenCode daemon (pkg/a4c/milestone12_live_opencode_test.go)
  6.2 Live Copilot Regression Verification
  6.3 Comparative Autonomous Engineering Experiment (Same task under Copilot vs OpenCode)

[Phase 7: Final Documentation & Release Gate]
  7.1 Author docs/milestone-12-opencode-report.md
  7.2 Final verification, git commit, and sign-off
```
