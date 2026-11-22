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

### 新增（L2 可用）

- 站点配置以 vhost 注释标记块为唯一事实来源：解析（`parse.go`）↔ 渲染（`render.go`）互为逆运算，
  写操作统一走 `applyEdit` 编排（备份 → 原子写 → `nginx -t` → reload → 失败回滚），
  目标状态与现状一致时短路（`changed=false` + `test_skipped`）。
- `site.bindSSL` / `site.unbindSSL`：443 监听 + 证书 + TLS1.2+ 固定套件 + 默认 80→443 跳转
  （`force_https=false` 时 80/443 并存）；证书路径必须规范化绝对路径、字符集白名单且真实存在。
- `site.setRewrite` / `site.getRewrite`：伪静态模板枚举 `none|wordpress|laravel|thinkphp|discuz|nextjs`，
  写入注释块并可整体替换；`getRewrite` 从注释块解析模板名；不接受自定义原始 nginx 片段。
- `site.setProxy` / `site.unsetProxy`：`location /` 反代，`ws=true` 补 WebSocket 升级头；
  上游只接受 `http://host:port`（正则挡 userinfo/路径/注入）。反代与伪静态互斥，互相设置时自动清对方。
- `site.setRedirect` / `site.unsetRedirect`：`location = <from_path>` 精确匹配重定向，`code` 默认 301；
  `to_url` 只允许 http(s) 或站内 `/` 路径（挡 `javascript:` / `data:` / `//host`），最多 32 条。
- `site.disable` / `site.enable`：vhost 改名 `.conf.disabled` / 改回 `.conf` + test/reload，失败改回原名，配置不删。
- `site.setIndex` / `site.get`：默认文档（默认 `index.html index.htm index.php`）与完整配置摘要
  （root/port/index/ssl/rewrite/proxy/redirects/enabled/disabled）。
- `site.list` / `site.status` 增补 `disabled` 信号：停用站点 `exists=true`、不参与"已加载"判定，
  不再让 `list.applied` 因设计内停用而变 false。
- 重新 `site.create` 覆盖既有托管站点时保留 L2 状态（SSL/伪静态/反代/重定向/index），只更新 root/port。
- 单测：12 个新方法的正常路径、参数校验、`nginx -t` 失败回滚、幂等短路；docker e2e 新增
  `TestDockerNginxL2`（https 200 / http 301 / wordpress / 反代后端 / 302 / 启停 / index 六步）。
- 文件按能力拆分，新增文件均 ≤200 行；只读巡检拆到 `inspect.go`。

### Fixed

- `site.list` / `site.status` 增加 `test_skipped` / `reload_skipped` / `test_output`，区分「本机未配置 nginx、没测」与「测了挂了」；前端据此渲染灰色未检测而不是红色失败。
