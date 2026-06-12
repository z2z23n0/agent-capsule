# Agent Capsule

Agent Capsule 用来把一个 Codex 会话打包成可分享的胶囊。

你可以把本地 Codex thread 导出成标准 `.capsule.zip` 文件，或者导出成加密分享链接；接收方可以在自己的 Codex 里导入它，打开完整对话，并继续接着工作。

CLI 命令叫 `capsule`。

[English README](README.md)

## 解决什么问题

你想分享整个 agent 对话，包括你思考、找到问题的过程，或是问题排查的记录。

你想完整地交接一段工作，例如未完成的 bug 定位，或是没写完的代码。

Agent Capsule 会把这段会话打包成一个可检查、可导入的胶囊，让接收方不只是读一段聊天记录，而是能把它恢复到自己的 Codex 里继续用。

## 当前状态

Agent Capsule 目前支持 Codex 的导出和导入。

Codex 会话里引用的图片上传会被保留。Agent Capsule 目前还不会打包任意非图片文件。

后续会支持 Claude Code，以及跨 agent 的导出和导入。

## 安装

```bash
go install github.com/z2z23n0/agent-capsule/cmd/capsule@main
```

## 快速开始：文件交接

导出当前 thread：

```bash
capsule export --thread current --name "handoff topic"
```

导入前先检查：

```bash
capsule inspect handoff-topic.capsule.zip
```

先 dry-run：

```bash
capsule import handoff-topic.capsule.zip --target codex --target-cwd .
```

确认后写入本地 Codex：

```bash
capsule import handoff-topic.capsule.zip --target codex --target-cwd . --execute
```

验证导入结果：

```bash
capsule verify --home ~/.codex --thread <new-thread-id> --target-cwd .
```

`capsule import` 默认只做 dry-run；只有带 `--execute` 才会真正写入。

## 链接分享

Agent Capsule 也可以生成加密分享链接：

```bash
capsule share --thread current --service worker --endpoint https://example.workers.dev
```

链接格式类似：

```text
https://<worker-host>/s/<share-id>#k=<base64url-key>
```

分享前，胶囊会先用 AES-256-GCM 加密。服务端保存 ciphertext 和 manifest；解密 key 放在 URL fragment 里，正常浏览器请求不会把 fragment 发给服务端。

打开链接后，浏览器页面会在本地解密并展示可读预览，同时给 agent 提供安装、dry-run 和导入命令。

如果会话包含图片，浏览器预览会在 preview 大小限制内展示图片缩略图。图片很多或很大的 session 仍然可以从完整加密胶囊导入。

如果链接上传因为 endpoint 缺失、服务不可用或 quota 限制失败，Agent Capsule 会回退生成本地 `.capsule.zip`，并返回 `status: fallback_zip`。

## 隐私承诺

链接分享时，Agent Capsule 会先在本机加密胶囊再上传。托管服务、Worker、R2 bucket 或 S3 兼容 bucket 只会收到加密后的胶囊字节和加密后的预览 payload。没有 `#k=...` fragment key，这些服务无法解密会话内容。

解密 key 在发送方机器上生成，只放在 URL fragment 里。正常浏览器请求不会把 URL fragment 发给服务端；CLI importer 在拉取 manifest 和 ciphertext 前也会先移除 fragment。

服务端仍然可以看到并保存链接元数据，包括 thread id、thread title、创建和过期时间、密文字节数、密文 hash、bundle URL，以及请求相关的运行元数据。

托管预览页会在浏览器里用 WebCrypto 解密 preview。如果你不信任页面托管方始终提供不会回传 key 的 JavaScript，请使用 CLI import 路径；它会直接拉取 manifest 和 ciphertext，并在本地解密。

## 官方服务、自建 Worker 和 S3

`capsule share` 默认使用 `--service official`。本地开发时不要假设官方服务已经可用，可以显式配置：

```bash
export CAPSULE_OFFICIAL_ENDPOINT=https://...
capsule share --thread current
```

自建 Cloudflare Worker：

```bash
capsule share --thread current \
  --service worker \
  --endpoint https://example.workers.dev
```

使用 S3/R2 这类兼容存储：

```bash
capsule share --thread current --service s3 \
  --s3-endpoint https://<account>.r2.cloudflarestorage.com \
  --s3-bucket agent-capsule \
  --s3-prefix shares \
  --s3-access-key-id "$CAPSULE_S3_ACCESS_KEY_ID" \
  --s3-secret-access-key "$CAPSULE_S3_SECRET_ACCESS_KEY" \
  --s3-public-base-url https://pub.example/capsules
```

## 部署自己的 Worker

Worker 模板在 `deploy/cloudflare-worker/`。

```bash
cd deploy/cloudflare-worker
npm install
cp wrangler.toml.example wrangler.toml
npm run dev
```

部署前需要绑定：

- 私有 R2 bucket：`CAPSULE_BUCKET`
- Durable Object：`BudgetGate`
- 可选上传鉴权：`CAPSULE_WORKER_TOKEN`

部署：

```bash
npm run deploy
```

不要把真实 `wrangler.toml` 或 secret 提交到仓库。

## 胶囊里有什么

一个 `.capsule.zip` 包含：

```text
manifest.json
AGENT_README.md
codex/session.jsonl
codex/index-entry.json
codex/thread-row.json
codex/assets/images.json              # 可选
codex/assets/images/<sha256>.<ext>    # 可选
agent/restore.md
safety/scan.json
checksums.json
```

只有当 Codex session 引用了本地图片时，胶囊里才会出现图片资产。导入时，这些图片会写到
`$CODEX_HOME/agent-capsule-assets/<new-thread-id>/images/`，并且导入后的 session 会把本地图片路径重写到这个新位置。

根目录的 `AGENT_README.md` 是给接收方 agent 看的入口。即使它还没安装 `capsule`，也可以先用普通 zip 工具解压，读说明，再决定是否导入。

## 安全边界

胶囊可能包含敏感会话内容、本地路径、工具输出、提示词、图片或截图，以及误写进会话里的 secret。

Agent Capsule 在 export/share 时会做 best-effort secret scan。发现高置信 secret 时，默认会阻断导出，除非你显式传：

```bash
--unsafe-include-secrets
```

只有在你已经检查过内容，并确认确实要分享时，才应该使用这个参数。

secret scan 只检查 session 文本，不会对图片做 OCR，也不会扫描图片像素。因此分享前需要自己检查截图和上传图片。

链接分享上传的是加密内容；但任何拿到完整链接，包括 `#k=...` 的人，都可以解密胶囊。

## Agent Capsule 不做什么

Agent Capsule 不迁移 provider credential、登录态、云端状态或 API key。

它不承诺一台机器上的 encrypted reasoning blob 能在另一台机器上以密码学等价方式继续。

## 开发

运行 Go 测试：

```bash
go test ./internal/capsule ./internal/codex
```

运行 Worker 检查：

```bash
npm --prefix deploy/cloudflare-worker test
npm --prefix deploy/cloudflare-worker run check
```

## 协议

Apache-2.0。见 [LICENSE](LICENSE)。
