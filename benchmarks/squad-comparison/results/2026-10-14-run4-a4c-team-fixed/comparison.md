# A4C vs Squad — live token benchmark

- Task: interval algebra in Go (`sched` package), identical fixture tree `83c6c3ef88f7`
- Host: GitHub Copilot CLI 1.0.91.
Run 'copilot update' to check for updates. (COPILOT_AUTO_UPDATE=false for both)
- Model pin: gpt-5.5 (A4C --model; Squad --model + .squad/config.json defaultModel Layer-0)
- Squad CLI: /var/folders/wp/qzs8rgx10jnfrk2vkxd_slb40000gn/T/opencode/bench/squad/packages/squad-cli/dist/cli-entry.js
- A4C strategy: team, budget 500000 tokens, max 30 steps
- Squad invocation: copilot --agent squad --yolo --additional-mcp-config @.mcp.json -p
- Measurement: ~/.copilot/session-store.db assistant_usage_events (host-reported), filtered by working directory

## Setup phase (cold)

- A4C `a4c init`: 0.4s — in(uncached)=0 cache_read=0 cache_write=0 out=0 reasoning=0 sessions=0 calls=0
- Squad `squad init --preset default`: 0.0s — unavailable

## Coding phase

- A4C: 109.9s — in(uncached)=42223 cache_read=117248 cache_write=0 out=9910 reasoning=2379 sessions=3 calls=14
- Squad: 0.0s — unavailable

## Models observed (verification)

- A4C events by model: map[gpt-5.5:14]
- Squad events by model: map[]
- A4C events by initiator: map[agent:11 user:3]
- Squad events by initiator: map[]

## Independent acceptance verification

- A4C: pass=true
- Squad: pass=false
