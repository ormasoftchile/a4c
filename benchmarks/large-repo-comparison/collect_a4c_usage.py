#!/usr/bin/env python3
"""Aggregate A4C activity metrics (host-agnostic) into a single usage summary.

Reads every activity record under an .a4c/activities dir and sums the
session-derived metrics the coordinator recorded from each host adapter's
Usage(). This is the source of truth for opencode/antigravity runs, where the
Copilot session-store DB does not apply.
"""
import json
import sys
import glob
import os


def collect(activities_dir: str) -> dict:
    totals = {
        "model_calls": 0,
        "sessions": 0,
        "uncached_input_tokens": 0,
        "cache_read_tokens": 0,
        "total_input_tokens_incl_cache": 0,
        "output_tokens": 0,
        "reasoning_tokens": 0,
        "reported_cost_usd": 0.0,
        "billed_tokens": 0,
        "activities": [],
        "models_by_activity": {},
    }
    for f in sorted(glob.glob(os.path.join(activities_dir, "*.json"))):
        try:
            d = json.load(open(f))
        except Exception:
            continue
        req = d.get("request", {}) or {}
        res = d.get("result", {}) or {}
        m = res.get("metrics", {}) or {}
        if not m and res.get("status") not in ("completed", "failed", "awaiting_human"):
            continue
        prompt = m.get("prompt_tokens") or 0
        completion = m.get("completion_tokens") or 0
        cached = m.get("cached_tokens") or 0
        total = m.get("total_tokens") or 0
        reasoning = m.get("reasoning_tokens") or 0
        cost = m.get("reported_cost_usd") or 0.0
        inv = m.get("model_invocations") or 0
        billed = m.get("billed_tokens") or 0
        # uncached input = prompt - cached (cache_read is a subset of input)
        uncached = max(0, prompt - cached)
        totals["uncached_input_tokens"] += uncached
        totals["cache_read_tokens"] += cached
        # total input incl cache: prefer host total input; approximate as prompt
        # (which includes cached reads on these hosts).
        totals["total_input_tokens_incl_cache"] += prompt
        totals["output_tokens"] += completion
        totals["reasoning_tokens"] += reasoning
        totals["reported_cost_usd"] += cost
        totals["billed_tokens"] += billed
        totals["model_calls"] += inv
        totals["sessions"] += 1
        aid = (req.get("id") or os.path.basename(f)).split("/")[-1]
        totals["activities"].append({
            "id": aid,
            "agent": req.get("agent_id"),
            "runtime": req.get("runtime"),
            "model": req.get("model"),
            "status": res.get("status") or d.get("status"),
            "total_tokens": total,
            "prompt_tokens": prompt,
            "completion_tokens": completion,
            "cached_tokens": cached,
            "reasoning_tokens": reasoning,
            "reported_cost_usd": cost,
            "model_invocations": inv,
        })
        mk = req.get("model") or "unknown"
        totals["models_by_activity"][mk] = totals["models_by_activity"].get(mk, 0) + inv
    return totals


if __name__ == "__main__":
    adir = sys.argv[1]
    out = sys.argv[2] if len(sys.argv) > 2 else None
    data = collect(adir)
    s = json.dumps(data, indent=2)
    if out:
        open(out, "w").write(s)
    print(s)
