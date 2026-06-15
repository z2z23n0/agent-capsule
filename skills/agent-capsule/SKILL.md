---
name: agent-capsule
description: Use when an agent needs to install or use Agent Capsule to export, share, inspect, import, restore, hand off, or verify Codex and Claude Code sessions, `.capsule.zip` files, encrypted Agent Capsule links, or same-machine cross-agent handoffs. Covers CLI setup, sender workflows, receiver bootstrap, approved import into local agent history, import-as-new semantics, secret-scan handling, and verification.
---

# Agent Capsule

## Overview

Agent Capsule is a CLI and capsule format for sharing coding agent sessions and
handing them off across local agent runtimes. Use this skill as the agent-facing
operating guide; do not reimplement the capsule format in the prompt.

The CLI is the source of truth. A `.capsule.zip` or share link must remain self-bootstrapping for agents that do not have this skill installed.

## CLI Setup

Check whether the CLI is available:

```bash
command -v capsule
capsule help
```

If the user asked to export, share, import, restore, inspect, hand off, or verify a capsule and the CLI is missing, install the latest released binary:

```bash
curl -fsSL https://raw.githubusercontent.com/z2z23n0/agent-capsule/main/install.sh | sh
```

This normal install path does not require Go. It downloads a platform archive
from GitHub Releases, verifies `checksums.txt`, and installs `capsule` into
`~/.local/bin` unless `INSTALL_DIR` is set.

If the user explicitly asks to install from source for repository development,
use Go:

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main
```

If installation fails, report the exact blocker and do not invent a manual restore path.

## Export Or Share

For a normal artifact handoff, export an encrypted share link. Codex remains the
default source; pass `--source claude` for Claude Code:

```bash
capsule export --source codex --thread current
capsule export --source claude --thread current
```

Only when the user explicitly asks for a local file or zip capsule, add
`--format zip`:

```bash
capsule export --source codex --thread current --format zip --name "<handoff topic>"
capsule export --source claude --thread current --format zip --name "<handoff topic>"
```

If the user provides a Worker or S3/R2 target, pass the matching `capsule export`
flags instead of uploading the raw session yourself.

If artifact export fails with a secret-scan warning, stop and tell the user what was detected. Only rerun with `--unsafe-include-secrets` after explicit user approval. Secret scan covers session text, not OCR or image pixels, so remind the user to review screenshots and uploaded images when relevant.

Treat a full share URL containing `#k=...` as sensitive. The URL fragment is the decryption key.

## Artifact Import Or Restore

Accept either a local `.capsule.zip` path or an encrypted share URL. If a link is missing `#k=...`, ask for the full link before importing.

For zip capsules, read the embedded agent instructions when useful:

```bash
unzip -p <file>.capsule.zip AGENT_README.md
unzip -p <file>.capsule.zip agent/restore.md
```

For local zip capsules, inspect first:

```bash
capsule inspect <file>.capsule.zip
```

For encrypted share links, the browser preview and manifest validate that the link shape is usable.

Import into the intended project directory after the user approves local agent
history writes:

```bash
capsule import <file-or-url> --target codex --target-cwd . --execute
capsule import <file-or-url> --target claude --target-cwd . --execute
```

Agent Capsule import always creates a new native thread/session, like a session
fork. Never design the workflow around replacing or overwriting the source
thread/session, even when source and target use the same agent home.

Same-agent native import is raw-transcript lossless for the source runtime:
Codex -> Codex restores a new Codex thread, and Claude -> Claude rewrites the
Claude JSONL into a new `sessionId` under the target Claude project. Cross-agent
artifact import preserves visible messages, tool evidence, working context, and
a raw source sidecar under the target agent home.

## Local Fast Handoff

When the user asks for a local cross-agent handoff, use `capsule handoff`
instead of creating a share link or zip. This reads the source agent's local
history and writes the target agent's native history directly:

```bash
capsule handoff --from codex --to claude --source-thread current --target-cwd . --execute
capsule handoff --from claude --to codex --source-thread current --target-cwd . --execute
```

If the source agent is obvious from the current runtime, `--from auto` is
acceptable:

```bash
capsule handoff --from auto --to claude --source-thread current --target-cwd . --execute
```

When the user explicitly asks for handoff/import and the target is local, the
agent may use `--execute` automatically and then verify. Omit `--execute` only
when the user asks for a dry-run or preview.

Local handoff still scans the source transcript for high-confidence secrets, but
findings are warnings rather than blockers because no share artifact is created.
Artifact export remains blocking unless the user explicitly approves
`--unsafe-include-secrets`.

Do not pass `--allow-model-call` unless the user explicitly approves a fallback
that may invoke a paid model through the target agent CLI.

## Open Or Resume Result

When the user asks you to open, resume, or continue a capsule result, use the
user's current agent surface when possible. Do not assume you are running in
Codex; the current agent may be Codex, Claude Code, Cursor, OpenCode, or another
agent.

```mermaid
flowchart TD
  A["Have capsule result or target session id"] --> B{"Current surface?"}
  B -->|App with thread tools| C["Use native app/thread tools"]
  C --> D["Pin, open, read, or continue target if supported"]
  D --> Z["Report verified result"]
  B -->|CLI / terminal| E["Infer host terminal"]
  E --> F{"Confident and controllable?"}
  F -->|Yes| G["Open a new tab/window in the same terminal"]
  F -->|No| H["Ask user or print exact command"]
  G --> Z
  H --> Z
  B -->|Unknown / remote / headless| I["Do not automate GUI"]
  I --> J["Print exact local command"]
  J --> Z
```

Prefer native app/thread tools over shell automation when available. For Codex
App, useful tools may include `set_thread_pinned`, `read_thread`, and
`send_message_to_thread`; pinning the target thread is preferred when direct
opening is unavailable.

In CLI/TUI, do not run a nested resume command inline in the active agent
session. Infer the host terminal from best-effort signals such as environment
variables, parent process chain, running apps, or app-state tools. Treat this as
heuristic, not ground truth; do not hard-code Terminal, Ghostty, iTerm2, Warp,
WezTerm, Kitty, Alacritty, or any other terminal as the default.

If detection is uncertain or GUI control is unsafe, ask the user which surface to
use, or print the exact command. Preserve the original model/provider when known.
If the normal launcher is blocked by wrapper or updater behavior, use a direct
installed binary only after confirming that blocker.

For Codex targets, the command shape is:

```bash
cd <cwd> && codex resume -m <model> <thread-id>
```

For Claude Code targets, the command shape is:

```bash
cd <cwd> && claude --session-id <session-id>
```

For other non-Codex targets, use that runtime's equivalent resume or open command.
Claim success only after verifying through native app tools, terminal contents,
or process inspection.

For Codex targets, verify thread existence with narrow checks such as
`session_index.jsonl`, an exact session file path, or the specific resume
process. Avoid broad recursive scans of `CODEX_HOME`; local Codex histories can
be large and may include sensitive capsule URLs.

## Verify

After an executed import, verify the new thread id from the import result:

```bash
capsule verify --target codex --home "${CODEX_HOME:-$HOME/.codex}" --thread <new-thread-id> --target-cwd .
capsule verify --target claude --home "${CLAUDE_CONFIG_DIR:-$HOME/.claude}" --thread <new-session-id> --target-cwd .
```

A capsule with `safety.status = blocked` can still be imported locally; treat it as a content warning that needs user awareness, not as a CLI runtime failure.

## Boundaries

Do not migrate provider credentials, auth sessions, cloud state, API keys,
filesystem checkpoints, or assume encrypted reasoning can be cryptographically
continued in another runtime.

Do not write to local agent history without `--execute` and explicit user
approval, except when the user has directly asked this agent to perform a local
handoff/import and verification.
