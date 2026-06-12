# agent-capsule

`agent-capsule` is a local Codex session handoff tool. It exports one Codex
thread into a standard `.capsule.zip` file and imports that file into another
local Codex UI as a new thread.

The CLI name is `capsule`.

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main

capsule export --thread current
capsule export --thread current --name "handoff topic"
capsule share --thread current
capsule share --thread current --format zip
capsule inspect session.capsule.zip
capsule import session.capsule.zip --target codex --target-cwd . --execute
capsule import "https://example.workers.dev/s/<id>#k=<key>" --target codex --target-cwd . --execute
capsule verify --home ~/.codex --thread <thread-id> --target-cwd .
```

The zip root includes `AGENT_README.md`, so a receiving agent can inspect the
file with ordinary zip tooling before installing this CLI.

## Link sharing

`capsule share` exports the thread to a temporary `.capsule.zip`, runs the same
secret scan as `capsule export`, encrypts the zip with AES-256-GCM, uploads only
ciphertext, and prints a link. The decryption key lives in the URL fragment:

```text
https://<worker-host>/s/<share-id>#k=<base64url-key>
```

Opening the link in a browser shows an encrypted, locally decrypted session
preview for humans. The same page also exposes install, dry-run, and import
commands so an agent can restore the complete session into the receiver's
native Codex UI as a new thread.

If link sharing fails because the Worker is unavailable, quota rejects the
upload, or the network fails, the command writes a local `.capsule.zip` and
returns `status: fallback_zip`.

Official sharing uses `--service official` and reads its endpoint from
`CAPSULE_OFFICIAL_ENDPOINT` unless `--endpoint` is provided. BYO Worker sharing
uses `--service worker --endpoint https://...` and optionally `--token` or
`CAPSULE_WORKER_TOKEN`. BYO S3/R2 sharing uses `--service s3` with these flags
or matching environment variables:

```bash
capsule share --thread current --service s3 \
  --s3-endpoint https://<account>.r2.cloudflarestorage.com \
  --s3-bucket agent-capsule \
  --s3-prefix shares \
  --s3-access-key-id "$CAPSULE_S3_ACCESS_KEY_ID" \
  --s3-secret-access-key "$CAPSULE_S3_SECRET_ACCESS_KEY" \
  --s3-public-base-url https://pub.example/capsules
```

The open Worker template lives in `deploy/cloudflare-worker/`. Copy
`wrangler.toml.example` to `wrangler.toml`, bind a private R2 bucket and the
`BudgetGate` Durable Object, then deploy with Wrangler. Official deployments use
the same code; bucket names, secrets, namespaces, and hosted endpoint settings
are intentionally not committed.

For BYO Worker deployments, set `CAPSULE_WORKER_TOKEN` with `wrangler secret put`
if you want uploads to require `capsule share --token ...` or
`CAPSULE_WORKER_TOKEN` on the client. Without that Worker secret, the template
continues to allow anonymous uploads with the built-in size and budget gates.
