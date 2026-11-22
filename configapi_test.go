package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// getConfig 调 site.config.get 并解析。
func getConfig(t *testing.T) configGetResult {
	t.Helper()
	raw := callOK(t, "site.config.get", map[string]any{})
	var got configGetResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析 config.get: %v（%s）", err, raw)
	}
	return got
}

// setConfig 调 site.config.set（成功）。
func setConfig(t *testing.T, values map[string]any) configSetResult {
	t.Helper()
	raw := callOK(t, "site.config.set", map[string]any{"values": values})
	var result configSetResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 config.set: %v（%s）", err, raw)
	}
	return result
}

// TestConfigGetDefaultsAndSchema 验证 schema 四字段齐全、默认值正确、source=default。
func TestConfigGetDefaultsAndSchema(t *testing.T) {
	isolatePluginEnv(t)
	got := getConfig(t)
	if got.Source != "default" {
		t.Fatalf("未保存时应 source=default: %s", got.Source)
	}
	if got.Schema.Title == "" || len(got.Schema.Fields) != 4 {
		t.Fatalf("schema 应有 4 个字段: %+v", got.Schema)
	}
	seen := map[string]bool{}
	for _, field := range got.Schema.Fields {
		if field.Key == "" || field.Label == "" || field.Help == "" || len(field.Default) == 0 {
			t.Fatalf("字段缺 key/label/help/default: %+v", field)
		}
		seen[field.Key] = true
	}
	for _, key := range []string{cfgNginxMode, cfgDockerContainer, cfgDockerConfDir, cfgHostConfDir} {
		if !seen[key] {
			t.Fatalf("schema 缺字段 %s", key)
		}
	}
	var values pluginSettings
	if err := json.Unmarshal(got.Values, &values); err != nil {
		t.Fatalf("values 解析失败: %v", err)
	}
	if values.NginxMode != nginxModeAuto || values.DockerConfDir != defaultDockerConfDir ||
		values.HostConfDir != defaultHostConfDir || values.DockerContainer != "" {
		t.Fatalf("默认值不对: %+v", values)
	}
}

// TestConfigSetRoundTrip 验证「校验 → 原子写 → 热生效」：source 变 file、权限 0600、
// 非法值被拒且旧配置不被覆盖、显式空串可清空容器名。
func TestConfigSetRoundTrip(t *testing.T) {
	stateDir := isolatePluginEnv(t)
	result := setConfig(t, map[string]any{
		"nginx_mode": "docker", "docker_container": "kore-nginx",
		"docker_conf_dir": "/etc/nginx/conf.d", "host_conf_dir": "/tmp/kore-site-conf",
	})
	if !result.Saved || !result.Applied || result.RestartRequired {
		t.Fatalf("config.set 应 saved/applied 且无需重启: %+v", result)
	}
	path := filepath.Join(stateDir, pluginConfigName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config.json 未落盘: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json 权限=%o，期望 0600", st.Mode().Perm())
	}
	got := getConfig(t)
	if got.Source != "file" {
		t.Fatalf("保存后 source 应为 file: %s", got.Source)
	}
	var values pluginSettings
	if err := json.Unmarshal(got.Values, &values); err != nil {
		t.Fatal(err)
	}
	if values.NginxMode != nginxModeDocker || values.DockerContainer != "kore-nginx" ||
		values.HostConfDir != "/tmp/kore-site-conf" {
		t.Fatalf("保存值未生效: %+v", values)
	}

	// 非法枚举：回 E_INVALID 且旧配置保持不变。
	callFail(t, "site.config.set", map[string]any{"values": map[string]any{"nginx_mode": "sidecar"}},
		pluginapi.E_INVALID, "nginx_mode 只能是")
	after := getConfig(t)
	var unchanged pluginSettings
	_ = json.Unmarshal(after.Values, &unchanged)
	if unchanged.NginxMode != nginxModeDocker || unchanged.DockerContainer != "kore-nginx" {
		t.Fatalf("非法提交不应覆盖旧配置: %+v", unchanged)
	}

	// 容器名从 "***" 之外的空串显式清空：先切回 host 才允许为空。
	setConfig(t, map[string]any{"nginx_mode": "host"})
	setConfig(t, map[string]any{"docker_container": ""})
	final := getConfig(t)
	var cleared pluginSettings
	_ = json.Unmarshal(final.Values, &cleared)
	if cleared.DockerContainer != "" || cleared.NginxMode != nginxModeHost {
		t.Fatalf("显式空串应能清空容器名: %+v", cleared)
	}
}

// TestConfigSetRejectsContainerInjection 验证注入串在 config.set 就被拒（E_INVALID）。
func TestConfigSetRejectsContainerInjection(t *testing.T) {
	isolatePluginEnv(t)
	for _, bad := range []string{"../../etc", "kore;rm -rf /", "$(id)", "a b"} {
		callFail(t, "site.config.set", map[string]any{"values": map[string]any{
			"nginx_mode": "docker", "docker_container": bad,
		}}, pluginapi.E_INVALID, "docker_container 非法")
	}
	if got := getConfig(t); got.Source != "default" {
		t.Fatalf("被拒的提交不应落盘: source=%s", got.Source)
	}
}

// TestConfigSetRejectsRelativeDirs 验证 conf.d 路径必须绝对且规范化。
func TestConfigSetRejectsRelativeDirs(t *testing.T) {
	isolatePluginEnv(t)
	callFail(t, "site.config.set", map[string]any{"values": map[string]any{"host_conf_dir": "conf.d"}},
		pluginapi.E_INVALID, "必须是绝对路径")
	callFail(t, "site.config.set", map[string]any{"values": map[string]any{"host_conf_dir": "/etc/../etc/nginx"}},
		pluginapi.E_INVALID, "冗余路径分量")
}

// TestConfigSourceEnv 验证环境变量配置的 source 为 env。
func TestConfigSourceEnv(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envNginxMode, "docker")
	t.Setenv(envDockerContainer, "kore-nginx")
	got := getConfig(t)
	if got.Source != "env" {
		t.Fatalf("环境变量配置应 source=env: %s", got.Source)
	}
	var values pluginSettings
	_ = json.Unmarshal(got.Values, &values)
	if values.NginxMode != nginxModeDocker || values.DockerContainer != "kore-nginx" {
		t.Fatalf("环境变量值未生效: %+v", values)
	}
}

// TestVhostDirFollowsConfigFile 验证保存 host_conf_dir 后下一次建站就写进新目录（热生效）。
func TestVhostDirFollowsConfigFile(t *testing.T) {
	isolatePluginEnv(t)
	confDir := t.TempDir()
	setConfig(t, map[string]any{"nginx_mode": "off", "host_conf_dir": confDir})
	raw := callOK(t, "site.create", map[string]any{"domain": "hot.example.com"})
	var created siteCreateResult
	_ = json.Unmarshal(raw, &created)
	want := filepath.Join(confDir, vhostFileName("hot.example.com"))
	if created.VHostPath != want {
		t.Fatalf("vhost 路径=%s，期望 %s", created.VHostPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("vhost 未写进新目录: %v", err)
	}
	if !strings.HasSuffix(created.NginxMode, nginxModeOff) {
		t.Fatalf("off 模式应上报: %+v", created)
	}
}
