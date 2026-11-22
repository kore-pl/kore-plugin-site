package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// isolatePluginEnv 把插件配置与命令环境全部隔离到临时目录/空值，
// 让 auto 探测只看到用例自己注入的假 nginx / 假 docker。
func isolatePluginEnv(t *testing.T) string {
	t.Helper()
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)
	for _, key := range []string{
		envNginxMode, envDockerContainer, envDockerConfDir, envHostConfDir,
		vhostDirEnv, nginxTestCmdEnv, nginxReloadCmdEnv, nginxDumpCmdEnv,
	} {
		t.Setenv(key, "")
	}
	return stateDir
}

// writeSettingsFile 直接落一份 config.json（模拟用户通过 config.set 保存后的状态）。
func writeSettingsFile(t *testing.T, stateDir string, settings pluginSettings) {
	t.Helper()
	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("序列化配置: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, pluginConfigName), payload, 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
}

// fakeBin 在临时目录放一个可执行假命令，并把 PATH 收窄到该目录。
func fakeBin(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("写假命令 %s: %v", name, err)
	}
	t.Setenv("PATH", dir)
	return path
}

// TestSettingsPriority 验证 config.json > 环境变量 > 代码默认值，且 source 如实上报。
func TestSettingsPriority(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	t.Setenv(envNginxMode, "host")

	settings, source, err := loadPluginSettings()
	if err != nil {
		t.Fatalf("默认解析失败: %v", err)
	}
	if source != "env" || settings.NginxMode != nginxModeHost {
		t.Fatalf("环境变量应生效: source=%s mode=%s", source, settings.NginxMode)
	}

	t.Setenv(vhostDirEnv, "/tmp/env-conf")
	settings, _, err = loadPluginSettings()
	if err != nil || settings.HostConfDir != "/tmp/env-conf" {
		t.Fatalf("KORE_SITE_CONF_DIR 历史别名应生效: %+v err=%v", settings, err)
	}

	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: "docker", DockerContainer: "kore-nginx",
		DockerConfDir: "/etc/nginx/conf.d", HostConfDir: "/tmp/file-conf",
	})
	settings, source, err = loadPluginSettings()
	if err != nil {
		t.Fatalf("文件解析失败: %v", err)
	}
	if source != "file" || settings.NginxMode != nginxModeDocker || settings.DockerContainer != "kore-nginx" ||
		settings.HostConfDir != "/tmp/file-conf" {
		t.Fatalf("config.json 应压过环境变量: %+v source=%s", settings, source)
	}

	if err := os.Remove(filepath.Join(stateDir, pluginConfigName)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envNginxMode, "")
	t.Setenv(vhostDirEnv, "")
	settings, source, err = loadPluginSettings()
	if err != nil || source != "default" || settings.NginxMode != nginxModeAuto {
		t.Fatalf("默认值应为 auto/default: %+v source=%s err=%v", settings, source, err)
	}
}

// TestAutoDetectionHostBranch 验证宿主有 nginx 命令时 auto 选 host。
func TestAutoDetectionHostBranch(t *testing.T) {
	isolatePluginEnv(t)
	fakeBin(t, "nginx", "exit 0")
	runner, perr := loadNginxRunner()
	if perr != nil {
		t.Fatalf("auto 探测失败: %v", perr.Message)
	}
	if runner.mode != nginxModeHost || runner.reason != "" {
		t.Fatalf("应命中 host 分支: mode=%s reason=%q", runner.mode, runner.reason)
	}
}

// TestAutoDetectionDockerBranch 验证宿主无 nginx 但 docker ps 有 nginx 容器时选 docker。
func TestAutoDetectionDockerBranch(t *testing.T) {
	isolatePluginEnv(t)
	fakeBin(t, "docker", `if [ "$1" = "ps" ]; then echo "kore-nginx nginx:alpine"; fi; exit 0`)
	runner, perr := loadNginxRunner()
	if perr != nil {
		t.Fatalf("auto 探测失败: %v", perr.Message)
	}
	if runner.mode != nginxModeDocker || runner.container != "kore-nginx" {
		t.Fatalf("应命中 docker 分支: mode=%s container=%q", runner.mode, runner.container)
	}
	want := []string{"docker", "exec", "kore-nginx", "nginx", "-t"}
	if got := runner.test.argv("-t"); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%q，期望 %q", got, want)
	}
}

// TestAutoDetectionOffBranch 验证宿主与容器都没有时明确 off + reason，而不是静默跳过。
func TestAutoDetectionOffBranch(t *testing.T) {
	isolatePluginEnv(t)
	confDir := t.TempDir()
	t.Setenv(vhostDirEnv, confDir)
	fakeBin(t, "docker", "exit 0")
	runner, perr := loadNginxRunner()
	if perr != nil {
		t.Fatalf("auto 探测失败: %v", perr.Message)
	}
	if runner.mode != nginxModeOff || runner.reason != autoOffReason {
		t.Fatalf("应命中 off 分支: mode=%s reason=%q", runner.mode, runner.reason)
	}
	if !runner.test.disabled || !runner.reload.disabled || !runner.dump.disabled {
		t.Fatal("off 分支三条命令都应是 disabled")
	}
	raw := callOK(t, "site.create", map[string]any{"domain": "auto-off.example.com"})
	var created siteCreateResult
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.NginxMode != nginxModeOff || created.NginxReason != autoOffReason || !created.TestSkipped {
		t.Fatalf("auto 落空应明确 off+reason: %+v", created)
	}
}

// TestDockerArgvFixed 验证 docker 模式 argv 是固定前缀 + 固定操作旗标，容器名独立成项。
func TestDockerArgvFixed(t *testing.T) {
	isolatePluginEnv(t)
	settings := defaultSettings()
	settings.NginxMode = nginxModeDocker
	settings.DockerContainer = "kore-nginx"
	runner, _, err := buildRunner(settings)
	if err != nil {
		t.Fatalf("构造 docker runner: %v", err)
	}
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"test", runner.test.argv("-t"), []string{"docker", "exec", "kore-nginx", "nginx", "-t"}},
		{"reload", runner.reload.argv("-s", "reload"), []string{"docker", "exec", "kore-nginx", "nginx", "-s", "reload"}},
		{"dump", runner.dump.argv("-T"), []string{"docker", "exec", "kore-nginx", "nginx", "-T"}},
	}
	for _, tc := range cases {
		if strings.Join(tc.got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Fatalf("%s argv=%q，期望 %q", tc.name, tc.got, tc.want)
		}
		for _, arg := range tc.got {
			if strings.ContainsAny(arg, " ;|&$`") {
				t.Fatalf("%s argv 含 shell 元字符: %q", tc.name, arg)
			}
		}
	}
}

// TestContainerNameRejectsInjection 验证容器名校验挡住路径穿越与命令注入。
func TestContainerNameRejectsInjection(t *testing.T) {
	isolatePluginEnv(t)
	bad := []string{
		"../../etc", "kore nginx", "kore;rm -rf /", "$(id)", "kore/nginx",
		"-rf", "kore\nnginx", strings.Repeat("a", 129),
	}
	for _, name := range bad {
		_, err := validateSettings(pluginSettings{
			NginxMode: nginxModeDocker, DockerContainer: name,
			DockerConfDir: "/etc/nginx/conf.d", HostConfDir: "/etc/nginx/conf.d",
		})
		if err == nil || !strings.Contains(err.Error(), "docker_container 非法") {
			t.Fatalf("容器名 %q 应被拒绝，实际 err=%v", name, err)
		}
	}
	for _, name := range []string{"kore-nginx", "a", "ctr_1.a-b", strings.Repeat("a", 128)} {
		if err := validateContainerName(name); err != nil {
			t.Fatalf("合法容器名 %q 被误拒: %v", name, err)
		}
	}
}

// TestDockerModeRequiresContainer 验证 nginx_mode=docker 缺容器名时配置即拒绝。
func TestDockerModeRequiresContainer(t *testing.T) {
	isolatePluginEnv(t)
	_, err := validateSettings(pluginSettings{
		NginxMode: nginxModeDocker, DockerConfDir: "/etc/nginx/conf.d", HostConfDir: "/etc/nginx/conf.d",
	})
	if err == nil || !strings.Contains(err.Error(), "必须同时配置 docker_container") {
		t.Fatalf("缺容器名应被拒绝: %v", err)
	}
}

// TestDockerModeMissingHostConfDir 验证宿主 conf.d 不存在时写操作回 E_INVALID 并给出挂载提示。
func TestDockerModeMissingHostConfDir(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	missing := filepath.Join(t.TempDir(), "no-such-conf.d")
	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: nginxModeDocker, DockerContainer: "kore-nginx",
		DockerConfDir: "/etc/nginx/conf.d", HostConfDir: missing,
	})
	callFail(t, "site.create", map[string]any{"domain": "ct.example.com"},
		pluginapi.E_INVALID, "不存在，请确认容器把 conf.d 挂载到了这个目录")
}

// TestDockerModeContainerMissingIsUpstream 验证容器不存在回 E_UPSTREAM + 明确文案。
func TestDockerModeContainerMissingIsUpstream(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	hostDir := t.TempDir()
	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: nginxModeDocker, DockerContainer: "kore-missing",
		DockerConfDir: "/etc/nginx/conf.d", HostConfDir: hostDir,
	})
	fakeBin(t, "docker", `echo "Error: No such object: kore-missing" >&2; exit 1`)
	callFail(t, "site.create", map[string]any{"domain": "ct.example.com"},
		pluginapi.E_UPSTREAM, "容器 kore-missing 不存在或 docker 不可用")
}

// TestDockerModeMountMismatch 验证容器 conf.d 来自别的宿主目录时拒绝写入。
func TestDockerModeMountMismatch(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	hostDir := t.TempDir()
	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: nginxModeDocker, DockerContainer: "kore-nginx",
		DockerConfDir: "/etc/nginx/conf.d", HostConfDir: hostDir,
	})
	fakeBin(t, "docker", `case "$1" in
  inspect) echo '[{"Type":"bind","Source":"/tmp/other-conf","Destination":"/etc/nginx/conf.d"}]'; exit 0;;
esac; exit 1`)
	callFail(t, "site.create", map[string]any{"domain": "ct.example.com"},
		pluginapi.E_INVALID, "与 host_conf_dir=")
}

// TestDockerModeMountMissing 验证容器没有把 conf.d 挂出来时拒绝写入。
func TestDockerModeMountMissing(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	hostDir := t.TempDir()
	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: nginxModeDocker, DockerContainer: "kore-nginx",
		DockerConfDir: "/etc/nginx/conf.d", HostConfDir: hostDir,
	})
	fakeBin(t, "docker", `case "$1" in
  inspect) echo '[{"Type":"volume","Source":"/var/lib/docker/volumes/x/_data","Destination":"/var/www"}]'; exit 0;;
esac; exit 1`)
	callFail(t, "site.create", map[string]any{"domain": "ct.example.com"},
		pluginapi.E_INVALID, "没有把 /etc/nginx/conf.d 挂载到宿主目录")
}

// TestOffModeReportsReason 验证 off 模式返回 nginx_mode=off + 明确原因，不再是无声 skipped。
func TestOffModeReportsReason(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	confDir := t.TempDir()
	t.Setenv(vhostDirEnv, confDir)
	writeSettingsFile(t, stateDir, pluginSettings{
		NginxMode: nginxModeOff, DockerConfDir: "/etc/nginx/conf.d", HostConfDir: confDir,
	})
	raw := callOK(t, "site.create", map[string]any{"domain": "off.example.com"})
	var created siteCreateResult
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	if !created.TestSkipped || created.NginxMode != nginxModeOff ||
		!strings.Contains(created.NginxReason, "nginx_mode=off") {
		t.Fatalf("off 模式应明确标记: %+v", created)
	}
	statusRaw := callOK(t, "site.status", map[string]any{"domain": "off.example.com"})
	var status siteStatusResult
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		t.Fatal(err)
	}
	if status.NginxMode != nginxModeOff || status.NginxReason == "" || !status.TestSkipped {
		t.Fatalf("status 也应带 off 与原因: %+v", status)
	}
}
