# Nginx 站点管理插件（site）

## 它做什么

kore 的 M2 站点管理插件，管理 nginx 站点生命周期：`site.create` 原子写独立 `conf.d/kore-site-<domain>.conf` → `nginx -t` 校验（失败回滚回 `E_INVALID`）→ `nginx -s reload`（失败回 `E_UPSTREAM`，明确"已写入但未生效"）。
不碰宿主 nginx 主配置、不用 `default_server`、不内置 nginx/caddy/httpd 二进制；host 模式下校验与 reload 经内核提权通道执行，插件自身不是 root。

## 协议与兼容性

协议 `kore.plugin.v1`，`lifecycle=resident`，`depth_level=production`，`requires_kore >=0.1.1 <0.2.0`；唯一第三方依赖 `github.com/pengtang1024/kore/pkg/pluginapi v0.1.1`。

## 主要能力

- 建站 / 删除 / 停用 / 启用 / 状态 / 列表 / 详情，写操作均 test + reload 且失败回滚。
- L2：SSL 绑定与解绑（TLS1.2+，默认附 80→443 跳转）、伪静态模板（wordpress / laravel / thinkphp / discuz / nextjs）、反向代理与 WebSocket、精确路径重定向、index 指令。
- PHP-FPM：`setPHP` / `unsetPHP` / `getPHP` 探测并配置站点 PHP；伪静态模板仍启用 PHP 时拒绝解绑，避免整站 404。
- `clone` 克隆站点配置；`setPassword` / `clearPassword` 配 Basic 认证（密码不回显）；`traffic` 只读解析 access_log 统计流量，读不到日志返回 `available=false` 不编数据。
- `config.get` / `config.set` 读写 5 个运行配置项（原子写 0600 状态文件，立即生效）。
- 三态口径：`test_skipped` / `reload_skipped` 表示没有可用 nginx，此时 `test_passed=false` 不代表站点故障。

## 安装与验证

`make check` 为本地门禁，`make dist` 产出位级可复现的 `dist/site-0.8.2.tar.gz`；root actions 与权限模型见 README「权限模型」。
漏洞按 [SECURITY.md](SECURITY.md) 私下披露；MIT 许可；贡献见 [CONTRIBUTING.md](CONTRIBUTING.md)。