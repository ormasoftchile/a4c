# A4C Knowledge — Architecture, MCP Contract, and Cross-Host Integration

**Status:** Implemented (first vertical slice) — see §0 for what is built vs deferred
**Scope:** Optional, modular engineering-knowledge subsystem for A4C
**Hosts:** GitHub Copilot CLI, OpenCode, Antigravity (`agy`)

---

## 0. Implementation status (first vertical slice)

This revision corrects three design issues and ships a working end-to-end slice.

### Corrections made

1. **MCP execution-context binding.** Replaced the workspace-shared
   `current.json` (which let a delayed call inherit a later activity's
   identity) with a **per-activity HMAC token**. Identity comes from the token,
   never from shared mutable state. Copilot binds it per-session via MCP server
   `Env` (strongest). OpenCode/Antigravity inject it into the session prompt and
   the model relays it as `context_token` (model-mediated, documented). A token
   for a completed activity fails its `StatusClaimed` check — a delayed call is
   rejected, not misattributed. `current.json` is **not** used.

2. **Transactional idempotency.** The idempotency key is resolved **inside** the
   transaction, before any record insert. On a key hit the original record is
   returned and nothing is written; on a new key exactly one record is created
   and associated atomically. A crashed/rolled-back transaction leaves no orphan.
   Content hash is integrity metadata only — never semantic dedup. Key reuse with
   a different payload returns the original and flags `conflict` (deterministic).

3. **Claim-assessment semantics.** `support` is **never** set at record time
   (enforced in `Store.Record`). Evidence/experiment records start `observed`;
   all others start `untested`. `knowledge_assess` is the single authoritative
   mutation path for `support` — it retains assessor, timestamp, evidence refs,
   scope, rationale, origin (agent/reviewer/human), and is append-only.
   `knowledge_revise` cannot change `support`. *(Assessment is designed and
   schema-defined; its MCP exposure is deferred — see below.)*

### Implemented and tested
- `pkg/a4c/knowledge/`: `model.go`, `context.go` (HMAC token), `store.go`
  (SQLite/WAL, multi-process, transactional idempotency), `config.go`,
  `service.go`.
- MCP `knowledge_record` + `knowledge_query` (`pkg/a4c/mcp/knowledge_tools.go`),
  gated by enablement + token.
- CLI `a4c knowledge enable|disable|status|list|query|show`.
- Coordinator issues a per-activity token; Copilot binds it via MCP `Env`;
  OpenCode/Antigravity inject it into the prompt.
- Governance: `knowledge_*` in allowlists + `ClassifyTool`.

### Implemented but not live-verified
- OpenCode and Antigravity token-in-prompt attribution (model-mediated). The MCP
  surface is verified via a real `a4c mcp` subprocess and three separate SQLite
  connections; a live agent spontaneously invoking `knowledge_record` was **not**
  observed (capture is best-effort by design).

### Designed but deferred (not in this slice)
- `knowledge_link`, `knowledge_assess`, `knowledge_revise` MCP exposure (contracts
  corrected in §5/§6; not registered yet).
- Deterministic evidence harvesting, milestone reconciliation, semantic
  dedup/analysis, export/import, publication.

### Known limitations
- OpenCode/Antigravity attribution is model-mediated (the model relays the
  token); a hostile model inside a live activity could replay its own token
  within TTL. Copilot's per-session env binding does not have this weakness.
- Agent-authored capture is best-effort; a model may decline to call
  `knowledge_record` (observed in live runs). Deterministic capture is not yet
  implemented, so no records are produced when the agent is silent.
- The shared knowledge DB lives in the main workspace; worktrees share it via
  the coordinator-passed `KnowledgeDir`.

---

> **Revision note.** This revision corrects multi-process persistence, MCP
> execution identity, the epistemic/verification model, the boundary between
> automatic evidence capture and knowledge creation, idempotency semantics,
> contradiction handling, and per-host MCP assumptions. Sections 1, 3–9, 11–12,
> 17–19, 21–22 were rewritten; obsolete statements were removed rather than
> appended to. All repository and host facts below were re-verified against the
> working tree and the installed host CLIs.

---

## 1. Executive summary

A4C already owns durable engineering state — activities, worktrees, verification
evidence, review findings, budgets, and a small "team memory" store. What it does
not own is *engineering understanding*: the hypotheses engineers form, the
experiments they run, the alternatives they reject, the negative results they
obtain, and the conclusions they carry forward. That understanding is recomputed
per task and lost when a task, project, or host ends.

**A4C Knowledge** is an optional subsystem that captures, evidence-links,
maintains, and exposes that understanding. It reuses A4C's existing governance,
MCP-server, and host-integration machinery. It is exposed to all three hosts
through the **existing A4C MCP server** (`a4c mcp`).

The design rests on five commitments:

1. **Knowledge is not a transcript.** Structured, typed, evidence-linked records.
2. **Evidence is not interpretation.** Observed results and agent conclusions are
   distinct first-class concepts, and a valid hash is not a true conclusion.
3. **Negative results are first-class.** A failed investigation still yields
   durable, retrievable knowledge.
4. **Capture is optional and cheap.** Disabled ⇒ zero tools, zero instructions,
   zero cost. Enabled ⇒ deterministic evidence capture plus a small agent-authored surface.
5. **Knowledge outlives its producer.** A failed project, retired host, or
   replaced A4C install does not destroy accumulated research.

Three corrections distinguish this revision from the first draft:

- **Persistence is multi-process.** A4C Knowledge is written by the coordinator
  *and* by separately-launched MCP server processes. A Go mutex protects only one
  process; the store is therefore **SQLite in WAL mode** (pure-Go driver), while
  A4C's existing single-writer JSON stores remain untouched for operational state.
- **Identity is a validated execution context, not CLI arguments or model input.**
  The coordinator issues a short-lived HMAC-signed context token bound to
  project/investigation/task/activity/role/host/session. The MCP server validates
  it on every call. Identifying a task is separated from authorizing a caller.
- **"Verified" is not a promotion target.** Evidence integrity, experiment
  validity, claim support, claim scope, and confidence are five separate
  properties. A reviewer approving code does not verify an architectural
  hypothesis; a passing unit test does not verify a performance conclusion.

The primary deliverable is a trustworthy, portable knowledge base. Publication
(PDF/LaTeX/books) is an independent downstream consumer and is explicitly out of
scope.

---

## 2. Goals, constraints, and non-goals

### 2.1 Goals

| # | Goal |
|---|---|
| G1 | Capture consequential discoveries as they occur, not only at task end. |
| G2 | Preserve original hypotheses and explicit rationale verbatim. |
| G3 | Connect every claim to independently inspectable evidence (with integrity hashes). |
| G4 | Track how understanding changes over time (revision history, explicit supersession). |
| G5 | Retain negative and contradictory findings as first-class records. |
| G6 | Accumulate knowledge across many A4C tasks within a project. |
| G7 | Make prior knowledge retrievable by future agents on any host. |
| G8 | Support new investigations that build on earlier research, including after a failed project. |
| G9 | Remain usable if A4C is replaced (open, machine-readable export). |
| G10 | Impose no measurable cost when disabled; minimal token cost when enabled. |

### 2.2 Constraints

- **C1 — Reuse, don't reinvent.** No new persistence, authorization, event, or
  host-integration abstraction when A4C already supplies one. §3 maps every seam.
- **C2 — MCP is the shared interface.** One tool surface, consumed by all three
  hosts via their verified MCP-client capabilities. No per-host knowledge
  implementations.
- **C3 — Governance is mandatory.** Every knowledge tool passes
  `PolicyGovernor.AuthorizeToolInvocation` and budget/tool-limit accounting.
- **C4 — Identity is A4C-validated.** Project, investigation, task, and activity
  identities are established by A4C and validated by the MCP server; they are
  never accepted on the model's word alone.
- **C5 — Provenance is enforced.** Agent-authored conclusions are never presented
  as independently established facts.
- **C6 — No runtime rewrite.** A4C Knowledge is a subsystem, not a new orchestrator.

### 2.3 Non-goals

- Generating PDFs, theses, books, or formatted publications.
- Replacing operational task state, checkpoints, or the memory store's
  architectural-convention role.
- Recording every model token, tool call, or hidden chain-of-thought.
- Adding a permanent research agent to every coding team.
- A general-purpose workflow or experiment-orchestration engine.
- Making knowledge capture mandatory.

---

## 3. Analysis of the existing A4C implementation

> Everything here is a **verified implementation fact** with file:line
> references (re-verified this revision). Proposed changes are in §4+.

### 3.1 MCP server and tool registration

- **Server:** `pkg/a4c/mcp/server.go:14-45` — wraps `github.com/mark3labs/mcp-go`
  `server.MCPServer`, holds a `a4c.TeamService` and an optional `role` string.
  `NewServer(service)` / `NewServerWithRole(service, role)`; `ServeStdio()`.
- **Role gating:** `registerTools()` `server.go:47-66` → `addAgentTool(forLead,forCoder,forReviewer,tool,handler)`.
  Un-roled server registers any tool permitted for ≥1 role; roled server
  (`--role lead|coder|reviewer`) registers only that role's. `team_accept` /
  `team_integrate` are never registered over MCP (`server.go:130-132`;
  `handleIntegrate` fails closed at `375-377`).
- **12 tools** (`server.go:68-211`): `team_delegate`, `team_list`, `team_status`,
  `team_result`, `team_diff`, `team_feedback`, `team_context`, `team_begin_task`,
  `team_assign_activity`, `team_checkpoint`, `team_complete_activity`,
  `team_review_result`. **Identity today is model-supplied `task_id`/`activity_id`
  tool arguments** (`server.go:95-203`) — there is no execution-context validation.
- **CLI:** `cmd/a4c/main.go:1283-1306` `runMCP` parses `--dir`, `--role|--agent`.

### 3.2 Governance / authorization

- **PolicyGovernor** `governor.go:35-45`; `SetAgentTools` `governor.go:279-289`;
  **`AuthorizeToolInvocation(agentID, toolName)`** `governor.go:730-748`
  (agent-specific set first, then global allowlist; `ErrUnauthorizedTool`);
  also a filter in `ResolveProfile` (`capability.go:162-171`).
- **Global allowlist** in three places (`main.go:332-335`, `1430-1433`, `1530-1533`).
- **Roster tool sets:** `DefaultTeamRoster` `team.go:111-188`.
- **`ClassifyTool`** `tool_governance.go:28-39` — terminal / exploration / work.

### 3.3 Durable stores (reuse pattern)

| Store | Dir | Record | Notes |
|---|---|---|---|
| `DurableActivityStore` | `.a4c/activities` | `ActivityRecord` `store.go:14-19` | `Get` re-reads from disk (`store.go:403`); `StatusClaimed` (`store.go:202`); `HasClaimable` (`store.go:448`) |
| `DurableMemoryStore` | `.a4c/memory` | `MemoryRecord` `memory.go:28-39` | propose→promote→deprecate `memory.go:102-168`; `FormatContext` `memory.go:199-213` |
| `DurableIntentJournal` | `.a4c/journal` | `IntentRecord` `types.go:268-279` | |
| `DurableLifecycleStore` | `.a4c/lifecycle` | `HumanLifecycleRecord` `service_types.go:45-63` | |

All four use per-record JSON + `sync.Mutex` + `.tmp`+`os.Rename`. **This is
correct for A4C's operational state because a single coordinator process holds
the exclusive kernel `flock` on `.a4c/workspace.lock` (`workspace_lock.go:26-60`)
while executing.** A4C Knowledge does *not* have that property (see §4.2).

### 3.4 Activity lifecycle and events

- Statuses `types.go:22-32`. Per-step closures (not an event bus): six
  `AgentConfig` callbacks (`provider.go:43-50`) wired at
  `team_coordinator.go:696-747`. Memory promotion at `team_coordinator.go:1084`,
  `1435-1438` (`verifier="native_tests"`), `1907-1911` (`verifier="reviewer:<id>"`).
- `SessionRef` (`session_ref.go:14-47`) durably maps activity → runtime + host
  session ID (`.a4c/sessions/<activity>.json`), atomic write.

### 3.5 Optional-module precedent

No feature-flag framework. The pattern is a **trusted workspace config file**:
`.a4c/verify.json` (`verification.go:225-237`), carried into worktrees by
`copyTrustedConfig` (`worktree.go:237-253`). `RuntimeCapabilities`
(`provider.go:70-79`) separates optional host capabilities. `RunDoctor`
(`roster_export.go:320+`) checks optional components.

### 3.6 Validation evidence (reuse for evidence links)

`TestRunner`/`ProjectTestRunner` (`test_runner.go:14-31`, `verification.go:77-168`);
`WriteDurableTestLog` (`test_runner.go:79-104`) → `.a4c/evidence/<id>.log`;
`ValidationEvidence` (`types.go:281-309`) with `EvidenceID`, `Command`,
`ExitCode`, `LogPath`, `TestsExecuted`, `Vacuous`, `SourceRevision`,
`WorkspaceSnapshotID`. `ReviewFinding` (`types.go:328-334`).

### 3.7 Cross-host MCP integration (re-verified this revision)

| Host | MCP client | Registration surface | Per-session MCP config? | Charter injection |
|---|---|---|---|---|
| **Copilot** | **Yes** — SDK `SessionConfig.MCPServers` (`go@v1.0.17/types.go:1266`, per-agent) with `MCPStdioServerConfig{Command,Args,Env,WorkingDirectory,Tools,Timeout}` (`types.go:1202-1211`). **A4C does not currently set it** (`copilot_adapter.go:755-766` sets no `MCPServers`). Workspace files `.mcp.json` / `~/.copilot/mcp-config.json` (`roster_export.go:93-134`). | **Yes (verified)** — SDK `Env`/`Args` are per-session | `SystemMessage{Content: Charter}` `copilot_adapter.go:763-765` |
| **OpenCode** | **Yes** — workspace `opencode.json` `mcp.<name>` with `environment` (schema-verified); `opencode mcp list` already attempts `a4c mcp`. MCP servers are launched by the shared `opencode serve` process, **not per A4C session**. | **No** (workspace-scoped) | `reqPayload["system"]=Charter` `opencode_adapter.go:423-425` |
| **Antigravity** | **Yes** — `agy mcp add <name> <cmd> [args…] --env KEY=VAL` (CLI-verified); config `~/.gemini/config/mcp_config.json`. A4C spawns `agy -p` per turn as a subprocess (`antigravity_adapter.go:249-264`) with `--conversation` for resume. **Whether agy launches MCP servers per subprocess or persists them across turns is UNVERIFIED and requires a live test.** | **No** (global user config) | No Charter channel; role via Handoff prompt + skill |

OpenCode's exported agents already deny MCP tools via permission rulesets with
arbitrary action names (`"a4c_team_accept": deny` — `roster_export.go:160-161`).

### 3.8 Reuse seams (summary)

| Need | Existing mechanism | Location |
|---|---|---|
| Cross-process lock | kernel `flock` on `.a4c/workspace.lock` | `workspace_lock.go` |
| MCP exposure | `addAgentTool` + `TeamService` | `mcp/server.go:53-66` |
| Authorization | allowlists + `SetAgentTools` + `AuthorizeToolInvocation` + `ClassifyTool` | `governor.go`, `tool_governance.go` |
| Events | `AgentConfig` callbacks + store mutation points | `provider.go:43-50` |
| Optional enablement | `.a4c/verify.json` trusted-config | `verification.go:225-237` |
| Session↔activity binding | `SessionRef` | `session_ref.go` |
| Evidence | `ValidationEvidence` + `.a4c/evidence` logs | `types.go:281-309` |
| Provenance audit | hash-chained JSONL | `task_integrity.go:41-143` |
| Host install | `a4c install copilot\|opencode\|antigravity` | `roster_export.go` |

---

## 4. Module boundary, persistence, and execution context

### 4.1 Component layout

```
pkg/a4c/knowledge/
  model.go        KnowledgeRecord, Kind, Support, Integrity, Status, Scope, Link, EvidenceRef
  store.go        KnowledgeStore (SQLite/WAL) + migrations
  context.go      ExecContext token issue/validate (HMAC)
  capture.go      deterministic evidence/experiment capture only
  assess.go       claim-support assessment (agent/human, evidence-cited)
  reconcile.go    bounded milestone reconciliation
  analyze.go      deterministic + model-assisted analysis
  export.go       open JSON/JSONL export
  config.go       .a4c/knowledge.json enablement (mirrors verification.go)
  service.go      KnowledgeService facade

pkg/a4c/mcp/
  knowledge_tools.go   MCP registration + handlers (gated by enablement + context)
```

`KnowledgeService` is constructed in `buildService` (`main.go:299-340`) **only
when enabled** and passed into the MCP server alongside `TeamService`. The
`DurableMemoryStore` is **not** extended; Knowledge gets its own store.

### 4.2 Persistence: SQLite WAL (multi-process)

**Problem.** The first draft proposed per-record JSON + `sync.Mutex`. A mutex
protects one process. A4C Knowledge is written concurrently by:

- the A4C coordinator process (deterministic capture at activity completion), and
- one or more `a4c mcp` server processes (one per host session, or one per
  workspace for OpenCode/Antigravity workspace-level registrations).

Two writers in different processes can (a) lose updates on read-modify-write of
an index, (b) create torn relationship sets, (c) double-apply a revision, or
(d) corrupt a sidecar index. `os.Rename` makes a *single file* atomic but does
not make a *multi-file update* (record + links + index) atomic across processes.

**Decision: SQLite in WAL mode, pure-Go driver `modernc.org/sqlite`.**

- *Why not JSON + flock-per-file*: flock serializes writers but not
  read-modify-write of shared indexes/relationship sets; unique-constraint
  idempotency and transactional multi-table updates are not expressible; crash
  recovery for half-written multi-file updates is manual.
- *Why not extend `DurableMemoryStore`*: its `sync.Mutex` + in-memory map is
  correct only under A4C's single-coordinator-with-workspace-lock model. That
  invariant does not hold for the MCP server processes.
- *Why SQLite/WAL*: cross-process concurrent readers + single-writer-with-readers;
  `BEGIN IMMEDIATE` transactions; `UNIQUE` constraints for idempotency; WAL
  replay on crash; `PRAGMA foreign_keys=ON` for relationship integrity; pure-Go
  (no cgo) so `go build ./...` stays portable. `go.mod` currently has no SQLite
  driver (verified); adding `modernc.org/sqlite` is a new but justified dependency.
- *Where existing JSON stores remain*: A4C's operational stores
  (activities/memory/journal/lifecycle/budgets) are **unchanged** — they are
  written only under the workspace lock by the coordinator. Knowledge reuses
  their *patterns* (atomic write, load-on-start, mutex for the in-process cache)
  but not their storage engine.

**Schema (v1):**

```sql
PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;

CREATE TABLE records (
  id              TEXT PRIMARY KEY,           -- k-<proj>-<kind>-<b32>
  project_id      TEXT NOT NULL,
  investigation_id TEXT,
  task_id         TEXT,
  activity_id     TEXT,
  kind            TEXT NOT NULL,              -- hypothesis|experiment|evidence|finding|decision|alternative|lesson|question
  statement       TEXT NOT NULL,
  rationale       TEXT,
  support         TEXT NOT NULL DEFAULT 'untested', -- untested|observed|supported|contradicted|inconclusive
  status          TEXT NOT NULL DEFAULT 'draft',    -- draft|published|retracted
  confidence      TEXT,                       -- high|medium|low|unknown
  integrity       TEXT NOT NULL DEFAULT 'pending',  -- pending|verified|broken
  scope_json      TEXT,                       -- {"configs":[],"workloads":[],"assumptions":[],"host":…}
  author          TEXT NOT NULL,              -- agent id | "deterministic"
  host            TEXT,
  revision        INTEGER NOT NULL DEFAULT 1,
  supersedes      TEXT,
  superseded_by   TEXT,
  content_hash    TEXT NOT NULL,              -- sha256(statement|rationale) — INTEGRITY ONLY
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  CHECK (support  IN ('untested','observed','supported','contradicted','inconclusive')),
  CHECK (status   IN ('draft','published','retracted')),
  CHECK (integrity IN ('pending','verified','broken'))
);
CREATE INDEX idx_rec_scope ON records(project_id, investigation_id);
CREATE INDEX idx_rec_support ON records(project_id, support);
CREATE INDEX idx_rec_task ON records(project_id, task_id);

CREATE TABLE evidence (
  record_id  TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
  idx        INTEGER NOT NULL,
  kind       TEXT NOT NULL,          -- validation_evidence|log|diff|commit|url|external
  artifact_id TEXT,
  hash       TEXT,                   -- sha256 of artifact bytes
  locator    TEXT,                   -- path/URL
  integrity  TEXT NOT NULL DEFAULT 'pending',  -- pending|verified|broken
  validity   TEXT,                   -- unknown|agent_asserted|reviewer_confirmed|human_confirmed
  PRIMARY KEY (record_id, idx)
);
CREATE TABLE links (
  from_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
  to_id   TEXT NOT NULL REFERENCES records(id),
  rel     TEXT NOT NULL,             -- supports|contradicts|qualifies|limits|supersedes|derived_from|corroborates|answers
  note    TEXT,
  PRIMARY KEY (from_id, to_id, rel)
);
CREATE TABLE revisions (
  record_id     TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  snapshot_json TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  PRIMARY KEY (record_id, revision)
);
CREATE TABLE idempotency (
  activity_id TEXT NOT NULL,
  client_key  TEXT NOT NULL,
  record_id   TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (activity_id, client_key)
);
```

**Write transaction (`knowledge_record`):**

```
BEGIN IMMEDIATE;
  INSERT INTO records (...) VALUES (...)            -- id = generated; ON CONFLICT(id) DO NOTHING
  INSERT INTO evidence (record_id, idx, ...) VALUES ...   -- per evidence ref, integrity resolved
  INSERT INTO links (from_id,to_id,rel,note) VALUES ...   -- ON CONFLICT DO NOTHING
  INSERT INTO revisions (record_id, revision, snapshot_json) VALUES (id, 1, snapshot)
  INSERT INTO idempotency (activity_id, client_key, record_id) VALUES (...)
      ON CONFLICT(activity_id, client_key) DO NOTHING;   -- retry-safe
COMMIT;
```

- **Uniqueness / idempotency:** `UNIQUE(activity_id, client_key)`. A retry with
  the same key returns the existing `record_id` (no duplicate). Content hash is
  **not** used for dedup (§5.4).
- **Concurrent updates:** `BEGIN IMMEDIATE` takes the write lock; concurrent
  processes block up to `busy_timeout` then fail cleanly. Revision increments are
  inside the transaction (`UPDATE records SET revision = revision + 1 …`).
- **Crash recovery:** WAL replay is automatic; a transaction that did not `COMMIT`
  is rolled back. On open, the store runs a integrity check (`PRAGMA quick_check`)
  and, if a `revisions` row references a missing `records` row, logs and quarantines.
- **Restart / cross-host visibility:** the DB is on shared workspace disk; every
  MCP process opens the same file. WAL gives read-your-writes across processes.
- **Migration / schema evolution:** `PRAGMA user_version`; forward migrations in
  `store.go`; readers tolerate unknown columns via `SELECT` of known columns only.

**Retention of A4C patterns:** the SQLite store still uses an in-process
`sync.RWMutex` for its cache and still loads state on start, mirroring the JSON
stores — but correctness does not depend on the mutex across processes.

### 4.3 Execution context: identity vs authorization

**Problem with the first draft.** `--task`/`--activity` CLI args are unreliable
because Copilot/OpenCode/Antigravity mostly register `a4c mcp` at **workspace**
scope, which survives many activities. And a model-supplied `task_id` is not
proof of execution identity.

**Design.** Two separate properties:

1. **Identification** — *which* task/activity/investigation/project/host/role
   does this call act for?
2. **Authorization** — *may* this caller act for it?

**Identification: HMAC-signed execution-context token.**

```go
type ExecContext struct {
  ProjectID, InvestigationID, TaskID, ActivityID, Role, Host, SessionID string
  IssuedAt, ExpiresAt time.Time   // default TTL 4h
}
// token = base64url(json(ExecContext)) + "." + hex(HMAC_SHA256(secret, json(ExecContext)))
// secret = .a4c/knowledge.secret  (0600, generated once at enable; never sent to models)
```

- The **coordinator** issues a token when it creates a host session, binding the
  identities it already owns (`ActivityRequest`, `SessionRef`, roster role).
- The MCP server **validates** the token on every knowledge call: parse, verify
  HMAC, check `ExpiresAt`, and check that the referenced task exists and its
  activity is currently `StatusClaimed` (via `DurableActivityStore.Get`,
  `store.go:403`). A stale or forged token is rejected before any store access.
- The **role** inside the token (not a model argument) is passed to
  `PolicyGovernor.AuthorizeToolInvocation`.

**Authorization is separate.** A valid token *identifies*; it does not by itself
authorize. Every knowledge tool still calls `AuthorizeToolInvocation(role, tool)`
and consumes budget/tool limits exactly like `governed_*` tools. A lead token
cannot call a reviewer-only operation even if the model tries.

**Per-host context delivery (verified vs unverified):**

| Host | Mechanism | Status |
|---|---|---|
| Copilot | Set `SessionConfig.MCPServers["a4c"] = MCPStdioServerConfig{Command:"a4c", Args:["mcp","--dir",KnowledgeDir], Env:{"A4C_KNOWLEDGE_TOKEN": token}}` per session. SDK `Env`/`Args` verified; **implemented** (`copilot_adapter.go`). The MCP server process is created/destroyed with the session, so a delayed call carries the originating activity's token. | **Implemented + subprocess-verified** |
| OpenCode | MCP server is workspace-scoped (launched by `opencode serve`), so per-session env is not reliable. The coordinator injects the token into the **session prompt**; the model relays it as the `context_token` argument. Weaker than Copilot's env binding (model-mediated) — documented, not claimed as cryptographic. | **Implemented; model-mediated** |
| Antigravity | Same as OpenCode: token injected into the `agy -p` session prompt; model relays it as `context_token`. Whether agy persists an MCP server across `--conversation` subprocesses is unverified. | **Implemented; model-mediated** |

`runMCP` (`main.go`) reads the token from `A4C_KNOWLEDGE_TOKEN` (env, Copilot)
or the `context_token` tool argument (OpenCode/Antigravity). The MCP server
refuses all knowledge operations when no valid, unexpired, activity-claimed
context is resolvable. Identity comes from the **token**, never from a shared
"current activity" file — a delayed call from a completed activity fails its
`StatusClaimed` check rather than being attributed to a later activity.

**Stale/revoked contexts.** Tokens expire (default 4h). The authoritative
liveness check is `StatusClaimed` against the A4C activity store; a completed
activity's token fails closed. `Revoker` adds an explicit fast-path revoke. A
resumed activity receives a fresh token.

**Cross-task / cross-project access.** The token binds `ProjectID`; the store is
opened with `--dir <workspace>`; a query for another project's task fails because
the token's project does not match and the store does not contain it.

**Documented limitation (enforceable boundary, not a claimed property).**
Stdio MCP cannot cryptographically bind a single tool call to a specific host
session. A hostile model *inside a claimed activity* could replay that activity's
token within its TTL. The enforceable boundary is: task-scoped short-TTL tokens
+ role authorization + the workspace lock serializing activities + a
hash-chained audit of every knowledge mutation (`task_integrity.go:41-143`).
Cross-project leakage is impossible (project-bound token + workspace-scoped
store). This is the same trust level A4C already grants a model writing
`governed_propose_memory`; Knowledge does not weaken it.

### 4.4 Enablement contract (disabled behavior)

**File:** `.a4c/knowledge.json` (mirrors `.a4c/verify.json`).

```json
{ "enabled": true, "project": "skiff", "scope": "project",
  "reconcile": "milestone", "max_records_per_task": 25, "context_ttl_seconds": 14400 }
```

Loaded by `knowledge/config.go:readKnowledgeConfig` (mirrors
`verification.go:225-237`), copied into worktrees via `copyTrustedConfig`
(`worktree.go:239`). **Default when absent: disabled.**

**Disabled ⇒ guaranteed no effect:**
1. `registerKnowledgeTools()` registers nothing → `tools/list` omits `knowledge_*`.
2. Every knowledge handler starts `if !enabled || ctx == nil { return error }` (fail-closed).
3. No capture instructions appended to Charter/Handoff/Skill.
4. No reconciliation model turn scheduled.
5. Deterministic capture hooks return immediately (checked once at store open).
6. No SQLite file is created when disabled.

### 4.5 Interface with the A4C runtime

```
TeamCoordinator.Step
  └─ (existing) Store.Complete / evidence creation / WriteDurableTestLog
        └─ knowledge.Capture.Observation(...)   [deterministic, no LLM, evidence/experiment only]

AgentConfig callbacks (existing)
  └─ OnKnowledgeRecord(...)   [new, agent-authored; wired like OnProposeMemory]

Host session creation (coordinator)
  └─ issue ExecContext token → Copilot MCP server env / OpenCode+Antigravity session prompt

MCP server (a4c mcp --context-file …)
  └─ validate token → knowledge_record/query/link/assess/revise/status
        └─ KnowledgeService → SQLite store

Milestone boundary (task completion / reviewer approval)
  └─ knowledge.Reconcile(...)  [bounded, ≤1 optional model turn]
```

---

## 5. Knowledge schema, support model, and lifecycle

### 5.1 One typed record; orthogonal fields (corrected)

The first draft's single `Epistemic` enum conflated *what a record is* with
*how well it is supported*, and `Status` overlapped with it (`proposed` vs
`hypothesis`). This revision makes the fields orthogonal:

- **`Kind`** — the record's *role* in an investigation (hypothesis, experiment,
  evidence, finding, decision, alternative, lesson, question). Unchanged set.
- **`Support`** — the *evidential state of the claim*: `untested | observed |
  supported | contradicted | inconclusive`. Replaces `Epistemic`.
- **`Integrity`** — whether referenced evidence is authentic and available:
  `pending | verified | broken`. Set by A4C, not the model.
- **`Status`** — lifecycle only: `draft | published | retracted`. No longer
  encodes evidential state.
- **`Scope`** — under which configurations/workloads/assumptions/host the claim
  applies. Required for `support: supported`.
- **`Confidence`** — `high | medium | low | unknown`.

```go
type Kind string     // hypothesis|experiment|evidence|finding|decision|alternative|lesson|question
type Support string  // untested|observed|supported|contradicted|inconclusive
type Integrity string// pending|verified|broken
type Status string   // draft|published|retracted

type Scope struct {
  Configurations []string `json:"configs,omitempty"`
  Workloads      []string `json:"workloads,omitempty"`
  Assumptions    []string `json:"assumptions,omitempty"`
  Host           string   `json:"host,omitempty"`
  Limitations    []string `json:"limitations,omitempty"`
}

type EvidenceRef struct {
  Kind      string `json:"kind"`      // validation_evidence|log|diff|commit|url|external
  ArtifactID string `json:"artifact_id,omitempty"`
  Hash      string `json:"hash,omitempty"`
  Locator   string `json:"locator,omitempty"`
  Integrity Integrity `json:"integrity"`   // set by A4C
  Validity  string   `json:"validity,omitempty"` // unknown|agent_asserted|reviewer_confirmed|human_confirmed
}

type Link struct { To, Rel, Note string }   // supports|contradicts|qualifies|limits|supersedes|derived_from|corroborates|answers

type KnowledgeRecord struct {
  ID, ProjectID, InvestigationID, TaskID, ActivityID string
  Kind Kind; Statement, Rationale string
  Support Support; Status Status; Integrity Integrity
  Confidence string; Scope Scope
  Author, Host string; Revision int
  Supersedes, SupersededBy string
  Evidence []EvidenceRef; Links []Link
  ContentHash string   // sha256(statement|rationale) — INTEGRITY ONLY, never dedup
  CreatedAt, UpdatedAt time.Time
}
```

### 5.2 The five verification properties (corrected)

The first draft treated "resolvable evidence + a verifier" as sufficient to mark
a claim `verified`. That conflates five distinct questions:

| Property | Question | Who establishes it | Mechanism |
|---|---|---|---|
| **Evidence integrity** | Is the artifact authentic and available? | **A4C (deterministic)** | hash re-computed on read; `Available`; `Integrity` |
| **Evidence validity** | Was the experiment conducted appropriately? | agent asserts; reviewer/human may confirm | `EvidenceRef.Validity` |
| **Claim support** | Does the evidence actually support *this* claim? | **agent/human, evidence-cited** | `knowledge_assess` → `Support` |
| **Claim scope** | Under what conditions does it hold? | agent, required for `supported` | `Scope` |
| **Confidence** | How strongly supported? | agent | `Confidence` |

**Claim-specific verification.** A benchmark showing +12% throughput on one
machine does **not** make "the architecture is faster" `supported`. It makes
*"on {config C, workload W, host H} the change improved throughput by 12%"*
`supported` with `Scope{configs:[C],workloads:[W],host:H}` and `Limitations`
naming what was not measured. A *broader* claim stays `untested` or is
`inconclusive` until scoped evidence exists.

**Promotion is no longer automatic.** The first draft promoted a record to
`verified` when native tests passed or a reviewer approved. That is wrong: a
passing unit test does not verify a performance conclusion, and a reviewer
approving an implementation does not verify an architectural hypothesis.
Corrected rules:

- `knowledge_record` **cannot** set `support: supported` at write time. A new
  claim is `untested` (hypothesis/question/decision/alternative) or `observed`
  (evidence/experiment, set by A4C's deterministic capture).
- `support` becomes `supported | contradicted | inconclusive` **only** via
  `knowledge_assess`, which requires: ≥1 cited `EvidenceRef` whose `Integrity`
  resolves to `verified`, an explicit `Scope`, and a `reason`. `knowledge_assess`
  is agent-callable; the assessment itself is recorded as an evidence-cited
  sub-record so the reasoning is auditable.
- A reviewer approving code sets `EvidenceRef.Validity = "reviewer_confirmed"` on
  the *evidence* record — it does **not** set `Support` on any claim.
- `Integrity: verified` requires A4C to recompute the hash and find the artifact.
  A model cannot assert integrity.

**Rule:** `knowledge_record` with `support:"supported"` is rejected at write time
(`unsupported_at_write`). The store never silently upgrades agent claims.

### 5.3 Contradiction and supersession (corrected)

The first draft auto-superseded a contradicted record. That is wrong: two
condition-scoped findings can both be true.

```
parallel_improves_large  support:supported  scope{workloads:["1M+ rows"]}
parallel_overhead_small  support:supported  scope{workloads:["<4KB"]}
   └─ link: qualifies (note: "benefit reverses below 4KB working set")
```

- **`contradicts`** is recorded but does **not** change `Status`. Both records
  stay `published`. A contradiction is a fact about the world, not a command to
  delete prior understanding.
- **`qualifies`** narrows a claim's applicability (adds `Scope` via
  `knowledge_revise`, preserving the original revision).
- **`limits`** records a known boundary.
- **`supersedes`** is set **only** explicitly via `knowledge_revise
  {supersede_by, reason}` — a deliberate revision, never an automatic reaction
  to a different experimental result.
- The contradicted record is never rewritten; history is append-only.

### 5.4 Idempotency, integrity, and duplicates (corrected)

The first draft deduplicated on content hash at write time. That destroys
legitimate independent corroboration (two experiments, same conclusion) and
collides on the same statement under different scopes.

Corrected separation:

| Concern | Mechanism |
|---|---|
| **Retry idempotency** | `UNIQUE(activity_id, client_key)` in the `idempotency` table. A retry with the same key returns the existing record. This is the *only* write-time dedup. |
| **Content integrity** | `content_hash` = sha256(statement‖rationale). Recomputed on read; mismatch ⇒ `Integrity: broken` + warning. Detects corruption, not duplication. |
| **Semantic similarity** | **Analytical only** (`knowledge_analyze --similar`). Never a write-time assumption; never merges records. |
| **Independent corroboration** | Two records with identical statements are both kept; linked `rel:"corroborates"`. Corroboration raises `Confidence` via an explicit assessment, not by merging. |
| **Contradiction** | `rel:"contradicts"`; both records `published`. |
| **Explicit supersession** | `knowledge_revise {supersede_by}` only. |

### 5.5 Lifecycle

```
draft ──(agent publishes)──► published ──(explicit revise-supersede)──► published(superseded link)
  │                              │
  └──(retract, audited)──────────┴──► retracted
```

`Support` evolves independently: `untested → observed → supported|contradicted|inconclusive`
via `knowledge_assess`. Negative findings (`support: contradicted`, or
`KindLesson` derived from a failed experiment) are ordinary records.

---

## 6. MCP tool catalog

**Implemented in this slice:** `knowledge_record` (§6.1) and `knowledge_query`
(§6.2). **Designed but deferred:** `knowledge_link` (§6.3), `knowledge_assess`
(§6.4), `knowledge_revise` (§6.5), `knowledge_status` (§6.6 — CLI `status`
exists; the MCP tool is deferred). Contracts below are corrected and internally
consistent; they are not yet registered.

All tools, when registered, are gated by enablement **and** a valid execution
context. All pass `AuthorizeToolInvocation`; all consume tool-call budget
(`knowledge_query` classified exploration, others work). No `--task` argument is
required from the model — identity comes from the validated token (§4.3). A
model *may* pass `investigation_id` to scope a query; it cannot forge
`project_id`/`task_id`/`activity_id`.

### 6.1 `knowledge_record` — IMPLEMENTED

Record one typed item. `support`, `scope`, `confidence`, and `limitations` are
**not** input fields — support is established only by `knowledge_assess`, and a
new record starts `untested` (or `observed` for evidence/experiment).
**Input:**
```json
{"kind":"finding","statement":"…","rationale":"…",
 "evidence":[{"kind":"experiment","artifact_id":"E-024","hash":"sha256:…"}],
 "investigation_id":"I-001","client_key":"turn-7-a1b2","publish":false}
```
**Output:** `{record:{id,kind,support,integrity,activity_id,author,host,content_hash,…},
reused:bool, conflict:bool, warnings:[]}`.
**Validation:** kind enum; `support` never accepted from input; evidence with a
`hash` is marked `integrity:"verified"`, others `pending`; `client_key` scopes
retry idempotency. **Errors:** `disabled`, `no_valid_context` (incl. forged/
expired/completed-activity token), `invalid_kind`, `context_expired`,
`budget_exhausted`.

### 6.2 `knowledge_query` — IMPLEMENTED

**Input:** `{investigation_id?, kinds?, supports?, status?, text?, rel?,
include_evidence?, limit≤50, offset}`. **Output:**
`{count,total,truncated,records:[{id,kind,statement,support,integrity,confidence,author,host,evidence_count,updated_at}]}`.
Bounded (default 10, cap 50); full evidence/rationale only with
`include_evidence`. Answers: "what was investigated" (kinds), "which
alternatives were rejected and why" (kind=alternative,status),
"which findings contradict this hypothesis" (rel=contradicts),
"what experiments support this decision" (rel=supports),
"what remains unanswered" (kind=question,support=untested),
"what did the previous investigation establish" (investigation_id).

### 6.3 `knowledge_link` — DEFERRED

`{from,to,rel,note}` → `{from,to,rel,created}`. Both endpoints must exist and
share `ProjectID`; `rel` from the enum; idempotent (existing edge ⇒
`created:false`).

### 6.4 `knowledge_assess` — DEFERRED (single authoritative support-mutation path)

Evaluate claim support against cited evidence. **Input:**
`{id, support:"supported|contradicted|inconclusive", evidence_ids:[…], scope:{…},
reason, client_key}`. **Output:** `{id, support, integrity, assessment_id}`.
**Validation:** every cited evidence must resolve with `integrity:"verified"`;
`scope` required for `supported`; the assessment is stored as an evidence-cited
sub-record (auditable). This is where claim-specific support is established —
never by a passing test or a code review alone.

### 6.5 `knowledge_revise` — DEFERRED

`{id, statement?, rationale?, scope?, limitations?, supersede_by?, reason}` →
`{id, revision, superseded_by}`. Append-only; original preserved;
`content_hash` updated; `supersede_by` is the **only** way to set `supersedes`.
**`support` is NOT a field of revise** — claim-support transitions are owned
exclusively by `knowledge_assess` (§6.4), so revise cannot bypass the
assessment/verification requirements.

### 6.6 `knowledge_status` — DEFERRED (CLI `a4c knowledge status` implemented)

`{investigation_id?}` → `{enabled, project, store_path, counts:{by_kind,by_support,by_status}, integrity_ok, context_valid, export_path}`. Read-only.

### 6.7 Tool-surface rationale

One recording tool (schema discriminates by `kind`) keeps the surface small and
token-cheap. `knowledge_assess` is separate from `knowledge_record` because
establishing support is a distinct, evidence-gated act. No `knowledge_analyze`
tool by default — analysis is CLI/optional to avoid per-turn overhead.

---

## 7. Cross-host MCP integration

### 7.1 Common design

Knowledge tools live on the **existing A4C MCP server**. Every host consumes
`a4c mcp` as an MCP client. The server is launched with a workspace `--dir` and
resolves its execution context from `A4C_KNOWLEDGE_TOKEN` or
`--context-file .a4c/contexts/current.json` (§4.3). The coordinator updates
`current.json` under the workspace lock so it always names the active activity.

**Enablement + context gate:** `registerKnowledgeTools()` is a no-op when
disabled; every handler additionally requires a valid, unexpired token.

### 7.2 GitHub Copilot CLI

- **Registration (verified path):** set `SessionConfig.MCPServers["a4c"] =
  MCPStdioServerConfig{Command:"a4c", Args:["mcp"], Env:{"A4C_KNOWLEDGE_TOKEN": token}}`
  in `copilot_adapter.go` at session build (`755-766`). The SDK copies this into
  the session request (`client.go:960`). This gives **per-session, per-activity**
  context — the strongest of the three hosts. Workspace `.mcp.json`
  (`roster_export.go:93-134`) remains for human/interactive use.
- **Tool discovery:** agent frontmatter (`roster_export.go:20-24`) already
  whitelists `a4c/*`; add the six `a4c/knowledge_*` names.
- **Instructions:** capture contract appended to `Charter` → `SystemMessage`
  (`copilot_adapter.go:763-765`) only when enabled.
- **Governance:** tools added to `AllowedTools` + capability profile;
  `AuthorizeToolInvocation` gates them; `ClassifyTool` counts them (query =
  exploration, rest = work).
- **Interruption/continuation:** durable SQLite store; resumed session gets a
  fresh token; `client_key` prevents duplicate records across retries.
- **Diagram:**
```
Copilot SDK session ──(MCPServers["a4c"].Env = TOKEN)──► a4c mcp
        │                       │ validate HMAC + claimed-activity + role
        ▼                       ▼
   knowledge_* tool calls ──► KnowledgeService ──► SQLite (.a4c/knowledge.db)
        ▲
A4C coordinator ──(deterministic evidence capture; issues TOKEN)──┘
```

### 7.3 OpenCode

- **Registration (verified):** `opencode.json` `mcp.a4c` (`roster_export.go:248-265`)
  with `"command":["a4c","mcp","--dir","<main-workspace>"]`. The MCP server is
  workspace-scoped (launched by `opencode serve`), so per-session env is not
  reliable.
- **Activity association (implemented, model-mediated):** the coordinator injects
  the token into the session prompt (`opencode_adapter.go`); the model relays it
  as the `context_token` argument. The HMAC authenticates it and the
  `StatusClaimed` check ties it to a live activity. Weaker than Copilot's env
  binding (the model is a conduit) — documented, not claimed as cryptographic.
- **Permissions:** gate `a4c/knowledge_*` via OpenCode permission rulesets
  (arbitrary action names; `"a4c_team_accept": deny` precedent).
- **No text-directive fallback:** knowledge uses a real MCP tool.
- **Diagram:**
```
opencode serve ──(workspace mcp.a4c → a4c mcp --dir <main-ws>)──► a4c mcp
        │                                       │ HMAC + claimed-activity + role
   session turn ── knowledge_record(context_token=TOK) ──┴──► KnowledgeService ──► SQLite
        ▲
A4C coordinator ──(issues TOK → injected into session prompt)──┘
```

### 7.4 Antigravity

- **Registration (verified CLI):** `agy mcp add a4c -- a4c mcp --dir <main-workspace>`
  lands in `~/.gemini/config/mcp_config.json`.
- **Activity association (implemented, model-mediated):** token injected into the
  `agy -p` session prompt; model relays it as `context_token`. Whether agy
  persists an MCP server across `--conversation` subprocesses is unverified; the
  design does not depend on it (the SQLite store is shared on disk and
  `client_key` makes resubmission idempotent either way).
- **Instructions:** capture contract in the installed skill
  (`.gemini/antigravity-cli/skills/a4c/SKILL.md`) and the Handoff prompt.
- **Bridge if a build lacks MCP client support (deferred):** a governed tool
  `governed_record_knowledge` routed to the same `KnowledgeService`. Same schema,
  store, and governance; added only on demonstrated incompatibility.
- **Diagram:**
```
agy -p (subprocess per turn) ──(mcp_config: a4c mcp --dir <main-ws>)──► a4c mcp
        │                                            │ HMAC + claimed-activity + role
   turn ── knowledge_record(context_token=TOK) ──────┴──► KnowledgeService ──► SQLite
        ▲
A4C coordinator ──(issues TOK → injected into session prompt)──┘
[--conversation resume → same store; client_key idempotent]
```

### 7.5 Cross-host invariants

1. Same `KnowledgeRecord` schema and SQLite store on all hosts.
2. Project/investigation/task/activity identity from a validated token, not the model.
3. `EvidenceRef.Hash` is content-addressed and host-independent.
4. Records persist across process restarts and host switches (shared workspace DB).
5. Retry idempotency via `(activity_id, client_key)`.
6. `AuthorizeToolInvocation(role, tool)` gates every call; role from the token.
7. Disabled ⇒ tools absent + handlers reject + no DB created.
8. `knowledge_query` returns identical results regardless of querying host.
9. A host change does not create a new `InvestigationID` (workspace-scoped).
10. `Author`/`Host` and `Support`/`Integrity` keep agent rationale distinguishable
    from deterministic observation and from integrity facts.

### 7.6 Cross-host continuation example

```
[Project skiff, Investigation I-001, shared .a4c + knowledge.db]

Copilot (coder), token bound to task T-101/act A-1:
  knowledge_record {kind:"hypothesis", statement:"LRU reduces p99 read latency.",
                    investigation_id:"I-001", client_key:"c-1"}
  → k-skiff-hyp-0001  support:untested  host:copilot

OpenCode (coder), token bound to task T-102/act A-2 [same .a4c]:
  knowledge_query {investigation_id:"I-001", kinds:["hypothesis"]}
  → k-skiff-hyp-0001 (integrity ok, provenance intact)
  [A4C deterministic capture after the benchmark activity completes:]
    → k-skiff-evi-0002  kind:evidence support:observed integrity:verified
       artifact_id:evi-abc hash:sha256:…  (the ValidationEvidence log)
  knowledge_record {kind:"finding", statement:"LRU improved p99 by 12% at 1M keys.",
                    evidence:[{artifact_id:"evi-abc"}],
                    scope:{workloads:["1M keys"],host:"m2-max"},
                    client_key:"c-2"}
  → k-skiff-find-0003  support:untested
  knowledge_assess {id:k-skiff-find-0003, support:"supported",
                    evidence_ids:["evi-abc"], scope:{…}, reason:"…"}
  → find-0003 support:supported integrity:verified (claim-specific, scoped)
  knowledge_link {from:find-0003,to:hyp-0001,rel:"supports"}

Antigravity (coder), token bound to task T-103/act A-3 [same .a4c]:
  [A4C capture:] → k-skiff-evi-0004 kind:evidence support:observed
       artifact_id:evi-x hash:sha256:…  (small-working-set benchmark)
  knowledge_record {kind:"finding",
    statement:"Under 4KB working set, LRU adds 8% overhead.",
    evidence:[{artifact_id:"evi-x"}], scope:{workloads:["<4KB"]},
    client_key:"c-3"}
  → k-skiff-find-0005
  knowledge_assess {id:find-0005, support:"supported", evidence_ids:["evi-x"], scope:{…}}
  knowledge_link {from:find-0005,to:find-0003,rel:"qualifies",
    note:"Benefit reverses below 4KB; both findings hold under their scopes."}

Result: hyp-0001 supports find-0003 (scoped 1M keys); find-0005 qualifies
find-0003 (scoped <4KB). Both findings remain published. No record rewritten.
```

---

## 8. Capture while work happens

The central product requirement is unchanged: capture significant engineering
understanding **as work happens**, not from a transcript after the project ends.
Three complementary mechanisms, with an honest account of what each guarantees.

### 8.1 Deterministic observation (always, no LLM)

**Records only `KindEvidence` and `KindExperiment`.** A4C knows an operational
event happened; it does **not** know what that event *means* for the
investigation. The first draft auto-mapped failed tests → findings, review
rejections → contradicted conclusions, budget reallocations → lessons, and human
review transitions → decisions. Those mappings are invalid and are removed.

| A4C event | Location | Record created |
|---|---|---|
| Native/project tests run | `team_coordinator.go` specialist branch | `KindEvidence{kind:validation_evidence, artifact_id:evi-ID, hash, locator:.a4c/evidence/…}` `support:observed`, `integrity` computed. **Skipped if `Vacuous`** (`types.go:307`). |
| Diff produced | `GetTaskDiff` `service.go:299` | `KindEvidence{kind:diff, hash}` if non-empty and not flagged by `FlagDiffHygiene` |
| Experiment command executed (configured verify) | `verification.go` | `KindExperiment{command, exit_code, source_revision, log_hash}` `support:observed` |
| Review verdict recorded | `OnSubmitReview` | **No knowledge record.** Sets `EvidenceRef.Validity="reviewer_confirmed"` on the referenced evidence only. |

Raw execution events are observations, never conclusions. A failed test is
captured as evidence (exit code + log hash); an agent may then record a
`KindFinding`/`KindLesson` referencing it. **Negative outcomes are preserved
without the model manufacturing a lesson** — the evidence record stands alone.

**Filtering:** capture only when enabled and only for non-vacuous, non-empty,
hygiene-clean artifacts. Records hold `EvidenceRef` pointers (id + hash +
locator), never copies of source or logs.

### 8.2 Agent-authored capture (host instruction contract)

When enabled, A4C appends **one concise block** to the role `Charter` (Copilot
`SystemMessage`, OpenCode `system`) and the Handoff `Instructions:` (Antigravity
skill). Identical text on all hosts; no divergent agent definitions.

```
KNOWLEDGE CAPTURE (enabled): Record significant engineering understanding with
the knowledge_record tool when you (a) form a consequential hypothesis,
(b) choose among important alternatives, (c) discover unexpected behavior,
(d) invalidate an assumption, (e) complete a meaningful experiment,
(f) identify a generalizable limitation, (g) change an architectural decision,
or (h) reach a significant positive/negative conclusion. Reference automatically
captured evidence by artifact_id. Do NOT record routine implementation steps.
Use knowledge_query before investigating to avoid duplicating prior work.
Support is never set at record time: after recording a finding, cite its
evidence and scope with knowledge_assess. Use a unique client_key per
submission so retries are idempotent.
```

Injected **only when enabled**. The **tool** enforces schema, scope, and the
`unsupported_at_write` rule — the prompt is guidance, not enforcement.

### 8.3 Bounded milestone reconciliation

At task completion and reviewer approval, `knowledge/reconcile.go`:

1. Load existing records for the task.
2. Collect deterministic observations already captured (§8.1).
3. Detect **significant uncaptured** items: e.g. a non-vacuous evidence record
   with no `KindFinding` referencing it; a rejected review with no
   `KindFinding`/`KindLesson` referencing the evidence.
4. **Bounded:** at most **one** optional model turn, only if step 3 finds
   candidates **and** budget headroom remains. The turn asks the agent to
   *articulate* a finding A4C already detected (by artifact_id), not open-ended analysis.
5. **"No significant new knowledge"** is a valid, zero-cost outcome.
6. Never blocks coding completion; reconciliation failure is logged, not fatal.

### 8.4 What happens when the agent ignores the instruction

Prompt instructions do not guarantee capture. The guarantees are:

- **Deterministic capture always runs** — evidence and experiments are recorded
  whether or not the agent says anything.
- **Reconciliation detects the omission** at the milestone and may spend one turn.
- **If the agent still records nothing,** the evidence records remain: a future
  investigation can `knowledge_query` them and record findings itself.
- Agent-authored capture is **best-effort by design**; it is not a hard
  requirement and no second continuously running agent is introduced.

### 8.5 Crash, retry, and reconnection

- SQLite WAL: an uncommitted transaction rolls back; a committed one is durable.
- `client_key` + `UNIQUE(activity_id, client_key)`: a retried
  `knowledge_record` after a crash or reconnect returns the existing record.
- Resumed activities (same host or cross-host) receive a fresh context token;
  the store is shared, so prior records are visible via `knowledge_query`.

---

## 9. Capture vs analysis separation

The schema encodes observation → interpretation → hypothesis → experiment →
finding as **linked records with distinct `Kind` and `Support`**, never
interchangeable:

| Concept | Kind | Support at creation | Evidence for `supported` |
|---|---|---|---|
| Observation | evidence / experiment | `observed` (A4C-set) | itself |
| Interpretation | finding | `untested` | via `knowledge_assess` |
| Hypothesis | hypothesis | `untested` | n/a (stays until assessed) |
| Experiment | experiment | `observed` | its result |
| Finding (supported) | finding | `supported` (assessed) | scoped evidence + reason |
| Finding (contradicted) | finding | `contradicted` (assessed) | contradicting evidence |

`Link.Rel` makes reasoning explicit without collapsing levels.

### Analytical capabilities (optional, explicit)

`a4c knowledge analyze` (CLI) and a future optional `knowledge_analyze` tool
(not registered by default):

**Deterministic (no LLM):** correlate evidence across activities (join on
`artifact_id`); detect `contradicts` edges and same-subject support conflicts
under **overlapping scopes** (a `qualifies` edge means the scopes differ and
both may hold); identify `untested` hypotheses with no `supports` edge; group
`alternative`s by subject; count experiments by outcome; find `question`s with
`support:untested`; list `lesson`s; compute evidence-coverage %.

**Model-assisted (optional, explicit):** synthesize a cross-activity narrative
**with source record IDs and uncertainty markers**. Never invents chain-of-thought
or fabricates rationale; only references existing `Rationale` fields.

---

## 10. Project scope and investigation lineage

```
Project (workspace .a4c + knowledge.db)
  └── Investigation (I-001, I-002, …)   ← workspace-scoped, not host-scoped
        ├── Hypothesis / Experiment / Evidence / Finding / Decision / Alternative / Lesson / Question
        └── A4C Tasks (task-…)           ← via TaskID/ActivityID on records
```

### Relaunch scenario (Skiff 1 → Skiff 2)

```
I-001 (discontinued):
  hyp-0001 "SQL Server wire compat feasible"        support:untested
  exp-0010 "Prototyped TDS parser"                  support:observed
  find-0011 "TDS numeric types lossy-mappable"      support:contradicted
     scope{formats:["TDS 7.4"]}; evidence: exp-0010
  dec-0012 "Discontinue I-001"                      support:untested
     links: derived_from find-0011
  lesson-0013 "Feasibility-gate format-compat targets first"  support:supported
     scope{domain:["wire-format engines"]}; evidence: find-0011

I-002 (revised objective):
  knowledge_query {investigation_id:"I-001", kinds:["finding","lesson"]}
  → find-0011 (contradicted, scoped), lesson-0013 (supported)
  knowledge_record {kind:"hypothesis",
    statement:"Columnar storage avoids the TDS mapping problem.",
    investigation_id:"I-002",
    links:[{to:"find-0011", rel:"derived_from",
            note:"Motivated by the TDS infeasibility finding."}]}
  → hyp-0201 support:untested (I-002 does NOT inherit hyp-0001 as fact)
```

- I-002 retrieves proven findings and lessons; `hyp-0001` remains
  `support:untested` in I-001 — never promoted.
- Negative results (`find-0011` `contradicted`) are independently retrievable.
- Lineage is explicit via `derived_from`; no history rewritten.

---

## 11. Persistence, durability, and evidence integrity

- **Storage:** SQLite WAL (§4.2). DB at `.a4c/knowledge.db` (+ `-wal`, `-shm`),
  registered in `getPaths`/`runInit`/`buildService`. Not created when disabled.
- **Durability:** `PRAGMA synchronous=NORMAL` under WAL; `BEGIN IMMEDIATE` for
  writes; `PRAGMA foreign_keys=ON`; `busy_timeout=5000`.
- **Idempotency:** `UNIQUE(activity_id, client_key)` (§5.4).
- **Append-only revisions:** `revisions` table; `knowledge_revise` inserts a new
  row; originals never mutated.
- **Schema evolution:** `PRAGMA user_version`; forward migrations; readers select
  known columns only.
- **Evidence integrity:** `EvidenceRef.Hash` is sha256 of artifact bytes.
  `knowledge_query --include-evidence` re-hashes; mismatch ⇒ `Integrity: broken`
  + warning (never silently trusts, never drops the claim).
- **Ephemeral artifacts:** evidence that exists only in a disposable worktree or
  temp benchmark dir is **copied into `.a4c/evidence/` at capture time** (by the
  coordinator, which already does this for `WriteDurableTestLog`), hash-verified,
  so the reference survives worktree removal.
- **Deleted/moved artifacts:** `Integrity: broken`, record retained. A finding is
  **not** auto-retracted; it is flagged for review. `content_hash` detects
  statement corruption independently of evidence.
- **Audit:** hash-chained JSONL (`task_integrity.go:41-143`) over knowledge
  mutations (create/assess/revise/retract) for tamper-evidence.
- **Portability:** existing JSON stores for A4C operational state are unchanged.

### 11.1 Portable export contract

`.a4c/knowledge/export.json` (or JSONL):

```json
{ "format":"a4c-knowledge/v1", "exported_at":"…", "project":"skiff",
  "investigations":["I-001","I-002"],
  "records":[ KnowledgeRecord… ],
  "evidence_manifest":[{artifact_id,hash,locator,embedded:false,bytes}],
  "links":[{from,to,rel,note}] }
```

Consumable without running A4C. Large artifacts referenced by hash
(`embedded:false`); `--embed-evidence` inlines small logs. **Not** a publication
format (no Markdown/PDF/LaTeX generation).

---

## 12. Provenance and evidence validation

1. **Author vs integrity vs support:** `Author` (agent or `"deterministic"`)
   never implies truth. `Integrity` is A4C-computed. `Support` is agent/human,
   evidence-cited, scope-required.
2. **Evidence resolution:** `knowledge_record`/`knowledge_query` resolve
   `EvidenceRef` against real artifacts (`.a4c/evidence`, git objects,
   `ValidationEvidence` IDs). Unresolvable ⇒ `Integrity: broken` + warning.
3. **No automatic "verified":** `support:"supported"` requires
   `knowledge_assess` with ≥1 `Integrity: verified` evidence, an explicit
   `Scope`, and a `reason`. Vacuous `ValidationEvidence` (`Vacuous:true`) can
   never be cited to support a claim.
4. **Reviewer approval ≠ claim verification:** approval sets
   `EvidenceRef.Validity="reviewer_confirmed"` on the evidence record only.
5. **Contradiction ≠ invalidation:** `rel:"contradicts"`; both records
   `published`; supersession only via explicit `knowledge_revise`.
6. **Forged references:** a model cannot forge `EvidenceRef.Hash` (content-addressed);
   a wrong hash ⇒ `Integrity: broken`. A model cannot set `Integrity` or
   `Verifier`.
7. **Cross-host identity:** project/investigation/task/activity come from the
   validated token; a model cannot invent scope (§4.3).

---

## 13. Governance and security

- **Project isolation:** token binds `ProjectID`; store opened with `--dir`;
  cross-project queries fail (token project ≠ store project; record absent).
- **Tool authorization:** every `knowledge_*` tool passes
  `AuthorizeToolInvocation(role, tool)`; role from the validated token; tool
  names added to the three global allowlists, roster `AllowedTools`, and
  capability profiles; `ClassifyTool` accounts them.
- **Agent identity & provenance:** `Author`/`Host`/`TaskID`/`ActivityID` from the
  token, not model input.
- **Cross-host visibility:** all hosts see the same workspace DB; no host has
  privileged write. Antigravity's `--dangerously-skip-permissions` is compensated
  by A4C-side validation (records are validated server-side regardless of host
  permission posture).
- **Confidentiality:** `.a4c/` is git-excluded (`worktree.go:131-160`); records
  contain statements/rationale, not source blobs (evidence by hash/locator).
  Exports are explicit and local.
- **Sensitive data:** the capture contract forbids secrets; a lightweight
  redaction check rejects records matching secret patterns at write time.
- **Export controls:** export is a manual CLI action; no automatic transmission.
  Providers receive only the tool results the agent requested.
- **Prompt injection via retrieved knowledge:** retrieved text is **data**, wrapped
  in a delimited envelope in the Handoff prompt, never executed as instructions.
  Query results are length-capped. A malicious prior record cannot inject tool
  calls.
- **Deletion/redaction:** `knowledge_revise {retract}` with an audit entry;
  physical purge is a manual audited CLI (`a4c knowledge purge --id --yes`).
- **Human-only boundaries:** a human sign-off is a distinct
  `EvidenceRef.Validity="human_confirmed"`; knowledge cannot self-approve into
  `supported` without cited evidence.

---

## 14. Performance and token economics

- **Disabled:** zero overhead — no DB file, no tools, no instructions, no turns.
- **Small schemas:** `knowledge_record` input ≤ ~1 KB; output ≤ ~200 B.
- **Compact results:** `knowledge_query` default 10 × ~150 B; `include_evidence`
  defers heavy loading.
- **Deferred evidence:** full logs loaded only on explicit expansion.
- **Bounded retrieval:** `limit` ≤ 50; `truncated` flag.
- **No per-turn analysis:** deterministic capture is free; reconciliation ≤1 turn
  at milestones only.
- **Model calls:** only agent-authored `knowledge_record`/`knowledge_assess`
  (agent-initiated) and optional reconciliation (≤1 turn). `query`/`link`/`revise`/
  `status` are deterministic.

**Metrics (instrumented, not asserted):** records/investigation; evidence
coverage (% with ≥1 verified evidence); duplicate rate (idempotent hits /
submissions); unsupported-conclusion rate (`unsupported_at_write` rejections);
retrieval relevance (% queried results later linked); cross-investigation reuse
(`derived_from` to prior investigation); reconciliation turns/task; knowledge
token overhead; task success rate with vs without (A/B); post-failure
recovery (prior-investigation records retrieved by I-002).

---

## 15. UX and operational surface

### 15.1 Host-native (primary)

> "Record this as a hypothesis." → `knowledge_record`
> "What did our earlier investigation establish?" → `knowledge_query {investigation_id:"I-001"}`
> "Why did we reject this architecture?" → `knowledge_query {kinds:["decision","alternative"], text:"architecture"}`
> "Show the evidence behind that conclusion." → `knowledge_query {include_evidence:true, …}`
> "What should we reconsider before restarting Skiff?" → `knowledge_query {kinds:["lesson","question","finding"]}`

### 15.2 CLI (noun-first, matching `main.go`)

```
a4c knowledge status [--json]
a4c knowledge enable | disable
a4c knowledge list [--investigation I] [--kind K] [--json]
a4c knowledge query <text> [--kind K] [--limit N] [--json]
a4c knowledge show <id> [--evidence]
a4c knowledge assess <id> --support S --evidence ID[,ID] --scope … --reason …
a4c knowledge investigations
a4c knowledge export [--out path] [--embed-evidence]
a4c knowledge import <path>
a4c knowledge start --project P --investigation "title"
a4c knowledge analyze [--contradictions] [--gaps] [--similar]
a4c knowledge purge <id> --yes        # audited hard delete
```

`--json` for machine output. No new graphical UI.

---

## 16. End-to-end scenarios

> Hypothetical and illustrative. No actual Skiff measurements are fabricated.

### 16.0 Sequence diagrams

**D1 — Knowledge record from a managed A4C activity (Copilot):**
```
Coordinator ──CreateSession──► Copilot SDK
   │  issues ExecContext token (HMAC, TTL 4h) ──► SessionConfig.MCPServers["a4c"].Env
   ▼
Copilot session ──agent turn──► model calls knowledge_record {kind, statement, client_key}
   ▼
a4c mcp (stdio, env TOKEN) ──validate HMAC + expiry + activity StatusClaimed + role──► OK
   ──AuthorizeToolInvocation(role,"knowledge_record")──► PolicyGovernor ──► allow
   ──BEGIN IMMEDIATE: INSERT records/evidence/links/revisions/idempotency──► COMMIT
   ◄── {id, revision, support:untested, integrity, evidence:[{integrity}]}
Coordinator ──Store.Complete──► deterministic capture also writes KindEvidence for the run
```

**D2 — Concurrent submissions from separate MCP processes:**
```
proc-A (a4c mcp, token→T-101)  ┐
proc-B (a4c mcp, token→T-102)  ├─► same .a4c/knowledge.db (WAL)
proc-C (coordinator capture)   ┘
   A: BEGIN IMMEDIATE … COMMIT  (holds write lock; B blocks ≤busy_timeout)
   B: BEGIN IMMEDIATE … COMMIT  (serializes; no lost update; UNIQUE(activity_id,client_key) enforced)
   C: INSERT evidence …         (append; no index RMW race)
   crash mid-A: WAL replay discards uncommitted txn; B/C unaffected
```

**D3 — Finding linked to experimental evidence:**
```
[A4C] benchmark activity completes → ValidationEvidence{evi-abc, exit0, tests12, not vacuous}
   → capture: record evi-abc (kind:evidence, support:observed, hash, locator)
agent: knowledge_record {kind:finding, evidence:[{artifact_id:"evi-abc"}], scope:{…}} → support:untested
agent: knowledge_assess {id, support:"supported", evidence_ids:["evi-abc"], scope, reason}
   → evidence resolved (hash recomputed → integrity:verified) → record.support=supported, integrity=verified
agent: knowledge_link {from:finding,to:hypothesis,rel:"supports"}
```

**D4 — Copilot → OpenCode → Antigravity investigation:** see §7.6.

**D5 — Crash recovery and idempotent resubmission:**
```
agent: knowledge_record {…, client_key:"turn-7"}
   a4c mcp: BEGIN IMMEDIATE; INSERT …; COMMIT  ── network/process dies BEFORE response
agent retries (same client_key) after restart / reconnect / host-switch
   a4c mcp: SELECT record_id FROM idempotency WHERE activity_id=? AND client_key=?
   → hit: return existing record (no duplicate INSERT)
   → miss (crash before COMMIT): WAL rolled back; INSERT proceeds once
```

### Scenario A — Positive experimental finding

```
1. Agent (OpenCode, task T-101, act A-1, valid token):
   knowledge_record {kind:"hypothesis", statement:"Batched writes raise throughput.",
                     investigation_id:"I-001", client_key:"k1"}
   → k-001 support:untested
2. [A4C deterministic] benchmark activity completes; ProjectTestRunner.Verify →
   ValidationEvidence{evi-abc, ExitCode:0, TestsExecuted:12, Vacuous:false, LogPath}
   → k-002 kind:evidence support:observed integrity:verified (hash computed)
   → k-003 kind:experiment support:observed (command, exit_code, source_revision)
3. Agent:
   knowledge_record {kind:"finding", statement:"Batching improved throughput ~3x on the recorded workload.",
                     evidence:[{artifact_id:"evi-abc"}],
                     scope:{workloads:["1M records"],host:"m2-max"}, client_key:"k2"}
   → k-004 support:untested
   knowledge_assess {id:k-004, support:"supported", evidence_ids:["evi-abc"],
                     scope:{workloads:["1M records"]}, reason:"Benchmark delta attributable to batching.", client_key:"k3"}
   → k-004 support:supported integrity:verified (scoped)
   knowledge_link {from:k-004,to:k-001,rel:"supports"}
```

### Scenario B — Negative finding

```
1. Agent records hypothesis H "In-memory index wins for all sizes." support:untested
2. Experiment E (deterministic capture) shows on-disk wins above 10M rows.
3. Agent records finding F {kind:finding, evidence:[E], scope:{workloads:[">10M rows"]}} support:untested
   knowledge_assess {id:F, support:"contradicted", evidence_ids:[E], scope:{…}, reason:"…"}
   → F support:contradicted
   knowledge_link {from:F,to:H,rel:"contradicts"}
4. Agent records decision D {kind:decision, statement:"Use on-disk index for large tables.",
   rationale:"F shows in-memory loses above 10M rows.", links:[D derived_from F]}
5. H is NOT rewritten; it stays support:untested with a contradicts edge.
   F is a first-class negative finding, retrievable independently.
```

### Scenario C — Cross-host continuity

See §7.6. Copilot hypothesis → OpenCode experiment+assessed finding →
Antigravity qualifying evidence, one investigation, history preserved, no
auto-supersession.

### Scenario D — Failed project and relaunch

See §10. Skiff I-001 discontinued; I-002 retrieves proven findings/lessons,
does not inherit obsolete hypotheses as fact, links `derived_from`.

---

## 17. Testing and acceptance criteria

### 17.1 Module isolation
- Disabled: `tools/list` has no `knowledge_*`; direct handler call returns
  `disabled`; no `.a4c/knowledge.db` created; Handoff prompt has no capture
  instructions; no reconciliation turn; existing A4C tests unchanged.

### 17.2 MCP + context
- Six tools registered when enabled; role gating from the **token**.
- Invalid/expired/forged token ⇒ `no_valid_context` / `context_expired` before
  any store access.
- Unauthorized role for an operation ⇒ `unauthorized` (governor).
- Idempotent submission (same `(activity_id, client_key)` ⇒ one record).
- Tool discovery on all three hosts (`copilot mcp list`, `opencode mcp list`,
  `agy mcp list` show the a4c server with knowledge tools).

### 17.3 Multi-process persistence
- Two concurrent `a4c mcp` processes + the coordinator writing simultaneously:
  no lost updates, no torn relationships, unique idempotency enforced.
- Crash mid-transaction ⇒ WAL rollback, no partial record; `quick_check` clean.
- Concurrent revisions increment without lost updates.

### 17.4 Knowledge correctness
- `support:"supported"` at write ⇒ `unsupported_at_write` rejection.
- `knowledge_assess` with unresolvable evidence ⇒ rejected; with vacuous
  `ValidationEvidence` ⇒ rejected.
- A broad claim cannot be `supported` without `Scope`.
- Reviewer approval sets `Validity` on evidence, never `Support` on a claim.
- `contradicts` does not change `Status`; both records `published`.
- `supersedes` set only via `knowledge_revise`.
- Independent identical statements are both kept + `corroborates` (not merged).
- Missing evidence ⇒ `Integrity: broken`, record retained.

### 17.5 Durability
- Process restart / host switch: records persist; query returns them.
- Interrupted write: no partial record.
- Schema v1 reader tolerates unknown fields.
- Export/import round-trip preserves records, links, hashes, scopes.

### 17.6 Agent behavior (live)
- Agent records knowledge at significant moments (not routine steps).
- Routine coding ≤ `max_records_per_task`.
- `knowledge_query` surfaces relevant prior findings in the Handoff prompt.
- Reconciliation detects an omission and, only then, spends one turn.
- Missing records never cause invented conclusions.

### 17.7 Performance
- Disabled: 0 DB writes, 0 extra tokens, 0 extra turns.
- `knowledge_record` response < 500 B; `knowledge_query` (limit 10) < 2 KB.
- Retrieval latency < 50 ms (local SQLite).
- Storage growth bounded by `max_records_per_task`.

### 17.8 Minimal live integration tests (one per host)
- **Copilot:** per-session `MCPServers` with token env; record + query in a real
  session; unauthorized tool rejected; stale token rejected.
- **OpenCode:** workspace MCP server + `current.json` token; record invoked by a
  real session; duplicate suppressed on reconnect; concurrent-activity token
  rotation under the workspace lock.
- **Antigravity:** `agy mcp add`; record with validated token; record survives a
  `--conversation` turn boundary; confirm MCP server lifecycle (persistent vs
  per-subprocess).

---

## 18. Implementation plan (5 phases)

**Phase 1 — Correct foundation.**
`model.go`, `store.go` (SQLite/WAL + migrations + idempotency + integrity),
`context.go` (token issue/validate), `config.go` (enablement),
`service.go`; `.a4c/knowledge.db` in `getPaths`/`runInit`/`buildService`;
MCP `knowledge_record` + `knowledge_query` + `knowledge_status` with context gate.
*Acceptance:* multi-process concurrent-write test green; forged/expired token
rejected; `support:"supported"` at write rejected; disabled ⇒ no DB, no tools.
*Depends on:* none.

**Phase 2 — Cross-host proof.**
Copilot per-session `MCPServers` with token env; OpenCode `--context-file` +
`current.json` rotation under the workspace lock; Antigravity `agy mcp add` +
`current.json`; roled launchers in `roster_export.go`; live record/query on all
three hosts; confirm agy MCP server lifecycle.
*Acceptance:* real Copilot, OpenCode, and Antigravity sessions each record and
query; token validation holds on all three; activity identity correct.
*Depends on:* 1.

**Phase 3 — Capture integration.**
Deterministic evidence/experiment capture (non-vacuous, hygiene-filtered) at
`Store.Complete`/`WriteDurableTestLog`/`GetTaskDiff`; host instruction injection
(Charter/Skill/Handoff, enabled-only); `OnKnowledgeRecord` callback; ephemeral
artifact copy + hash.
*Acceptance:* a real task produces evidence records without agent action; agent
records a finding citing an artifact_id; prompt injection envelope verified.
*Depends on:* 1.

**Phase 4 — Knowledge evolution.**
`knowledge_link`, `knowledge_assess`, `knowledge_revise`; `contradicts`/`qualifies`
semantics (no auto-supersede); investigation lineage; export/import;
audit hash-chain.
*Acceptance:* scoped supported/contradicted findings; both records published on
contradiction; supersession only via revise; export round-trip.
*Depends on:* 1.

**Phase 5 — Analysis and reconciliation.**
`analyze.go` (contradiction/gap/similarity over overlapping scopes);
`reconcile.go` (≤1 bounded turn); `a4c knowledge analyze` CLI; metrics.
*Acceptance:* reconciliation detects a planted omission and spends ≤1 turn;
"detects nothing" is zero-cost; contradiction detection respects `qualifies`.
*Depends on:* 3, 4.

No phase is built in this assignment.

---

## 19. Design tradeoffs and rejected alternatives

| Decision | Chosen | Rejected | Tradeoff |
|---|---|---|---|
| Storage | SQLite WAL (pure-Go) | JSON+mutex / JSON+flock / extend memory store | Correct multi-process semantics, transactional idempotency, crash recovery; one new dependency; not the existing store pattern. |
| Identity | HMAC execution-context token + governor | CLI `--task` args / model-supplied IDs / no binding | Works with workspace-scoped MCP; separates identify from authorize; stdio can't bind a single call to a session (documented). |
| Support model | `Support` + `Integrity` + `Scope` orthogonal | Single `Epistemic` enum + auto-"verified" | Claim-specific; no overclaiming; more fields. |
| "Verified" | removed as a promotion target | auto-promote on test/review | Review/test no longer verify claims; `knowledge_assess` does. |
| Deterministic capture | evidence/experiment only | auto-findings/lessons/decisions | No invalid inference; agents interpret. |
| Idempotency | `(activity_id, client_key)` unique | content-hash dedup | Preserves independent corroboration; requires agent key discipline. |
| Contradiction | `contradicts` + `qualifies`, no auto-supersede | auto-supersede | Both scoped findings survive; supersession explicit. |
| MCP surface | 6 tools, one record op + one assess op | per-kind tools / required analyze tool | Compact, token-cheap; analysis optional. |
| Enablement | `.a4c/knowledge.json` | feature-flag framework | Mirrors `.a4c/verify.json`; no new framework. |
| Antigravity transport | MCP client + `current.json` | governed-tool bridge only | Real MCP; bridge only on demonstrated incompatibility. |
| Cross-host scope | workspace-scoped investigation | host-scoped | Host change doesn't fork history. |
| Capture cost | deterministic + ≤1 milestone turn | per-turn narration | Meets G10; misses some mid-turn insights (agent can still record). |

---

## 20. Open questions requiring empirical verification

1. **Does knowledge capture improve later-task outcomes?** A/B on a real
   multi-investigation project (Skiff I-001→I-002). Metrics §14.
2. **Antigravity MCP server lifecycle:** does `agy -p` persist the MCP server
   across `--conversation` subprocess turns, or relaunch per subprocess? Live test.
3. **OpenCode token env propagation:** does `opencode serve` pass the workspace
   MCP `environment` through, and is `current.json` re-read per call (not cached
   at server start)? Live test.
4. **Optimal reconciliation trigger** and significance threshold for spending a turn.
5. **Agent compliance:** do real models invoke `knowledge_record`/`knowledge_assess`
   at consequential moments without excessive prompting? Measure records/task.
6. **Evidence-copy cost** for ephemeral worktrees; I/O impact.
7. **Cross-project sharing** (explicitly scoped) — deferred pending privacy review.
8. **Schema ceiling:** whether 8 `kind`s are necessary or some collapse in practice.
9. **`context_ttl_seconds` tuning** for long investigations vs token-leak exposure.

---

## 21. Verified facts vs proposals (traceability)

| Claim | Status |
|---|---|
| A4C MCP server, role gating, 12 tools, `a4c mcp` CLI | **Verified** (`mcp/server.go`, `main.go:1283`) |
| MCP identity is today model-supplied `task_id` args | **Verified** (`mcp/server.go:95-203`) |
| Kernel `flock` workspace lock; single-coordinator invariant | **Verified** (`workspace_lock.go:26-60`) |
| JSON stores + `sync.Mutex` + `.tmp`/rename | **Verified** (`store.go`, `memory.go`) |
| No SQLite dependency in `go.mod`; go1.25.7 | **Verified** |
| Copilot SDK `SessionConfig.MCPServers` + `MCPStdioServerConfig{Env,Args}` | **Verified** (`copilot-sdk/go@v1.0.17/types.go:1202-1211,1266`) |
| A4C does **not** currently set `MCPServers` | **Verified** (`copilot_adapter.go:755-766`) |
| OpenCode MCP config `environment`; workspace-scoped; `opencode mcp list` attempts `a4c mcp` | **Verified** (schema + CLI) |
| `agy mcp add <name> <cmd> [args…] --env`; per-subprocess `agy -p` | **Verified** (CLI + `antigravity_adapter.go:249-264`) |
| agy MCP server persistence across turns | **UNVERIFIED** — live test required |
| OpenCode token env propagation / `current.json` re-read | **UNVERIFIED** — live test required |
| `ValidationEvidence` Vacuous/LogPath/SourceRevision | **Verified** (`types.go:281-309`) |
| `SessionRef` activity→runtime+session | **Verified** (`session_ref.go:14-47`) |
| `.a4c/verify.json` trusted-config pattern | **Verified** (`verification.go:225-237`) |
| Memory propose→promote→deprecate | **Verified** (`memory.go:102-168`) |
| `ClassifyTool` terminal/exploration/work | **Verified** (`tool_governance.go:28-39`) |
| All proposed types, token, schema, capture rules, phases | **Proposed** (this document) |
| Skiff scenarios / measurements | **Hypothetical, illustrative** |

---

## 22. Central answer and summary

**How can A4C accumulate trustworthy engineering understanding during autonomous
work, preserve it across tasks and hosts, and make it reusable after an
unsuccessful project?**

By treating knowledge as a **typed, evidence-linked, scope-bounded,
A4C-validated durable record** — captured deterministically as observations and
authored by agents through a **single MCP tool surface** all three hosts already
consume; identified by a **short-lived HMAC execution-context token** that
separates identification from authorization; stored in a **multi-process-safe
SQLite WAL** database so coordinator and MCP-server processes never lose or
double-apply a record; validated so that **evidence integrity, experiment
validity, claim support, claim scope, and confidence are never conflated** and
"verified" is never an automatic promotion; scoped to a workspace investigation
rather than a host; and exported in an open format so it outlives A4C itself.

### Architectural corrections made

1. **Persistence** — JSON+mutex → SQLite WAL (pure-Go), because separate MCP
   server processes write concurrently; transactional idempotency and crash
   recovery are required. Existing JSON stores kept for A4C operational state.
2. **Execution identity** — CLI `--task` args / model-supplied IDs → HMAC-signed
   execution-context token validated server-side; identification separated from
   authorization; per-host delivery (Copilot SDK env verified; OpenCode/Antigravity
   `current.json`, live-test pending).
3. **Epistemic model** — single `Epistemic` enum + auto-"verified" → orthogonal
   `Support`/`Integrity`/`Status`/`Scope`/`Confidence`; `knowledge_assess` is the
   only path to `supported`; reviewer approval and passing tests no longer verify
   claims.
4. **Capture separation** — deterministic capture limited to evidence/experiment
   observations; invalid auto-inference (failed test→finding, review
   rejection→contradicted, budget→lesson, human review→decision) removed.
5. **Idempotency** — content-hash write-time dedup removed (destroys independent
   corroboration); retry idempotency via `UNIQUE(activity_id, client_key)`;
   semantic dedup moved to analysis.
6. **Contradictions** — no auto-supersede; added `qualifies` + `Scope`; both
   scoped findings remain `published`; supersession explicit only.
7. **Host MCP assumptions** — Copilot per-session `MCPServers` (verified SDK);
   OpenCode/Antigravity workspace-scoped with `current.json` token, flagged for
   live verification rather than claimed.

### Decisions retained

Optional module disabled by default; existing A4C MCP server as the integration
point; shared MCP surface for all three hosts; small tool catalog; structured
evidence-grounded records; project/investigation lineage; negative findings as
first-class; observation vs interpretation distinction; portable KB; separate
publication pipeline; no new orchestration engine; no A4C runtime redesign;
governance via `PolicyGovernor`; enablement via `.a4c/knowledge.json`.

### Remaining uncertainties requiring live verification

Antigravity MCP server persistence across `agy -p` turns; OpenCode token env
propagation and `current.json` re-read cadence; whether real models invoke the
capture/assess tools at consequential moments; reconciliation trigger tuning;
and whether knowledge capture measurably improves later-task outcomes (A/B).

### Ready for implementation?

**Yes — with two gates.** Phase 1 (foundation) and Phase 4 (evolution) are fully
specified and independently implementable now. Phase 2 (cross-host proof) is
specified but its OpenCode/Antigravity acceptance criteria are explicitly
"live-test required" — those two host behaviors must be confirmed empirically
before Phase 2 is declared complete; the design does not claim them. No
unresolved correctness issue blocks starting Phase 1.
