package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestVhost 在 conf.d 落一份最小 vhost（status 只需要 root 指令）。
func writeTestVhost(t *testing.T, confDir, domain, root string) {
	t.Helper()
	content := "server {\n    listen 80;\n    server_name " + domain + ";\n    root " + root + ";\n}\n"
	if err := os.WriteFile(filepath.Join(confDir, vhostFileName(domain)), []byte(content), 0o600); err != nil {
		t.Fatalf("写 vhost 失败: %v", err)
	}
}

// TestProbeSiteDirHost 覆盖宿主判定的三态：真的存在 / 确定不存在 / 查不到。
func TestProbeSiteDirHost(t *testing.T) {
	runner := &nginxRunner{mode: nginxModeHost}
	dir := t.TempDir()
	root := filepath.Join(dir, "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		root      string
		wantState string
	}{
		{"存在的目录", root, siteDirPresent},
		{"不存在的目录", filepath.Join(dir, "missing"), siteDirAbsent},
		{"存在但不是目录", filepath.Join(dir, "file"), siteDirAbsent},
		{"空 root 无法判定", "", siteDirUnknown},
		{"相对 root 无法判定", "relative/www", siteDirUnknown},
		// 父路径是普通文件 → ENOTDIR：这是「读不到」而不是「不存在」。
		{"ENOTDIR 无法判定", filepath.Join(dir, "file", "sub"), siteDirUnknown},
	}
	filePath := filepath.Join(dir, "file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := probeSiteDir(runner, tc.root)
			if state != tc.wantState {
				t.Fatalf("state=%q，期望 %q（reason=%q）", state, tc.wantState, reason)
			}
			if state == siteDirUnknown && reason == "" {
				t.Fatalf("unknown 必须带可操作 reason（G-16）")
			}
			if state != siteDirUnknown && reason != "" {
				t.Fatalf("%s 不应带 reason，实际 %q", state, reason)
			}
		})
	}
}

// TestDockerSiteDirState 用假 docker 覆盖容器模式判定：退出码 + 输出共同决定三态。
// 真 docker 不可用时（CI / 开发机）这些用例仍然可跑，不 skip。
func TestDockerSiteDirState(t *testing.T) {
	cases := []struct {
		name      string
		script    string
		wantState string
		wantHint  string
	}{
		{"容器内存在", "exit 0", siteDirPresent, ""},
		{"容器内不存在（test -d 无输出退出 1）", "exit 1", siteDirAbsent, ""},
		{"docker 守护进程报错也算未知", `echo "Error response from daemon: No such container: kore-nginx" >&2; exit 1`, siteDirUnknown, "退出码 1"},
		{"docker 参数错误退出 125", "exit 125", siteDirUnknown, "退出码 125"},
		{"容器内命令执行失败退出 126", `echo "OCI runtime exec failed" >&2; exit 126`, siteDirUnknown, "退出码 126"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin(t, "docker", tc.script)
			state, reason := dockerSiteDirState("kore-nginx", "/usr/share/nginx/html")
			if state != tc.wantState {
				t.Fatalf("state=%q，期望 %q（reason=%q）", state, tc.wantState, reason)
			}
			if state == siteDirUnknown {
				if reason == "" || !strings.Contains(reason, "kore-nginx") {
					t.Fatalf("unknown 的 reason 必须点名容器并给操作建议，实际 %q", reason)
				}
				if tc.wantHint != "" && !strings.Contains(reason, tc.wantHint) {
					t.Fatalf("reason 应含 %q，实际 %q", tc.wantHint, reason)
				}
			}
		})
	}
}

// TestDockerSiteDirStateDegraded 覆盖「容器相关命令找不到」与「命令超时」两条降级路径：
// 必须给可操作错误，不静默当成 absent（G-16）。
func TestDockerSiteDirStateDegraded(t *testing.T) {
	t.Run("docker 不在 PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		state, reason := dockerSiteDirState("kore-nginx", "/usr/share/nginx/html")
		if state != siteDirUnknown {
			t.Fatalf("docker 缺失应为 unknown，实际 %q", state)
		}
		if !strings.Contains(reason, "PATH") || !strings.Contains(reason, "nginx_mode") {
			t.Fatalf("reason 应给出安装/PATH/切换模式建议，实际 %q", reason)
		}
	})

	t.Run("docker exec 超时", func(t *testing.T) {
		fakeBin(t, "docker", "exec /bin/sleep 5")
		original := siteDirProbeTimeout
		siteDirProbeTimeout = 100 * time.Millisecond
		t.Cleanup(func() { siteDirProbeTimeout = original })

		state, reason := dockerSiteDirState("kore-nginx", "/usr/share/nginx/html")
		if state != siteDirUnknown {
			t.Fatalf("超时应为 unknown，实际 %q", state)
		}
		if !strings.Contains(reason, "没有返回") {
			t.Fatalf("reason 应说明超时，实际 %q", reason)
		}
	})
}

// TestStatusSiteDirThreeState 验证 site.status 的字段接线：
// docker 模式三种结果都落到 site_dir_state，且 site_dir_exists 恒等于 state==present。
func TestStatusSiteDirThreeState(t *testing.T) {
	const domain = "docker.example.com"
	cases := []struct {
		name      string
		script    string
		wantState string
	}{
		{"容器内存在", "exit 0", siteDirPresent},
		{"容器内不存在", "exit 1", siteDirAbsent},
		{"docker 报错未知", `echo "Error response from daemon: Container kore-nginx is not running" >&2; exit 1`, siteDirUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := isolatePluginEnv(t)
			confDir := t.TempDir()
			writeSettingsFile(t, stateDir, pluginSettings{
				NginxMode: nginxModeDocker, DockerContainer: "kore-nginx",
				DockerConfDir: "/etc/nginx/conf.d", HostConfDir: confDir,
			})
			writeTestVhost(t, confDir, domain, "/usr/share/nginx/html")
			fakeBin(t, "docker", tc.script)

			svc, perr := newSiteService()
			if perr != nil {
				t.Fatalf("构造 service 失败: %s", perr.Message)
			}
			status, perr := svc.status(domain)
			if perr != nil {
				t.Fatalf("status 失败: %s", perr.Message)
			}
			if status.NginxMode != nginxModeDocker {
				t.Fatalf("nginx_mode=%q，期望 docker", status.NginxMode)
			}
			if status.SiteDirState != tc.wantState {
				t.Fatalf("site_dir_state=%q，期望 %q（reason=%q）", status.SiteDirState, tc.wantState, status.SiteDirReason)
			}
			if status.SiteDirExists != (tc.wantState == siteDirPresent) {
				t.Fatalf("site_dir_exists=%v 与 state=%q 不一致", status.SiteDirExists, tc.wantState)
			}
			if tc.wantState == siteDirUnknown && status.SiteDirReason == "" {
				t.Fatalf("unknown 必须带 site_dir_reason")
			}
		})
	}
}

// TestStatusSiteDirNoManifest 验证没有 vhost 时目录状态是 unknown（查不到依据），
// 而不是误报「不存在」；同时保留 conf_exists=false 的既有语义。
func TestStatusSiteDirNoManifest(t *testing.T) {
	useTempConfDir(t)
	svc, perr := newSiteService()
	if perr != nil {
		t.Fatalf("构造 service 失败: %s", perr.Message)
	}
	status, perr := svc.status("missing.example.com")
	if perr != nil {
		t.Fatalf("status 失败: %s", perr.Message)
	}
	if status.ConfExists {
		t.Fatalf("conf_exists 应为 false: %+v", status)
	}
	if status.SiteDirState != siteDirUnknown || status.SiteDirExists || status.SiteDirReason == "" {
		t.Fatalf("无 vhost 时目录应为 unknown+reason: state=%q exists=%v reason=%q",
			status.SiteDirState, status.SiteDirExists, status.SiteDirReason)
	}
}

// TestGetSiteDirThreeState 验证 site.get 与 site.status 同口径：
// 容器模式下 get 也走容器内探测，不再用宿主 os.Stat 误报目录不存在。
func TestGetSiteDirThreeState(t *testing.T) {
	const domain = "get.example.com"
	cases := []struct {
		name      string
		script    string
		wantState string
	}{
		{"容器内存在", "exit 0", siteDirPresent},
		{"容器内不存在", "exit 1", siteDirAbsent},
		{"docker 报错未知", `echo "Error response from daemon: Container kore-nginx is not running" >&2; exit 1`, siteDirUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := isolatePluginEnv(t)
			confDir := t.TempDir()
			writeSettingsFile(t, stateDir, pluginSettings{
				NginxMode: nginxModeDocker, DockerContainer: "kore-nginx",
				DockerConfDir: "/etc/nginx/conf.d", HostConfDir: confDir,
			})
			writeTestVhost(t, confDir, domain, "/usr/share/nginx/html")
			fakeBin(t, "docker", tc.script)

			svc, perr := newSiteService()
			if perr != nil {
				t.Fatalf("构造 service 失败: %s", perr.Message)
			}
			payload, perr := getHandler(svc, json.RawMessage(`{"domain":"`+domain+`"}`))
			if perr != nil {
				t.Fatalf("get 失败: %s", perr.Message)
			}
			result, ok := payload.(siteGetResult)
			if !ok {
				t.Fatalf("get 结果类型=%T，期望 siteGetResult", payload)
			}
			if result.SiteDirState != tc.wantState {
				t.Fatalf("site_dir_state=%q，期望 %q（reason=%q）",
					result.SiteDirState, tc.wantState, result.SiteDirReason)
			}
			if result.SiteDirExists != (tc.wantState == siteDirPresent) {
				t.Fatalf("site_dir_exists=%v 与 state=%q 不一致", result.SiteDirExists, tc.wantState)
			}
		})
	}
}
