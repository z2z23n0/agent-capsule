---
name: agent-capsule
description: Use when Codex needs to install or use Agent Capsule to export, share, inspect, import, restore, or verify coding agent sessions, `.capsule.zip` files, or encrypted Agent Capsule links. Covers CLI setup, sender workflows, receiver bootstrap, approved import into local Codex history, import-as-new semantics, secret-scan handling, and verification.
---

# Agent Capsule

## Overview

Agent Capsule is a CLI and capsule format for sharing coding agent sessions. Use this skill as the agent-facing operating guide; do not reimplement the capsule format in the prompt.

The CLI is the source of truth. A `.capsule.zip` or share link must remain self-bootstrapping for agents that do not have this skill installed.

## CLI Setup

Check whether the CLI is available:

```bash
command -v capsule
capsule help
```

If the user asked to export, share, import, restore, inspect, or verify a capsule and the CLI is missing, install it:

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main
```

If `go` is missing or installation fails, report the exact blocker and do not invent a manual restore path.

## Export

For a normal handoff, export an encrypted share link:

```bash
capsule export --thread current
```

Only when the user explicitly asks for a local file or zip capsule, add
`--format zip`:

```bash
capsule export --thread current --format zip --name "<handoff topic>"
```

If the user provides a Worker or S3/R2 target, pass the matching `capsule export`
flags instead of uploading the raw session yourself.

If export fails with a secret-scan warning, stop and tell the user what was detected. Only rerun with `--unsafe-include-secrets` after explicit user approval. Secret scan covers session text, not OCR or image pixels, so remind the user to review screenshots and uploaded images when relevant.

Treat a full share URL containing `#k=...` as sensitive. The URL fragment is the decryption key.

## Import Or Restore

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

Import into the intended project directory after the user approves local Codex history writes:

```bash
capsule import <file-or-url> --target codex --target-cwd . --execute
```

Agent Capsule import always creates a new Codex thread, like a session fork. Never design the workflow around replacing or overwriting the source thread, even when source and target use the same `CODEX_HOME`.

## Verify

After an executed import, verify the new thread id from the import result:

```bash
capsule verify --home "${CODEX_HOME:-$HOME/.codex}" --thread <new-thread-id> --target-cwd .
```

A capsule with `safety.status = blocked` can still be imported locally; treat it as a content warning that needs user awareness, not as a CLI runtime failure.

## Boundaries

Do not migrate provider credentials, auth sessions, cloud state, API keys, or assume encrypted reasoning can be cryptographically continued on another machine.

Do not write to local Codex history without `--execute` and explicit user approval.
