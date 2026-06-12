# Contributing

Thanks for helping improve Agent Capsule. Please keep changes focused and small
enough to review.

## Local checks

Run the checks that match the area you changed:

```bash
go test ./...
```

```bash
cd deploy/cloudflare-worker
npm ci
npm run check
npm test
```

CI runs the Go and Worker checks on pull requests and pushes to `main`.

## Safety

Do not commit real capsule exports, unpacked capsule directories, Codex history,
agent local state, API keys, tokens, `.env` files, `wrangler.toml`, or other
secrets. Use synthetic fixtures when tests need session or capsule data.

Before opening a pull request, check that `git status --ignored --short` does
not show private files that should be removed from the working tree.
