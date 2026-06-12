# Agent Capsule

Agent Capsule turns a Codex session into a shareable capsule.

You can export a local Codex thread as a standard `.capsule.zip` file, or as an
encrypted share link. The receiver can import it into their own Codex setup,
open the full conversation, and continue the work.

The CLI command is `capsule`.

[中文 README](README.zh-CN.md)

## Why

Sometimes you want to share the whole agent conversation: the reasoning trail,
the debugging path, and the record of how a problem was found.

Sometimes you want to hand off unfinished work: a bug investigation that is not
done yet, or code that is only halfway through.

Agent Capsule packages that session into an inspectable, importable capsule so
the receiver gets more than a chat transcript. They can restore it into Codex
and keep working from there.

## Status

Agent Capsule currently supports Codex export and import.

Claude Code support and cross-agent export/import are planned next.

## Install

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main
```

## Quick start: file handoff

Export the current thread:

```bash
capsule export --thread current --name "handoff topic"
```

Inspect the capsule before importing:

```bash
capsule inspect handoff-topic.capsule.zip
```

Dry-run the import:

```bash
capsule import handoff-topic.capsule.zip --target codex --target-cwd .
```

Write the imported thread into your local Codex home:

```bash
capsule import handoff-topic.capsule.zip --target codex --target-cwd . --execute
```

Verify the imported thread:

```bash
capsule verify --home ~/.codex --thread <new-thread-id> --target-cwd .
```

`capsule import` is a dry-run unless `--execute` is provided.

## Link sharing

Agent Capsule can also create encrypted share links:

```bash
capsule share --thread current --service worker --endpoint https://example.workers.dev
```

A share link looks like this:

```text
https://<worker-host>/s/<share-id>#k=<base64url-key>
```

The capsule is encrypted with AES-256-GCM before upload. The service stores the
ciphertext and manifest; the decryption key lives in the URL fragment and is not
sent to the server by normal browser requests.

The browser page shows a locally decrypted preview and includes agent-friendly
install, dry-run, and import commands.

If link upload fails because the endpoint is missing, unavailable, or over
quota, Agent Capsule writes a local `.capsule.zip` fallback and returns
`status: fallback_zip`.

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

`capsule share` defaults to `--service official`. In local development, do not
assume an official endpoint is available. Configure one explicitly:

```bash
export CAPSULE_OFFICIAL_ENDPOINT=https://...
capsule share --thread current
```

For a self-hosted Cloudflare Worker:

```bash
capsule share --thread current \
  --service worker \
  --endpoint https://example.workers.dev
```

For S3-compatible storage such as R2:

```bash
capsule share --thread current --service s3 \
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
codex/session.jsonl
codex/index-entry.json
codex/thread-row.json
agent/restore.md
safety/scan.json
checksums.json
```

The root `AGENT_README.md` exists so a receiving agent can inspect an ordinary
zip file and understand how to restore it before installing anything.

## Safety model

Capsules can contain sensitive conversation content, local paths, tool output,
prompts, and accidental secrets.

Agent Capsule runs a best-effort secret scan during export and share. If it
finds high-confidence secrets, export fails unless you explicitly pass:

```bash
--unsafe-include-secrets
```

Only use that flag when you have reviewed the capsule and intentionally want to
share it.

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
