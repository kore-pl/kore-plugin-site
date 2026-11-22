# kore-plugin-site

kore 的 **M2 站点管理插件**（nginx 站点生命周期）。`info.json.depth_level = "production"`。

## 定位与边界

- 协议：`kore.plugin.v1`，`lifecycle = resident`，唯一第三方依赖 `github.com/pengtang1024/kore/pkg/pluginapi v0.1.1`（其余为标准库）。
- **当前批次（0.1.0）只做参数校验与结构化返回，不实际改动 nginx。** 真实配置落盘留待 M2-A 第二批。
- 不碰宿主 nginx 主配置（`nginx.conf`）；未来落盘时只写独立 `conf.d/<domain>.conf` vhost 并通过 `include` 引入（提案 §2 M2-A）。
- 不使用 `default_server`；`default_server=true` 的创建请求直接回 `E_INVALID`（红队 R6）。

## 方法表

| 方法（裸名 / 全名） | 行为 |
| --- | --- |
| `site.list` / `list` | 返回 `{"sites":[],"count":0,"applied":false}`，当前无真实站点来源 |
| `site.create` / `create` | 校验 `domain` → 返回目标 vhost 路径与 `applied:false` 的提案 |
| `site.delete` / `delete` | 校验 `domain` → 返回目标 vhost 路径与 `applied:false` 的提案 |
| `kore.ping` / `kore.reload` / `kore.shutdown` | 协议必选方法；`kore.reload` 无外部配置，`config_digest` 为空 |

`domain` 约束：小写、每个标签以字母数字开头结尾、至少两个标签、总长 ≤ 253；拒绝路径分隔符与 `..`，防止 vhost 路径穿越。

响应帧一律回显请求的 `id` 与 `req_id`；未知方法回 `E_NOTFOUND`；参数非法回 `E_INVALID`。

## 环境变量

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `KORE_PLUGIN_SOCK` | 是 | 主壳创建的 UDS 路径 |
| `KORE_PLUGIN_ID` | 否 | 内核登记 id，缺省取内嵌 `info.json.id` |
| `KORE_PLUGIN_TOKEN` | 否 | 内核注入令牌；当前只随 `kore.hello` 上报 |
| `KORE_LOG_LEVEL` | 否 | `debug`/`info`/`warn`/`error`，缺省 `warn`；日志只写 stderr |

`stdout` 只留给协议帧，任何日志都不得写 stdout。

## 构建与打包

```bash
make build   # CGO_ENABLED=0 GOAMD64=v1 go build -trimpath -buildvcs=false -ldflags="-s -w" -o bin/site .
make dist    # 产出 dist/site-0.1.0.tar.gz（info.json + bin/site + SHA256SUMS）与同名 .sha256
make check   # gofmt → go vet → go test -timeout 200s
```

`dist` 会按包内实际字节数回填 `info.json.size_bytes`（仓库里的清单保持 `0`，由打包脚本写入真实值）。

本地联调依赖**不提交**的 `go.work`（`use .` + `use ../kore/pkg/pluginapi`）；跨仓 `replace` 禁止写入 `go.mod`（G-17）。

## 未完成项（M2-A 第二批）

- `site.create` / `site.delete` 的 nginx conf.d vhost 实际写入与删除。
- 独立 vhost 的 `include` 接线与 `nginx -t` 校验、reload 流程。
- 站点列表的真实来源（当前返回空列表）。
