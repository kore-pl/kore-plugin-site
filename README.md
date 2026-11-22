# kore-plugin-site

kore 的 **M2 站点管理插件**（nginx 站点生命周期）。`info.json.depth_level = "production"`。

## 定位与边界

- 协议：`kore.plugin.v1`，`lifecycle = resident`，唯一第三方依赖 `github.com/pengtang1024/kore/pkg/pluginapi v0.1.1`（其余为标准库）。
- **建站链路**：原子写独立 `conf.d/kore-site-<domain>.conf` → `nginx -t` 校验（失败回滚并回 `E_INVALID`）→ `nginx -s reload`（失败回 `E_UPSTREAM`，明确"已写入但未生效"）。
- 不碰宿主 nginx 主配置（`nginx.conf`）；只写独立 vhost 片段，由宿主既有的 `conf.d/*.conf` include 引入。
- 不使用 `default_server`；`default_server=true` 的创建请求直接回 `E_INVALID`（红队 R6）。
- **不内置、不捆绑** nginx/caddy/httpd 二进制（裁决37）：命令默认调用宿主已安装的 nginx，可通过环境变量指向容器内执行。

## 方法表

| 方法（裸名 / 全名） | 行为 |
| --- | --- |
| `site.create` / `create` | 写 vhost + 清单 → `nginx -t` → `nginx -s reload`；返回 `vhost_path` / `backup_path` / `test_passed` / `reloaded` / `applied`。校验失败回滚，重载失败回 `E_UPSTREAM`。 |
| `site.delete` / `delete` | 备份并删除 vhost + 清单 → `nginx -t` → `nginx -s reload`；任一失败恢复删除前状态。 |
| `site.status` / `status` | 返回 `conf_exists` / `test_passed` / `test_skipped` / `site_dir_exists` / `loaded` / `load_checked` / `reload_skipped` / `managed`。只读，不 reload。 |
| `site.list` / `list` | 每条带 `exists` / `enabled` / `disabled` / `test_ok` / `test_skipped` / `reloaded` / `reload_skipped` / `test_output` 真实状态；`nginx -t` 与 `nginx -T` 各只跑一次。 |
| `site.get` / `get` | 完整配置摘要：`root` / `port` / `index` / `ssl` / `rewrite` / `proxy` / `redirects` / `enabled`。只读，不 reload。 |
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
| `kore.ping` / `kore.reload` / `kore.shutdown` | 协议必选方法；`kore.reload` 无外部配置，`config_digest` 为空。 |

**三态口径**：`test_skipped=true` / `reload_skipped=true` 表示本机没有可用的 nginx（未配置或 `KORE_NGINX_*_CMD=off` 显式关闭），此时 `test_passed=false` **不代表站点故障**；只有 `test_passed=false && !test_skipped` 才是真的 `nginx -t` 失败，并附 `test_output` 摘要。

`domain` 约束：小写、每个标签以字母数字开头结尾、至少两个标签、总长 ≤ 253；拒绝路径分隔符与 `..`，防止 vhost 路径穿越。
`root` 约束：绝对路径、无冗余分量、仅允许 `A-Za-z0-9._/@+-`，拒绝空白/分号/花括号/换行等配置注入字符。

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

- 命令前缀来自 `KORE_NGINX_TEST_CMD` / `KORE_NGINX_RELOAD_CMD` / `KORE_NGINX_DUMP_CMD`，按 shell-like 规则切分为 argv，**不经过 `/bin/sh`**。
- 操作旗标由插件固定注入：校验 `-t`、重载 `-s reload`、转储 `-T`；前缀自带 `-t`/`-T`/`-s` 会被判配置非法。
- RPC 入参（`domain` / `root` / `port`）只参与文件路径与已校验的模板，**永不进入 argv**。
- 每条命令有 20s 超时；进入错误消息的输出截断到 4096 字符。

## 环境变量

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `KORE_PLUGIN_SOCK` | 是 | 主壳创建的 UDS 路径 |
| `KORE_PLUGIN_ID` | 否 | 内核登记 id，缺省取内嵌 `info.json.id` |
| `KORE_PLUGIN_TOKEN` | 否 | 内核注入令牌；当前只随 `kore.hello` 上报 |
| `KORE_LOG_LEVEL` | 否 | `debug`/`info`/`warn`/`error`，缺省 `warn`；日志只写 stderr |
| `KORE_SITE_CONF_DIR` | 否 | conf.d 目录，缺省 `/etc/nginx/conf.d`；测试与自定义布局用 |
| `KORE_NGINX_TEST_CMD` | 否 | 校验命令前缀，缺省 `nginx`（实际执行 `nginx -t`）；`off` 显式跳过校验 |
| `KORE_NGINX_RELOAD_CMD` | 否 | 重载命令前缀，缺省 `nginx`（实际执行 `nginx -s reload`）；`off` 显式跳过重载 |
| `KORE_NGINX_DUMP_CMD` | 否 | 转储命令前缀，缺省由校验前缀推导并补 `-T`；`off` 显式跳过加载检测 |

容器联调示例：

```bash
KORE_NGINX_TEST_CMD="docker exec kore-site-test nginx -t" \
KORE_NGINX_RELOAD_CMD="docker exec kore-site-test nginx -s reload" \
KORE_SITE_CONF_DIR=/tmp/conf.d \
./bin/site
```

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
反代命中容器内 `127.0.0.1:18990` 后端 → `/old` 302 → disable 后不再命中、enable 恢复 → index 指令）：

```bash
go test -tags e2e -timeout 300s -v ./tests/e2e
```

L2 反代后端是 `tests/e2e/testdata/backend`（`CGO_ENABLED=0` 静态二进制，运行期 `docker cp` 进容器），
因为 nginx:alpine 的 busybox 不含 httpd applet、容器内也没有 python。

本地联调依赖**不提交**的 `go.work`（`use .` + `use ../kore/pkg/pluginapi`）；跨仓 `replace` 禁止写入 `go.mod`（G-17）。

## 明确不做

- 不写宿主 `nginx.conf`、不注入 include、不抢 `default_server`。
- 不内置/捆绑任何 web server 二进制，不自动安装 nginx。
- 不做证书签发/续期、DNS、防火墙等站点外资源编排：SSL 只引用运维已提供的证书路径。
