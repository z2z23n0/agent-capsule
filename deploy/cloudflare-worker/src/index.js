const LINK_SCHEMA = "agent-capsule.link.v1";
const GB = 1024 * 1024 * 1024;
const DEFAULT_INSTALL_COMMAND = "go install github.com/z2z23n0/agent-capsule/cmd/capsule@main";
const DEFAULT_DOCS_URL = "https://github.com/z2z23n0/agent-capsule";
const DEFAULT_DRY_RUN_COMMAND = "capsule import \"<this-url>\" --target codex --target-cwd .";
const DEFAULT_EXECUTE_COMMAND = "capsule import \"<this-url>\" --target codex --target-cwd . --execute";

export class BudgetGate {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    const url = new URL(request.url);
    try {
      if (url.pathname === "/reserve") return json(await this.reserve(await request.json()));
      if (url.pathname === "/release") return json(await this.release(await request.json()));
      if (url.pathname === "/commit") return json(await this.commit(await request.json()));
      if (url.pathname === "/share") return json(await this.share(await request.json()));
      if (url.pathname === "/download") return json(await this.download(await request.json()));
      if (url.pathname === "/cleanup") return json(await this.cleanup());
      return json({ ok: false, error: "not_found" }, 404);
    } catch (error) {
      return json({ ok: false, error: String(error && error.message ? error.message : error) }, 500);
    }
  }

  async reserve(input) {
    const bytes = positiveInt(input.bytes);
    const limits = readLimits(this.env);
    if (bytes <= 0 || bytes > limits.maxBlobBytes) {
      return { ok: false, status: 413, error: "blob_too_large" };
    }
    const budget = await this.rollBudget(await this.loadBudget());
    const nextLive = budget.liveBytes + bytes;
    const nextPeak = Math.max(budget.todayPeakBytes, nextLive);
    const projectedGbDays = budget.gbDays + nextPeak / GB;
    if (nextLive > limits.liveBytesLimit) return { ok: false, status: 507, error: "live_bytes_limit" };
    if (projectedGbDays > limits.monthlyGbDaysLimit) return { ok: false, status: 507, error: "monthly_gb_days_limit" };
    if (budget.puts + 1 > limits.monthlyPutLimit) return { ok: false, status: 507, error: "monthly_put_limit" };
    budget.liveBytes = nextLive;
    budget.todayPeakBytes = nextPeak;
    budget.puts += 1;
    await this.saveBudget(budget);
    return { ok: true };
  }

  async release(input) {
    const bytes = positiveInt(input.bytes);
    const budget = await this.rollBudget(await this.loadBudget());
    budget.liveBytes = Math.max(0, budget.liveBytes - bytes);
    await this.saveBudget(budget);
    return { ok: true };
  }

  async commit(input) {
    const share = input.share || {};
    if (!validID(share.id)) return { ok: false, status: 400, error: "bad_share_id" };
    await this.state.storage.put("share:" + share.id, share);
    return { ok: true };
  }

  async share(input) {
    const id = String(input.id || "");
    const share = validID(id) ? await this.state.storage.get("share:" + id) : null;
    if (!share) return { ok: false, status: 404, error: "not_found" };
    if (Date.parse(share.expires_at) <= Date.now()) return { ok: false, status: 410, error: "expired" };
    return { ok: true, share };
  }

  async download(input) {
    const id = String(input.id || "");
    const limits = readLimits(this.env);
    const share = validID(id) ? await this.state.storage.get("share:" + id) : null;
    if (!share) return { ok: false, status: 404, error: "not_found" };
    if (Date.parse(share.expires_at) <= Date.now()) return { ok: false, status: 410, error: "expired" };
    if ((share.downloads || 0) >= limits.maxDownloadsPerShare) {
      return { ok: false, status: 429, error: "download_limit" };
    }
    const budget = await this.rollBudget(await this.loadBudget());
    if (budget.gets + 1 > limits.monthlyGetLimit) {
      return { ok: false, status: 507, error: "monthly_get_limit" };
    }
    budget.gets += 1;
    share.downloads = (share.downloads || 0) + 1;
    await this.state.storage.put("share:" + id, share);
    await this.saveBudget(budget);
    return { ok: true, share };
  }

  async cleanup() {
    const budget = await this.rollBudget(await this.loadBudget());
    const now = Date.now();
    const expired = await this.state.storage.list({ prefix: "share:" });
    let deleted = 0;
    let freed = 0;
    for (const [key, share] of expired) {
      if (Date.parse(share.expires_at) > now) continue;
      if (share.object_key) await this.env.CAPSULE_BUCKET.delete(share.object_key);
      await this.state.storage.delete(key);
      deleted += 1;
      freed += share.bytes || 0;
    }
    budget.liveBytes = Math.max(0, budget.liveBytes - freed);
    await this.saveBudget(budget);
    return { ok: true, deleted, freed };
  }

  async loadBudget() {
    return (await this.state.storage.get("budget")) || {
      month: monthKey(),
      day: dayKey(),
      liveBytes: 0,
      todayPeakBytes: 0,
      gbDays: 0,
      puts: 0,
      gets: 0
    };
  }

  async saveBudget(budget) {
    await this.state.storage.put("budget", budget);
  }

  async rollBudget(budget) {
    const month = monthKey();
    const day = dayKey();
    if (budget.month !== month) {
      return {
        month,
        day,
        liveBytes: budget.liveBytes || 0,
        todayPeakBytes: budget.liveBytes || 0,
        gbDays: 0,
        puts: 0,
        gets: 0
      };
    }
    if (budget.day !== day) {
      budget.gbDays = (budget.gbDays || 0) + (budget.todayPeakBytes || 0) / GB;
      budget.day = day;
      budget.todayPeakBytes = budget.liveBytes || 0;
    }
    return budget;
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "OPTIONS") return cors(new Response(null, { status: 204 }));
    try {
      if (url.pathname === "/v1/capabilities" && request.method === "GET") {
        const limits = readLimits(env);
        return json({
          schema: LINK_SCHEMA,
          service: "agent-capsule-worker",
          max_blob_bytes: limits.maxBlobBytes,
          max_ttl_seconds: limits.maxTtlSeconds,
          quota_policy: "anonymous-small",
          auth_required: false
        });
      }
      if (url.pathname === "/v1/shares" && request.method === "POST") return await createShare(request, env, url.origin);
      const v1Share = url.pathname.match(/^\/v1\/shares\/([A-Za-z0-9_-]+)(\/blob)?$/);
      if (v1Share && request.method === "GET") {
        return v1Share[2] ? await getBlob(env, v1Share[1]) : await getManifest(env, v1Share[1]);
      }
      const pageShare = url.pathname.match(/^\/s\/([A-Za-z0-9_-]+)$/);
      if (pageShare && request.method === "GET") return await sharePage(request, env, pageShare[1]);
      return json({ ok: false, error: "not_found" }, 404);
    } catch (error) {
      return json({ ok: false, error: String(error && error.message ? error.message : error) }, 500);
    }
  },

  async scheduled(_event, env, ctx) {
    ctx.waitUntil(gate(env, "/cleanup", {}));
  }
};

async function createShare(request, env, origin) {
  const limits = readLimits(env);
  const form = await request.formData();
  const blob = form.get("blob");
  const manifestText = form.get("manifest");
  if (!blob || typeof blob.arrayBuffer !== "function") return json({ ok: false, error: "missing_blob" }, 400);
  if (!manifestText) return json({ ok: false, error: "missing_manifest" }, 400);
  const bytes = blob.size || 0;
  const reserve = await gateJSON(env, "/reserve", { bytes });
  if (!reserve.ok) return json({ ok: false, error: reserve.error }, reserve.status || 507);

  const id = validID(form.get("share_id")) ? String(form.get("share_id")) : crypto.randomUUID();
  const objectKey = "shares/" + id + "/blob.enc";
  try {
    const manifest = JSON.parse(String(manifestText));
    if (manifest.schema !== LINK_SCHEMA) throw new Error("unsupported manifest schema");
    const expiresAt = new Date(Date.now() + limits.maxTtlSeconds * 1000).toISOString();
    manifest.expires_at = expiresAt;
    manifest.bundle.url = origin + "/v1/shares/" + id + "/blob";
    manifest.service = { type: "worker", quota_policy: "anonymous-small" };
    const data = await blob.arrayBuffer();
    await env.CAPSULE_BUCKET.put(objectKey, data, {
      httpMetadata: { contentType: "application/octet-stream" }
    });
    const commit = await gateJSON(env, "/commit", {
      share: {
        id,
        object_key: objectKey,
        bytes,
        downloads: 0,
        expires_at: expiresAt,
        manifest
      }
    });
    if (!commit.ok) {
      await env.CAPSULE_BUCKET.delete(objectKey);
      throw new Error(commit.error || "metadata_commit_failed");
    }
    return json({
      share_url: origin + "/s/" + id,
      manifest_url: origin + "/v1/shares/" + id,
      expires_at: expiresAt
    }, 201);
  } catch (error) {
    await gateJSON(env, "/release", { bytes });
    throw error;
  }
}

async function getManifest(env, id) {
  const result = await gateJSON(env, "/share", { id });
  if (!result.ok) return json({ ok: false, error: result.error }, result.status || 404);
  return json(manifestForResponse(result.share.manifest));
}

async function getBlob(env, id) {
  const result = await gateJSON(env, "/download", { id });
  if (!result.ok) return json({ ok: false, error: result.error }, result.status || 404);
  const object = await env.CAPSULE_BUCKET.get(result.share.object_key);
  if (!object) return json({ ok: false, error: "blob_missing" }, 404);
  return cors(new Response(object.body, {
    headers: { "content-type": "application/octet-stream" }
  }));
}

async function sharePage(request, env, id) {
  const result = await gateJSON(env, "/share", { id });
  if (!result.ok) return html("Agent Capsule link unavailable", 404);
  const manifest = manifestForResponse(result.share.manifest);
  const accept = request.headers.get("accept") || "";
  if (accept.includes("application/json")) return json(manifest);
  return htmlDocument(sharePageHTML(request, manifest, id));
}

function manifestForResponse(manifest) {
  const out = JSON.parse(JSON.stringify(manifest || {}));
  out.import = importInfo(out.import);
  return out;
}

function importInfo(value = {}) {
  return {
    tool: value.tool || "capsule",
    command: quoteThisURL(value.command || DEFAULT_EXECUTE_COMMAND),
    install_command: value.install_command || DEFAULT_INSTALL_COMMAND,
    dry_run_command: quoteThisURL(value.dry_run_command || DEFAULT_DRY_RUN_COMMAND),
    execute_command: quoteThisURL(value.execute_command || value.command || DEFAULT_EXECUTE_COMMAND),
    docs_url: value.docs_url || DEFAULT_DOCS_URL
  };
}

function quoteThisURL(command) {
  const text = String(command || "");
  if (text.includes("\"<this-url>\"")) return text;
  return text.replaceAll("<this-url>", "\"<this-url>\"");
}

function sharePageHTML(request, manifest, id) {
  const url = new URL(request.url);
  const title = manifest.thread && manifest.thread.title ? manifest.thread.title : "Agent Capsule";
  const metadata = {
    schema: "agent-capsule.share-page.v1",
    share_url: url.origin + "/s/" + id,
    manifest_url: url.origin + "/v1/shares/" + id,
    key_ref: "url-fragment:k",
    import: manifest.import
  };
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${escapeHTML(title)} - Capsule</title>
  <style>${sharePageCSS()}</style>
</head>
<body>
  <script id="agent-capsule-metadata" type="application/agent-capsule+json">${scriptJSON(metadata)}</script>
  <div class="app-shell">
    <header class="app-header">
      <div class="header-title">
        <span class="brand">Capsule</span>
        <span id="header-session-title">${escapeHTML(title)}</span>
      </div>
      <div class="header-meta">
        <span id="expires-at">Encrypted link</span>
        <a class="docs-link" href="${escapeHTML(manifest.import.docs_url)}" rel="noreferrer">Docs</a>
      </div>
    </header>

    <main class="thread-shell">
      <section class="session-head" aria-labelledby="session-title">
        <p class="eyebrow">Encrypted Codex session preview</p>
        <h1 id="session-title">${escapeHTML(title)}</h1>
        <p class="preview-note">This is a readable preview, not the full native thread. Give this link to an agent to restore the complete session into your own Codex UI.</p>
        <div class="meta-row">
          <span id="counts">Waiting for preview</span>
          <span id="privacy-note">Decrypted locally with the URL key</span>
        </div>
      </section>

      <details class="restore-panel">
        <summary>
          <span>Restore full session in Codex</span>
          <small>Agent import commands</small>
        </summary>
        <div class="restore-grid">
          <div class="command-block">
            <div class="command-head">
              <span>Install</span>
              <button type="button" data-copy="install-command">Copy</button>
            </div>
            <pre id="install-command"></pre>
          </div>

          <div class="command-block">
            <div class="command-head">
              <span>Dry run</span>
              <button type="button" data-copy="dry-run-command">Copy</button>
            </div>
            <pre id="dry-run-command"></pre>
          </div>

          <div class="command-block emphasized">
            <div class="command-head">
              <span>Import</span>
              <button type="button" data-copy="execute-command">Copy</button>
            </div>
            <pre id="execute-command"></pre>
          </div>
        </div>
      </details>

      <div id="status" class="status">Loading encrypted preview from this link.</div>
      <section id="transcript" class="transcript" aria-live="polite"></section>
    </main>
  </div>
  <script>${sharePageJS()}</script>
</body>
</html>`;
}

function sharePageCSS() {
  return `
:root {
  color-scheme: light;
  --bg: #f6f5f1;
  --surface: #ffffff;
  --surface-soft: #f0efea;
  --ink: #1f201d;
  --muted: #6b6a62;
  --line: #dedbd2;
  --line-strong: #c9c4b8;
  --user-bg: #ecefeb;
  --assistant-bg: #ffffff;
  --process-bg: #f8f7f3;
  --accent: #315f57;
  --accent-soft: #e7f0ed;
  --warning: #855f22;
  --error: #9a3329;
  --code-bg: #171a18;
  --code-ink: #eef3ef;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100dvh;
  background: var(--bg);
  color: var(--ink);
  font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  line-height: 1.5;
}
button, a { font: inherit; }
.app-shell { min-height: 100dvh; }
.app-header {
  position: sticky;
  top: 0;
  z-index: 5;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  min-height: 58px;
  padding: 10px max(18px, calc((100vw - 1060px) / 2));
  border-bottom: 1px solid var(--line);
  background: rgba(246,245,241,.92);
  backdrop-filter: blur(14px);
}
.header-title { display: flex; align-items: center; min-width: 0; gap: 10px; }
.brand {
  flex: 0 0 auto;
  font-weight: 760;
  font-size: 15px;
  letter-spacing: 0;
}
#header-session-title {
  min-width: 0;
  color: var(--muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.header-meta { display: flex; align-items: center; gap: 10px; color: var(--muted); font-size: 13px; }
.docs-link {
  color: var(--accent);
  text-decoration: none;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 5px 10px;
  background: rgba(255,255,255,.7);
}
.thread-shell {
  width: min(960px, calc(100% - 32px));
  margin: 0 auto;
  padding: 34px 0 72px;
}
.session-head { margin-bottom: 18px; }
.eyebrow {
  margin: 0 0 10px;
  color: var(--accent);
  font-size: 12px;
  font-weight: 720;
  text-transform: uppercase;
}
h1 { margin: 0; font-size: clamp(24px, 4vw, 34px); line-height: 1.15; letter-spacing: 0; overflow-wrap: anywhere; }
.preview-note { max-width: 72ch; margin: 12px 0 0; color: var(--muted); font-size: 15px; }
.meta-row { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 20px; }
.meta-row span {
  border: 1px solid var(--line);
  background: var(--surface-soft);
  border-radius: 999px;
  padding: 5px 10px;
  color: var(--muted);
  font-size: 13px;
}
.restore-panel {
  margin: 22px 0 18px;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: var(--surface);
  box-shadow: 0 14px 42px rgba(31,32,29,.05);
  overflow: hidden;
}
.restore-panel summary {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 14px;
  cursor: pointer;
  padding: 14px 16px;
  font-weight: 680;
}
.restore-panel summary small { color: var(--muted); font-weight: 500; }
.restore-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  padding: 0 14px 14px;
}
.status {
  margin: 18px 0 20px;
  border-left: 3px solid var(--accent);
  background: var(--accent-soft);
  color: var(--accent);
  padding: 10px 13px;
  border-radius: 8px;
  font-size: 14px;
}
.status[data-kind="warn"] { border-left-color: var(--warning); background: #f8f1e4; color: var(--warning); }
.status[data-kind="error"] { border-left-color: var(--error); background: #f8e9e6; color: var(--error); }
.transcript { display: grid; gap: 14px; }
.message-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  min-width: 0;
}
.message-row.user { justify-items: end; }
.message-row.assistant { justify-items: start; }
.bubble {
  width: fit-content;
  max-width: min(760px, 86%);
  min-width: 0;
}
.message-row.user .bubble {
  background: var(--user-bg);
  border: 1px solid var(--line);
  border-radius: 18px 18px 5px 18px;
  padding: 12px 14px;
}
.message-row.assistant .bubble {
  background: var(--assistant-bg);
  border: 1px solid var(--line);
  border-radius: 18px 18px 18px 5px;
  padding: 14px 16px;
  box-shadow: 0 10px 32px rgba(31,32,29,.04);
}
.role {
  margin-bottom: 7px;
  color: var(--muted);
  font-size: 12px;
  font-weight: 680;
}
.markdown {
  font-size: 15px;
  color: var(--ink);
}
.markdown > *:first-child { margin-top: 0; }
.markdown > *:last-child { margin-bottom: 0; }
.markdown p { margin: 0 0 10px; }
.markdown h1, .markdown h2, .markdown h3 {
  margin: 14px 0 8px;
  font-size: 1.05rem;
  line-height: 1.3;
}
.markdown ul, .markdown ol { margin: 8px 0 10px; padding-left: 22px; }
.markdown li { margin: 4px 0; }
.markdown blockquote {
  margin: 10px 0;
  padding: 2px 0 2px 12px;
  border-left: 3px solid var(--line-strong);
  color: var(--muted);
}
.markdown a { color: #315f86; text-decoration-thickness: 1px; text-underline-offset: 2px; }
.markdown code, .tool-input, .command-block pre {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
}
.markdown code {
  border: 1px solid var(--line);
  border-radius: 5px;
  background: #f4f3ee;
  padding: 1px 4px;
  font-size: .92em;
}
.markdown pre {
  margin: 10px 0;
  padding: 12px;
  overflow-x: auto;
  white-space: pre;
  background: var(--code-bg);
  color: var(--code-ink);
  border-radius: 8px;
}
.markdown pre code {
  border: 0;
  background: transparent;
  padding: 0;
  color: inherit;
}
.tool-input, .command-block pre {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0;
}
.process-row {
  display: grid;
  justify-items: start;
}
details.process-card {
  width: min(720px, 84%);
  background: var(--process-bg);
  border: 1px solid var(--line);
  border-radius: 10px;
  color: var(--muted);
  overflow: hidden;
}
details.process-card summary {
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 9px 12px;
  list-style: none;
  font-size: 13px;
}
details.process-card summary::-webkit-details-marker { display: none; }
.chevron {
  width: 8px;
  height: 8px;
  border-right: 1.5px solid currentColor;
  border-bottom: 1.5px solid currentColor;
  transform: rotate(-45deg);
  transition: transform .15s ease;
}
details.process-card[open] .chevron { transform: rotate(45deg); }
.process-title {
  min-width: 0;
  color: var(--ink);
  font-weight: 620;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.process-meta { flex: 0 0 auto; color: var(--muted); }
.tool-input {
  border-top: 1px solid var(--line);
  padding: 12px;
  color: var(--muted);
  font-size: 12px;
  background: #fffdfa;
}
.command-block {
  border: 1px solid var(--line);
  border-radius: 8px;
  overflow: hidden;
  background: var(--code-bg);
  min-width: 0;
}
.command-block.emphasized { border-color: rgba(49,95,87,.55); }
.command-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  background: #f2f1ec;
  color: var(--ink);
  padding: 8px 10px;
  font-size: 13px;
  font-weight: 680;
}
.command-head button {
  border: 1px solid var(--line);
  background: white;
  color: var(--accent);
  border-radius: 999px;
  padding: 4px 9px;
  cursor: pointer;
}
.command-block pre {
  color: var(--code-ink);
  padding: 13px;
  font-size: 12px;
}
@media (max-width: 760px) {
  .app-header { align-items: flex-start; flex-direction: column; padding: 10px; }
  .header-meta { width: 100%; justify-content: space-between; }
  .thread-shell { width: min(100% - 20px, 680px); padding-top: 22px; }
  .restore-grid { grid-template-columns: 1fr; }
  .bubble, details.process-card { max-width: 100%; width: 100%; }
  .message-row.user .bubble, .message-row.assistant .bubble { border-radius: 14px; }
}
`;
}

function sharePageJS() {
  return `
const metadata = JSON.parse(document.getElementById("agent-capsule-metadata").textContent);
const $ = (id) => document.getElementById(id);
const fenceMarker = String.fromCharCode(96, 96, 96);

function fullShareURL() {
  return location.origin + location.pathname + location.search + location.hash;
}

function commandText(template) {
  const url = location.hash ? fullShareURL() : metadata.share_url + "#k=...";
  return String(template || "").replaceAll("<this-url>", url);
}

function setStatus(text, kind = "info") {
  const node = $("status");
  node.textContent = text;
  node.dataset.kind = kind;
}

function renderCommands(importInfo) {
  $("install-command").textContent = importInfo.install_command || metadata.import.install_command;
  $("dry-run-command").textContent = commandText(importInfo.dry_run_command || metadata.import.dry_run_command);
  $("execute-command").textContent = commandText(importInfo.execute_command || importInfo.command || metadata.import.execute_command);
}

function renderManifestInfo(manifest) {
  if (manifest.thread && manifest.thread.title) {
    document.title = manifest.thread.title + " - Capsule";
    $("session-title").textContent = manifest.thread.title;
    $("header-session-title").textContent = manifest.thread.title;
  }
  $("expires-at").textContent = manifest.expires_at ? "Expires " + new Date(manifest.expires_at).toLocaleString() : "Encrypted link";
}

function fragmentKey() {
  const value = new URLSearchParams(location.hash.slice(1)).get("k");
  return value || "";
}

function base64urlToBytes(value) {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64 + "=".repeat((4 - base64.length % 4) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

async function decryptPreview(preview, keyText) {
  if (!crypto.subtle) throw new Error("WebCrypto is unavailable in this browser");
  const keyBytes = base64urlToBytes(keyText);
  const nonce = base64urlToBytes(preview.crypto.nonce);
  const ciphertext = base64urlToBytes(preview.payload);
  const key = await crypto.subtle.importKey("raw", keyBytes, { name: "AES-GCM" }, false, ["decrypt"]);
  const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, key, ciphertext);
  return JSON.parse(new TextDecoder().decode(plain));
}

function renderTranscript(transcript) {
  const entries = (transcript.entries || []).filter((entry) => !isInternalContextEntry(entry));
  const messageCount = entries.filter((entry) => entry.kind === "message").length;
  const toolCount = entries.filter((entry) => entry.kind === "tool").length;
  $("counts").textContent = messageCount + " messages - " + toolCount + " process steps";
  const root = $("transcript");
  root.replaceChildren();
  if (transcript.truncated) {
    const note = document.createElement("div");
    note.className = "status";
    note.textContent = "This preview is truncated. Import the Capsule to continue in the complete Codex thread.";
    root.appendChild(note);
  }
  if (entries.length === 0) {
    const empty = document.createElement("div");
    empty.className = "status";
    empty.textContent = "No public preview entries are available for this session. An agent can still restore the full Capsule in Codex.";
    root.appendChild(empty);
    return;
  }
  for (const entry of entries) {
    root.appendChild(entry.kind === "tool" ? toolNode(entry) : messageNode(entry));
  }
}

function isInternalContextEntry(entry) {
  if (!entry || entry.kind !== "message") return false;
  const text = String(entry.text || "").trim();
  return text.startsWith("# AGENTS.md instructions for ") ||
    text.startsWith("<codex_internal_context") ||
    text.startsWith("<environment_context>") ||
    text.startsWith("<INSTRUCTIONS>");
}

function messageNode(entry) {
  const article = document.createElement("article");
  article.className = "message-row " + (entry.role || "");
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  const role = document.createElement("div");
  role.className = "role";
  role.textContent = roleLabel(entry.role);
  bubble.append(role, renderMarkdown(entry.text || ""));
  article.appendChild(bubble);
  return article;
}

function roleLabel(role) {
  if (role === "user") return "You";
  if (role === "assistant") return "Codex";
  return role || "Message";
}

function toolNode(entry) {
  const row = document.createElement("div");
  row.className = "process-row";
  const details = document.createElement("details");
  details.className = "process-card";
  const summary = document.createElement("summary");
  const chevron = document.createElement("span");
  chevron.className = "chevron";
  const title = document.createElement("span");
  title.className = "process-title";
  title.textContent = "Process - " + (entry.tool || "tool");
  const meta = document.createElement("span");
  meta.className = "process-meta";
  meta.textContent = [entry.status || "called", formatBytes(entry.output_bytes)].filter(Boolean).join(" - ");
  summary.append(chevron, title, meta);
  const input = document.createElement("pre");
  input.className = "tool-input";
  input.textContent = entry.input_preview || "No input preview";
  details.append(summary, input);
  row.appendChild(details);
  return row;
}

function formatBytes(value) {
  const bytes = Number(value || 0);
  if (!bytes) return "";
  if (bytes < 1024) return bytes + " B";
  if (bytes < 1024 * 1024) return Math.round(bytes / 1024) + " KB";
  return (bytes / (1024 * 1024)).toFixed(1) + " MB";
}

function renderMarkdown(text) {
  const root = document.createElement("div");
  root.className = "markdown";
  const lines = String(text || "").replace(/\\r\\n/g, "\\n").split("\\n");
  let i = 0;
  while (i < lines.length) {
    if (isBlank(lines[i])) {
      i += 1;
      continue;
    }
    if (lines[i].startsWith(fenceMarker)) {
      const codeLines = [];
      i += 1;
      while (i < lines.length && !lines[i].startsWith(fenceMarker)) {
        codeLines.push(lines[i]);
        i += 1;
      }
      if (i < lines.length) i += 1;
      root.appendChild(codeBlock(codeLines.join("\\n")));
      continue;
    }
    const heading = lines[i].match(/^(#{1,3})\\s+(.+)$/);
    if (heading) {
      const node = document.createElement("h" + heading[1].length);
      appendInline(node, heading[2]);
      root.appendChild(node);
      i += 1;
      continue;
    }
    if (/^>\\s?/.test(lines[i])) {
      const quoteLines = [];
      while (i < lines.length && /^>\\s?/.test(lines[i])) {
        quoteLines.push(lines[i].replace(/^>\\s?/, ""));
        i += 1;
      }
      const quote = document.createElement("blockquote");
      appendInlineWithBreaks(quote, quoteLines.join("\\n"));
      root.appendChild(quote);
      continue;
    }
    const list = listMatch(lines[i]);
    if (list) {
      const ordered = Boolean(list.ordered);
      const node = document.createElement(ordered ? "ol" : "ul");
      while (i < lines.length) {
        const item = listMatch(lines[i]);
        if (!item || Boolean(item.ordered) !== ordered) break;
        const li = document.createElement("li");
        appendInline(li, item.text);
        node.appendChild(li);
        i += 1;
      }
      root.appendChild(node);
      continue;
    }
    const paragraph = [];
    while (i < lines.length && !isBlank(lines[i]) && !isSpecialMarkdownStart(lines[i])) {
      paragraph.push(lines[i]);
      i += 1;
    }
    const p = document.createElement("p");
    appendInlineWithBreaks(p, paragraph.join("\\n"));
    root.appendChild(p);
  }
  if (!root.childNodes.length) {
    const p = document.createElement("p");
    p.textContent = "";
    root.appendChild(p);
  }
  return root;
}

function codeBlock(text) {
  const pre = document.createElement("pre");
  const code = document.createElement("code");
  code.textContent = text;
  pre.appendChild(code);
  return pre;
}

function isBlank(line) {
  return /^\\s*$/.test(line);
}

function isSpecialMarkdownStart(line) {
  return line.startsWith(fenceMarker) ||
    /^(#{1,3})\\s+/.test(line) ||
    /^>\\s?/.test(line) ||
    Boolean(listMatch(line));
}

function listMatch(line) {
  const unordered = line.match(/^\\s*[-*+]\\s+(.+)$/);
  if (unordered) return { ordered: false, text: unordered[1] };
  const ordered = line.match(/^\\s*\\d+[.)]\\s+(.+)$/);
  if (ordered) return { ordered: true, text: ordered[1] };
  return null;
}

function appendInlineWithBreaks(parent, text) {
  const parts = String(text || "").split("\\n");
  for (let i = 0; i < parts.length; i += 1) {
    if (i > 0) parent.appendChild(document.createElement("br"));
    appendInline(parent, parts[i]);
  }
}

function appendInline(parent, text) {
  const pattern = new RegExp("(\\\\x60[^\\\\x60]+\\\\x60)|(\\\\*\\\\*[^*]+\\\\*\\\\*)|(__[^_]+__)|(\\\\[[^\\\\]]+\\\\]\\\\(https?://[^\\\\s)]+\\\\))|(\\\\*[^*\\\\n]+\\\\*)|(_[^_\\\\n]+_)", "g");
  const source = String(text || "");
  let index = 0;
  let match;
  while ((match = pattern.exec(source)) !== null) {
    if (match.index > index) parent.appendChild(document.createTextNode(source.slice(index, match.index)));
    appendInlineToken(parent, match[0]);
    index = pattern.lastIndex;
  }
  if (index < source.length) parent.appendChild(document.createTextNode(source.slice(index)));
}

function appendInlineToken(parent, token) {
  if (token.charCodeAt(0) === 96 && token.charCodeAt(token.length - 1) === 96) {
    const code = document.createElement("code");
    code.textContent = token.slice(1, -1);
    parent.appendChild(code);
    return;
  }
  const link = token.match(/^\\[([^\\]]+)\\]\\((https?:\\/\\/[^\\s)]+)\\)$/);
  if (link) {
    const a = document.createElement("a");
    a.href = link[2];
    a.rel = "noreferrer";
    a.textContent = link[1];
    parent.appendChild(a);
    return;
  }
  if ((token.startsWith("**") && token.endsWith("**")) || (token.startsWith("__") && token.endsWith("__"))) {
    const strong = document.createElement("strong");
    strong.textContent = token.slice(2, -2);
    parent.appendChild(strong);
    return;
  }
  if ((token.startsWith("*") && token.endsWith("*")) || (token.startsWith("_") && token.endsWith("_"))) {
    const em = document.createElement("em");
    em.textContent = token.slice(1, -1);
    parent.appendChild(em);
    return;
  }
  parent.appendChild(document.createTextNode(token));
}

document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-copy]");
  if (!button) return;
  const node = $(button.dataset.copy);
  if (!node) return;
  await navigator.clipboard.writeText(node.textContent);
  const old = button.textContent;
  button.textContent = "Copied";
  setTimeout(() => { button.textContent = old; }, 1200);
});

async function boot() {
  try {
    const response = await fetch(location.pathname, { headers: { accept: "application/json" } });
    if (!response.ok) throw new Error("Link unavailable: " + response.status);
    const manifest = await response.json();
    renderManifestInfo(manifest);
    renderCommands(manifest.import || metadata.import);
    if (!manifest.preview) {
      $("counts").textContent = "Legacy link";
      setStatus("This older Capsule link has no browser preview. An agent can still import the full session into Codex.", "warn");
      return;
    }
    const key = fragmentKey();
    if (!key) {
      $("counts").textContent = "Missing key";
      setStatus("This link is missing the #k decryption key. Use the full URL that was produced by capsule share.", "warn");
      return;
    }
    const transcript = await decryptPreview(manifest.preview, key);
    renderTranscript(transcript);
    setStatus("Preview decrypted locally in this browser. The complete session stays in the encrypted Capsule until an agent imports it.");
  } catch (error) {
    $("counts").textContent = "Preview unavailable";
    setStatus(error && error.message ? error.message : String(error), "error");
  }
}

boot();
`;
}

async function gateJSON(env, path, body) {
  const response = await gate(env, path, body);
  return await response.json();
}

function gate(env, path, body) {
  const id = env.BUDGET_GATE.idFromName("global");
  const stub = env.BUDGET_GATE.get(id);
  return stub.fetch("https://budget.local" + path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body || {})
  });
}

function readLimits(env) {
  return {
    maxBlobBytes: envInt(env, "MAX_BLOB_BYTES", 2 * 1024 * 1024),
    maxTtlSeconds: envInt(env, "MAX_TTL_SECONDS", 24 * 60 * 60),
    maxDownloadsPerShare: envInt(env, "MAX_DOWNLOADS_PER_SHARE", 3),
    liveBytesLimit: envInt(env, "LIVE_BYTES_LIMIT", 4 * GB),
    monthlyGbDaysLimit: envInt(env, "MONTHLY_GB_DAYS_LIMIT", 120),
    monthlyPutLimit: envInt(env, "MONTHLY_PUT_LIMIT", 100000),
    monthlyGetLimit: envInt(env, "MONTHLY_GET_LIMIT", 1000000)
  };
}

function envInt(env, key, fallback) {
  const value = Number(env[key]);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

function json(value, status = 200) {
  return cors(new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json" }
  }));
}

function html(value, status = 200) {
  return cors(new Response("<!doctype html><meta charset=\"utf-8\"><title>Agent Capsule</title>" + value, {
    status,
    headers: { "content-type": "text/html; charset=utf-8" }
  }));
}

function htmlDocument(value, status = 200) {
  return cors(new Response(value, {
    status,
    headers: { "content-type": "text/html; charset=utf-8" }
  }));
}

function cors(response) {
  response.headers.set("access-control-allow-origin", "*");
  response.headers.set("access-control-allow-methods", "GET,POST,OPTIONS");
  response.headers.set("access-control-allow-headers", "authorization,content-type,accept");
  return response;
}

function positiveInt(value) {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? Math.floor(number) : 0;
}

function validID(value) {
  return typeof value === "string" && /^[A-Za-z0-9_-]{1,80}$/.test(value);
}

function dayKey() {
  return new Date().toISOString().slice(0, 10);
}

function monthKey() {
  return new Date().toISOString().slice(0, 7);
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;"
  })[ch]);
}

function scriptJSON(value) {
  return JSON.stringify(value).replace(/[<>&]/g, (ch) => ({
    "<": "\\u003c",
    ">": "\\u003e",
    "&": "\\u0026"
  })[ch]);
}
