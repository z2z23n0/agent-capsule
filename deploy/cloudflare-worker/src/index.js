const LINK_SCHEMA = "agent-capsule.link.v1";
const GB = 1024 * 1024 * 1024;
const DEFAULT_INSTALL_COMMAND = "go install github.com/z2z23n0/agent-capsule/cmd/capsule@main";
const DEFAULT_DOCS_URL = "https://github.com/z2z23n0/agent-capsule";
const DEFAULT_SKILL_URL = "https://github.com/z2z23n0/agent-capsule/tree/main/skills/agent-capsule";
const DEFAULT_EXECUTE_COMMAND = "capsule import \"<this-url>\" --target codex --target-cwd . --execute";
const SHARE_ID_BYTES = 12;
const SHARE_ID_MAX_ATTEMPTS = 5;
const STORAGE_ID_BYTES = 12;

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
    const blobBytes = positiveInt(input.blob_bytes || input.bytes);
    const limits = readLimits(this.env);
    if (blobBytes <= 0 || blobBytes > limits.maxBlobBytes) {
      return { ok: false, status: 413, error: "blob_too_large" };
    }
    if (bytes <= 0 || bytes > limits.maxShareBytes) {
      return { ok: false, status: 413, error: "share_too_large" };
    }
    const budget = await this.rollBudget(await this.loadBudget());
    const nextLive = budget.liveBytes + bytes;
    const nextPeak = Math.max(budget.todayPeakBytes, nextLive);
    const projectedGbDays = budget.gbDays + nextPeak / GB;
    if (nextLive > limits.liveBytesLimit) return { ok: false, status: 507, error: "live_bytes_limit" };
    if (projectedGbDays > limits.monthlyGbDaysLimit) return { ok: false, status: 507, error: "monthly_gb_days_limit" };
    budget.liveBytes = nextLive;
    budget.todayPeakBytes = nextPeak;
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
    if (await this.state.storage.get("share:" + share.id)) {
      return { ok: false, status: 409, error: "share_exists" };
    }
    const budget = await this.rollBudget(await this.loadBudget());
    const limits = readLimits(this.env);
    if (budget.puts + 1 > limits.monthlyPutLimit) {
      return { ok: false, status: 507, error: "monthly_put_limit" };
    }
    budget.puts += 1;
    await this.state.storage.put("share:" + share.id, share);
    await this.saveBudget(budget);
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
          auth_required: uploadAuthRequired(env)
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
  if (!(await verifyUploadToken(request, env))) return json({ ok: false, error: "unauthorized" }, 401);
  const requestBytes = positiveInt(request.headers.get("content-length"));
  if (requestBytes > 0 && requestBytes > limits.maxRequestBytes) {
    return json({ ok: false, error: "request_too_large" }, 413);
  }
  const form = await request.formData();
  const blob = form.get("blob");
  const manifestText = form.get("manifest");
  if (!blob || typeof blob.arrayBuffer !== "function") return json({ ok: false, error: "missing_blob" }, 400);
  if (!manifestText) return json({ ok: false, error: "missing_manifest" }, 400);
  const blobBytes = blob.size || 0;
  const manifestBytes = byteLength(String(manifestText));
  if (manifestBytes > limits.maxManifestBytes) return json({ ok: false, error: "manifest_too_large" }, 413);

  let manifest;
  try {
    manifest = normalizeManifest(JSON.parse(String(manifestText)), limits, origin);
  } catch (error) {
    return json({ ok: false, error: String(error && error.message ? error.message : error) }, 400);
  }

  const accountedBytes = blobBytes + manifestBytes;
  const reserve = await gateJSON(env, "/reserve", { bytes: accountedBytes, blob_bytes: blobBytes });
  if (!reserve.ok) return json({ ok: false, error: reserve.error }, reserve.status || 507);

  try {
    const data = await blob.arrayBuffer();
    const expiresAt = new Date(Date.now() + limits.maxTtlSeconds * 1000).toISOString();
    for (let attempt = 0; attempt < SHARE_ID_MAX_ATTEMPTS; attempt += 1) {
      const id = randomBase64URL(SHARE_ID_BYTES);
      const objectKey = "shares/" + id + "/" + randomBase64URL(STORAGE_ID_BYTES) + "/blob.enc";
      manifest.expires_at = expiresAt;
      manifest.bundle.bytes = blobBytes;
      manifest.bundle.url = origin + "/v1/shares/" + id + "/blob";
      manifest.service = { type: "worker", quota_policy: "anonymous-small" };
      await env.CAPSULE_BUCKET.put(objectKey, data, {
        httpMetadata: { contentType: "application/octet-stream" }
      });
      const commit = await gateJSON(env, "/commit", {
        share: {
          id,
          object_key: objectKey,
          bytes: accountedBytes,
          blob_bytes: blobBytes,
          manifest_bytes: manifestBytes,
          downloads: 0,
          expires_at: expiresAt,
          manifest
        }
      });
      if (commit.ok) {
        return json({
          share_url: origin + "/s/" + id,
          manifest_url: origin + "/v1/shares/" + id,
          expires_at: expiresAt
        }, 201);
      }
      await env.CAPSULE_BUCKET.delete(objectKey);
      if (commit.error !== "share_exists") {
        throw new Error(commit.error || "metadata_commit_failed");
      }
    }
    throw new Error("share_id_collision");
  } catch (error) {
    await gateJSON(env, "/release", { bytes: accountedBytes });
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
  out.import = importInfo();
  return out;
}

function importInfo() {
  return {
    tool: "capsule",
    command: quoteThisURL(DEFAULT_EXECUTE_COMMAND),
    install_command: DEFAULT_INSTALL_COMMAND,
    execute_command: quoteThisURL(DEFAULT_EXECUTE_COMMAND),
    docs_url: DEFAULT_DOCS_URL,
    skill_url: DEFAULT_SKILL_URL
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
  <title>${escapeHTML(title)} - Agent Capsule preview</title>
  <style>${sharePageCSS()}</style>
</head>
<body>
  <script id="agent-capsule-metadata" type="application/agent-capsule+json">${scriptJSON(metadata)}</script>
  <main class="share-layout">
    <section class="preview-column" aria-label="Capsule preview">
      <header class="preview-header">
        <p class="preview-kicker">Capsule preview</p>
        <h1 id="page-title">${escapeHTML(title)}</h1>
        <p class="preview-subtitle">这里默认先显示轻量预览；也可以在本页加载完整可见 transcript，或交给 agent 导入到原生 UI 继续。</p>
        <p class="preview-meta" aria-live="polite">
          <span id="counts">正在等待预览</span>
          <span id="expires-at">加密链接</span>
        </p>
        <hr class="preview-rule">
        <p id="status" class="status">正在读取这个链接里的加密预览。</p>
        <div id="full-transcript-actions" class="preview-actions" hidden>
          <button id="load-full-transcript" class="secondary-action" type="button">加载完整会话</button>
          <span id="full-transcript-status" class="preview-action-status" aria-live="polite"></span>
        </div>
      </header>
      <section id="transcript" class="codex-thread" aria-label="Session preview" aria-live="polite"></section>
    </section>

    <aside class="agents-panel" aria-labelledby="agents-title">
      <section class="agents-card">
        <p class="agents-kicker">FOR AGENTS</p>
        <h2 id="agents-title">Restore in Codex</h2>
        <p class="agents-copy">Give this URL to a coding agent. It can install the importer and import the complete session as a new Codex thread.</p>

        <div class="command-block">
          <div class="command-head">
            <span>Install</span>
            <button type="button" data-copy="install-command">Copy</button>
          </div>
          <pre id="install-command"></pre>
        </div>

        <div class="command-block emphasized">
          <div class="command-head">
            <span>Import</span>
            <button type="button" data-copy="execute-command">Copy</button>
          </div>
          <pre id="execute-command"></pre>
        </div>
      </section>
    </aside>
  </main>
  <script>${sharePageJS()}</script>
</body>
</html>`;
}

function sharePageCSS() {
  return `
:root {
  color-scheme: light;
  --page: #ffffff;
  --ink: #1f2328;
  --muted: #8a8f98;
  --muted-strong: #686d75;
  --line: #eceef0;
  --line-strong: #d8dce0;
  --bubble: #f1f2f4;
  --panel: #eeeeef;
  --panel-soft: #f7f7f8;
  --code-bg: #eceeef;
  --command-bg: #101513;
  --command-ink: #eef3ef;
  --link: #1f73d2;
  --accent: #2f6f66;
  --warn: #86651f;
  --error: #a33a32;
  --shadow: 0 18px 48px rgba(26, 33, 43, 0.08);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100dvh;
  background: var(--page);
  color: var(--ink);
  font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  line-height: 1.6;
}
button, a { font: inherit; }
.share-layout {
  width: min(1500px, calc(100vw - 72px));
  margin: 44px auto 72px;
  display: grid;
  grid-template-columns: minmax(0, 1fr) 430px;
  column-gap: 48px;
  align-items: start;
}
.preview-column {
  min-width: 0;
}
.preview-header {
  padding: 0 44px 0 72px;
}
.preview-kicker {
  margin: 0 0 4px;
  color: #969da8;
  font-size: 15px;
  font-weight: 700;
  letter-spacing: .01em;
}
#page-title {
  margin: 0 0 12px;
  font-size: clamp(28px, 3vw, 38px);
  line-height: 1.18;
  letter-spacing: 0;
  font-weight: 780;
}
.preview-subtitle {
  max-width: 880px;
  margin: 0;
  color: var(--muted-strong);
  font-size: 18px;
  font-weight: 560;
}
.preview-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 0 18px;
  margin: 10px 0 0;
  color: var(--muted);
  font-size: 15px;
  font-weight: 600;
}
.preview-rule {
  border: 0;
  border-top: 1px solid var(--line);
  margin: 30px 0 28px;
}
.status {
  max-width: 760px;
  margin: 0;
  color: var(--muted-strong);
  font-size: 17px;
  line-height: 1.55;
  font-weight: 560;
}
.status[data-kind="success"] { color: var(--muted-strong); }
.status[data-kind="warn"] { color: var(--warn); }
.status[data-kind="error"] { color: var(--error); }
.preview-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 14px;
  margin-top: 18px;
}
.secondary-action {
  min-height: 38px;
  border: 1px solid var(--line-strong);
  border-radius: 8px;
  background: #fff;
  color: #24302e;
  padding: 6px 14px;
  cursor: pointer;
  font-size: 15px;
  font-weight: 760;
}
.secondary-action:hover {
  border-color: #b8c0c7;
  background: #f8fafb;
}
.secondary-action:active {
  transform: translateY(1px);
}
.secondary-action:disabled {
  cursor: progress;
  opacity: .68;
  transform: none;
}
.preview-action-status {
  color: var(--muted);
  font-size: 15px;
  font-weight: 600;
}
.codex-thread {
  display: block;
  min-width: 0;
  margin-top: 82px;
  padding: 0 44px 0 72px;
}
.agents-panel {
  position: sticky;
  top: 28px;
  margin-top: 178px;
}
.agents-card {
  border: 1px solid var(--line-strong);
  border-radius: 8px;
  background: #fff;
  box-shadow: var(--shadow);
  padding: 38px 30px 32px;
}
.agents-kicker {
  margin: 0 0 10px;
  color: var(--accent);
  font-size: 16px;
  font-weight: 800;
  letter-spacing: .04em;
}
.agents-card h2 {
  margin: 0 0 18px;
  color: #24282f;
  font-size: 34px;
  line-height: 1.12;
  letter-spacing: 0;
}
.agents-copy {
  margin: 0 0 32px;
  color: var(--muted-strong);
  font-size: 20px;
  line-height: 1.45;
  font-weight: 520;
}
.message-row {
  display: flex;
  min-width: 0;
  margin: 34px 0;
}
.message-row.user {
  justify-content: flex-end;
  margin-top: 4px;
  margin-bottom: 86px;
}
.message-row.assistant { justify-content: flex-start; }
.bubble {
  min-width: 0;
  max-width: 100%;
}
.message-row.user .bubble {
  width: fit-content;
  max-width: min(1080px, 78%);
  background: var(--bubble);
  border-radius: 28px;
  padding: 20px 28px;
}
.skill-message {
  min-width: 0;
  color: var(--ink);
  font-size: 24px;
  line-height: 1.42;
  overflow-wrap: anywhere;
}
.skill-chip {
  display: inline-block;
  max-width: 100%;
  margin-right: 8px;
  vertical-align: middle;
}
.skill-chip > summary {
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  min-height: 32px;
  list-style: none;
  color: var(--link);
  font-weight: 650;
  line-height: 1;
  white-space: nowrap;
}
.skill-chip > summary::-webkit-details-marker { display: none; }
.skill-chip[open] {
  display: block;
  margin: 0 0 12px;
}
.skill-icon {
  display: inline-flex;
  width: 18px;
  height: 18px;
  color: var(--link);
  flex: 0 0 auto;
}
.skill-icon svg {
  width: 18px;
  height: 18px;
  display: block;
}
.skill-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}
.skill-message-text {
  vertical-align: middle;
}
.skill-chip-body {
  margin: 12px 0 0;
  border-radius: 8px;
  background: #e7e9ec;
  color: var(--ink);
  padding: 16px;
  max-height: 58vh;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
  font-size: 15px;
  line-height: 1.55;
}
.message-row.assistant .bubble {
  width: 100%;
  max-width: 100%;
}
.role {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
}
.markdown {
  font-size: 24px;
  line-height: 1.62;
  color: var(--ink);
  min-width: 0;
  overflow-wrap: anywhere;
}
.message-row.user .markdown {
  font-size: 24px;
  line-height: 1.42;
}
.markdown > *:first-child { margin-top: 0; }
.markdown > *:last-child { margin-bottom: 0; }
.markdown p { margin: 0 0 16px; overflow-wrap: anywhere; }
.message-row.user .markdown p { margin-bottom: 8px; }
.markdown h1, .markdown h2, .markdown h3 {
  margin: 22px 0 10px;
  font-size: 1.06em;
  line-height: 1.35;
  overflow-wrap: anywhere;
}
.markdown ul, .markdown ol {
  margin: 10px 0 18px;
  padding-left: 26px;
}
.markdown li {
  margin: 8px 0;
  padding-left: 2px;
  overflow-wrap: anywhere;
}
.markdown blockquote {
  margin: 14px 0;
  padding: 2px 0 2px 14px;
  border-left: 3px solid var(--line-strong);
  color: var(--muted);
}
.markdown a,
.file-link {
  color: var(--link);
  text-decoration: none;
}
.markdown a:hover { text-decoration: underline; text-underline-offset: 2px; }
.file-link { font-weight: 500; }
.markdown code, .terminal-body, .command-block pre {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
}
.markdown code {
  border-radius: 9px;
  background: var(--code-bg);
  padding: 1px 8px;
  font-size: .91em;
}
.markdown pre {
  margin: 14px 0;
  padding: 14px;
  overflow-x: auto;
  white-space: pre;
  background: var(--panel);
  color: var(--ink);
  border-radius: 8px;
}
.markdown pre code {
  border-radius: 0;
  background: transparent;
  padding: 0;
  color: inherit;
}
.image-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(148px, 1fr));
  gap: 10px;
  margin-top: 12px;
  max-width: min(760px, 100%);
}
.message-row.user .image-grid {
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
}
.preview-image {
  display: block;
  width: 100%;
  max-height: 420px;
  object-fit: contain;
  border: 1px solid var(--line);
  border-radius: 8px;
  background: #fff;
}
.image-omitted {
  display: grid;
  place-items: center;
  min-height: 104px;
  padding: 12px;
  border: 1px dashed var(--line-strong);
  border-radius: 8px;
  color: var(--muted-strong);
  background: var(--panel-soft);
  font-size: 14px;
  text-align: center;
}
.turn-process-row {
  margin: 0 0 54px;
  padding-bottom: 22px;
  border-bottom: 1px solid var(--line);
}
.turn-process > summary {
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 12px;
  min-height: 40px;
  list-style: none;
  color: var(--muted);
  font-size: 24px;
  line-height: 1.3;
}
.turn-process > summary::-webkit-details-marker { display: none; }
.turn-process-title {
  font-weight: 560;
}
.turn-process-body {
  display: grid;
  gap: 28px;
  margin-top: 34px;
}
.process-message {
  max-width: 100%;
}
.process-message .markdown {
  color: var(--ink);
}
.tool-group {
  color: var(--muted);
}
.tool-group > summary,
.tool-action > summary {
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 12px;
  min-height: 34px;
  list-style: none;
  color: var(--muted);
  font-size: 23px;
  line-height: 1.35;
}
.tool-group > summary::-webkit-details-marker,
.tool-action > summary::-webkit-details-marker { display: none; }
.tool-action-list {
  display: grid;
  gap: 14px;
  margin: 10px 0 0 0;
}
.tool-action {
  min-width: 0;
}
.tool-action > summary {
  margin-left: 0;
  font-size: 21px;
}
.tool-action-title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.terminal-body, .command-block pre {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0;
}
.process-icon {
  display: inline-grid;
  place-items: center;
  width: 22px;
  height: 22px;
  border: 1.5px solid currentColor;
  border-radius: 6px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
  font-size: 13px;
  line-height: 1;
  flex: 0 0 auto;
}
.chevron {
  width: 8px;
  height: 8px;
  border-right: 1.5px solid currentColor;
  border-bottom: 1.5px solid currentColor;
  transform: rotate(45deg);
  transition: transform .15s ease;
  margin-left: 2px;
  flex: 0 0 auto;
}
.turn-process[open] > summary .chevron,
.tool-group[open] > summary .chevron,
.tool-action[open] > summary .chevron { transform: rotate(-135deg); }
.process-panel {
  margin: 14px 0 10px 0;
  border-radius: 12px;
  background: var(--panel);
  color: var(--ink);
  overflow: hidden;
}
.process-panel-head {
  padding: 16px 20px 0;
  color: var(--muted-strong);
  font-size: 22px;
}
.terminal-body {
  padding: 24px 20px 18px;
  font-size: 22px;
  line-height: 1.55;
  max-height: 70vh;
  overflow: auto;
}
.process-result {
  display: flex;
  justify-content: flex-end;
  padding: 0 20px 18px;
  color: var(--muted);
  font-size: 22px;
}
.command-block {
  margin-top: 18px;
  border: 1px solid #e0e4e9;
  border-radius: 8px;
  overflow: hidden;
  background: #f7f8fa;
  min-width: 0;
}
.command-block.emphasized { border-color: rgba(47,111,102,.65); }
.command-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  background: #f7f7f8;
  color: var(--ink);
  min-height: 50px;
  padding: 0 12px 0 16px;
  font-size: 16px;
  font-weight: 760;
}
.command-head button {
  border: 1px solid #e1e6eb;
  background: white;
  color: #123d37;
  border-radius: 999px;
  padding: 5px 14px;
  cursor: pointer;
  white-space: nowrap;
  font-size: 14px;
  font-weight: 760;
}
.command-head button:active {
  transform: translateY(1px);
}
.command-block pre {
  background: var(--command-bg);
  color: var(--command-ink);
  padding: 18px;
  font-size: 15px;
  line-height: 1.5;
}
@media (max-width: 1120px) {
  .share-layout {
    width: min(940px, calc(100vw - 40px));
    grid-template-columns: 1fr;
  }
  .preview-header,
  .codex-thread {
    padding: 0;
  }
  .agents-panel {
    position: static;
    grid-row: 2;
    margin-top: 48px;
  }
  .codex-thread {
    grid-row: 3;
    margin-top: 56px;
  }
}
@media (max-width: 760px) {
  .share-layout {
    width: min(100% - 28px, 640px);
    margin-top: 24px;
  }
  #page-title { font-size: 28px; }
  .preview-subtitle,
  .status,
  .agents-copy { font-size: 16px; }
  .codex-thread { margin-top: 44px; }
  .message-row { margin: 24px 0; }
  .message-row.user { margin-bottom: 38px; }
  .message-row.user .bubble {
    max-width: 92%;
    padding: 12px 15px;
    border-radius: 18px;
  }
  .skill-message { font-size: 15px; }
  .skill-chip > summary {
    min-height: 26px;
    gap: 6px;
  }
  .skill-icon,
  .skill-icon svg {
    width: 16px;
    height: 16px;
  }
  .skill-chip-body {
    padding: 12px;
    font-size: 12px;
  }
  .markdown { font-size: 16px; line-height: 1.68; }
  .message-row.user .markdown { font-size: 15px; }
  .turn-process-row {
    margin-bottom: 34px;
    padding-bottom: 16px;
  }
  .turn-process > summary,
  .tool-group > summary {
    font-size: 16px;
  }
  .turn-process-body {
    gap: 18px;
    margin-top: 22px;
  }
  .tool-action > summary {
    font-size: 15px;
  }
  .agents-card {
    padding: 28px 20px 24px;
  }
  .agents-card h2 {
    font-size: 29px;
  }
  .process-panel { border-radius: 10px; }
  .process-panel-head,
  .terminal-body,
  .process-result {
    font-size: 14px;
  }
  .terminal-body {
    padding: 18px 14px 12px;
    max-height: 68vh;
  }
  .process-panel-head { padding: 12px 14px 0; }
  .process-result { padding: 0 14px 12px; }
  .process-icon {
    width: 18px;
    height: 18px;
    font-size: 11px;
  }
}
`;
}

function sharePageJS() {
  return `
const metadata = JSON.parse(document.getElementById("agent-capsule-metadata").textContent);
const $ = (id) => document.getElementById(id);
const fenceMarker = String.fromCharCode(96, 96, 96);
let activeManifest = null;
let activeKey = "";
let activeTranscriptSource = "";

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
  node.hidden = false;
}

function renderCommands(importInfo) {
  $("install-command").textContent = importInfo.install_command || metadata.import.install_command;
  $("execute-command").textContent = commandText(importInfo.execute_command || importInfo.command || metadata.import.execute_command);
}

function renderManifestInfo(manifest) {
  if (manifest.thread && manifest.thread.title) {
    $("page-title").textContent = manifest.thread.title;
    document.title = manifest.thread.title + " - Agent Capsule preview";
  }
  $("expires-at").textContent = manifest.expires_at ? "过期时间 " + new Date(manifest.expires_at).toLocaleString() : "加密链接";
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

async function decryptBundle(manifest, keyText) {
  if (!crypto.subtle) throw new Error("WebCrypto is unavailable in this browser");
  if (!manifest || !manifest.bundle || !manifest.bundle.url) throw new Error("这个链接缺少完整 bundle。");
  const response = await fetch(manifest.bundle.url);
  if (!response.ok) throw new Error("完整 bundle 下载失败: " + response.status);
  const ciphertext = new Uint8Array(await response.arrayBuffer());
  if (manifest.bundle.bytes && ciphertext.byteLength !== manifest.bundle.bytes) {
    throw new Error("完整 bundle 字节数不匹配。");
  }
  const digest = await crypto.subtle.digest("SHA-256", ciphertext);
  const actualSHA256 = hexFromBytes(new Uint8Array(digest));
  const expectedSHA256 = String(manifest.bundle.sha256 || "").toLowerCase();
  if (expectedSHA256 && actualSHA256 !== expectedSHA256) {
    throw new Error("完整 bundle sha256 校验失败。");
  }
  const keyBytes = base64urlToBytes(keyText);
  const nonce = base64urlToBytes(manifest.crypto && manifest.crypto.nonce || "");
  const key = await crypto.subtle.importKey("raw", keyBytes, { name: "AES-GCM" }, false, ["decrypt"]);
  return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, key, ciphertext));
}

function hexFromBytes(bytes) {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
}

function renderTranscript(transcript, options = {}) {
  activeTranscriptSource = String(transcript.source || transcript.source_agent || "");
  const entries = (transcript.entries || []).filter((entry) => !isInternalContextEntry(entry));
  const messageCount = entries.filter((entry) => entry.kind === "message").length;
  const toolCount = entries.filter((entry) => entry.kind === "tool").length;
  const imageCount = entries.reduce((count, entry) => count + (entry.images || []).filter((image) => !image.omitted).length, 0);
  const omittedImages = entries.reduce((count, entry) => count + Number(entry.omitted_images || 0), 0);
  const scope = options.complete ? "完整会话" : "预览";
  $("counts").textContent = [scope, messageCount + " 条消息", toolCount + " 个过程步骤", imageCount ? imageCount + " 张图片" : "", omittedImages ? "省略 " + omittedImages + " 张图片" : ""].filter(Boolean).join(" - ");
  const root = $("transcript");
  root.replaceChildren();
  if (entries.length === 0) {
    const empty = document.createElement("div");
    empty.className = "status";
    empty.textContent = "这个 session 没有可公开预览的消息。agent 仍然可以把完整 Capsule 导入到 Codex。";
    root.appendChild(empty);
    return;
  }
  renderThreadEntries(root, entries);
}

function renderThreadEntries(root, entries) {
  let turn = [];
  const flushTurn = () => {
    if (!turn.length) return;
    appendAssistantTurn(root, turn);
    turn = [];
  };
  for (const entry of entries) {
    if (entry.kind === "message" && entry.role === "user") {
      flushTurn();
      root.appendChild(messageNode(entry));
      continue;
    }
    turn.push(entry);
  }
  flushTurn();
}

function appendAssistantTurn(root, entries) {
  const finalIndex = lastAssistantMessageIndex(entries);
  const processEntries = finalIndex >= 0 ? entries.slice(0, finalIndex).concat(entries.slice(finalIndex + 1)) : entries.slice();
  const finalMessage = finalIndex >= 0 ? entries[finalIndex] : null;
  if (processEntries.length) root.appendChild(turnProcessNode(processEntries));
  if (finalMessage) root.appendChild(messageNode(finalMessage));
}

function lastAssistantMessageIndex(entries) {
  for (let i = entries.length - 1; i >= 0; i -= 1) {
    if (entries[i].kind === "message" && entries[i].role === "assistant") return i;
  }
  return -1;
}

function isInternalContextEntry(entry) {
  if (!entry || entry.kind !== "message") return false;
  const text = String(entry.text || "").trim();
  return text.startsWith("# AGENTS.md instructions for ") ||
    text.startsWith("<codex_internal_context") ||
    text.startsWith("<environment_context>") ||
    text.startsWith("<INSTRUCTIONS>") ||
    text.startsWith("<skill>");
}

function messageNode(entry) {
  const article = document.createElement("article");
  article.className = "message-row " + (entry.role || "");
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  const role = document.createElement("div");
  role.className = "role";
  role.textContent = roleLabel(entry.role);
  bubble.appendChild(role);
  const skills = Array.isArray(entry.skills) ? entry.skills : [];
  if (entry.role === "user" && skills.length) {
    bubble.classList.add("with-skills");
    bubble.appendChild(skillMessageNode(entry, skills));
  } else if (String(entry.text || "").trim()) {
    bubble.appendChild(renderMarkdown(entry.text || ""));
  }
  if ((entry.images || []).length || entry.omitted_images) bubble.appendChild(imageGallery(entry));
  article.appendChild(bubble);
  return article;
}

function skillMessageNode(entry, skills) {
  const node = document.createElement("div");
  node.className = "skill-message";
  for (const skill of skills) node.appendChild(skillDetailsNode(skill));
  const text = stripSkillInvocation(entry.text || "", skills).trim();
  if (text) {
    const span = document.createElement("span");
    span.className = "skill-message-text";
    appendInlineWithBreaks(span, text);
    node.appendChild(span);
  }
  return node;
}

function skillDetailsNode(skill) {
  const details = document.createElement("details");
  details.className = "skill-chip";
  const summary = document.createElement("summary");
  const label = document.createElement("span");
  label.className = "skill-name";
  label.textContent = formatSkillName(skill && skill.name);
  summary.append(skillIconNode(), label);
  const body = document.createElement("pre");
  body.className = "skill-chip-body";
  body.textContent = String((skill && skill.text) || (skill && skill.path) || "");
  details.append(summary, body);
  return details;
}

function skillIconNode() {
  const icon = document.createElement("span");
  icon.className = "skill-icon";
  icon.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M12 3.4 5.2 7.1v9.7l6.8 3.8 6.8-3.8V7.1L12 3.4Z" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linejoin="round"/><path d="m5.6 7.4 6.4 3.5 6.4-3.5M12 10.9v9.1" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  return icon;
}

function formatSkillName(name) {
  const text = String(name || "skill").trim().replace(/^[$@]/, "");
  return text.split(/[-_\\s]+/).filter(Boolean).map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ") || "Skill";
}

function stripSkillInvocation(text, skills) {
  let out = String(text || "");
  out = out.replace(/^\\s*\\[([^\\]]+)\\]\\(([^)]+)\\)\\s*/, (match, label) => {
    const normalized = String(label || "").replace(/^[$@]/, "").toLowerCase();
    const matched = skills.some((skill) => normalized === String(skill && skill.name || "").toLowerCase());
    return matched ? "" : match;
  });
  return out;
}

function imageGallery(entry) {
  const grid = document.createElement("div");
  grid.className = "image-grid";
  for (const image of entry.images || []) {
    if (image.omitted) {
      grid.appendChild(omittedImageNode(1));
      continue;
    }
    if (!/^data:image\\//.test(String(image.src || ""))) continue;
    const img = document.createElement("img");
    img.className = "preview-image";
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = image.alt || "Uploaded image";
    img.src = image.src;
    grid.appendChild(img);
  }
  if (entry.omitted_images) grid.appendChild(omittedImageNode(Number(entry.omitted_images || 0)));
  return grid;
}

function omittedImageNode(count) {
  const node = document.createElement("div");
  node.className = "image-omitted";
  node.textContent = count > 1 ? "已省略 " + count + " 张图片，导入后可查看完整图片。" : "已省略 1 张图片，导入后可查看完整图片。";
  return node;
}

function roleLabel(role) {
  if (role === "user") return "You";
  if (role === "assistant") return activeTranscriptSource === "claude" ? "Claude" : activeTranscriptSource === "codex" ? "Codex" : "Assistant";
  return role || "Message";
}

function turnProcessNode(entries) {
  const row = document.createElement("div");
  row.className = "turn-process-row";
  const details = document.createElement("details");
  details.className = "turn-process";
  const summary = document.createElement("summary");
  const title = document.createElement("span");
  title.className = "turn-process-title";
  title.textContent = processedLabel(entries);
  summary.append(title, chevronNode());
  const body = document.createElement("div");
  body.className = "turn-process-body";
  for (const group of processGroups(entries)) {
    if (group.kind === "message") body.appendChild(processMessageNode(group.entry));
    if (group.kind === "tools") body.appendChild(toolGroupNode(group.entries));
  }
  details.append(summary, body);
  row.appendChild(details);
  return row;
}

function processMessageNode(entry) {
  const node = document.createElement("div");
  node.className = "process-message";
  if (String(entry.text || "").trim()) node.appendChild(renderMarkdown(entry.text || ""));
  if ((entry.images || []).length || entry.omitted_images) node.appendChild(imageGallery(entry));
  return node;
}

function processGroups(entries) {
  const groups = [];
  let tools = [];
  const flushTools = () => {
    if (tools.length) groups.push({ kind: "tools", entries: tools });
    tools = [];
  };
  for (const entry of entries) {
    if (entry.kind === "tool") {
      tools.push(entry);
      continue;
    }
    flushTools();
    if (entry.kind === "message") groups.push({ kind: "message", entry });
  }
  flushTools();
  return groups;
}

function toolGroupNode(entries) {
  const details = document.createElement("details");
  details.className = "tool-group";
  const summary = document.createElement("summary");
  summary.append(processIconNode(), document.createTextNode(toolGroupSummary(entries)), chevronNode());
  const list = document.createElement("div");
  list.className = "tool-action-list";
  for (const entry of entries) list.appendChild(toolActionNode(entry));
  details.append(summary, list);
  return details;
}

function toolActionNode(entry) {
  const details = document.createElement("details");
  details.className = "tool-action";
  const summary = document.createElement("summary");
  const title = document.createElement("span");
  title.className = "tool-action-title";
  title.textContent = toolActionSummary(entry);
  summary.append(title, chevronNode());
  details.append(summary, toolPanelNode(entry));
  return details;
}

function toolPanelNode(entry) {
  const panel = document.createElement("div");
  panel.className = "process-panel";
  const head = document.createElement("div");
  head.className = "process-panel-head";
  head.textContent = processPanelTitle(entry);
  const body = document.createElement("pre");
  body.className = "terminal-body";
  body.textContent = processBody(entry);
  const result = document.createElement("div");
  result.className = "process-result";
  result.textContent = processResult(entry);
  panel.append(head, body, result);
  return panel;
}

function processedLabel(entries) {
  const duration = durationFromEntries(entries);
  return duration ? "已处理 " + duration : "已处理";
}

function durationFromEntries(entries) {
  const list = entries || [];
  let durationMS = 0;
  for (const entry of list) {
    const value = Number(entry && entry.duration_ms || 0);
    if (Number.isFinite(value) && value > durationMS) durationMS = value;
  }
  if (durationMS > 0) return formatDurationMillis(durationMS);
  return formatDuration(list[0] && list[0].timestamp, list[list.length - 1] && list[list.length - 1].timestamp);
}

function formatDurationMillis(value) {
  const milliseconds = Number(value || 0);
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) return "";
  const seconds = Math.max(1, Math.round(milliseconds / 1000));
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  if (minutes <= 0) return rest + "s";
  return minutes + "m " + String(rest).padStart(2, "0") + "s";
}

function formatDuration(start, end) {
  const first = Date.parse(start || "");
  const last = Date.parse(end || "");
  if (!Number.isFinite(first) || !Number.isFinite(last) || last <= first) return "";
  return formatDurationMillis(last - first);
}

function toolGroupSummary(entries) {
  const searched = entries.filter((entry) => isSearchCommand(entry)).length;
  const explored = entries.filter((entry) => isExploreCommand(entry)).length;
  const commands = entries.filter((entry) => isShellCommand(entry)).length;
  const files = entries.reduce((count, entry) => count + exploredFileCount(entry), 0);
  const patches = entries.filter((entry) => String(entry.tool || "").includes("apply_patch")).length;
  const web = entries.filter((entry) => /web|browser/.test(String(entry.tool || ""))).length;
  const parts = [];
  if (files) parts.push("已探索 " + files + " 个文件");
  if (searched) parts.push(searched + " 次搜索");
  if (commands) parts.push("已运行 " + commands + " 条命令");
  if (patches) parts.push("已修改 " + patches + " 次");
  if (web) parts.push("已查询 " + web + " 次");
  return parts.length ? parts.join("") : "已处理 " + entries.length + " 个步骤";
}

function toolActionSummary(entry) {
  const command = extractCommand(entry.input_preview || "");
  const tool = String(entry.tool || "");
  if (command) return "已运行 " + command;
  if (tool.includes("apply_patch")) return "已应用补丁";
  if (tool.includes("web") || tool.includes("browser")) return "已查询网络";
  if (tool.includes("tool_search")) return "已搜索工具";
  return "已调用 " + (entry.tool || "工具");
}

function isShellCommand(entry) {
  return Boolean(extractCommand(entry.input_preview || "")) || String(entry.tool || "").includes("exec");
}

function isSearchCommand(entry) {
  const command = extractCommand(entry.input_preview || "");
  return /\\b(rg|grep|find)\\b/.test(command) || String(entry.tool || "").includes("tool_search");
}

function isExploreCommand(entry) {
  const command = extractCommand(entry.input_preview || "");
  return /\\b(sed|cat|nl|ls|wc)\\b|\\bgit\\s+(show|log|status|diff)\\b/.test(command);
}

function exploredFileCount(entry) {
  if (!isExploreCommand(entry)) return 0;
  const command = extractCommand(entry.input_preview || "");
  const matches = command.match(/(?:^|\\s)(?:[./~A-Za-z0-9_-][^\\s|;&]*)/g) || [];
  const files = matches.filter((part) => /[./]/.test(part) && !/^\\s*-/.test(part));
  return Math.max(1, Math.min(files.length, 12));
}

function processPanelTitle(entry) {
  const tool = String(entry.tool || "");
  if (tool.includes("exec") || extractCommand(entry.input_preview || "")) return "Shell";
  if (tool.includes("apply_patch")) return "Patch";
  return entry.tool || "Process";
}

function processBody(entry) {
  const input = String(entry.input_preview || "");
  const command = extractCommand(input);
  const output = String(entry.output || "");
  if (command) return "$ " + command + (output ? "\\n\\n" + output : "");
  if (output) return (input ? input + "\\n\\n" : "") + output;
  return input || "没有输入或输出。";
}

function processResult(entry) {
  const status = statusLabel(entry.status);
  return status ? "✓ " + status : "✓ 已记录";
}

function processIconNode() {
  const icon = document.createElement("span");
  icon.className = "process-icon";
  icon.textContent = ">";
  return icon;
}

function chevronNode() {
  const chevron = document.createElement("span");
  chevron.className = "chevron";
  return chevron;
}

function statusLabel(status) {
  const value = String(status || "").toLowerCase();
  if (!value) return "";
  if (["ok", "success", "succeeded", "completed", "complete", "done"].includes(value)) return "成功";
  if (["error", "failed", "failure"].includes(value)) return "失败";
  if (value === "cancelled" || value === "canceled") return "已取消";
  return status;
}

function extractCommand(inputPreview) {
  const text = String(inputPreview || "");
  try {
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed.cmd === "string") return parsed.cmd;
  } catch (_error) {
  }
  const match = text.match(/"cmd"\\s*:\\s*"((?:\\\\.|[^"\\\\])*)"/);
  if (!match) return "";
  try {
    return JSON.parse('"' + match[1] + '"');
  } catch (_error) {
    return match[1];
  }
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
  const pattern = new RegExp("(\\\\x60[^\\\\x60]+\\\\x60)|(\\\\*\\\\*[^*]+\\\\*\\\\*)|(__[^_]+__)|(\\\\[[^\\\\]]+\\\\]\\\\([^\\\\s)]+\\\\))|(\\\\*[^*\\\\n]+\\\\*)|(_[^_\\\\n]+_)", "g");
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
  const link = token.match(/^\\[([^\\]]+)\\]\\(([^\\s)]+)\\)$/);
  if (link) {
    if (/^https?:\\/\\//.test(link[2])) {
      const a = document.createElement("a");
      a.href = link[2];
      a.rel = "noreferrer";
      a.textContent = link[1];
      parent.appendChild(a);
    } else {
      const span = document.createElement("span");
      span.className = "file-link";
      span.textContent = link[1];
      parent.appendChild(span);
    }
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

function configureFullTranscriptAction(manifest, keyText) {
  activeManifest = manifest;
  activeKey = keyText || "";
  const action = $("full-transcript-actions");
  const status = $("full-transcript-status");
  if (!action || !manifest || !manifest.bundle || !manifest.bundle.url || !activeKey) return;
  action.hidden = false;
  status.textContent = manifest.bundle.bytes ? "完整包 " + formatBytes(manifest.bundle.bytes) : "";
}

async function loadFullTranscript() {
  const button = $("load-full-transcript");
  const status = $("full-transcript-status");
  if (!activeManifest || !activeKey) return;
  button.disabled = true;
  const old = button.textContent;
  try {
    button.textContent = "加载中";
    status.textContent = "正在下载、校验并解密完整 capsule...";
    const plainZip = await decryptBundle(activeManifest, activeKey);
    status.textContent = "正在解包并读取原生 transcript...";
    const files = await unzipFiles(plainZip);
    const transcript = await transcriptFromCapsuleFiles(files);
    renderTranscript(transcript, { complete: true });
    status.textContent = sourceLabel(transcript.source) + " 原生 transcript 已完整加载。";
    setStatus("完整可见 transcript 已在浏览器本地解密并渲染；内部上下文和不可见状态仍会被过滤。", "success");
  } catch (error) {
    button.disabled = false;
    button.textContent = old;
    status.textContent = error && error.message ? error.message : String(error);
    setStatus("完整会话加载失败：" + status.textContent, "error");
  }
}

function sourceLabel(source) {
  if (source === "codex") return "Codex";
  if (source === "claude") return "Claude";
  if (source === "neutral") return "Neutral";
  return "Capsule";
}

async function unzipFiles(zipBytes) {
  const bytes = zipBytes instanceof Uint8Array ? zipBytes : new Uint8Array(zipBytes);
  const eocd = findEndOfCentralDirectory(bytes);
  const entryCount = u16(bytes, eocd + 10);
  const centralDirOffset = u32(bytes, eocd + 16);
  const files = new Map();
  let offset = centralDirOffset;
  for (let i = 0; i < entryCount; i += 1) {
    if (u32(bytes, offset) !== 0x02014b50) throw new Error("zip central directory 格式不支持。");
    const method = u16(bytes, offset + 10);
    const compressedSize = u32(bytes, offset + 20);
    const nameLen = u16(bytes, offset + 28);
    const extraLen = u16(bytes, offset + 30);
    const commentLen = u16(bytes, offset + 32);
    const localOffset = u32(bytes, offset + 42);
    const name = new TextDecoder().decode(bytes.slice(offset + 46, offset + 46 + nameLen));
    offset += 46 + nameLen + extraLen + commentLen;
    if (!name || name.endsWith("/")) continue;
    if (u32(bytes, localOffset) !== 0x04034b50) throw new Error("zip local header 格式不支持。");
    const localNameLen = u16(bytes, localOffset + 26);
    const localExtraLen = u16(bytes, localOffset + 28);
    const dataStart = localOffset + 30 + localNameLen + localExtraLen;
    const compressed = bytes.slice(dataStart, dataStart + compressedSize);
    files.set(name, await unzipEntryData(method, compressed));
  }
  return files;
}

function findEndOfCentralDirectory(bytes) {
  const min = Math.max(0, bytes.length - 22 - 65535);
  for (let i = bytes.length - 22; i >= min; i -= 1) {
    if (u32(bytes, i) === 0x06054b50) return i;
  }
  throw new Error("不是可识别的 capsule zip。");
}

async function unzipEntryData(method, compressed) {
  if (method === 0) return compressed;
  if (method === 8) return await inflateRaw(compressed);
  throw new Error("zip 压缩方法不支持: " + method);
}

async function inflateRaw(bytes) {
  if (typeof DecompressionStream !== "function") {
    throw new Error("当前浏览器不支持 zip deflate 解压，请用 agent import。");
  }
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("deflate-raw"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

function u16(bytes, offset) {
  return bytes[offset] | (bytes[offset + 1] << 8);
}

function u32(bytes, offset) {
  return (bytes[offset] | (bytes[offset + 1] << 8) | (bytes[offset + 2] << 16) | (bytes[offset + 3] << 24)) >>> 0;
}

async function transcriptFromCapsuleFiles(files) {
  const manifest = readJSONFile(files, "manifest.json") || {};
  const source = String(manifest.source_agent || "").toLowerCase();
  const imageAssets = imageAssetsFromCapsule(files);
  if (source === "codex" && files.has("codex/session.jsonl")) {
    return codexTranscriptFromSession(files.get("codex/session.jsonl"), manifest, imageAssets);
  }
  if (source === "claude" && files.has("claude/session.jsonl")) {
    return claudeTranscriptFromSession(files.get("claude/session.jsonl"), manifest);
  }
  if (files.has("agent/neutral.json")) return neutralTranscriptFromFile(files.get("agent/neutral.json"));
  if (files.has("codex/session.jsonl")) return codexTranscriptFromSession(files.get("codex/session.jsonl"), manifest, imageAssets);
  if (files.has("claude/session.jsonl")) return claudeTranscriptFromSession(files.get("claude/session.jsonl"), manifest);
  throw new Error("完整 capsule 里没有可网页展示的 transcript；旧 capsule 请用 agent import。");
}

function codexTranscriptFromSession(bytes, manifest, imageAssets) {
  const transcript = baseTranscript(manifest, "codex");
  const pendingTools = new Map();
  for (const item of parseJSONLines(bytes)) {
    const timestamp = textValue(item.timestamp);
    const payload = objectRecord(item.payload);
    if (textValue(item.type) !== "response_item") continue;
    switch (textValue(payload.type)) {
    case "message": {
      const role = textValue(payload.role);
      if (role !== "user" && role !== "assistant") break;
      const content = codexMessageContent(payload.content, imageAssets);
      if (!content.text && !content.images.length && !content.omittedImages) break;
      const skill = skillFromText(content.text);
      if (skill) {
        attachSkill(transcript.entries, skill);
        break;
      }
      if (isInternalText(content.text)) break;
      transcript.entries.push({
        kind: "message",
        timestamp,
        role,
        text: content.text,
        images: content.images,
        omitted_images: content.omittedImages
      });
      break;
    }
    case "function_call":
    case "custom_tool_call":
    case "tool_search_call": {
      const entry = {
        kind: "tool",
        timestamp,
        tool: codexToolName(payload),
        status: textValue(payload.status) || "called",
        input_preview: fullValue(firstPresent(payload.arguments, payload.input))
      };
      transcript.entries.push(entry);
      const callID = textValue(payload.call_id);
      if (callID) pendingTools.set(callID, entry);
      break;
    }
    case "function_call_output":
    case "custom_tool_call_output":
    case "tool_search_output": {
      const callID = textValue(payload.call_id);
      const output = fullValue(payload.output);
      const status = textValue(payload.status) || "completed";
      const pending = pendingTools.get(callID);
      if (pending) {
        pending.output = output;
        pending.output_bytes = byteLengthInBrowser(output);
        if (!pending.status || pending.status === "called") pending.status = status;
      } else {
        transcript.entries.push({ kind: "tool", timestamp, tool: "tool output", status, output, output_bytes: byteLengthInBrowser(output) });
      }
      break;
    }
    default:
      break;
    }
  }
  return transcript;
}

function claudeTranscriptFromSession(bytes, manifest) {
  const transcript = baseTranscript(manifest, "claude");
  const pendingTools = new Map();
  for (const item of parseJSONLines(bytes)) {
    if (item && item.isMeta) continue;
    const type = textValue(item.type);
    const timestamp = textValue(item.timestamp);
    if (type === "user" || type === "assistant") {
      const message = objectRecord(item.message);
      const role = textValue(message.role) || type;
      if (role !== "user" && role !== "assistant") continue;
      appendClaudeContent(transcript.entries, pendingTools, message.content, role, timestamp);
      continue;
    }
    if (type === "attachment" || type === "file-history-snapshot") {
      const output = fullValue(firstPresent(item.attachment, item.snapshot));
      if (output) transcript.entries.push({ kind: "tool", timestamp, tool: type, status: "recorded", output, output_bytes: byteLengthInBrowser(output) });
    }
  }
  return transcript;
}

function neutralTranscriptFromFile(bytes) {
  const neutral = JSON.parse(readTextFileBytes(bytes));
  const entries = [];
  for (const entry of neutral.entries || []) {
    if (!entry || typeof entry !== "object") continue;
    if (entry.kind === "message") {
      const text = textValue(entry.text);
      if (!text || isInternalText(text)) continue;
      entries.push({
        kind: "message",
        timestamp: textValue(entry.timestamp),
        role: textValue(entry.role),
        text
      });
      continue;
    }
    if (entry.kind === "tool") {
      const output = textValue(entry.output);
      entries.push({
        kind: "tool",
        timestamp: textValue(entry.timestamp),
        tool: textValue(entry.tool) || "tool",
        status: textValue(entry.status),
        input_preview: textValue(entry.input) || textValue(entry.input_preview),
        output,
        output_bytes: byteLengthInBrowser(output)
      });
    }
  }
  return {
    schema: textValue(neutral.schema) || "agent-capsule.neutral.v1",
    source: "neutral",
    thread_id: textValue(neutral.source_id),
    title: textValue(neutral.title),
    source_cwd: textValue(neutral.source_cwd),
    created_at: textValue(neutral.created_at),
    entries
  };
}

function baseTranscript(manifest, source) {
  return {
    schema: "agent-capsule.web-transcript.v1",
    source,
    thread_id: textValue(manifest.thread_id),
    title: textValue(manifest.thread_title),
    source_cwd: textValue(manifest.source_cwd),
    created_at: textValue(manifest.created_at),
    entries: []
  };
}

function codexMessageContent(content, imageAssets) {
  const parts = [];
  const tagPaths = [];
  const inputImages = [];
  for (const item of Array.isArray(content) ? content : []) {
    const block = objectRecord(item);
    for (const key of ["text", "output_text"]) {
      const text = textValue(block[key]);
      if (text) {
        parts.push(text);
        tagPaths.push(...imageTagPaths(text));
      }
    }
    if (textValue(block.type) === "input_image") {
      const image = dataImage(textValue(block.image_url), textValue(block.detail));
      if (image) inputImages.push(image);
    }
  }
  const images = inputImages.length ? inputImages : imagesForPaths(tagPaths, imageAssets);
  return {
    text: parts.join("\\n").trim(),
    images,
    omittedImages: 0
  };
}

function appendClaudeContent(entries, pendingTools, content, role, timestamp) {
  if (typeof content === "string") {
    const text = content.trim();
    if (text && !isInternalText(text)) entries.push({ kind: "message", timestamp, role, text });
    return;
  }
  const textParts = [];
  const flushText = () => {
    const text = textParts.join("\\n").trim();
    textParts.length = 0;
    if (text && !isInternalText(text)) entries.push({ kind: "message", timestamp, role, text });
  };
  for (const blockValue of Array.isArray(content) ? content : []) {
    const block = objectRecord(blockValue);
    switch (textValue(block.type)) {
    case "text":
      if (textValue(block.text)) textParts.push(textValue(block.text));
      break;
    case "tool_use": {
      flushText();
      const entry = {
        kind: "tool",
        timestamp,
        tool: textValue(block.name) || "tool_use",
        status: "called",
        input_preview: fullValue(block.input)
      };
      entries.push(entry);
      const id = textValue(block.id);
      if (id) pendingTools.set(id, entry);
      break;
    }
    case "tool_result": {
      flushText();
      const id = textValue(block.tool_use_id) || textValue(block.id);
      const output = claudeToolResultText(block.content);
      const status = block.is_error ? "error" : "completed";
      const pending = pendingTools.get(id);
      if (pending) {
        pending.output = pending.output ? pending.output + "\\n\\n" + output : output;
        pending.output_bytes = byteLengthInBrowser(pending.output || "");
        pending.status = status;
      } else {
        entries.push({ kind: "tool", timestamp, tool: "tool_result", status, output, output_bytes: byteLengthInBrowser(output) });
      }
      break;
    }
    default:
      break;
    }
  }
  flushText();
}

function claudeToolResultText(content) {
  if (typeof content === "string") return content.trim();
  if (!Array.isArray(content)) return fullValue(content);
  const parts = [];
  for (const value of content) {
    const block = objectRecord(value);
    if (textValue(block.type) === "text" && textValue(block.text)) {
      parts.push(textValue(block.text));
    } else {
      const rendered = fullValue(value);
      if (rendered) parts.push(rendered);
    }
  }
  return parts.join("\\n").trim();
}

function codexToolName(payload) {
  let name = textValue(payload.name);
  const namespace = textValue(payload.namespace);
  if (namespace) name = namespace + "." + name;
  return name || textValue(payload.type) || "tool";
}

function attachSkill(entries, skill) {
  for (let i = entries.length - 1; i >= 0; i -= 1) {
    const entry = entries[i];
    if (entry.kind === "message" && entry.role === "user") {
      entry.skills = (entry.skills || []).concat([skill]);
      return;
    }
  }
}

function skillFromText(text) {
  const value = String(text || "").trim();
  if (!value.startsWith("<skill>")) return null;
  return {
    name: xmlTag(value, "name") || "skill",
    path: xmlTag(value, "path"),
    text: value
  };
}

function xmlTag(text, tag) {
  const open = "<" + tag + ">";
  const close = "</" + tag + ">";
  const start = String(text || "").indexOf(open);
  if (start < 0) return "";
  const bodyStart = start + open.length;
  const end = String(text || "").indexOf(close, bodyStart);
  return end >= 0 ? String(text || "").slice(bodyStart, end).trim() : "";
}

function isInternalText(text) {
  const value = String(text || "").trim();
  return value.startsWith("# AGENTS.md instructions for ") ||
    value.startsWith("<codex_internal_context") ||
    value.startsWith("<environment_context>") ||
    value.startsWith("<INSTRUCTIONS>") ||
    value.startsWith("<turn_aborted>") ||
    value.startsWith("<skill>");
}

function imageAssetsFromCapsule(files) {
  const bySource = new Map();
  const manifest = readJSONFile(files, "codex/assets/images.json");
  if (!manifest || !Array.isArray(manifest.images)) return { bySource };
  for (const image of manifest.images) {
    const sourcePath = textValue(image.source_path);
    const zipPath = textValue(image.zip_path);
    const mime = textValue(image.mime);
    if (textValue(image.status) !== "copied" || !sourcePath || !zipPath || !files.has(zipPath) || !mime.startsWith("image/")) continue;
    const content = files.get(zipPath);
    bySource.set(sourcePath, {
      src: "data:" + mime + ";base64," + bytesToBase64(content),
      mime,
      bytes: content.byteLength,
      alt: textValue(image.original_name) || sourcePath
    });
  }
  return { bySource };
}

function imagesForPaths(paths, imageAssets) {
  const out = [];
  const seen = new Set();
  for (const path of paths) {
    const image = imageAssets && imageAssets.bySource && imageAssets.bySource.get(path);
    if (!image || seen.has(image.src)) continue;
    seen.add(image.src);
    out.push(image);
  }
  return out;
}

function imageTagPaths(text) {
  const pattern = /<image\\b[^>]*\\bpath=(?:"([^"]+)"|'([^']+)')[^>]*>/g;
  const paths = [];
  let match;
  while ((match = pattern.exec(String(text || ""))) !== null) paths.push(match[1] || match[2] || "");
  return paths.filter(Boolean);
}

function dataImage(src, detail) {
  if (!String(src || "").startsWith("data:image/")) return null;
  const mimeEnd = src.indexOf(";");
  const comma = src.indexOf(",");
  const mime = mimeEnd > 5 ? src.slice(5, mimeEnd) : "image";
  return {
    src,
    mime,
    bytes: dataURLDecodedBytes(src),
    alt: detail ? "uploaded image (" + detail + ")" : "uploaded image"
  };
}

function dataURLDecodedBytes(src) {
  const comma = String(src || "").indexOf(",");
  if (comma < 0) return 0;
  const payload = String(src || "").slice(comma + 1).replace(/\\s+/g, "");
  return Math.max(0, Math.floor(payload.length * 3 / 4) - (payload.endsWith("==") ? 2 : payload.endsWith("=") ? 1 : 0));
}

function parseJSONLines(bytes) {
  const out = [];
  for (const line of readTextFileBytes(bytes).split(/\\r?\\n/)) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      out.push(JSON.parse(trimmed));
    } catch (_error) {
    }
  }
  return out;
}

function readJSONFile(files, path) {
  if (!files.has(path)) return null;
  try {
    return JSON.parse(readTextFileBytes(files.get(path)));
  } catch (_error) {
    return null;
  }
}

function readTextFileBytes(bytes) {
  return new TextDecoder().decode(bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes));
}

function textValue(value) {
  return typeof value === "string" ? value.trim() : "";
}

function objectRecord(value) {
  return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function firstPresent(...values) {
  for (const value of values) {
    if (value !== undefined && value !== null) return value;
  }
  return null;
}

function fullValue(value) {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value.trim();
  try {
    return JSON.stringify(value, null, 2);
  } catch (_error) {
    return String(value).trim();
  }
}

function bytesToBase64(bytes) {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

function byteLengthInBrowser(value) {
  return new TextEncoder().encode(String(value || "")).byteLength;
}

document.addEventListener("click", async (event) => {
  const fullButton = event.target.closest("#load-full-transcript");
  if (fullButton) {
    await loadFullTranscript();
    return;
  }
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
    const key = fragmentKey();
    configureFullTranscriptAction(manifest, key);
    if (!manifest.preview) {
      $("counts").textContent = "旧版链接";
      setStatus("这个链接没有轻量预览；如果带有 #k，可以点击加载完整会话，或用 agent import。", "warn");
      return;
    }
    if (!key) {
      $("counts").textContent = "缺少 key";
      setStatus("这个链接缺少 #k 解密 key。请使用 capsule export 生成的完整 URL。", "warn");
      return;
    }
    const transcript = await decryptPreview(manifest.preview, key);
    renderTranscript(transcript);
    if (transcript.truncated) {
      setStatus("预览已在浏览器本地解密，但内容已截断。可以点击加载完整会话查看完整可见 transcript。", "warn");
    } else {
      setStatus("预览已在浏览器本地解密。需要完整可见 transcript 时，可以点击加载完整会话。", "success");
    }
  } catch (error) {
    $("counts").textContent = "预览不可用";
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

function normalizeManifest(input, limits) {
  if (!input || typeof input !== "object") throw new Error("bad_manifest");
  if (input.schema !== LINK_SCHEMA) throw new Error("unsupported manifest schema");
  const bundle = objectValue(input.bundle);
  const cryptoInfo = objectValue(input.crypto);
  const thread = objectValue(input.thread);
  const sha256 = stringValue(bundle.sha256).toLowerCase();
  if (!/^[a-f0-9]{64}$/.test(sha256)) throw new Error("bad_bundle_sha256");
  if (cryptoInfo.alg !== "AES-256-GCM") throw new Error("unsupported manifest crypto");
  const nonce = stringValue(cryptoInfo.nonce);
  if (!/^[A-Za-z0-9_-]{16,128}$/.test(nonce)) throw new Error("bad_crypto_nonce");
  if (cryptoInfo.key_ref !== "url-fragment:k") throw new Error("unsupported_key_ref");

  const out = {
    schema: LINK_SCHEMA,
    created_at: stringValue(input.created_at).slice(0, 64),
    thread: {
      id: stringValue(thread.id).slice(0, limits.maxThreadIDChars),
      title: stringValue(thread.title).slice(0, limits.maxTitleChars)
    },
    bundle: {
      url: "",
      sha256,
      bytes: 0
    },
    crypto: {
      alg: "AES-256-GCM",
      nonce,
      key_ref: "url-fragment:k"
    },
    import: importInfo()
  };
  const preview = normalizePreview(input.preview, limits);
  if (preview) out.preview = preview;
  return out;
}

function normalizePreview(value, limits) {
  if (value == null) return null;
  if (!value || typeof value !== "object") throw new Error("bad_preview");
  if (value.schema !== "agent-capsule.preview.v1") throw new Error("unsupported_preview_schema");
  const cryptoInfo = objectValue(value.crypto);
  if (cryptoInfo.alg !== "AES-256-GCM") throw new Error("unsupported_preview_crypto");
  const nonce = stringValue(cryptoInfo.nonce);
  if (!/^[A-Za-z0-9_-]{16,128}$/.test(nonce)) throw new Error("bad_preview_nonce");
  if (cryptoInfo.key_ref !== "url-fragment:k") throw new Error("unsupported_preview_key_ref");
  const payload = stringValue(value.payload);
  if (byteLength(payload) > limits.maxPreviewPayloadBytes) throw new Error("preview_too_large");
  return {
    schema: "agent-capsule.preview.v1",
    crypto: {
      alg: "AES-256-GCM",
      nonce,
      key_ref: "url-fragment:k"
    },
    payload
  };
}

async function verifyUploadToken(request, env) {
  const expected = uploadToken(env);
  if (expected === "") return true;
  const provided = bearerToken(request);
  if (provided === "") return false;
  return timingSafeTokenEqual(provided, expected);
}

function uploadAuthRequired(env) {
  return uploadToken(env) !== "";
}

function uploadToken(env) {
  return stringValue(env.CAPSULE_WORKER_TOKEN).trim();
}

function bearerToken(request) {
  const value = request.headers.get("authorization") || "";
  const match = value.match(/^Bearer\s+(.+)$/i);
  return match ? match[1].trim() : "";
}

async function timingSafeTokenEqual(provided, expected) {
  const encoder = new TextEncoder();
  const [providedHash, expectedHash] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(provided)),
    crypto.subtle.digest("SHA-256", encoder.encode(expected))
  ]);
  const left = new Uint8Array(providedHash);
  const right = new Uint8Array(expectedHash);
  let diff = left.length ^ right.length;
  for (let i = 0; i < Math.max(left.length, right.length); i += 1) {
    diff |= (left[i] || 0) ^ (right[i] || 0);
  }
  return diff === 0;
}

function readLimits(env) {
  const maxBlobBytes = envInt(env, "MAX_BLOB_BYTES", 32 * 1024 * 1024);
  const maxManifestBytes = envInt(env, "MAX_MANIFEST_BYTES", 8 * 1024 * 1024);
  return {
    maxBlobBytes,
    maxManifestBytes,
    maxPreviewPayloadBytes: envInt(env, "MAX_PREVIEW_PAYLOAD_BYTES", 6 * 1024 * 1024),
    maxRequestBytes: envInt(env, "MAX_REQUEST_BYTES", maxBlobBytes + maxManifestBytes + 64 * 1024),
    maxShareBytes: envInt(env, "MAX_SHARE_BYTES", maxBlobBytes + maxManifestBytes),
    maxTitleChars: envInt(env, "MAX_TITLE_CHARS", 180),
    maxThreadIDChars: envInt(env, "MAX_THREAD_ID_CHARS", 128),
    maxTtlSeconds: envInt(env, "MAX_TTL_SECONDS", 24 * 60 * 60),
    maxDownloadsPerShare: envInt(env, "MAX_DOWNLOADS_PER_SHARE", 10),
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

function objectValue(value) {
  return value && typeof value === "object" ? value : {};
}

function stringValue(value) {
  return typeof value === "string" ? value.trim() : "";
}

function byteLength(value) {
  return new TextEncoder().encode(String(value || "")).byteLength;
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

function randomBase64URL(byteLength) {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
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
