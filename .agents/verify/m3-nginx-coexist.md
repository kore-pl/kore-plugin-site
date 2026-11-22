[09:55:25] === 1. 环境探测 ===
[09:55:25]   宿主机 web server: none
## 环境
- 探测: nginx=MISSING caddy=MISSING httpd=MISSING
[09:55:25]   命中 container 模式（docker 可用，宿主机无 nginx）
- env_kind: container
- detected_server: nginx
- 脚本承诺: 不安装任何 web server、不造 default_server
[09:55:26] === 2b. container nginx 共存验证 ===
[09:55:26]   起容器 kore-coexist-3170867 （nginx:alpine）
[09:55:26]   容器初始 nginx -t
[09:55:26]   主配置前 sha256: 1106c372c80faa48be7d36e787d6745a722518b8f4c0f1306a70123cd3ba4805
[09:55:26]   conf.d 已存在文件数: 0
[09:55:26]   写入独立 vhost: /tmp/webserver-coexist-XOvTMC/nginx-conf/conf.d/kore-plugin-site.testdomain.local.conf
[09:55:26]   注入 include 锚定 + 行
[09:55:26]   运行: container_test
### nginx -t 输出
```
nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
nginx: configuration file /etc/nginx/nginx.conf test is successful
```
[09:55:26]   主配置后 sha256: 29fbe24f47ca79707f6ab9f68c62ddc6de00427d8d3890ca2f0161237fdbed0a（DIFF 为预期，等待回滚验证）
- 主配置 sha256: BEFORE=1106c372c80faa48be7d36e787d6745a722518b8f4c0f1306a70123cd3ba4805 AFTER=29fbe24f47ca79707f6ab9f68c62ddc6de00427d8d3890ca2f0161237fdbed0a（DIFF 为 fixture 注入 include 的预期行为）
[09:55:26]   回滚: 移除 include 锚定 + 行
[09:55:26]   回滚: 删除独立 vhost 文件
[09:55:26]   回滚后重跑: container_test
- 回滚后 nginx -t: OK (exit 0)
- 回滚后 sha256 → MATCH（与 before 一致）

## 结果
[09:55:26]   verdict: PARTIAL（container — 条件通过，不计全绿）
- verdict: **PARTIAL**（env_kind=container，条件通过；真实宿主共存挂账 P2 未证明）

## FAKE-PASS 反向验证
- 反向口径: env_kind≠real 且 verdict=PASS → FAKE-PASS（命中假标记）
- 反向输出: **FAKE-PASS**（= FAKE-PASS 表示反向验证有效）

========== webserver-coexist.sh 摘要 ==========
env_kind   = container
verdict    = PARTIAL
exit_code  = 0
artifact   = /tmp/webserver-coexist-XOvTMC/verification.md
run log    = /tmp/webserver-coexist-XOvTMC/run.log
反向验证    = FAKE-PASS（必须是 OK 才算通过）
================================================

## 环境
- 探测: nginx=MISSING caddy=MISSING httpd=MISSING
- env_kind: container
- detected_server: nginx
- 脚本承诺: 不安装任何 web server、不造 default_server
### nginx -t 输出
```
nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
nginx: configuration file /etc/nginx/nginx.conf test is successful
```
- 主配置 sha256: BEFORE=1106c372c80faa48be7d36e787d6745a722518b8f4c0f1306a70123cd3ba4805 AFTER=29fbe24f47ca79707f6ab9f68c62ddc6de00427d8d3890ca2f0161237fdbed0a（DIFF 为 fixture 注入 include 的预期行为）
- 回滚后 nginx -t: OK (exit 0)
- 回滚后 sha256 → MATCH（与 before 一致）

## 结果
- verdict: **PARTIAL**（env_kind=container，条件通过；真实宿主共存挂账 P2 未证明）

## FAKE-PASS 反向验证
- 反向口径: env_kind≠real 且 verdict=PASS → FAKE-PASS（命中假标记）
- 反向输出: **FAKE-PASS**（= FAKE-PASS 表示反向验证有效）
