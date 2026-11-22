#!/usr/bin/env bash
# webserver-coexist.sh — M3-A fixture
# 验证 site 插件的"独立 conf.d vhost + include、不改宿主主配置、不抢 default_server"
# 设计意图在不触碰宿主机任何 web server 二进制的前提下自洽。
#
# 三条架构红线（kore 内核与插件都不内置/不捆绑 web server 二进制，nginx/caddy/httpd
# 只是 site 插件可对接的宿主已安装实现之一——裁决37）：
#   ① 本脚本**不得安装** nginx/caddy/httpd（apt / yum / apk / curl 下载一律禁止）。
#   ② 本脚本**不得自造 default_server**（模拟或容器 fixture 也不塞 default 标记）。
#   ③ env_kind != real 时 verdict 只能是 PARTIAL，反向 FAKE-PASS 必须被判 FAIL。
#
# 探测到的环境决定 env_kind：
#   real       — 宿主机已有 nginx 且 nginx -t 能过
#   container  — 宿主机无 nginx 但 docker 可用，脚本起临时 nginx 容器
#   simulated  — 以上都没有，脚本用纯 shell 模拟 conf.d + include 行为，
#                不调用任何真实 web server，nginx -t 环节用占位 stub

set -euo pipefail

# ---------- 目录与日志 ----------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACT_DIR="$(mktemp -d -t webserver-coexist-XXXXXX)"
trap 'rm -rf "$ARTIFACT_DIR"' EXIT

LOG="$ARTIFACT_DIR/run.log"
: > "$LOG"
log() { printf '[%s] %s\n' "$(date '+%H:%M:%S')" "$*" | tee -a "$LOG"; }

# ---------- 验收产物 ----------
OUT_FILE="$ARTIFACT_DIR/verification.md"
: > "$OUT_FILE"
emit() { printf '%s\n' "$*" | tee -a "$OUT_FILE"; }

env_kind="simulated"         # 最终值：real | container | simulated
verdict="PARTIAL"            # PASS | PARTIAL | FAIL
exit_code=1

# ---------- 1. 环境探测 ----------
log "=== 1. 环境探测 ==="

probe_host_nginx() {
    local bin="${1:-nginx}"
    if command -v "$bin" >/dev/null 2>&1; then
        local major
        major="$("$bin" -v 2>&1 | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1)"
        if [ -n "$major" ]; then
            echo "FOUND|$bin|$major"
            return 0
        fi
    fi
    echo "MISSING|$bin|"
    return 1
}

declare -A SERVER_BIN=()
declare -A SERVER_DIR=()
# 每个候选：二进制 → 配置根（宿主机约定路径）
SERVER_BIN[nginx]="$(command -v nginx 2>/dev/null || true)"
SERVER_BIN[caddy]="$(command -v caddy 2>/dev/null || true)"
SERVER_BIN[httpd]="$(command -v httpd 2>/dev/null || command -v apache2 2>/dev/null || true)"
SERVER_DIR[nginx]="/etc/nginx"
SERVER_DIR[caddy]="/etc/caddy"
SERVER_DIR[httpd]="/etc/httpd"

detected_host=""
for name in nginx caddy httpd; do
    bin="${SERVER_BIN[$name]}"
    if [ -n "$bin" ] && [ -x "$bin" ]; then
        detected_host="$name"
        break
    fi
done

log "  宿主机 web server: ${detected_host:-none}"
emit "## 环境"
emit "- 探测: nginx=${SERVER_BIN[nginx]:-MISSING} caddy=${SERVER_BIN[caddy]:-MISSING} httpd=${SERVER_BIN[httpd]:-MISSING}"

detected_server=""
config_root=""
t_test=""
t_reload=""
t_version=""

# ---------- 2. 选择 env_kind 与测试目标 ----------
select_target() {
    # 优先级：real nginx > container nginx > simulated
    if [ -n "$detected_host" ]; then
        case "$detected_host" in
            nginx)
                local root="${SERVER_DIR[nginx]}"
                if [ -f "$root/nginx.conf" ]; then
                    env_kind="real"
                    detected_server="nginx"
                    config_root="$root"
                    t_test="nginx -t -c ${config_root}/nginx.conf"
                    t_reload="nginx -s reload -c ${config_root}/nginx.conf"
                    t_version="nginx -v"
                    log "  命中 real nginx at $root"
                    return 0
                fi
                ;;
        esac
    fi

    # docker 路径：拉起临时 nginx:alpine，映射空 conf.d 目录
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
        env_kind="container"
        detected_server="nginx"
        log "  命中 container 模式（docker 可用，宿主机无 nginx）"
        return 0
    fi

    env_kind="simulated"
    detected_server="nginx"
    log "  命中 simulated 模式（宿主机无 nginx，docker 不可用）"
    return 0
}
select_target
emit "- env_kind: ${env_kind}"
emit "- detected_server: ${detected_server}"
emit "- 脚本承诺: 不安装任何 web server、不造 default_server"

# ---------- 3. 宿主快照 + 共存验证 ----------
snapshot_hash() { sha256sum "$1" | awk '{print $1}'; }

backup_dir() {
    local src="$1" dst="$2"
    mkdir -p "$dst"
    cp -r "$src"/* "$dst"/ 2>/dev/null || true
}

# 三类模式共享的逻辑：给一个 conf.d 目录、一个主配置路径、一个 include 注入函数
run_coexist_scenario() {
    local _root="$1"
    local _main="$2"
    local _conf_d="$3"            # 目标 conf.d 目录
    local _include_marker="$4"    # 注入 include 的锚定注释（写入 main）
    local _test_cmd="$5"          # 验证配置可通过的命令
    local _reload_cmd="$6"        # 回滚前的重载（可选）

    local _tmp_vhost="$ARTIFACT_DIR/kore-plugin-site.testdomain.local.conf"
    local _include_line="# kore-plugin-site-include-anchor: conf.d/*.conf"
    local _include_real="include ${_conf_d}/*.conf;"

    mkdir -p "$_conf_d"

    # --- 快照主配置（前后比对的基准） ---
    local _before
    _before="$(snapshot_hash "$_main")"
    log "  主配置前 sha256: ${_before}"

    # --- 快照 conf.d 已存在文件（回滚时清理） ---
    local _pre_existing=()
    if [ -d "$_conf_d" ]; then
        while IFS= read -r -d '' f; do _pre_existing+=("$f"); done \
            < <(find "$_conf_d" -maxdepth 1 -type f -print0)
    fi
    log "  conf.d 已存在文件数: ${#_pre_existing[@]}"

    # --- 写独立 vhost 片段：只写 server 块，不含 default_server ---
    cat > "$_tmp_vhost" <<'VHOST_EOF'
# kore-plugin-site managed vhost for testdomain.local
# WARNING: do NOT add 'default_server' directive here — M3-A R6 禁止抢占默认站点
server {
    listen 80;
    server_name testdomain.local;
    root /var/www/testdomain;
    index index.html;
    location / {
        try_files $uri $uri/ =404;
    }
}
VHOST_EOF

    # 安全校验：写出的 vhost 里绝不能出现 default_server 指令。
    # 只在包含 server { 指令的行上搜，注释行天然不含 server 指令，不会误报。
    if grep -qE '^[^#]*default_server[^#]*server|^[^#]*server[^#]*default_server' "$_tmp_vhost"; then
        log "  FAIL: 生成的 vhost 含 default_server 指令（红线 S-01）"
        return 2
    fi

    # --- 把 vhost 放进 conf.d ---
    cp "$_tmp_vhost" "$_conf_d/kore-plugin-site.testdomain.local.conf"
    log "  写入独立 vhost: ${_conf_d}/kore-plugin-site.testdomain.local.conf"

    # --- 注入 include：先加锚定注释，再补 include 行（幂等） ---
    if ! grep -qF "$_include_marker" "$_main"; then
        python3 - "$_main" "$_include_marker" "$_include_real" <<'PYEOF'
import sys
path, anchor, include = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "a") as f:
    f.write(anchor + "\n")
    f.write(include + "\n")
PYEOF
        log "  注入 include 锚定 + 行"
    else
        log "  include 锚定已存在，保持幂等"
    fi

    # --- 验证配置可通过 ---
    log "  运行: $_test_cmd"
    local _test_out _test_rc=0
    _test_out="$($_test_cmd 2>&1)" || _test_rc=$?
    emit "### nginx -t 输出"
    emit '```'
    printf '%s\n' "$_test_out" | tee -a "$OUT_FILE"
    emit '```'

    if [ "$_test_rc" -ne 0 ]; then
        log "  FAIL: nginx -t 退出码 $_test_rc"
        return 3
    fi

    # --- 应用后快照：此时 DIFF 是预期（fixture 自己刚注入了 include 行），
    #     不作为 FAIL 判据；真正的验收点是"回滚后能否回到 BEFORE" ---
    local _after
    _after="$(snapshot_hash "$_main")"
    log "  主配置后 sha256: ${_after}（DIFF 为预期，等待回滚验证）"
    emit "- 主配置 sha256: BEFORE=${_before} AFTER=${_after}（DIFF 为 fixture 注入 include 的预期行为）"

    # --- 逆序回滚：用 Python 做文件改写（sed 对 . * / 等元字符的转义链太脆） ---
    # 删掉锚定行 + include 行，其他原样保留。
    python3 - "$_main" "$_include_marker" "$_include_real" <<'PYEOF'
import sys
path, anchor, include = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "r") as f:
    lines = f.readlines()
kept = [ln for ln in lines if ln.rstrip("\n") != anchor and ln.rstrip("\n") != include]
with open(path, "w") as f:
    f.writelines(kept)
PYEOF
    log "  回滚: 移除 include 锚定 + 行"
    rm -f "$_conf_d/kore-plugin-site.testdomain.local.conf"
    log "  回滚: 删除独立 vhost 文件"

    # --- 回滚后再验证（配置仍应合法） ---
    log "  回滚后重跑: $_test_cmd"
    local _roll_out _roll_rc=0
    _roll_out="$($_test_cmd 2>&1)" || _roll_rc=$?
    if [ "$_roll_rc" -ne 0 ]; then
        log "  FAIL: 回滚后配置非法 (exit $_roll_rc)"
        emit "- 回滚后 nginx -t 退出码: ${_roll_rc} → 配置已坏"
        return 5
    fi
    emit "- 回滚后 nginx -t: OK (exit 0)"

    # --- 回滚后的主配置也应与 before 一致 ---
    local _final_hash
    _final_hash="$(snapshot_hash "$_main")"
    if [ "$_final_hash" != "$_before" ]; then
        log "  回滚后主配置 sha256 与 before 不一致"
        emit "- 回滚后 sha256: ${_final_hash} → 与 before ${_before} DIFF"
        return 6
    fi
    emit "- 回滚后 sha256 → MATCH（与 before 一致）"

    return 0
}

# ---------- 4. 按 env_kind 执行 ----------
case "$env_kind" in
    real)
        log "=== 2a. real nginx 宿主共存验证 ==="
        if [ "$detected_server" = "nginx" ] && [ -d "$config_root" ]; then
            run_coexist_scenario \
                "$config_root" \
                "$config_root/nginx.conf" \
                "$config_root/conf.d" \
                "# kore-plugin-site-include-anchor: conf.d/*.conf" \
                "nginx -t -c ${config_root}/nginx.conf" \
                "nginx -s reload -c ${config_root}/nginx.conf"
            rc=$?
        else
            rc=10
        fi
        ;;

    container)
        log "=== 2b. container nginx 共存验证 ==="
        CONTAINER_NAME="kore-coexist-$$"
        CONF_HOST="$ARTIFACT_DIR/nginx-conf"
        CONF_HOST_MAIN="$CONF_HOST/nginx.conf"
        CONF_HOST_D="$CONF_HOST/conf.d"
        mkdir -p "$CONF_HOST_D"

        # 构造最小 nginx.conf：默认自带一个 conf.d 目录引用（宿主常见布局）
        # 故意**不造 default_server**——fixture 本身也遵守红线 S-01
        cat > "$CONF_HOST_MAIN" <<'MAIN_EOF'
worker_processes 1;
pid /tmp/nginx.pid;
events { worker_connections 1024; }
http {
    server_tokens off;
    types { text/html html; }
    default_type application/octet-stream;
    sendfile on;
    # 模拟宿主的 conf.d 布局：预置一个 vhost 片段引用（不含 default_server）
    include /etc/nginx/conf.d/*.conf;
}
MAIN_EOF

        # 起容器
        log "  起容器 $CONTAINER_NAME （nginx:alpine）"
        docker run --rm --name "$CONTAINER_NAME" \
            -v "$CONF_HOST_MAIN:/etc/nginx/nginx.conf:ro" \
            -v "$CONF_HOST_D:/etc/nginx/conf.d" \
            -d nginx:alpine >/dev/null 2>&1 || { log "  FAIL: docker run"; exit 11; }

        # 容器内 nginx -t 的包装：先把命令送进容器、拿退出码
        container_test() {
            docker exec "$CONTAINER_NAME" nginx -t 2>&1
        }
        # 容器内 nginx 不会 reload 已加载配置文件（reload 只重读 conf），
        # 回滚验证直接用 nginx -t 就够了，stop 时容器销毁干净无残留
        container_stop() {
            docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
            docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
        }
        trap container_stop EXIT

        # 第一次 nginx -t 必须过 — 证明我们造的容器宿主是健康的
        log "  容器初始 nginx -t"
        if ! container_test >/dev/null 2>&1; then
            log "  FAIL: 容器初始 nginx -t 未过"
            container_stop; rc=12
        else
            run_coexist_scenario \
                "$CONF_HOST" \
                "$CONF_HOST_MAIN" \
                "$CONF_HOST_D" \
                "# kore-plugin-site-include-anchor: conf.d/*.conf" \
                "container_test" \
                "true"
            rc=$?
        fi
        ;;

    simulated)
        log "=== 2c. simulated 共存验证（纯 shell，无真实 web server） ==="
        SIM_ROOT="$ARTIFACT_DIR/sim-nginx"
        SIM_MAIN="$SIM_ROOT/nginx.conf"
        SIM_D="$SIM_ROOT/conf.d"
        mkdir -p "$SIM_D"

        # 与 container 相同的最小主配置，不带 default_server
        cat > "$SIM_MAIN" <<'MAIN_EOF'
worker_processes 1;
pid /tmp/nginx.pid;
events { worker_connections 1024; }
http {
    server_tokens off;
    types { text/html html; }
    default_type application/octet-stream;
    sendfile on;
    include conf.d/*.conf;
}
MAIN_EOF

        # simulated nginx -t stub：只做基础语法检查，不造 default_server
        sim_test() {
            local _cfg="$SIM_MAIN"
            # 拒绝 default_server 指令（红线 S-01 延伸：fixture 也不造 default）
            if grep -qE '^[^#]*default_server[^#]*server|^[^#]*server[^#]*default_server' "$_cfg"; then
                echo "[emerg] simulated nginx -t: default_server found (fixture self-check)"
                return 1
            fi
            # include 展开后必须有 conf.d/*.conf 行（或我们的锚定）
            if grep -qF 'include conf.d/*.conf' "$_cfg" || grep -qF 'kore-plugin-site-include-anchor' "$_cfg"; then
                echo "simulated nginx: configuration file $_cfg test is successful (stub)"
                return 0
            fi
            echo "[emerg] simulated nginx -t: no include anchor found"
            return 1
        }

        run_coexist_scenario \
            "$SIM_ROOT" \
            "$SIM_MAIN" \
            "$SIM_D" \
            "# kore-plugin-site-include-anchor: conf.d/*.conf" \
            "sim_test" \
            "true"
        rc=$?
        ;;
esac

# ---------- 5. 判定 verdict ----------
emit ""
emit "## 结果"

if [ "$env_kind" = "real" ]; then
    if [ "$rc" -eq 0 ]; then
        verdict="PASS"
        log "  verdict: PASS (real 宿主)"
        emit "- verdict: **PASS**（real 宿主共存通过，sha256 MATCH）"
    elif [ "$rc" -eq 4 ]; then
        verdict="FAIL"
        log "  verdict: FAIL (sha256 DIFF — 主配置被改)"
        emit "- verdict: **FAIL**（sha256 DIFF — site 插件改了主配置）"
    else
        verdict="FAIL"
        log "  verdict: FAIL (rc=$rc)"
        emit "- verdict: **FAIL**（rc=$rc）"
    fi
else
    # container / simulated — 只能 PARTIAL，不计入四轨全绿
    if [ "$rc" -eq 0 ]; then
        verdict="PARTIAL"
        log "  verdict: PARTIAL（${env_kind} — 条件通过，不计全绿）"
        emit "- verdict: **PARTIAL**（env_kind=${env_kind}，条件通过；真实宿主共存挂账 P2 未证明）"
    elif [ "$rc" -eq 4 ]; then
        verdict="FAIL"
        log "  verdict: FAIL (sha256 DIFF)"
        emit "- verdict: **FAIL**（sha256 DIFF）"
    else
        verdict="FAIL"
        log "  verdict: FAIL (rc=$rc)"
        emit "- verdict: **FAIL**（rc=$rc）"
    fi
fi

# ---------- 6. FAKE-PASS 反向验证 ----------
# 故意注入"模拟却标 PASS"的假结果文件，确认反向 grep 命令能抓到并判 FAIL。
# 判定口径：env_kind≠real 的场景，若出现 verdict=PASS 则反向输出 FAKE-PASS（命中假标记）。
emit ""
emit "## FAKE-PASS 反向验证"

FAKE_ARTIFACT="$ARTIFACT_DIR/fake-pass.md"
cat > "$FAKE_ARTIFACT" <<'FAKE_EOF'
# fake 证据 —— 模拟却标 PASS（故意违反 R1）
env_kind: simulated
verdict: PASS
原因：本机没 nginx 但我说是 PASS
FAKE_EOF

reverse_ok="OK"
if grep -qE 'env_kind: (simulated|container)' "$FAKE_ARTIFACT" && \
   grep -qE 'verdict: PASS' "$FAKE_ARTIFACT"; then
    reverse_ok="FAKE-PASS"
fi
emit "- 反向口径: env_kind≠real 且 verdict=PASS → FAKE-PASS（命中假标记）"
emit "- 反向输出: **${reverse_ok}**（= FAKE-PASS 表示反向验证有效）"

# 反过来说：只要 env_kind != real 就绝不能标 PASS —— 这是 R1 的硬约束。
if [ "$env_kind" != "real" ] && [ "$verdict" = "PASS" ]; then
    emit "- **反例命中**: env_kind=${env_kind} 却标了 PASS → 强制回退为 FAIL"
    verdict="FAIL"
fi

# ---------- 7. 退出码 ----------
if [ "$verdict" = "PASS" ]; then
    exit_code=0
elif [ "$verdict" = "PARTIAL" ]; then
    # PARTIAL 也算"脚本跑通但不全绿"——退出码 0 让上层能读到证据，但证据里自己会标 PARTIAL
    exit_code=0
else
    exit_code=1
fi

# ---------- 8. 打印摘要 ----------
echo
echo "========== webserver-coexist.sh 摘要 =========="
echo "env_kind   = ${env_kind}"
echo "verdict    = ${verdict}"
echo "exit_code  = ${exit_code}"
echo "artifact   = ${OUT_FILE}"
echo "run log    = ${LOG}"
echo "反向验证    = ${reverse_ok}（必须是 OK 才算通过）"
echo "================================================"

# 同时把完整 verification.md 的路径打印到 stdout 最后
echo
cat "$OUT_FILE"

exit "$exit_code"
