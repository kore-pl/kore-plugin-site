# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本。

## [0.5.0] - 2026-11-24

### 新增

- **声明 3 条 `root_actions`，修复「建站功能根本不可用」（P0）**：插件降权后以 `kore-site` 运行，
  写 `/etc/nginx/conf.d`、`nginx -t`、`nginx -s reload` 都需要 root，此前**零声明**等于连申请通道都没有。
  现按 ADR-0022 声明 `nginx-test`（`nginx -t`）、`nginx-reload`（`nginx -s reload`）、
  `nginx-dump`（`nginx -T`）三条固定 argv，形状全部落在 `rootpolicy.go` 的 `nginx` 条目内，
  且该形状**没有 Tail**——运行时追加任何参数都会被内核拒绝（`TestRootActionsRejectAppendedArgs`）。
- **执行改走内核提权通道**：host 模式下 `nginx -t/-T/-s reload` 经内核 UDS 的
  `POST /api/v1/plugins/site/root-action` 执行（`rootaction_client.go`，Bearer token，只允许调用本插件已声明动作），
  不再由插件进程直连 `exec` 去撞权限；docker 模式不变（`docker exec … nginx` 在容器内执行）。
- **非 root 降级（E_NEED_ROOT + 可操作提示，不崩）**：
  - 命令因权限被拒（内核 `E_NEED_ROOT` 或直连输出 `permission denied`）→ `E_NEED_ROOT` + hint；
  - 写 vhost 失败且 `conf.d` 属主是 root、当前用户非 root → `E_NEED_ROOT` + 目录授权 hint；
  - 其余权限失败保持 `E_INTERNAL`（不把所有失败归因 root，`cf0cd25` 的 boundary 断言原样通过）。

### 边界与未验证项

- **写 vhost 仍未走提权通道**：ADR-0022 的 13 条受控命令没有文件写原语；ADR-0028 一期的受控写白名单
  不含 `/etc/nginx/conf.d` 且尚未接线到 root-action。要真正以 root 写 `conf.d` 需内核侧先改 ADR-0028 §3（复审条件 #1）。
  当前可行解：给插件用户目录写权限（hint 内已给出 `chgrp`/`chmod`/`setfacl` 具体命令），或把 `host_conf_dir`
  指向插件可写目录并由宿主 `nginx.conf` include。
- **root 下的通道成功路径未在本机验证**（本机非 root）：只实测了非 root 的 `E_NEED_ROOT` 路径
  （`tests/boundary/needroot_test.go`，实测输出见发版证据）。验证方式见 README「权限模型」末段。
- 新增单测：声明双向一致（`TestRootActionsDeclared`）、形状无停服语句（`TestRootActionsReadOnlyShape`）、
  追加参数被拒（`TestRootActionsRejectAppendedArgs`）、通道优先与 id 对齐（`TestHostRunnerUsesPrivilegeChannel`）、
  `E_NEED_ROOT` 结构化升级（`TestNeedRootFromChannel`）、非权限错误不误归因（`TestOtherChannelErrorsNotAttributedToRoot`）。
- 版本 0.4.4 → 0.5.0；manifest.json 同步。

## [0.4.4] - 2026-11-24

### 修复

- **并发建站丢站点（P1 数据丢失）**：`kore-site-manifest.json` 的「读-改-写」没有跨进程互斥，
  多实例共享同一 conf.d 时（内核重启/升级期间新旧实例重叠，或两实例共用配置目录），
  各自读到旧清单再写回，后写者覆盖先写者。实测 8 个真实进程并发建站后清单只剩 1–2 条，
  文件层面的 vhost 都在、`site.list` 却漏站点。
  修复：`site.create` 与 `site.delete` 的「读清单 → 落 vhost → 写回清单」整段进入
  `flock`（Windows 为 `LockFileEx`）排他临界区；锁固定在旁路文件
  `kore-site-manifest.json.lock` 上（清单本身用 rename 原子替换，锁在清单 inode 上会随替换失效）。
  读者（`site.list` / `site.get` / `site.status` / `site.clone` / 配置读取）**不加锁**：
  rename 保证任何时刻读到的都是某个完整版本，避免无谓的读侧串行化。
- 锁由内核在进程退出时释放（故障注入用例会 `SIGKILL` 插件），不会留下悬挂锁；
  遇到不支持跨进程文件锁的平台显式报错，不退化成无声的进程内互斥。
- 清单**文件格式与语义不变**（`version: 1`，同字段同含义），不涉及协议兼容性变更。
- 边界用例新增 `TestBoundaryConcurrencyManifestNoLostUpdate`：8 实例并发建站后断言
  ① 清单条目数 == 8、② 每个 vhost 的 `server_name` 唯一且无交叉串扰、③ 无 `.tmp` 残留 / 无数据竞争
  （`-race` 插桩进程），并断言事后 `site.list` 返回 8 条。修复前该用例稳定 FAIL（清单只剩 1 条）。
- 版本 0.4.3 → 0.4.4；manifest.json 同步。

## [0.4.3] - 2026-11-23

### 变更

- **消费 ADR-0023 阶段 C 插件数据根**：未显式指定 `root` 时，默认站点根改为
  `$KORE_PLUGIN_DATA/sites/<domain>`（内核注入，落在安装目录内、属主 `kore-site`）；
  未注入该变量时回退历史默认 `/var/www/<domain>`，并向 stderr 打一条 WARN 说明"非零侵入模式"
  （不静默降级）。显式配置（`site.create` / `site.clone` 传入的 `root`）永远优先，插件不改写。
- **既有站点绝不迁移**：重复 `site.create` 未带 `root` 时保留原 vhost 里的根目录，不会因为默认值
  变化把线上站点根搬走（回归测试 `TestExistingSiteRootNotMigrated`）。
- 新增只读标记：`site.status` / `site.list` 在站点根位于 `KORE_INSTALL_DIR` 之外时返回
  `outside_install: true` + `outside_install_hint`（提示可用 `site.move` 迁入）。**不实现自动搬迁**；
  未注入 `KORE_INSTALL_DIR` 时无法判定，不误报。
- 单测：有/无 `KORE_PLUGIN_DATA` 的默认根解析、显式 root 优先、旧站点保持、outside_install 标记
  （含安装目录内外两种场景）。
- 版本 0.4.2 → 0.4.3；manifest.json 同步。

## [0.4.2] - 2026-11-23

### 新增

- `site.traffic {domain, log_path?, lines?}`：只读站点流量统计，解析 nginx **combined** access_log。
  数据源优先级：显式 `log_path` > vhost 里的 `access_log` 指令 > 约定路径
  `/var/log/nginx/<domain>.access.log` > `/var/log/nginx/access.log`。
  返回总请求数、发送字节、独立 IP 数、时间窗口、状态码分类（2xx/3xx/4xx/5xx）、热门路径 Top10、
  请求 IP Top10，并按实际解析行数如实给出 `parsed` / `skipped`。
- **只读且不伪造**：只读日志末尾最多 4 MiB（`lines` 1..5000，默认 500），不写任何文件、不联网、
  不起定时器（G-19）。读不到日志时返回 `available=false` + `reason`（`no_log_file` /
  `permission_denied` / `no_entries`）+ `tried` 路径列表 + 可操作 hint（如何加 access_log 或修权限），
  不返回任何编造数字。
- 安全：`log_path` 只允许 `/var/log/` 与 `KORE_STATE_DIR` 内的文件，拒绝相对路径与 `..`，
  避免插件变成任意文件读取器。
- `method_capabilities` 补 `site.traffic = system:read`（只读分类表同步）；单测覆盖解析统计、
  无法解析行计数、非法路径拒绝、三种空态。
- 版本 0.4.1 → 0.4.2；manifest.json 同步。

## [0.4.1] - 2026-11-23

### 新增

- `site.setPassword {domain, username, password}` / `site.clearPassword {domain}`：站点级
  HTTP Basic 认证。密码只以 **Apache apr1（MD5-crypt，`$apr1$`）哈希**写入 vhost 同目录的
  `kore-site-<domain>.htpasswd`（0644，nginx worker 可读）；明文不落盘、不进 vhost、不进任何返回体，
  返回只给 `enabled` / `username` / `changed` / `hash_scheme` 与生效状态。
- vhost 改动复用 `applyEdit` 的「原子写 → nginx -t → reload → 失败回滚」闭环；auth 段是受管注释块
  （`kore-site:auth`），清除即整段移除并删除密码文件。设置失败会同时回滚 vhost 与 htpasswd。
- 幂等：同用户同密码不重写 vhost、不触发 reload（复用旧盐重算校验）；重复清除返回 `changed=false`。
- 口令策略：密码 8..128 字符、拒绝换行/NUL；用户名 1..32 位字母数字与 `._-`，字母数字开头。
- `method_capabilities` 补 `site.setPassword` / `site.clearPassword = system:admin`；
  单测覆盖设置、清除、幂等、弱口令拒绝与**返回体不回显密码/哈希**，并用 openssl apr1 向量对照哈希实现。
- 版本 0.4.0 → 0.4.1；manifest.json 同步。

## [0.4.0] - 2026-11-23

### 新增

- `site.clone {source_domain, new_domain, root?, copy_root?}`：复制已托管站点的完整 vhost 配置
  （server_name / root / SSL / 伪静态 / 反向代理 / 重定向 / index）到新域名；复用既有
  `store.create`（原子写 + 清单 + 冲突保护）与 `applyEdit`（nginx -t → reload → 失败回滚）闭环。
- `copy_root=true` 时递归复制站点根目录；按 ADR-0023 零侵入 / G-31，**源与目标目录都必须在
  `KORE_STATE_DIR` 内**，拒绝符号链接与设备文件，上限 10 万文件 / 2 GiB；宿主 `/var/www`
  请只克隆配置（copy_root=false）后由运维迁移。
- 冲突语义：目标域名已托管 → `E_CONFLICT`（不覆盖）；源站点不在清单 → `E_NOTFOUND`；
  同域名克隆 → `E_INVALID`。
- `method_capabilities` 补 `site.clone = system:admin`（写类）。
- 版本 0.3.1 → 0.4.0；manifest.json 同步。

## [0.3.1] - 2026-11-23

### 修复（补全方法级 capability，§3.5 #12）

- **补 `config.get` / `config.set` 方法级声明**：`site.config.get = system:read`、`site.config.set = system:admin`。0.3.0 只声明了 16 个业务方法，`config.*` 未声明时内核回退插件级 `capabilities`（`system:admin`），于是**配置读取也按 admin 类写 `invoke.*` 审计**——本次补齐后 `config.get` 按 `system:read` 判定，**配置读取不再进 admin 审计**；`config.set` 仍为 admin。
- `method_capabilities` 16 → 18 条。内核侧无需改动：`validate.go` 的键域校验（`<id>.<action>`）、动作段规则（`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`，天然接受 `config.get` 这种点分动作）与 6 项特权白名单，及 `invoke_capability.go` 的取用顺序（方法级命中优先、未命中回退插件级）本来就覆盖这两条。
- 版本号 0.3.0 → 0.3.1（patch：只补 capability 声明与版本元数据，不改任何业务逻辑——三态 `site_dir_state` / `site_dir_reason` 与 16 个 verb 行为保持 0.3.0 原样），`manifest.json`（内嵌 manifest）同步 bump。

### 测试

- `methodcaps_test.go` 断言拆成两组，对应插件里两条真实分发路径：
  - 业务方法（16 条）：继续与 `siteverbs.go` 的 `verbRegistry` 双向对齐（声明 ⊆ 实现、实现 ⊆ 声明）+ 锁定只读/写分类；
  - `config.*`（2 条）：单独断言「必须存在且 `get=system:read`、`set=system:admin`」。config 由 `methods.go` 的 `dispatch` 直接分发（配置面板通用契约）、不实现 `verbHandler`，**不塞进 `verbRegistry`**，否则分发 switch 与注册表会出现两个事实来源。
- 新增防回归断言 `TestMethodCapabilitiesDomainScoped`：`info.json` 声明的每个键都必须以 `site.` 为域（内核该条 fail-closed，写错一个键整份清单加载失败）。
- 反例验证（干净树复现）：新断言 + 0.3.0 的 `info.json`（16 条）→ `FAIL: 声明 16 条，期望 18 条`；旧断言 + 0.3.1 的 `info.json`（18 条）→ `FAIL: 声明 18 条，期望 16 条`——证明断言必须拆组，不能靠把 config 塞进 verbRegistry 过关。

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
