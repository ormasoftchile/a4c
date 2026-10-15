# A4C Team-Mode Budget Economics Fix — Verification (2026-10-14)

Re-run of the **previously failing A4C team task** (identical disposable Go
fixture, identical task text, same host Copilot CLI 1.0.91, same model
gpt-5.5, parent budget 500,000) after the coordination/budget fix.

Artifacts: `results/2026-10-14-run2/` (before) ·
`results/2026-10-14-run4-a4c-team-fixed/` (after).

## Outcome

| | Before (run2) | After (run4) |
|---|---|---|
| Acceptance (hidden, harness-injected) | **FAIL** (no implementation) | **PASS** (`ok example.com/sched`) |
| Task outcome | failed: `budget_exhausted` | completed: implemented, natively verified, **reviewed** |
| Sessions | 2 (lead + strangled coder) | 3 (lead + coder + reviewer) |
| Model calls | 11 | 14 |
| Total input (incl. cache reads) | 110,445 | 159,471 |
| Uncached input | 21,357 | 42,223 |
| Output tokens | 4,284 | 9,910 |
| Billed consumption (new accounting) | — | 52,133 |
| Host-reported cost | 27.98 AI credits | 56.70 AI credits |
| Wall time | 56.7 s | 109.9 s |
| Lead zero-code overhead | 30,590 total-in (27.7% of run) | 21,021 total-in / 8,336 billed (13.2% of run) |

## What the fixed run did

- Lead delegated promptly (2 file reads vs 4 before), proposed **viable**
  budgets — coder 60,000 / reviewer 45,000 — no floor clamping needed; the
  updated charter's budget guidance was followed.
- Coder allocated 60,000, consumed 30,848 billed (116,332 total input of
  which 92,160 were cache reads — cache re-serves no longer consume budget),
  passed native tests.
- Reviewer ran and approved (12,949 billed); no ceremony was skipped.
- No budget rejections, no recovery retries needed — the failure mode was
  prevented upstream rather than recovered.

## Context (same task, same host, same model)

| Configuration | Total input | AI credits | Result |
|---|---:|---:|---|
| A4C direct | 91,589 | 23.99 | PASS |
| **A4C team (fixed)** | **159,471** | **56.70** | **PASS (with review)** |
| A4C team (before fix) | 110,445 | 27.98 | FAIL |
| Squad | 511,413 | 70.05 | PASS |

Fixed A4C team mode now completes with full review at 3.2× less total input
and lower cost than Squad on this task.

## Deterministic test coverage added

`pkg/a4c/team_budget_economics_test.go` + `pkg/a4c/copilot_budget_internal_test.go`:

- impossibly small child proposal raised to viable floor
- parent that cannot fund a viable child → explicit `ErrInsufficientBudget`
- parent reservation/exhaustion invariants (never exceed available-after-reserve)
- end-to-end recovery from budget exhaustion (one bounded reallocation, workspace preserved)
- recovery bounded: second exhaustion is terminal `budget_exhausted`
- direct mode preserved (root budget untouched, completes normally)
- billed accounting excludes cache reads; abort-once on mid-session violation

Full `go test -short ./...` passes except the pre-existing environmental
failure `TestIndependentAcceptance_TaskInspectVerification` (hardcoded
`/Users/cristianormazabal/...` path; fails identically on pristine HEAD
commit `1450154`).
