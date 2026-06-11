const LINK_SCHEMA = "agent-capsule.link.v1";
const GB = 1024 * 1024 * 1024;

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
  return json(result.share.manifest);
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
  const manifest = result.share.manifest;
  const accept = request.headers.get("accept") || "";
  if (accept.includes("application/json")) return json(manifest);
  const title = escapeHTML(manifest.thread && manifest.thread.title ? manifest.thread.title : "Agent Capsule");
  const command = "capsule import \"" + new URL(request.url).origin + "/s/" + id + "#k=...\" --target codex --target-cwd . --execute";
  return html("<h1>" + title + "</h1><p>This Agent Capsule link needs the decryption key from the URL fragment.</p><pre>" + escapeHTML(command) + "</pre>");
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
