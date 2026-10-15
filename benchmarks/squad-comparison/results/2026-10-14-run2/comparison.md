# A4C vs Squad — live token benchmark

- Task: interval algebra in Go (`sched` package), identical fixture tree `83c6c3ef88f7`
- Host: GitHub Copilot CLI 1.0.91.
Run 'copilot update' to check for updates. (COPILOT_AUTO_UPDATE=false for both)
- Model pin: gpt-5.5 (A4C --model; Squad --model + .squad/config.json defaultModel Layer-0)
- Squad CLI: /var/folders/wp/qzs8rgx10jnfrk2vkxd_slb40000gn/T/opencode/bench/squad/packages/squad-cli/dist/cli-entry.js
- A4C strategy: team (lead+coder+reviewer), budget 500000 tokens, max 30 steps
- Squad invocation: copilot --agent squad --yolo --additional-mcp-config @.mcp.json -p
- Measurement: ~/.copilot/session-store.db assistant_usage_events (host-reported), filtered by working directory

## Setup phase (cold)

- A4C `a4c init`: 0.4s — in(uncached)=0 cache_read=0 cache_write=0 out=0 reasoning=0 sessions=0 calls=0
- Squad `squad init --preset default`: 0.8s — in(uncached)=0 cache_read=0 cache_write=0 out=0 reasoning=0 sessions=0 calls=0

## Coding phase

- A4C: 56.7s — in(uncached)=21357 cache_read=89088 cache_write=0 out=4284 reasoning=972 sessions=2 calls=11
- Squad: 81.2s — in(uncached)=59829 cache_read=451584 cache_write=0 out=5852 reasoning=1202 sessions=1 calls=18

## Models observed (verification)

- A4C events by model: map[gpt-5.5:11]
- Squad events by model: map[gpt-5.5:18]
- A4C events by initiator: map[agent:9 user:2]
- Squad events by initiator: map[agent:7 sub-agent:10 user:1]

## Independent acceptance verification

- A4C: pass=false
- Squad: pass=true
