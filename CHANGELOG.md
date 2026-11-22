# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本。

## [0.1.0] - 2026-11-22

### 新增

- 首个 M2 站点管理插件骨架（`depth_level = production`），协议 `kore.plugin.v1` / `lifecycle = resident`，能力 `system:admin`。
- `kore.hello` 三步握手：发 `kore.hello`（§5 全字段 + `id`/`token` 别名）→ 读响应帧 → 校验 `ok`/`id`/协议主版本。
- 会话循环：读帧 → 按 `method` 分发 → 写 `response`，回显 `id` 与 `req_id`；未知方法回 `E_NOTFOUND`。
- 站点方法 `site.list` / `site.create` / `site.delete`：当前只做参数校验与结构化返回（`applied:false`），**不实际改动 nginx**。
- 安全校验：`domain` 正则白名单（小写、≥2 标签、≤253 字符）拒绝路径穿越；`default_server=true` 直接回 `E_INVALID`（红队 R6）。
- 目标路径固定为独立 `conf.d/<domain>.conf`，不触碰宿主主配置（提案 §2 M2-A）。
- `Makefile`：`build` / `dist`（回填 `size_bytes`、生成包内 `SHA256SUMS` 与包旁 `.sha256`）/ `check` / `clean`。
- 单测：`net.Pipe` 模拟主壳侧，覆盖握手、三个站点方法、非法参数与 `req_id` 回显。

### 说明

- 真实 conf.d vhost 写入、`include` 接线与站点列表数据源留待 M2-A 第二批。
