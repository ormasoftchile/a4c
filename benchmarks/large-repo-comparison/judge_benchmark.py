#!/usr/bin/env python3
"""Unattended benchmark completion judge.

Determines a definitive PASS/FAIL from authoritative persisted evidence WITHOUT
waiting for human approval. Production task state (e.g. awaiting_human_review)
is recorded separately and never mutated. Human approval is never forged.

PASS requires ALL of:
  - implementation completed (task did not fail; a coder activity completed),
  - independent acceptance passed (5/5),
  - required agent review approved (a reviewer verdict approved=true).
FAIL otherwise (missing delegation, failed implementation, blocked/rejected
review, or failed acceptance).
"""
import json
import sys
import os
import glob


def judge(out_dir: str) -> dict:
    result = {
        "implementation": "unknown",
        "verification": "unknown",
        "agent_review": "unknown",
        "production_state": "unknown",
        "benchmark_result": "FAIL",
        "reasons": [],
    }

    # Acceptance verdict (independent hidden tests).
    acc_path = os.path.join(out_dir, "acceptance.verdict")
    acc = "unknown"
    if os.path.exists(acc_path):
        acc = open(acc_path).read().strip()
    result["verification"] = "passed" if acc == "PASS" else ("failed" if acc == "FAIL" else "unknown")
    if acc != "PASS":
        result["reasons"].append(f"independent acceptance = {acc}")

    # Task inspect: authoritative persisted state.
    ti_path = os.path.join(out_dir, "task-inspect.json")
    task = {}
    if os.path.exists(ti_path):
        try:
            task = json.load(open(ti_path))
        except Exception:
            task = {}
    result["production_state"] = task.get("status", "unknown")

    # Implementation completed? Look for a completed coder activity and no
    # terminal failure.
    acts_dir = os.path.join(out_dir, "repo", ".a4c", "activities")
    coder_completed = False
    review_approved = False
    any_failed = False
    for f in glob.glob(os.path.join(acts_dir, "*.json")):
        try:
            d = json.load(open(f))
        except Exception:
            continue
        req = d.get("request", {}) or {}
        res = d.get("result", {}) or {}
        agent = (req.get("agent_id") or "").lower()
        status = res.get("status") or d.get("status")
        if status == "failed":
            any_failed = True
        if agent in ("coder",) or ("coder" in agent):
            if status == "completed":
                coder_completed = True
        # Reviewer approval: an explicit approved verdict persisted.
        if agent in ("reviewer", "charlie") or "review" in agent:
            if res.get("outputs", {}).get("approved") is True:
                review_approved = True

    if coder_completed:
        result["implementation"] = "completed"
    else:
        result["implementation"] = "not_completed"
        result["reasons"].append("no completed coder activity")
    if any_failed and not coder_completed:
        result["implementation"] = "failed"

    # Agent review outcome.
    if review_approved:
        result["agent_review"] = "approved"
    else:
        result["agent_review"] = "not_approved"
        result["reasons"].append("no approved independent agent review")

    # Definitive result.
    if (result["implementation"] == "completed"
            and result["verification"] == "passed"
            and result["agent_review"] == "approved"):
        result["benchmark_result"] = "PASS"
    else:
        result["benchmark_result"] = "FAIL"

    return result


if __name__ == "__main__":
    out_dir = sys.argv[1]
    res = judge(out_dir)
    # The human-approval gate is preserved: production_state is reported as-is
    # and never changed here. benchmark_result reflects ONLY autonomous work.
    print(json.dumps(res, indent=2))
    # Exit 0 on PASS, 1 on FAIL so a harness can branch, but never blocks.
    sys.exit(0 if res["benchmark_result"] == "PASS" else 1)
