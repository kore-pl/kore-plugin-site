# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本。

## [0.1.0] - 2026-11-22

### 新增

- 首个 M2 站点管理插件骨架（`depth_level = production`），协议 `kore.plugin.v1` / `lifecycle = resident`，能力 `system:admin`。
- `kore.hello` 三步握手：发 `kore.hello`（§5 全字段 + `id`/`token` 别名）→ 读响应帧 → 校验 `ok`/`id`/协议主版本。
- 会话循环：读帧 → 按 `method` 分发 → 写 `response`，回显 `id` 与 `req_id`；未知方法回 `E_NOTFOUND`。
- 站点方法 `site.list` / `site.create` / `site.delete`：参数校验 + 独立 conf.d vhost 原子写/备份/清单（M4-B）。
- 安全校验：`domain` 正则白名单（小写、≥2 标签、≤253 字符）拒绝路径穿越；`default_server=true` 直接回 `E_INVALID`（红队 R6）。
- 目标路径固定为独立 `conf.d/<domain>.conf`，不触碰宿主主配置（提案 §2 M2-A）。
- `Makefile`：`build` / `dist`（回填 `size_bytes`、生成包内 `SHA256SUMS` 与包旁 `.sha256`）/ `check` / `clean`。
- 单测：`net.Pipe` 模拟主壳侧，覆盖握手、站点方法、非法参数与 `req_id` 回显。

### 建站闭环（M4-C）

- `site.create` 补完整链路：原子写 vhost/清单 → `nginx -t` 校验（失败回滚 conf 与清单并回 `E_INVALID` + nginx 输出）→ `nginx -s reload`（失败回 `E_UPSTREAM`，明确站点已写入但未生效）。
- 新增 `site.status`：返回 `conf_exists` / `test_passed` / `site_dir_exists` / `loaded`（`nginx -T` 精确匹配 `server_name`）/ `managed`。
- `site.delete` 删除后同样执行校验与重载；任一失败恢复删除前 vhost 与清单。
- `site.list` 每条补齐 `exists` / `test_ok` / `reloaded` 真实状态。
- 命令执行安全：环境变量只提供命令前缀，`-t` / `-s reload` / `-T` 由插件固定注入，argv 不拼接任何 RPC 入参；`root` 增加配置注入字符校验。
- 新增 docker nginx:alpine 端到端（`-tags e2e`）：create 命中 200、坏 conf 被 `nginx -t` 拦截、失败回滚后配置仍合法、delete 后不再命中站点。

## [Unreleased]

### Fixed

- `site.list` / `site.status` 增加 `test_skipped` / `reload_skipped` / `test_output`，区分「本机未配置 nginx、没测」与「测了挂了」；前端据此渲染灰色未检测而不是红色失败。
