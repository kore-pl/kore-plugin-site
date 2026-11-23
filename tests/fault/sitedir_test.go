// Package fault 的 sitedir 用例：容器模式下无法判定站点目录时，
// site.status 必须给三态 unknown（带可操作原因），而不是误报「目录不存在」。
//
// 观测语义（§3.5 #6 / G-16）：site_dir_state=unknown + site_dir_exists=false +
// site_dir_reason 非空；插件不 panic、不静默降级成 absent，仍能正常应答 shutdown。
// sanity 反证由同一包内的 sanityHealthy 提供（正常 create 链路仍可跑）。
package fault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const (
	siteDirDomain    = "sitedir.example.com"
	siteDirContainer = "kore-site-probe"
)

// statusPayload 是 site.status 结果中本用例断言的三态字段。
type statusPayload struct {
	NginxMode     string `json:"nginx_mode"`
	SiteDirExists bool   `json:"site_dir_exists"`
	SiteDirState  string `json:"site_dir_state"`
	SiteDirReason string `json:"site_dir_reason"`
}

// writeDockerModeConfig 在临时 state 目录写 docker 模式配置，并在 conf.d 落 vhost。
// root 传空串表示 vhost 不含 root 指令（覆盖「无法解析 root」分支）。
func writeDockerModeConfig(t *testing.T, confDir, root string) string {
	t.Helper()
	stateDir := t.TempDir()
	settings := `{"nginx_mode":"docker","docker_container":"` + siteDirContainer +
		`","docker_conf_dir":"/etc/nginx/conf.d","host_conf_dir":` + strconv.Quote(confDir) + `}`
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("写 config.json 失败: %v", err)
	}
	body := "server {\n    listen 80;\n    server_name " + siteDirDomain + ";\n"
	if root != "" {
		body += "    root " + root + ";\n"
	}
	body += "}\n"
	if err := os.WriteFile(filepath.Join(confDir, "kore-site-"+siteDirDomain+".conf"), []byte(body), 0o600); err != nil {
		t.Fatalf("写 vhost 失败: %v", err)
	}
	return stateDir
}

// fakeDockerDir 在临时目录放一个假 docker 可执行文件，返回只含它的 PATH。
func fakeDockerDir(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("写假 docker 失败: %v", err)
	}
	return dir
}

// assertSiteDirUnknown 断言三态契约：unknown + exists=false + reason 非空且点名容器。
func assertSiteDirUnknown(t *testing.T, response *pluginapi.Response, hint string) {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("site.status 不应报错: %s/%s", response.Error.Code, response.Error.Message)
	}
	var payload statusPayload
	if err := json.Unmarshal(response.Result, &payload); err != nil {
		t.Fatalf("解析 status 失败: %v (%s)", err, response.Result)
	}
	if payload.NginxMode != "docker" {
		t.Fatalf("nginx_mode=%q，期望 docker", payload.NginxMode)
	}
	if payload.SiteDirState != "unknown" {
		t.Fatalf("site_dir_state=%q，期望 unknown（reason=%q）", payload.SiteDirState, payload.SiteDirReason)
	}
	if payload.SiteDirExists {
		t.Fatalf("unknown 时 site_dir_exists 必须为 false（true 会让前端误以为目录存在）")
	}
	if payload.SiteDirReason == "" {
		t.Fatalf("unknown 必须带 site_dir_reason（G-16 不静默降级）")
	}
	if hint != "" && !strings.Contains(payload.SiteDirReason, hint) {
		t.Fatalf("reason 应含 %q，实际 %q", hint, payload.SiteDirReason)
	}
}

// TestFaultSiteDirDockerUnavailable 容器模式 + 环境里没有 docker：
// 目录状态必须是 unknown，而不是「不存在」。
func TestFaultSiteDirDockerUnavailable(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	confDir := healthyConfDir(t)
	stateDir := writeDockerModeConfig(t, confDir, "/usr/share/nginx/html")
	cmd, accept := startPluginWithEnv(t, binary, confDir, 0, map[string]string{
		"KORE_STATE_DIR": stateDir,
		"PATH":           t.TempDir(), // 收窄 PATH：docker 找不到
	})
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	response := callMethod(t, conn, "site.status",
		json.RawMessage(`{"domain":"`+siteDirDomain+`"}`), "r-sitedir-missing")
	assertSiteDirUnknown(t, response, "docker")

	// 降级路径不影响协议循环：进程仍能正常应答 shutdown。
	reply := callMethod(t, conn, "kore.shutdown", nil, "r-sitedir-shutdown")
	if reply.Error != nil {
		t.Fatalf("docker 缺失后插件不应失活: %s", reply.Error.Message)
	}
	_ = cmd.Process.Kill()
}

// TestFaultSiteDirDockerDaemonError docker 可执行但容器异常（daemon 级错误）：
// 退出码同为 1，但必须按 unknown 处理（带 stderr 的不等于「目录不存在」）。
func TestFaultSiteDirDockerDaemonError(t *testing.T) {
	binary := findBinary(t)
	confDir := healthyConfDir(t)
	stateDir := writeDockerModeConfig(t, confDir, "/usr/share/nginx/html")
	fakeDir := fakeDockerDir(t, `echo "Error response from daemon: Container `+siteDirContainer+` is not running" >&2; exit 1`)

	cmd, accept := startPluginWithEnv(t, binary, confDir, 0, map[string]string{
		"KORE_STATE_DIR": stateDir,
		"PATH":           fakeDir,
	})
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	response := callMethod(t, conn, "site.status",
		json.RawMessage(`{"domain":"`+siteDirDomain+`"}`), "r-sitedir-daemon")
	assertSiteDirUnknown(t, response, "退出码")
	shutdownWithReply(t, conn)
	_ = cmd.Process.Kill()
}

// TestFaultSiteDirNoRootDirective vhost 没有 root 指令：没有判定依据，
// 目录状态是 unknown 且原因点名 root，不允许沉默地回 false。
func TestFaultSiteDirNoRootDirective(t *testing.T) {
	binary := findBinary(t)
	confDir := healthyConfDir(t)
	stateDir := writeDockerModeConfig(t, confDir, "")
	cmd, accept := startPluginWithEnv(t, binary, confDir, 0, map[string]string{
		"KORE_STATE_DIR": stateDir,
		"PATH":           t.TempDir(),
	})
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	response := callMethod(t, conn, "site.status",
		json.RawMessage(`{"domain":"`+siteDirDomain+`"}`), "r-sitedir-noroot")
	assertSiteDirUnknown(t, response, "root")
	shutdownWithReply(t, conn)
	_ = cmd.Process.Kill()
}
