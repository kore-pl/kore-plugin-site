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
| `site.status` / `status` | 返回 `conf_exists` / `test_passed` / `site_dir_exists` / `loaded`（`nginx -T` 精确匹配 `server_name`）/ `managed`。只读，不 reload。 |
| `site.list` / `list` | 每条带 `exists` / `test_ok` / `reloaded` 真实状态；`nginx -t` 与 `nginx -T` 各只跑一次。 |
| `kore.ping` / `kore.reload` / `kore.shutdown` | 协议必选方法；`kore.reload` 无外部配置，`config_digest` 为空。 |

`domain` 约束：小写、每个标签以字母数字开头结尾、至少两个标签、总长 ≤ 253；拒绝路径分隔符与 `..`，防止 vhost 路径穿越。
`root` 约束：绝对路径、无冗余分量、仅允许 `A-Za-z0-9._/@+-`，拒绝空白/分号/花括号/换行等配置注入字符。

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

docker 端到端（真实 nginx:alpine，四步生命周期）：

```bash
go test -tags e2e -timeout 240s -v ./tests/e2e
```

本地联调依赖**不提交**的 `go.work`（`use .` + `use ../kore/pkg/pluginapi`）；跨仓 `replace` 禁止写入 `go.mod`（G-17）。

## 明确不做

- 不写宿主 `nginx.conf`、不注入 include、不抢 `default_server`。
- 不内置/捆绑任何 web server 二进制，不自动安装 nginx。
- 不做证书、DNS、防火墙等站点外资源编排（后续能力域单独提案）。
