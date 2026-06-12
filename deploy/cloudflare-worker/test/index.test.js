import assert from "node:assert/strict";
import test from "node:test";

import worker, { BudgetGate } from "../src/index.js";

const BASE_URL = "https://capsule.example";

test("anonymous upload/download happy path", async () => {
  const env = fakeEnv();
  const blob = new Blob([new Uint8Array([1, 2, 3])], { type: "application/octet-stream" });
  const upload = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(blob)
  }), env);
  assert.equal(upload.status, 201);
  const created = await upload.json();
  assert.match(created.share_url, /^https:\/\/capsule\.example\/s\//);

  const manifest = await worker.fetch(new Request(created.manifest_url), env);
  assert.equal(manifest.status, 200);
  const manifestJSON = await manifest.json();
  assert.equal(manifestJSON.schema, "agent-capsule.link.v1");
  assert.match(manifestJSON.bundle.url, /\/v1\/shares\/.+\/blob$/);
  assert.equal(manifestJSON.import.install_command, "go install github.com/z2z23n0/agent-capsule/cmd/capsule@main");

  const downloaded = await worker.fetch(new Request(manifestJSON.bundle.url), env);
  assert.equal(downloaded.status, 200);
  assert.deepEqual(new Uint8Array(await downloaded.arrayBuffer()), new Uint8Array([1, 2, 3]));
});

test("upload preserves encrypted preview metadata", async () => {
  const env = fakeEnv();
  const input = manifest();
  input.preview = {
    schema: "agent-capsule.preview.v1",
    crypto: { alg: "AES-256-GCM", nonce: "nonce", key_ref: "url-fragment:k" },
    payload: "payload"
  };
  const form = new FormData();
  form.set("manifest", JSON.stringify(input));
  form.set("blob", new Blob(["hello"]), "blob.enc");
  const upload = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: form
  }), env);
  assert.equal(upload.status, 201);
  const created = await upload.json();
  const output = await (await worker.fetch(new Request(created.manifest_url), env)).json();
  assert.equal(output.preview.schema, "agent-capsule.preview.v1");
  assert.equal(output.preview.payload, "payload");
});

test("share page serves human preview shell and agent metadata", async () => {
  const env = fakeEnv();
  const upload = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(new Blob(["hello"]))
  }), env);
  assert.equal(upload.status, 201);
  const created = await upload.json();

  const page = await worker.fetch(new Request(created.share_url + "#k=test"), env);
  assert.equal(page.status, 200);
  const html = await page.text();
  assert.match(html, /This is a readable preview/);
  assert.match(html, /Restore full session in Codex/);
  assert.match(html, /message-row/);
  assert.match(html, /process-card/);
  assert.match(html, /function renderMarkdown/);
  assert.match(html, /function isInternalContextEntry/);
  assert.doesNotMatch(html, /<details class="restore-panel" open>/);
  assert.match(html, /application\/agent-capsule\+json/);
  assert.match(html, /go install github\.com\/z2z23n0\/agent-capsule\/cmd\/capsule@main/);

  const jsonResponse = await worker.fetch(new Request(created.share_url, {
    headers: { accept: "application/json" }
  }), env);
  assert.equal(jsonResponse.status, 200);
  const manifestJSON = await jsonResponse.json();
  assert.equal(manifestJSON.import.dry_run_command, "capsule import \"<this-url>\" --target codex --target-cwd .");
  assert.equal(manifestJSON.import.execute_command, "capsule import \"<this-url>\" --target codex --target-cwd . --execute");
});

test("max blob size blocks upload", async () => {
  const env = fakeEnv({ MAX_BLOB_BYTES: "2" });
  const response = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(new Blob([new Uint8Array([1, 2, 3])]))
  }), env);
  assert.equal(response.status, 413);
  assert.equal((await response.json()).error, "blob_too_large");
});

test("download count limit blocks later downloads", async () => {
  const env = fakeEnv({ MAX_DOWNLOADS_PER_SHARE: "1" });
  const upload = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(new Blob(["hello"]))
  }), env);
  const created = await upload.json();
  const manifest = await (await worker.fetch(new Request(created.manifest_url), env)).json();

  assert.equal((await worker.fetch(new Request(manifest.bundle.url), env)).status, 200);
  const blocked = await worker.fetch(new Request(manifest.bundle.url), env);
  assert.equal(blocked.status, 429);
  assert.equal((await blocked.json()).error, "download_limit");
});

test("R2 failure fails closed and releases live budget", async () => {
  const env = fakeEnv({ LIVE_BYTES_LIMIT: "4" });
  env.CAPSULE_BUCKET.failPut = true;
  const failed = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(new Blob(["1234"]))
  }), env);
  assert.equal(failed.status, 500);

  env.CAPSULE_BUCKET.failPut = false;
  const retry = await worker.fetch(new Request(BASE_URL + "/v1/shares", {
    method: "POST",
    body: shareForm(new Blob(["1234"]))
  }), env);
  assert.equal(retry.status, 201);
});

test("cleanup removes expired objects and frees live bytes", async () => {
  const env = fakeEnv();
  await env.CAPSULE_BUCKET.put("shares/old/blob.enc", new Uint8Array([1, 2, 3]));
  const gate = env.BUDGET_GATE.instance;
  await gate.fetch(new Request("https://budget.local/reserve", {
    method: "POST",
    body: JSON.stringify({ bytes: 3 })
  }));
  await gate.fetch(new Request("https://budget.local/commit", {
    method: "POST",
    body: JSON.stringify({
      share: {
        id: "old",
        object_key: "shares/old/blob.enc",
        bytes: 3,
        downloads: 0,
        expires_at: "2000-01-01T00:00:00.000Z",
        manifest: manifest()
      }
    })
  }));

  let scheduledPromise;
  const cleanup = await worker.scheduled({}, env, {
    waitUntil: (promise) => {
      scheduledPromise = promise;
    }
  });
  assert.equal(cleanup, undefined);
  await scheduledPromise;
  assert.equal(env.CAPSULE_BUCKET.objects.has("shares/old/blob.enc"), false);
  const budget = await env.BUDGET_GATE.state.storage.get("budget");
  assert.equal(budget.liveBytes, 0);
});

function shareForm(blob) {
  const form = new FormData();
  form.set("manifest", JSON.stringify(manifest()));
  form.set("blob", blob, "blob.enc");
  return form;
}

function manifest() {
  return {
    schema: "agent-capsule.link.v1",
    created_at: "2026-06-12T00:00:00Z",
    thread: { id: "thread-id", title: "Thread" },
    bundle: { url: "", sha256: "abc", bytes: 3 },
    crypto: { alg: "AES-256-GCM", nonce: "nonce", key_ref: "url-fragment:k" },
    import: { tool: "capsule", command: "capsule import <this-url> --target codex --target-cwd . --execute" }
  };
}

function fakeEnv(vars = {}) {
  const bucket = new FakeBucket();
  const state = { storage: new FakeStorage() };
  const env = {
    CAPSULE_BUCKET: bucket,
    ...vars
  };
  const instance = new BudgetGate(state, env);
  env.BUDGET_GATE = {
    state,
    instance,
    idFromName: (name) => name,
    get: () => ({ fetch: (input, init) => instance.fetch(new Request(input, init)) })
  };
  return env;
}

class FakeBucket {
  constructor() {
    this.objects = new Map();
    this.failPut = false;
  }

  async put(key, value) {
    if (this.failPut) throw new Error("r2 put failed");
    this.objects.set(key, new Uint8Array(value));
  }

  async get(key) {
    const body = this.objects.get(key);
    return body ? { body } : null;
  }

  async delete(key) {
    this.objects.delete(key);
  }
}

class FakeStorage {
  constructor() {
    this.values = new Map();
  }

  async get(key) {
    return this.values.get(key);
  }

  async put(key, value) {
    this.values.set(key, value);
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix } = {}) {
    const out = new Map();
    for (const [key, value] of this.values) {
      if (!prefix || key.startsWith(prefix)) out.set(key, value);
    }
    return out;
  }
}
