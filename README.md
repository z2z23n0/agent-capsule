# Agent Capsule

Agent Capsule turns a complete Codex / Claude Code conversation into a shareable
link.

You only need to send the link to someone else. They can import that
conversation into their own local Codex or Claude Code, get a native
thread/session like the one on your machine, with the full context, and continue
working directly inside the conversation.

After they hand the link to their own Codex or Claude Code, the agent can import
the session into the local native UI/UX and continue from there.

The CLI command is `capsule`.

[中文 README](README.zh-CN.md)

## Why

Sometimes you want to hand off the whole agent workspace: the conversation, the
investigation path, tool use, working context, and unfinished next steps.

Agent Capsule packages that session into a capsule that can be imported in one
step. The receiver gets more than a record of what you talked about; they can
restore it into their own agent and keep working.

## Status

Agent Capsule currently supports Codex and Claude Code export/import, plus
Codex <-> Claude Code cross-agent handoff.

Codex image uploads referenced by a session are preserved. Agent Capsule does
not package arbitrary non-image files yet.

Same-agent imports create a new native session/thread. Cross-agent imports
preserve the visible transcript, tool evidence, working context, and a raw
source sidecar for later inspection. They do not migrate provider credentials,
login state, cloud state, filesystem checkpoints, or private encrypted agent
state.

## Install

Install `capsule`:

```bash
curl -fsSL https://raw.githubusercontent.com/z2z23n0/agent-capsule/main/install.sh | sh
```

If your shell cannot find `capsule` after installation, add `~/.local/bin` to
your `PATH`.

## Agent skill

Agents can optionally install the Agent Capsule skill from
[`skills/agent-capsule`](skills/agent-capsule/SKILL.md). The skill teaches the
agent when to install the CLI, how to export or share a session, how to import
after inspection and explicit approval, how to perform local Codex <-> Claude
Code handoffs.

Capsule files and links do not depend on the skill. They include agent-facing
bootstrap instructions so a receiving agent can understand the workflow, install
the CLI, inspect the capsule, import it into the local native Codex / Claude Code
UI/UX, and verify the restored thread/session even without a preinstalled skill.

## Quick start: link handoff

Export the current Codex thread as an encrypted share link:

```bash
capsule export --source codex --thread current
```

Export the current Claude Code session instead:

```bash
capsule export --source claude --thread current
```

The default export format is `link`. A share link looks like this:

```text
https://<worker-host>/s/<share-id>#k=<base64url-key>
```

The capsule is encrypted with AES-256-GCM before upload. The service stores the
ciphertext and manifest; the decryption key lives in the URL fragment and is not
sent to the server by normal browser requests.

The browser page shows a locally decrypted preview and includes agent-friendly
CLI install, inspect, and import commands.

For sessions with images, the browser preview shows image thumbnails when they
fit the preview size limit. Large image-heavy sessions still import from the
complete encrypted capsule.

If link upload fails because the service is unavailable or over quota, Agent
Capsule writes a local `.capsule.zip` fallback and returns
`status: fallback_zip`.

## File handoff

Export the current session as a local zip capsule only when you explicitly need
a file:

```bash
capsule export --source codex --thread current --format zip --name "handoff topic"
capsule export --source claude --thread current --format zip --name "handoff topic"
```

Inspect the capsule before importing:

```bash
capsule inspect handoff-topic.capsule.zip
```

Write the imported thread/session into local agent history:

```bash
capsule import handoff-topic.capsule.zip --target codex --target-cwd . --execute
capsule import handoff-topic.capsule.zip --target claude --target-cwd . --execute
```

Verify the imported thread/session:

```bash
capsule verify --target codex --home ~/.codex --thread <new-thread-id> --target-cwd .
capsule verify --target claude --home ~/.claude --thread <new-session-id> --target-cwd .
```

## Local fast handoff

For same-machine handoffs, `capsule handoff` reads the source agent's local
history and writes a new native target thread/session directly:

```bash
capsule handoff --from codex --to claude --source-thread current --target-cwd . --execute
capsule handoff --from claude --to codex --source-thread current --target-cwd . --execute
```

Use dry-run by omitting `--execute`. Local handoff still runs the secret scan,
but high-confidence findings are reported as warnings instead of blocking,
because no share artifact is created.

When writing Claude Code history directly is not enough for the local Claude
runtime, the result includes a precise fallback command such as:

```bash
cd "<target-cwd>" && claude --session-id <new-session-id>
```

## Privacy commitments

For link sharing, Agent Capsule encrypts the capsule locally before upload. The
hosted service, Worker, R2 bucket, or S3-compatible bucket receives only the
encrypted capsule bytes and encrypted preview payload. Without the `#k=...`
fragment key, those services cannot decrypt the conversation content.

The decryption key is generated on the sender's machine and placed only in the
URL fragment. Normal browser requests do not send URL fragments to the server,
and the CLI importer removes the fragment before fetching the manifest and
ciphertext.

The service can still see and store link metadata, including thread id, thread
title, creation and expiry timestamps, ciphertext size, ciphertext hash, bundle
URL, and operational request metadata.

The hosted preview page decrypts the preview in the browser with WebCrypto. If
you do not trust the page host to serve honest JavaScript, use the CLI import
path instead; it fetches the manifest and ciphertext directly and decrypts
locally.

## Official, Worker, and S3 sharing

`capsule export` defaults to `--service official` and uses the hosted endpoint
`https://agent-capsule.z2z23n0.workers.dev`. In local development, override it
with `CAPSULE_OFFICIAL_ENDPOINT`:

```bash
export CAPSULE_OFFICIAL_ENDPOINT=https://...
capsule export --thread current
```

For a self-hosted Cloudflare Worker:

```bash
capsule export --thread current \
  --service worker \
  --endpoint https://example.workers.dev
```

For S3-compatible storage such as R2:

```bash
capsule export --thread current --service s3 \
  --s3-endpoint https://<account>.r2.cloudflarestorage.com \
  --s3-bucket agent-capsule \
  --s3-prefix shares \
  --s3-access-key-id "$CAPSULE_S3_ACCESS_KEY_ID" \
  --s3-secret-access-key "$CAPSULE_S3_SECRET_ACCESS_KEY" \
  --s3-public-base-url https://pub.example/capsules
```

## Deploy your own Worker

The Worker template lives in `deploy/cloudflare-worker/`.

```bash
cd deploy/cloudflare-worker
npm install
cp wrangler.toml.example wrangler.toml
npm run dev
```

Before deploying, bind:

- a private R2 bucket as `CAPSULE_BUCKET`
- the `BudgetGate` Durable Object
- optional upload auth with `CAPSULE_WORKER_TOKEN`

Deploy with:

```bash
npm run deploy
```

Do not commit real `wrangler.toml` files or secrets.

## What is inside a capsule

A `.capsule.zip` contains:

```text
manifest.json
AGENT_README.md
codex/session.jsonl                   # Codex source capsules
codex/index-entry.json                 # Codex source capsules
codex/thread-row.json                  # Codex source capsules
codex/assets/images.json               # optional Codex images
codex/assets/images/<sha256>.<ext>     # optional Codex images
claude/session.jsonl                   # Claude source capsules
claude/session-index-entry.json        # optional Claude index entry
agent/neutral.json
agent/restore.md
safety/scan.json
checksums.json
```

`manifest.json` records `source_agent`, target support, payload inventory, and
the lossless level. Legacy Codex capsules without those fields still import as
Codex capsules.

Image asset files are present only when the Codex session references local
images. During import, those images are written under
`$CODEX_HOME/agent-capsule-assets/<new-thread-id>/images/`, and the imported
session rewrites local image paths to that new location.

Cross-agent imports also write a raw source sidecar under the target agent home,
for example `$CODEX_HOME/agent-capsule-sources/<new-thread-id>/` or
`$CLAUDE_CONFIG_DIR/agent-capsule-sources/<new-session-id>/`.

The root `AGENT_README.md` exists so a receiving agent can inspect an ordinary
zip file and understand how to restore it before installing anything.

## Safety model

Capsules can contain sensitive conversation content, local paths, tool output,
prompts, images or screenshots, and accidental secrets.

Agent Capsule runs a best-effort secret scan during export and share. If it
finds high-confidence secrets, export fails unless you explicitly pass:

```bash
--unsafe-include-secrets
```

Only use that flag when you have reviewed the capsule and intentionally want to
share it.

The secret scan covers session text. It does not OCR images or scan image
pixels, so review screenshots and uploaded images before sharing.

Link sharing uploads encrypted bytes, but anyone with the full URL including
`#k=...` can decrypt the capsule.

## What Agent Capsule does not do

Agent Capsule does not migrate provider credentials, auth sessions, cloud state,
or API keys.

It does not guarantee that encrypted reasoning blobs from one machine can be
cryptographically continued on another machine.

## Development

Run the Go tests:

```bash
go test ./internal/capsule ./internal/codex
```

Run Worker checks:

```bash
npm --prefix deploy/cloudflare-worker test
npm --prefix deploy/cloudflare-worker run check
```

## License

Apache-2.0. See [LICENSE](LICENSE).
