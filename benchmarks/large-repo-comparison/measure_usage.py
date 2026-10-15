#!/usr/bin/env python3
"""Measure Copilot host usage for a given working directory watermark."""
import json
import os
import sqlite3
import sys


def collect(db_path: str, watermark: int, cwd_prefix: str) -> dict:
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        """
        SELECT u.id, u.session_id, s.cwd, u.model, u.initiator,
               u.input_tokens, u.output_tokens, u.cache_read_tokens,
               u.cache_write_tokens, u.reasoning_tokens, u.total_nano_aiu,
               u.duration_ms, u.agent_id, u.parent_tool_call_id,
               u.token_details_json, u.created_at
        FROM assistant_usage_events u
        JOIN sessions s ON s.id = u.session_id
        WHERE u.id > ?
        ORDER BY u.id
        """,
        (watermark,),
    ).fetchall()
    conn.close()

    prefix = os.path.realpath(cwd_prefix)
    kept = []
    for r in rows:
        cwd = os.path.realpath(r["cwd"] or "")
        if cwd == prefix or cwd.startswith(prefix + os.sep):
            kept.append(r)

    models: dict[str, int] = {}
    initiators: dict[str, int] = {}
    sessions: set[str] = set()
    uncached = cache_read = cache_write = output = reasoning = total_in = 0
    cost = 0.0
    duration = 0
    sub_agents = 0
    events = []

    for e in kept:
        models[e["model"]] = models.get(e["model"], 0) + 1
        initiators[e["initiator"]] = initiators.get(e["initiator"], 0) + 1
        sessions.add(e["session_id"])
        cache_read += e["cache_read_tokens"] or 0
        cache_write += e["cache_write_tokens"] or 0
        reasoning += e["reasoning_tokens"] or 0
        total_in += e["input_tokens"] or 0
        output += e["output_tokens"] or 0
        cost += (e["total_nano_aiu"] or 0) / 1e9
        duration += e["duration_ms"] or 0
        if e["initiator"] == "sub-agent":
            sub_agents += 1
        u = None
        details = e["token_details_json"]
        if details:
            try:
                entries = json.loads(details)
                for te in entries:
                    if te.get("tokenType") == "input":
                        u = (u or 0) + te.get("tokenCount", 0)
            except Exception:
                u = None
        if u is None:
            u = max(0, (e["input_tokens"] or 0) - (e["cache_read_tokens"] or 0) - (e["cache_write_tokens"] or 0))
        uncached += u
        events.append(
            {
                "id": e["id"],
                "session_id": e["session_id"],
                "cwd": e["cwd"],
                "model": e["model"],
                "initiator": e["initiator"],
                "agent_id": e["agent_id"],
                "input_tokens": e["input_tokens"],
                "output_tokens": e["output_tokens"],
                "cache_read_tokens": e["cache_read_tokens"],
                "uncached_input": u,
                "total_nano_aiu": e["total_nano_aiu"],
                "duration_ms": e["duration_ms"],
                "created_at": e["created_at"],
            }
        )

    return {
        "model_calls": len(kept),
        "sessions": len(sessions),
        "models_by_calls": models,
        "calls_by_initiator": initiators,
        "uncached_input_tokens": uncached,
        "cache_read_tokens": cache_read,
        "cache_write_tokens": cache_write,
        "output_tokens": output,
        "reasoning_tokens": reasoning,
        "total_input_tokens_incl_cache": total_in,
        "total_output_tokens": output,
        "reported_cost_ai_credits": round(cost, 4),
        "sum_model_call_duration_ms": duration,
        "sub_agent_calls": sub_agents,
        "events": events,
    }


def watermark(db_path: str) -> int:
    conn = sqlite3.connect(db_path)
    row = conn.execute("SELECT COALESCE(MAX(id),0) FROM assistant_usage_events").fetchone()
    conn.close()
    return row[0]


if __name__ == "__main__":
    cmd = sys.argv[1]
    db = sys.argv[2]
    if cmd == "watermark":
        print(watermark(db))
    elif cmd == "collect":
        wm = int(sys.argv[3])
        cwd = sys.argv[4]
        out_path = sys.argv[5] if len(sys.argv) > 5 else None
        agg = collect(db, wm, cwd)
        text = json.dumps(agg, indent=2)
        if out_path:
            with open(out_path, "w") as f:
                f.write(text + "\n")
        # Print summary without events
        summary = {k: v for k, v in agg.items() if k != "events"}
        print(json.dumps(summary))
    else:
        sys.exit(f"unknown cmd {cmd}")
