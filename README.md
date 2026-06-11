# agent-capsule

`agent-capsule` is a local Codex session handoff tool. It exports one Codex
thread into a standard `.capsule.zip` file and imports that file into another
local Codex UI as a new thread.

The CLI name is `capsule`.

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main

capsule export --thread current
capsule export --thread current --name "handoff topic"
capsule inspect session.capsule.zip
capsule import session.capsule.zip --target codex --target-cwd . --execute
capsule verify --home ~/.codex --thread <thread-id> --target-cwd .
```

The zip root includes `AGENT_README.md`, so a receiving agent can inspect the
file with ordinary zip tooling before installing this CLI.
