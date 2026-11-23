# kore-plugin-site

kore 的 **M2 站点管理插件**（nginx 站点生命周期）。`info.json.depth_level = "production"`。

## 定位与边界

- 协议：`kore.plugin.v1`，`lifecycle = resident`，唯一第三方依赖 `github.com/pengtang1024/kore/pkg/pluginapi v0.1.1`（其余为标准库）。
- **建站链路**：原子写独立 `conf.d/kore-site-<domain>.conf` → `nginx -t` 校验（失败回滚并回 `E_INVALID`）→ `nginx -s reload`（失败回 `E_UPSTREAM`，明确"已写入但未生效"）。
- 不碰宿主 nginx 主配置（`nginx.conf`）；只写独立 vhost 片段，由宿主既有的 `conf.d/*.conf` include 引入。
- 不使用 `default_server`；`default_server=true` 的创建请求直接回 `E_INVALID`（红队 R6）。
- **不内置、不捆绑** nginx/caddy/httpd 二进制（裁决37）：`nginx_mode=auto` 先找宿主 `nginx`，找不到再找运行中的 nginx 容器并改用 `docker exec`；也可强制 `host` / `docker` / `off`。

## 方法表

| 方法（裸名 / 全名） | 行为 |
| --- | --- |
| `site.create` / `create` | 写 vhost + 清单 → `nginx -t` → `nginx -s reload`；返回 `vhost_path` / `backup_path` / `test_passed` / `reloaded` / `applied`。校验失败回滚，重载失败回 `E_UPSTREAM`。 |
| `site.delete` / `delete` | 备份并删除 vhost + 清单 → `nginx -t` → `nginx -s reload`；任一失败恢复删除前状态。 |
| `site.status` / `status` | 返回 `conf_exists` / `test_passed` / `test_skipped` / `site_dir_state`（`present` / `absent` / `unknown`）/ `site_dir_exists`（兼容布尔，仅 `present` 为 true）/ `site_dir_reason`（unknown 的原因与处置）/ `loaded` / `load_checked` / `reload_skipped` / `managed`。只读，不 reload。 |
| `site.list` / `list` | 每条带 `exists` / `enabled` / `disabled` / `test_ok` / `test_skipped` / `reloaded` / `reload_skipped` / `test_output` 真实状态；`nginx -t` 与 `nginx -T` 各只跑一次。 |
| `site.get` / `get` | 完整配置摘要：`root` / `port` / `index` / `ssl` / `rewrite` / `proxy` / `redirects` / `enabled`，以及与 `status` 同口径的 `site_dir_state` / `site_dir_exists` / `site_dir_reason`。只读，不 reload。 |
| `site.bindSSL` / `bindSSL` | 在 vhost 内写 `listen 443 ssl` + 证书 + TLS1.2+ 套件，默认附 80→443 跳转；`force_https=false` 时 80/443 并存。 |
| `site.unbindSSL` / `unbindSSL` | 移除整个 SSL 块（含 443 监听与跳转），站点回到纯 HTTP。 |
| `site.setRewrite` / `setRewrite` | 伪静态模板枚举 `none \| wordpress \| laravel \| thinkphp \| discuz \| nextjs`，整块替换。 |
| `site.getRewrite` / `getRewrite` | 从注释块解析当前模板名（禁用站点同样可读）。 |
| `site.setProxy` / `setProxy` | 生成 `location /` 反代；`ws=true` 补 WebSocket 升级头；`upstream` 只接受 `http://host:port`。 |
| `site.unsetProxy` / `unsetProxy` | 移除反代块。 |
| `site.setRedirect` / `setRedirect` | `location = <from_path>` 精确匹配重定向，`code` 默认 301、只允许 301/302。 |
| `site.unsetRedirect` / `unsetRedirect` | 按 `from_path` 删除重定向（不存在时幂等）。 |
| `site.disable` / `site.enable` | 把 vhost 改名为 `.conf.disabled` / 改回 `.conf` 后 test+reload；配置不删。 |
| `site.setIndex` / `setIndex` | 设置 `index` 指令，默认 `index.html index.htm index.php`。 |
| `site.clone` / `clone` | 克隆站点 `{source_domain, new_domain, root?, copy_root?}`：复制 vhost 全量配置（SSL/伪静态/反代/重定向/index）；`copy_root=true` 仅复制 `KORE_STATE_DIR` 内的站点根（ADR-0023）。 |
| `site.setPassword` / `clearPassword` | 站点级 Basic 认证：`setPassword {domain, username, password}` 写入 apr1 哈希到 vhost 同目录 htpasswd（密码不回显）；`clearPassword {domain}` 移除 auth 段并删除密码文件。 |
| `site.config.get` / `config.get` | 返回 4 个配置项的 schema、当前值与来源（`file` / `env` / `default`）。只读。 |
| `site.config.set` / `config.set` | 校验 → 原子写 `<KORE_STATE_DIR>/config.json`（0600）→ 下一次调用立即生效（`restart_required=false`）。 |
| `kore.ping` / `kore.reload` / `kore.shutdown` | 协议必选方法；`kore.reload` 无外部配置，`config_digest` 为空。 |

**三态口径**：`test_skipped=true` / `reload_skipped=true` 表示本机没有可用的 nginx（auto 探测落空或 `KORE_NGINX_*_CMD=off` 显式关闭），此时 `test_passed=false` **不代表站点故障**；只有 `test_passed=false && !test_skipped` 才是真的 `nginx -t` 失败，并附 `test_output` 摘要。

所有写操作与 `list` / `status` 结果都带 `nginx_mode`（`host` / `docker` / `off`）与 `nginx_reason`（跳过原因）。真实探测落空时 `nginx_reason="未检测到 nginx（宿主与容器都没有）"`，前端可据此把「未检测」渲染成「为什么没检测」，而不是一个灰点。

**站点目录三态**：vhost 里的 `root` 在容器模式下是**容器内路径**，宿主 `os.Stat` 会把它误判成「不存在」。因此 `site.status` 与 `site.get` 统一用三态表达：

| `site_dir_state` | 含义 | `site_dir_exists`（兼容字段） |
| --- | --- | --- |
| `present` | 确认存在 | `true` |
| `absent` | 确认不存在（`test -d` 无输出退出 1 / 宿主 `ENOENT` / 存在但不是目录） | `false` |
| `unknown` | **查不到**：docker 不可用、容器未运行、命令超时、vhost 无 `root` 指令、宿主读取失败 | `false` |

`unknown` 时 `site_dir_reason` 给出原因与处置（例如「安装 docker 或把 `nginx_mode` 改为 host」）。前端必须区分「未知」与「不存在」——**`unknown` 不得渲染成「目录不存在」，更不得据此提示删除站点**。
容器模式判定用 `docker exec <container> test -d <root>`（argv 固定、不经 shell、20s 超时）：退出码 0 = `present`；退出码 1 且无任何输出 = `absent`；其余（daemon 报错、125/126、启动失败、超时）= `unknown`。

`domain` 约束：小写、每个标签以字母数字开头结尾、至少两个标签、总长 ≤ 253；拒绝路径分隔符与 `..`，防止 vhost 路径穿越。

`root` 约束：绝对路径、无冗余分量、仅允许 `A-Za-z0-9._/@+-`，拒绝空白/分号/花括号/换行等配置注入字符。

## 方法级能力（method_capabilities）

`info.json` 逐方法声明宿主特权能力（协议见 `docs/PLUGIN-PROTOCOL.md` §3.1.2.1，格式照 `kore-plugin-cron` / `kore-plugin-waf`）：内核按方法取用，命中即单独生效，未声明的方法回退插件级 `capabilities`。

| 类别 | 方法 | 能力 |
| --- | --- | --- |
| 只读（4） | `list` / `status` / `get` / `getRewrite` | `system:read` |
| 写（12） | `create` / `delete` / `bindSSL` / `unbindSSL` / `setRewrite` / `setProxy` / `unsetProxy` / `setRedirect` / `unsetRedirect` / `disable` / `enable` / `setIndex` | `system:admin` |
| 配置契约（2） | `config.get` / `config.set` | 读 `system:read` / 写 `system:admin` |

收益：只读调用按 `system:read` 判定，不再按插件级 `system:admin` 写 `invoke.*` 审计（§3.5 #12）；写方法仍按 admin 审计。`config.get` / `config.set` 自 0.3.1 起也逐条声明（读=read、写=admin），**配置读取不再按 admin 类进审计**。

`config.*` 由 `methods.go` 的 `dispatch` 直接分发（配置面板通用契约），不实现 `verbHandler`、**不注册进 `verbRegistry`**。单测因此分两组断言：业务方法（16 条）与 `verbRegistry` 双向对齐（声明 ⊆ 实现、实现 ⊆ 声明），`config.*`（2 条）单独断言「必须存在且读=read、写=admin」；另有一条防回归断言要求每个声明键都以 `site.` 为域。新增/删除方法时 info.json 与对应实现必须同改。

## L2 能力（可用）：SSL / 伪静态 / 反代 / 重定向 / 启停 / index

L2 配置的**唯一事实来源就是 vhost 文件本身**：受管片段用注释标记块
（`# >>> kore-site:<key> >>>` … `# <<< kore-site:<key> <<<`）括起来，每次改动都走
「解析现状 → 改状态 → 整体重渲染 → 备份 → 原子写 → `nginx -t` → reload → 失败回滚」
这一条编排（`applyEdit`），不存在第二份状态文件。

- **写操作统一闭环**：任何写入先留同目录 `*.bak.<ts>` 备份，`nginx -t` 失败回滚并回
  `E_INVALID`（附 nginx 输出），reload 失败回滚并回 `E_UPSTREAM`。
  目标状态与现状字节一致时短路：不写盘、不跑 nginx，回 `changed=false` + `test_skipped`。
- **SSL**：`cert_path` / `key_path` 必须是规范化绝对路径、字符集 `A-Za-z0-9._/@+-` 且真实存在；
  TLS 套件固定 TLS1.2+，不接受自定义原始片段。`force_https` 默认 `true`（80 由独立 server 块 301 到 https）。
- **伪静态**：只给枚举模板，不允许自定义 nginx 片段；`none` 表示默认 `try_files $uri $uri/ =404`。
- **反代与伪静态互斥**：两者都占用 `location /`，同时存在会让 `nginx -t` 报 duplicate location，
  因此互相设置时会自动清掉对方，并在结果里回 `rewrite_cleared` / `proxy_cleared`。
- **重定向**：`from_path` 必须 `/` 开头且不含 `..`；`to_url` 只允许 `http://` / `https://` 或站内 `/` 路径
  （`javascript:` / `data:` / 协议相对 `//host` 一律拒绝），字符集排除引号、分号、`#` 与空白。
- **启停**：`disable` 只把 `<domain>.conf` 改名为 `<domain>.conf.disabled`（不被 `conf.d/*.conf` 匹配），
  配置与备份全部保留；`enable` 改回。任一步 test/reload 失败都会改回原名。
- **单文件 ≤200 行**：能力按 `ssl.go` / `rewrite.go` / `proxy.go` / `redirect.go` / `toggle.go` /
  `index.go` / `get.go` 拆分，解析与渲染分别是 `parse.go` / `render.go`。

## 命令执行安全

- **host 模式**：命令前缀来自 `KORE_NGINX_TEST_CMD` / `KORE_NGINX_RELOAD_CMD` / `KORE_NGINX_DUMP_CMD`，按 shell-like 规则切分为 argv，**不经过 `/bin/sh`**；未配置时默认 `nginx`。
- **docker 模式**：argv 固定为 `["docker","exec",<container>,"nginx"]` + 插件注入的操作旗标（`-t` / `-s reload` / `-T`），容器名是独立 argv，不拼进任何 shell 字符串。
- 容器名硬校验 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`，拒绝 `../`、空白、分号、`$()` 等注入面；`config.set` 阶段即回 `E_INVALID`。
- 操作旗标由插件固定注入：校验 `-t`、重载 `-s reload`、转储 `-T`；host 前缀自带 `-t`/`-T`/`-s` 会被判配置非法。
- RPC 入参（`domain` / `root` / `port`）只参与文件路径与已校验的模板，**永不进入 argv**。
- 每条命令（含 `docker exec` / `docker inspect`）都有 20s 超时；进入错误消息的输出截断到 4096 字符。
- **写路径前置校验**（仅 docker 模式）：宿主 `host_conf_dir` 必须存在且可写 → 容器必须存在（`docker inspect` 失败回 `E_UPSTREAM`）→ 容器内 `docker_conf_dir` 必须确实由该宿主目录挂载而来（否则 `E_INVALID` 并给出应改成的路径）。这样不会出现「配置写成功但容器读不到」的假生效。
- 只读方法（`list` / `status`）不做额外 `docker inspect`：容器不存在时由 `nginx -t` 的真实失败出现在 `test_output` 里（不是静默 skipped），不因一次查看而放大容器调用。`status` 在 docker 模式只额外执行一次目录探测 `docker exec <container> test -d <root>`（20s 超时，用于三态 `site_dir_state`）。

## 环境变量

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `KORE_PLUGIN_SOCK` | 是 | 主壳创建的 UDS 路径 |
| `KORE_PLUGIN_ID` | 否 | 内核登记 id，缺省取内嵌 `info.json.id` |
| `KORE_PLUGIN_TOKEN` | 否 | 内核注入令牌；当前只随 `kore.hello` 上报 |
| `KORE_LOG_LEVEL` | 否 | `debug`/`info`/`warn`/`error`，缺省 `warn`；日志只写 stderr |
| `KORE_NGINX_MODE` | 否 | `auto`/`host`/`docker`/`off`，缺省 `auto`；对应 `config.json.nginx_mode` |
| `KORE_DOCKER_CONTAINER` | 否 | 容器名，对应 `docker_container`；`docker` 模式必填 |
| `KORE_DOCKER_CONF_DIR` | 否 | 容器内 conf.d 路径，缺省 `/etc/nginx/conf.d`；对应 `docker_conf_dir` |
| `KORE_HOST_CONF_DIR` | 否 | 宿主 conf.d 路径，缺省 `/etc/nginx/conf.d`；对应 `host_conf_dir` |
| `KORE_SITE_CONF_DIR` | 否 | `KORE_HOST_CONF_DIR` 的历史别名（测试与自定义布局用） |
| `KORE_NGINX_TEST_CMD` | 否 | host 模式校验命令前缀，缺省 `nginx`（实际执行 `nginx -t`）；`off` 显式跳过校验 |
| `KORE_NGINX_RELOAD_CMD` | 否 | host 模式重载命令前缀，缺省 `nginx`（实际执行 `nginx -s reload`）；`off` 显式跳过重载 |
| `KORE_NGINX_DUMP_CMD` | 否 | host 模式转储命令前缀，缺省由校验前缀推导并补 `-T`；`off` 显式跳过加载检测 |

配置优先级：`<KORE_STATE_DIR>/config.json` > 环境变量 > 代码默认值。`config.json` 由 `site.config.set` 写入（0600、原子替换），保存后下一次方法调用立即生效，无需重启插件；显式给了 `nginx_mode` 时它优先于 `KORE_NGINX_*_CMD`（auto 模式下显式命令前缀仍优先，保证老部署升级后行为不变）。

容器模式示例（容器把宿主 `/tmp/kore-site-cnf` 挂到 `/etc/nginx/conf.d`）：

```bash
docker run -d --name kore-nginx -p 18082:80 \
  -v /tmp/kore-site-cnf:/etc/nginx/conf.d:ro nginx:alpine

KORE_NGINX_MODE=docker \
KORE_DOCKER_CONTAINER=kore-nginx \
KORE_HOST_CONF_DIR=/tmp/kore-site-cnf \
./bin/site
```

等价地通过 `site.config.set` 保存（`{"values":{"nginx_mode":"docker","docker_container":"kore-nginx","docker_conf_dir":"/etc/nginx/conf.d","host_conf_dir":"/tmp/kore-site-cnf"}}`），保存后立即生效。不配 `docker_container` 时也可用 `nginx_mode=auto` 自动挑运行中的 nginx 容器。

`stdout` 只留给协议帧，任何日志都不得写 stdout。

## 构建与打包

```bash
make build   # CGO_ENABLED=0 GOAMD64=v1 go build -trimpath -buildvcs=false -ldflags="-s -w" -o bin/site .
make dist    # 产出 dist/site-0.1.0.tar.gz（info.json + bin/site + SHA256SUMS）与同名 .sha256
make check   # gofmt → go vet → go test -timeout 200s
```

`dist` 会按包内实际字节数回填 `info.json.size_bytes`（仓库里的清单保持 `0`，由打包脚本写入真实值）。

docker 端到端（真实 nginx:alpine）：`TestDockerNginxLifecycle` 覆盖 v1 四步生命周期，
`TestDockerNginxL2` 覆盖 L2 六步（create+bindSSL 的 https 200 / http 301 → wordpress 伪静态 →
反代命中容器内 `127.0.0.1:18990` 后端 → `/old` 302 → disable 后不再命中、enable 恢复 → index 指令），
`TestDockerContainerMode` 覆盖容器模式（`config.set` 配 docker → create 后 curl 200 + `nginx -T` 命中 →
坏 conf 时 status `test_passed=false` 且插件回滚、真实 `nginx -t` 仍通过 → delete 后 curl 不再命中且
`nginx -T` 已卸载 → 注入式容器名被拒）：

```bash
go test -tags e2e -timeout 300s -v ./tests/e2e
```

L2 反代后端是 `tests/e2e/testdata/backend`（`CGO_ENABLED=0` 静态二进制，运行期 `docker cp` 进容器），
因为 nginx:alpine 的 busybox 不含 httpd applet、容器内也没有 python。

本地联调依赖**不提交**的 `go.work`（`use .` + `use ../kore/pkg/pluginapi`）；跨仓 `replace` 禁止写入 `go.mod`（G-17）。

## 明确不做

- 不写宿主 `nginx.conf`、不注入 include、不抢 `default_server`。
- 不内置/捆绑任何 web server 二进制，不自动安装 nginx、不自动拉起容器。
- 不管理容器的生命周期（不负责 `docker run` / 重启 / 删除），只通过 `docker exec` 使用已运行的容器。
- 不做证书签发/续期、DNS、防火墙等站点外资源编排：SSL 只引用运维已提供的证书路径。

<!-- oss:begin -->
## 开源与贡献

[![CI](https://github.com/kore-pl/kore-plugin-site/actions/workflows/ci.yml/badge.svg)](https://github.com/kore-pl/kore-plugin-site/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)

本仓库是 **kore** 的 `Nginx 站点管理插件`（id `site`），独立仓库、独立版本线、独立 CI（少触发：push main / tag / PR / 手动）。内核与协议见 [kore](https://github.com/kore-pl/kore)。

- **许可**：MIT（inbound = outbound，不要求 CLA）；提交用 `git commit -s` 签 DCO。
- **贡献**：[CONTRIBUTING.md](CONTRIBUTING.md)（构建、门禁、发版、边界）。
- **安全**：[SECURITY.md](SECURITY.md)——漏洞不要开公开 issue。
- **行为准则**：[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。
- **兼容性**：`protocol: kore.plugin.v1` · `requires_kore: >=0.1.0 <0.2.0` · `lifecycle: resident` · 深度等级 `production`。
- **验证方式**：本地 `make check` 优先（CI 只在 push main / tag / PR / 手动跑；发布前 `make dist` 连跑两次核对 sha 一致）。
- **发布产物**：`make dist` → `dist/site-0.3.1.tar.gz` + `.sha256`（位级可复现，连跑两次 sha 相同）。
- **相关仓库**：[索引 kore-plugins](https://github.com/kore-pl/kore-plugins) · [协议文档](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-PROTOCOL.md) · [SDK 指南](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-SDK.md)

<!-- oss:end -->
