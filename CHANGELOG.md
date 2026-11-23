# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本。

## [0.3.0] - 2026-11-23

### 新增（方法级 capability，§3.5 #12）

- `info.json` 新增 `method_capabilities`（16 条）：只读方法 `list` / `status` / `get` / `getRewrite` → `system:read`，其余 12 个写方法 → `system:admin`。内核按方法取用（`validate.go` 校验 + `invoke_capability.go` 取用），只读调用不再按 admin 类写 `invoke.*` 审计；未声明的方法仍回退插件级 `capabilities`。
- 版本号 0.2.0 → 0.3.0（新增对外字段，minor），`manifest.json`（内嵌 manifest）同步 bump。
- 兼容披露：`config.get` / `config.set` 本次未纳入方法级声明，仍回退插件级 `system:admin`（`config.get` 仍按 admin 判定）；如需一并下沉到 `system:read` / `system:admin`，另行发版。

### 修复（容器模式站点目录三态，§3.5 #6）

- `site.status` / `site.get` 新增 `site_dir_state` ∈ `present` / `absent` / `unknown` 与 `site_dir_reason`（仅 unknown 非空）；两处同口径。容器模式改用 `docker exec <container> test -d <root>` 判定（argv 固定、不经 shell、20s 超时）：退出码 0 = present；退出码 1 且无输出 = absent；docker daemon 报错 / 125-126 / 启动失败 / 超时 / vhost 无 root = unknown，不再把「查不到」误报成「目录不存在」。
- 兼容策略：保留 `site_dir_exists` 布尔字段，取值恒为 `site_dir_state == "present"`，既有调用方语义不变（unknown 与 absent 都是 false）；需要区分「未知」与「不存在」的调用方读新字段。宿主模式判定不变（`os.Stat`：ENOENT / 非目录 = absent，其余读失败 = unknown）。

### 测试

- 单测（`sitedir_test.go`）：宿主三态（存在 / 缺失 / 非目录 / 空 root / 相对 root / ENOTDIR）、容器三态（present / absent / 退出码 1 带 stderr / 125 / 126）、降级路径（docker 不在 PATH、docker exec 超时）、`site.status` 字段接线、「无 vhost → unknown」、`site.get` 同口径。
- 一致性测试（`methodcaps_test.go`）：`info.json.method_capabilities` 与 `siteverbs.go` 的 `verbRegistry` 双向断言（声明 ⊆ 实现、实现 ⊆ 声明）+ 锁定 16 方法与只读/写分类；`info.json` 与 `manifest.json` 的 id/version/capabilities 同步断言。
- 故障注入（`tests/fault/sitedir_test.go`）：docker 缺失、docker daemon 报错、vhost 无 root 三条降级路径均回 `unknown` 且插件仍能正常应答 `kore.shutdown`。

## [0.2.0] - 2026-11-23

- feat: 支持容器内 nginx（docker exec 校验与重载）
- build: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛），二进制改嵌 manifest.json，消除同版本 sha 漂移

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

### 新增（容器内 nginx）

- 新增 `nginx_mode`（`auto|host|docker|off`，默认 `auto`）：auto 先找宿主 `nginx` 命令，找不到再用
  `docker ps` 挑镜像名含 nginx 的运行中容器，都没有则 `off` 并在 `nginx_reason` 明确写出
  「未检测到 nginx（宿主与容器都没有）」，不再静默 `test_skipped`。
- docker 模式命令固定 argv：`["docker","exec",<container>,"nginx"]` + `-t` / `-s reload` / `-T`；
  容器名硬校验 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`，不经过 shell；命令超时 20s。
- 新增配置契约 `site.config.get` / `site.config.set`：`nginx_mode` / `docker_container` /
  `docker_conf_dir` / `host_conf_dir` 四项写入 `<KORE_STATE_DIR>/config.json`（0600、原子替换），
  保存后下一次调用立即生效（`restart_required=false`）；环境变量 `KORE_NGINX_MODE` 等作为兼容入口。
- 写路径前置校验（docker 模式）：宿主 `host_conf_dir` 存在且可写（否则 `E_INVALID` + 挂载提示）→
  容器存在（`docker inspect` 失败回 `E_UPSTREAM`）→ 容器内 `docker_conf_dir` 必须确实由该宿主目录
  挂载而来（`-v /etc/nginx:/etc/nginx` 这类父目录挂载按最长前缀匹配），避免「写成功但容器读不到」。
- 所有写操作与 `list` / `status` 结果新增 `nginx_mode` / `nginx_reason`，前端可把「未检测」渲染成原因。
- 新增真实容器 E2E `TestDockerContainerMode`：`config.set` 配 docker → create 后
  `curl Host: ct.kore.test` 返回 200 + 标记、`nginx -T` 命中 server_name → 坏 conf 时
  `status.test_passed=false` 且插件回滚、真实 `nginx -t` 仍通过 → delete 后 curl 不再命中、`nginx -T` 已卸载 →
  `docker_container="../../etc"` 被 `E_INVALID` 拒绝。

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
