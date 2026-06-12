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
  <div class="page-shell">
    <header class="topbar">
      <div>
        <div class="brand">Capsule</div>
        <p class="tagline">Encrypted Codex session preview</p>
      </div>
      <a class="docs-link" href="${escapeHTML(manifest.import.docs_url)}" rel="noreferrer">Docs</a>
    </header>

    <main class="layout">
      <section class="conversation-panel" aria-labelledby="session-title">
        <p class="eyebrow">Shared session</p>
        <h1 id="session-title">${escapeHTML(title)}</h1>
        <p class="preview-note">This page is a readable preview. To restore the full session, ask an agent to import this link into your own Codex native UI.</p>
        <div class="meta-row">
          <span id="expires-at">Encrypted link</span>
          <span id="counts">Waiting for preview</span>
        </div>
        <div id="status" class="status">Loading encrypted preview from this link.</div>
        <div id="transcript" class="transcript" aria-live="polite"></div>
      </section>

      <aside class="agent-panel" aria-labelledby="agent-title">
        <p class="eyebrow">For agents</p>
        <h2 id="agent-title">Restore in Codex</h2>
        <p class="agent-copy">Give this URL to a coding agent. It can install the importer, dry-run the write, then import the complete session as a new Codex thread.</p>

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
      </aside>
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
  --bg: #f7f7f4;
  --panel: #ffffff;
  --ink: #151614;
  --muted: #62665f;
  --line: #dedfd8;
  --soft: #eceee8;
  --accent: #2f6f68;
  --accent-ink: #0f312d;
  --code-bg: #101413;
  --code-ink: #eef5ef;
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
.page-shell { width: min(1180px, calc(100% - 32px)); margin: 0 auto; padding: 28px 0 56px; }
.topbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 34px; }
.brand { font-weight: 750; font-size: 18px; letter-spacing: 0; }
.tagline, .agent-copy, .preview-note { margin: 4px 0 0; color: var(--muted); }
.docs-link {
  color: var(--accent-ink);
  text-decoration: none;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 8px 13px;
  background: rgba(255,255,255,.65);
}
.layout { display: grid; grid-template-columns: minmax(0, 1fr) 360px; gap: 28px; align-items: start; }
.conversation-panel, .agent-panel {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  box-shadow: 0 18px 50px rgba(21,22,20,.06);
}
.conversation-panel { padding: 32px; min-width: 0; }
.agent-panel { padding: 22px; position: sticky; top: 20px; }
.eyebrow {
  margin: 0 0 10px;
  color: var(--accent);
  font-size: 12px;
  font-weight: 720;
  text-transform: uppercase;
}
h1 { margin: 0; font-size: clamp(30px, 5vw, 58px); line-height: 1.05; letter-spacing: 0; max-width: 920px; overflow-wrap: anywhere; }
h2 { margin: 0; font-size: 22px; line-height: 1.2; letter-spacing: 0; }
.preview-note { max-width: 68ch; margin-top: 18px; font-size: 16px; }
.meta-row { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 20px; }
.meta-row span {
  border: 1px solid var(--line);
  background: var(--soft);
  border-radius: 999px;
  padding: 6px 10px;
  color: var(--muted);
  font-size: 13px;
}
.status {
  margin-top: 18px;
  border-left: 3px solid var(--accent);
  background: #edf5f2;
  color: var(--accent-ink);
  padding: 12px 14px;
  border-radius: 6px;
}
.transcript { margin-top: 26px; display: grid; gap: 16px; }
.entry {
  border-top: 1px solid var(--line);
  padding-top: 16px;
  min-width: 0;
}
.role, .tool-label { color: var(--muted); font-size: 12px; font-weight: 720; text-transform: uppercase; margin-bottom: 8px; }
.message-text, .tool-input, .command-block pre {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
}
.message-text {
  font-family: inherit;
  font-size: 16px;
  color: var(--ink);
}
details.tool {
  background: #fafaf8;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 12px 14px;
}
details.tool summary { cursor: pointer; color: var(--ink); font-weight: 650; }
.tool-input { margin-top: 12px; color: var(--muted); font-size: 13px; }
.agent-panel .eyebrow { margin-bottom: 8px; }
.agent-copy { font-size: 14px; margin: 10px 0 18px; }
.command-block {
  border: 1px solid var(--line);
  border-radius: 8px;
  overflow: hidden;
  margin-top: 12px;
  background: var(--code-bg);
}
.command-block.emphasized { border-color: rgba(47,111,104,.55); }
.command-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  background: #f0f2ed;
  color: var(--ink);
  padding: 8px 10px;
  font-size: 13px;
  font-weight: 680;
}
.command-head button {
  border: 1px solid var(--line);
  background: white;
  color: var(--accent-ink);
  border-radius: 999px;
  padding: 4px 9px;
  cursor: pointer;
}
.command-block pre {
  color: var(--code-ink);
  padding: 13px;
  font-size: 12px;
}
@media (max-width: 880px) {
  .page-shell { width: min(100% - 20px, 720px); padding-top: 18px; }
  .layout { grid-template-columns: 1fr; }
  .agent-panel { position: static; order: -1; }
  .conversation-panel { padding: 22px; }
}
`;
}

function sharePageJS() {
  return `
const metadata = JSON.parse(document.getElementById("agent-capsule-metadata").textContent);
const $ = (id) => document.getElementById(id);

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
  $("counts").textContent = transcript.message_count + " messages - " + transcript.tool_count + " tool summaries";
  const root = $("transcript");
  root.replaceChildren();
  if (transcript.truncated) {
    const note = document.createElement("div");
    note.className = "status";
    note.textContent = "This preview is truncated. Import the Capsule to continue in the complete Codex thread.";
    root.appendChild(note);
  }
  for (const entry of transcript.entries || []) {
    root.appendChild(entry.kind === "tool" ? toolNode(entry) : messageNode(entry));
  }
}

function messageNode(entry) {
  const article = document.createElement("article");
  article.className = "entry message " + (entry.role || "");
  const role = document.createElement("div");
  role.className = "role";
  role.textContent = entry.role || "message";
  const text = document.createElement("div");
  text.className = "message-text";
  text.textContent = entry.text || "";
  article.append(role, text);
  return article;
}

function toolNode(entry) {
  const details = document.createElement("details");
  details.className = "entry tool";
  const summary = document.createElement("summary");
  const output = entry.output_bytes ? " - " + entry.output_bytes + " bytes output" : "";
  summary.textContent = (entry.tool || "tool") + " - " + (entry.status || "called") + output;
  const input = document.createElement("pre");
  input.className = "tool-input";
  input.textContent = entry.input_preview || "No input preview";
  details.append(summary, input);
  return details;
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
