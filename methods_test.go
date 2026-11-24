package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// invoke 发一个请求并读回响应与 serve 结束状态（每个用例独立连接）。
func invoke(t *testing.T, method, reqID string, params json.RawMessage) *pluginapi.Response {
	t.Helper()
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := startServe(pluginConn)
	writeRequest(t, kernelConn, method, reqID, params)
	response := readResponse(t, kernelConn)
	writeRequest(t, kernelConn, "kore.shutdown", "r-shutdown", nil)
	if shutdown := readResponse(t, kernelConn); string(shutdown.Result) != `{"ok":true}` {
		t.Fatalf("shutdown 结果=%s", shutdown.Result)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
	return response
}

// useTempConfDir 把每个用例的 conf.d 指到独立临时目录，并用 t.Setenv 自动还原。
// 同时把 nginx 三条命令换成 /bin/true：单测不依赖宿主是否安装 nginx，
// 需要失败分支的用例在用例内再 t.Setenv 覆盖。
func useTempConfDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(vhostDirEnv, dir)
	t.Setenv(nginxTestCmdEnv, "/bin/true")
	t.Setenv(nginxReloadCmdEnv, "/bin/true")
	t.Setenv(nginxDumpCmdEnv, "/bin/true")
	// PHP-FPM 扫描目录同样指到空目录：用例结论不能取决于跑测试的机器是否装了 PHP。
	// 需要 PHP 的用例自己调 fakeFPMSocket / emptyFPMSocketDir 覆盖。
	t.Setenv(envPHPSocketDirs, t.TempDir())
	return dir
}

// resultString 断言无错误并返回 result 原文。
func resultString(t *testing.T, response *pluginapi.Response) string {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("意外错误: code=%s message=%s", response.Error.Code, response.Error.Message)
	}
	return string(response.Result)
}

// TestSiteMethods 覆盖 list 裸名/全名与 req_id 回显（list 读真实清单）。
func TestSiteMethods(t *testing.T) {
	useTempConfDir(t)
	for _, method := range []string{"list", "site.list"} {
		t.Run(method, func(t *testing.T) {
			response := invoke(t, method, "r-1", nil)
			if response.ReqID != "r-1" {
				t.Fatalf("req_id=%q，期望回显 r-1", response.ReqID)
			}
			var result siteListResult
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatalf("解析 list 结果失败: %v (%s)", err, response.Result)
			}
			if result.Count != 0 || len(result.Sites) != 0 || !result.Applied {
				t.Fatalf("空清单结果=%+v，期望 count=0/sites=[]/applied=true", result)
			}
		})
	}
}

// TestSiteInvalidParams 覆盖非法参数：缺字段、路径穿越、大写、default_server。
func TestSiteInvalidParams(t *testing.T) {
	useTempConfDir(t)
	cases := []struct {
		name     string
		params   json.RawMessage
		contains string
	}{
		{name: "缺少 domain", params: json.RawMessage(`{}`), contains: "缺少必填字段 domain"},
		{name: "params 非对象", params: json.RawMessage(`"x"`), contains: "params 必须是 JSON 对象"},
		{name: "路径穿越", params: json.RawMessage(`{"domain":"../etc/nginx"}`), contains: "domain 非法"},
		{name: "大写域名", params: json.RawMessage(`{"domain":"Example.COM"}`), contains: "小写"},
		{name: "单标签", params: json.RawMessage(`{"domain":"localhost"}`), contains: "domain 非法"},
		{name: "默认站点", params: json.RawMessage(`{"domain":"example.com","default_server":true}`), contains: "default_server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := invoke(t, "site.create", "r-1", tc.params)
			if response.Error == nil {
				t.Fatalf("期望 E_INVALID，实际 result=%s", response.Result)
			}
			if response.Error.Code != "E_INVALID" {
				t.Fatalf("error code=%s，期望 E_INVALID", response.Error.Code)
			}
			if !strings.Contains(response.Error.Message, tc.contains) {
				t.Fatalf("error message=%q，期望包含 %q", response.Error.Message, tc.contains)
			}
		})
	}
}

// TestUnknownMethod 未知方法回 E_NOTFOUND。
func TestUnknownMethod(t *testing.T) {
	useTempConfDir(t)
	response := invoke(t, "site.nope", "r-1", nil)
	if response.Error == nil || response.Error.Code != "E_NOTFOUND" {
		t.Fatalf("error=%v，期望 E_NOTFOUND", response.Error)
	}
}

// TestVHostCreateWrite 验证 create 真写独立 vhost 并返回真实校验/重载状态：
// server/listen/server_name/location 齐全，且不含 default_server；清单登记该文件。
func TestVHostCreateWrite(t *testing.T) {
	dir := useTempConfDir(t)
	params := json.RawMessage(`{"domain":"example.com","port":8080,"root":"/srv/example"}`)
	response := invoke(t, "site.create", "r-create", params)
	var result siteCreateResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("解析 create 结果失败: %v (%s)", err, response.Result)
	}
	if result.Domain != "example.com" || result.VHostPath != filepath.Join(dir, "kore-site-example.com.conf") {
		t.Fatalf("create 结果域/路径不对: %+v", result)
	}
	if !result.Created || !result.Applied || !result.TestPassed || !result.Reloaded {
		t.Fatalf("create 应 created/applied/test_passed/reloaded 全 true: %+v", result)
	}
	if result.DefaultServer {
		t.Fatalf("create 不应占用 default_server: %+v", result)
	}

	path := filepath.Join(dir, "kore-site-example.com.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 vhost 失败: %v", err)
	}
	content := string(b)
	for _, directive := range []string{"server {", "listen 8080;", "server_name example.com;", "root /srv/example;", "location / {"} {
		if !strings.Contains(content, directive) {
			t.Fatalf("vhost 缺少 %q：\n%s", directive, content)
		}
	}
	if strings.Contains(stripComments(b), "default_server") {
		t.Fatalf("vhost 含 default_server（红队 R6）：\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		t.Fatalf("清单文件未生成: %v", err)
	}
}

// TestGeneratedVHostNoDefaultServer 直接把生成的 vhost 内容落盘并做字面 grep，
// 证明生成文件里连 default_server 这个词都不存在——命令级可复跑的红队 R6 守卫。
func TestGeneratedVHostNoDefaultServer(t *testing.T) {
	content := vhostTemplate("literal.example.com", "/var/www/literal.example.com", defaultPort)
	path := filepath.Join(t.TempDir(), "generated.conf")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "default_server") {
		t.Fatalf("生成文件字面含 default_server：\n%s", b)
	}
	t.Logf("生成文件零命中 default_server，内容:\n%s", b)
}

// TestVHostCreateIdempotentAndBackup 验证同名域名幂等：第二次 create 覆盖前先备份，
// 清单不叠加、vhost 文件仍只有一个。
func TestVHostCreateIdempotentAndBackup(t *testing.T) {
	dir := useTempConfDir(t)
	params := json.RawMessage(`{"domain":"shop.example.com"}`)
	first := invoke(t, "site.create", "r-1", params)
	var r1 siteCreateResult
	if err := json.Unmarshal(first.Result, &r1); err != nil {
		t.Fatal(err)
	}
	if !r1.Created || r1.BackupPath != "" {
		t.Fatalf("首次 create 应 created=true、无备份，实际 %+v", r1)
	}
	// 手动写入一个可识别的旧内容，再覆盖，验证备份确实留住了旧内容。
	path := filepath.Join(dir, "kore-site-shop.example.com.conf")
	if err := os.WriteFile(path, []byte("OLD-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := invoke(t, "site.create", "r-2", params)
	var r2 siteCreateResult
	if err := json.Unmarshal(second.Result, &r2); err != nil {
		t.Fatal(err)
	}
	if r2.Created || r2.BackupPath == "" {
		t.Fatalf("二次 create 应 created=false 且有备份，实际 %+v", r2)
	}
	backup, err := os.ReadFile(r2.BackupPath)
	if err != nil || string(backup) != "OLD-CONTENT\n" {
		t.Fatalf("备份内容不对: %q err=%v", backup, err)
	}
	// 清单只有一条，conf.d 里 vhost 只有一个（备份不算 vhost）。
	var m siteManifest
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil || json.Unmarshal(b, &m) != nil || len(m.Sites) != 1 {
		t.Fatalf("清单应只有一条，实际 %s err=%v", b, err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "kore-site-*.conf"))
	if len(entries) != 1 {
		t.Fatalf("vhost 文件应只有一个，实际 %v", entries)
	}
}

// TestVHostDelete 验证 delete 只删自己生成的文件；删除不存在域名不报错且 deleted=false。
func TestVHostDelete(t *testing.T) {
	dir := useTempConfDir(t)
	// 先放一个宿主既有 vhost（非本插件前缀），确认 delete 不会碰它。
	foreign := filepath.Join(dir, "host-existing.conf")
	if err := os.WriteFile(foreign, []byte("server { listen 80; server_name host.local; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"gone.example.com"}`))
	managed := filepath.Join(dir, "kore-site-gone.example.com.conf")
	if _, err := os.Stat(managed); err != nil {
		t.Fatalf("create 未落盘: %v", err)
	}

	// 删除不存在的域名：不报错、deleted=false。
	missing := invoke(t, "site.delete", "r-2", json.RawMessage(`{"domain":"never.example.com"}`))
	if missing.Error != nil {
		t.Fatalf("删除不存在域名不应报错: %v", missing.Error)
	}
	var rm siteDeleteResult
	if err := json.Unmarshal(missing.Result, &rm); err != nil || rm.Deleted {
		t.Fatalf("删除不存在域名应 deleted=false，实际 %s (%v)", missing.Result, err)
	}

	// 删除已存在域名。
	ok := invoke(t, "site.delete", "r-3", json.RawMessage(`{"domain":"gone.example.com"}`))
	var rd siteDeleteResult
	if err := json.Unmarshal(ok.Result, &rd); err != nil || !rd.Deleted {
		t.Fatalf("删除应 deleted=true，实际 %s (%v)", ok.Result, err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed vhost 未删除: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("宿主既有 vhost 被误删: %v", err)
	}
	// 重复删除仍幂等。
	again := invoke(t, "site.delete", "r-4", json.RawMessage(`{"domain":"gone.example.com"}`))
	if again.Error != nil {
		t.Fatalf("重复删除不应报错: %v", again.Error)
	}
}

// TestVHostRollbackOnConflict 验证：目标路径有宿主既有文件但不在清单时，
// create 拒绝覆盖（E_CONFLICT），原文件字节级不变——回滚/防误伤闭环。
func TestVHostRollbackOnConflict(t *testing.T) {
	dir := useTempConfDir(t)
	path := filepath.Join(dir, "kore-site-taken.example.com.conf")
	original := []byte("HOST-OWNED-DO-NOT-TOUCH\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	response := invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"taken.example.com"}`))
	if response.Error == nil || response.Error.Code != "E_CONFLICT" {
		t.Fatalf("应 E_CONFLICT，实际 error=%v result=%s", response.Error, response.Result)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatalf("宿主文件被改动: %q err=%v", after, err)
	}
}

// TestNoWebServerBinary 验证生成的 dist 包与 bin 目录不含 nginx/caddy/httpd 二进制。
// 这是裁决37的可执行守卫：site 只生成配置片段，绝不内置 web server。
func TestNoWebServerBinary(t *testing.T) {
	for _, dir := range []string{"bin", "dist"} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("读取目录 %s: %v", dir, err)
		}
		for _, e := range entries {
			lower := strings.ToLower(e.Name())
			for _, forbidden := range []string{"nginx", "caddy", "httpd", "apache"} {
				if strings.Contains(lower, forbidden) {
					t.Fatalf("%s/%s 含禁用的 web server 二进制标识 %q", dir, e.Name(), forbidden)
				}
			}
		}
	}
}

// readManifestFile 读取并解析清单，测试用。
func readManifestFile(t *testing.T, dir string) siteManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	var m siteManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("解析清单失败: %v", err)
	}
	return m
}

// TestNginxCommandConstruction 验证环境命令只提供前缀，操作旗标由插件固定注入，
// 且 RPC 入参（domain）不会出现在 argv 里。
func TestNginxCommandConstruction(t *testing.T) {
	cases := []struct {
		name    string
		testRaw string
		want    []string
	}{
		{name: "默认补 -t", testRaw: "nginx", want: []string{"nginx", "-t"}},
		{name: "完整命令不重复", testRaw: "nginx -t", want: []string{"nginx", "-t"}},
		{name: "docker exec 完整命令", testRaw: "docker exec kore-site-test nginx -t", want: []string{"docker", "exec", "kore-site-test", "nginx", "-t"}},
		{name: "docker exec 前缀补旗标", testRaw: "docker exec kore-site-test nginx", want: []string{"docker", "exec", "kore-site-test", "nginx", "-t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(nginxTestCmdEnv, tc.testRaw)
			t.Setenv(nginxReloadCmdEnv, "/bin/true")
			t.Setenv(nginxDumpCmdEnv, "/bin/true")
			runner, perr := loadNginxRunner()
			if perr != nil {
				t.Fatalf("解析命令失败: %v", perr.Message)
			}
			got := runner.test.argv("-t")
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("argv=%q，期望 %q", got, tc.want)
			}
			for _, arg := range got {
				if strings.Contains(arg, "example.com") {
					t.Fatalf("argv 混入 RPC 入参: %q", got)
				}
			}
		})
	}
}

// TestNginxCommandRejectsInjectedFlags 验证前缀自带操作旗标会被拒绝（E_INTERNAL），
// 防止 "nginx -s stop" 之类的命令借环境变量越权。
func TestNginxCommandRejectsInjectedFlags(t *testing.T) {
	cases := []string{
		`docker exec -t kore-site-test nginx`,
		`nginx -s stop`,
		`nginx -t -c /etc/nginx/nginx.conf`,
		`sh -c 'unterminated`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(nginxTestCmdEnv, raw)
			t.Setenv(nginxReloadCmdEnv, "/bin/true")
			t.Setenv(nginxDumpCmdEnv, "/bin/true")
			runner, perr := loadNginxRunner()
			if perr == nil {
				t.Fatalf("命令 %q 应被拒绝，实际 argv=%q", raw, runner.test.prefix)
			}
			if perr.Code != pluginapi.E_INTERNAL {
				t.Fatalf("错误码=%s，期望 E_INTERNAL", perr.Code)
			}
		})
	}
}

// TestRootInjectionRejected 验证 root 也会被当作配置注入面拦截。
func TestRootInjectionRejected(t *testing.T) {
	useTempConfDir(t)
	cases := []string{
		`/srv/x;\nserver {}`,
		`/srv/x;rm -rf /`,
		`relative/path`,
		`/srv/x y`,
		`/srv/x#`,
	}
	for i, root := range cases {
		t.Run(root, func(t *testing.T) {
			params := json.RawMessage(`{"domain":"inj` + itoa(i) + `.example.com","root":` + quoteJSON(root) + `}`)
			response := invoke(t, "site.create", "r-root", params)
			if response.Error == nil || response.Error.Code != pluginapi.E_INVALID {
				t.Fatalf("root=%q 应 E_INVALID，实际 error=%v result=%s", root, response.Error, response.Result)
			}
			if !strings.Contains(response.Error.Message, "root") {
				t.Fatalf("错误信息应提到 root: %q", response.Error.Message)
			}
		})
	}
}

// quoteJSON 把字符串转成 JSON 字面量，测试构造用。
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestCreateTestFailureRollsBack 验证 nginx -t 失败时：新站点删除、覆盖站点恢复原字节、
// 清单同步回滚，且错误码 E_INVALID 带 nginx 输出。
func TestCreateTestFailureRollsBack(t *testing.T) {
	dir := useTempConfDir(t)
	response := invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"example.com","root":"/srv/original"}`))
	if response.Error != nil {
		t.Fatalf("首次 create 失败: %s", response.Error.Message)
	}
	path := filepath.Join(dir, "kore-site-example.com.conf")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "nginx: [emerg] simulated bad config" >&2; exit 1'`)
	overwrite := invoke(t, "site.create", "r-2", json.RawMessage(`{"domain":"example.com","root":"/srv/changed"}`))
	if overwrite.Error == nil || overwrite.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("覆盖时校验失败应 E_INVALID，实际 error=%v result=%s", overwrite.Error, overwrite.Result)
	}
	if !strings.Contains(overwrite.Error.Message, "nginx: [emerg] simulated bad config") {
		t.Fatalf("错误信息应含 nginx 输出: %q", overwrite.Error.Message)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatalf("覆盖失败后未恢复原 vhost: %q err=%v", after, err)
	}
	if _, ok := readManifestFile(t, dir).Sites["example.com"]; !ok {
		t.Fatal("覆盖失败后清单丢失 example.com")
	}

	newSite := invoke(t, "site.create", "r-3", json.RawMessage(`{"domain":"fresh.example.com"}`))
	if newSite.Error == nil || newSite.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("新建站点校验失败应 E_INVALID，实际 error=%v", newSite.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "kore-site-fresh.example.com.conf")); !os.IsNotExist(err) {
		t.Fatalf("校验失败后新 vhost 应被删除: %v", err)
	}
	if _, ok := readManifestFile(t, dir).Sites["fresh.example.com"]; ok {
		t.Fatal("校验失败后清单不应登记新站点")
	}
}

// TestCreateReloadFailureKeepsWrittenSite 验证 reload 失败回 E_UPSTREAM，
// 且站点已写入（磁盘可见）——不做回滚，明确告知"未生效"。
func TestCreateReloadFailureKeepsWrittenSite(t *testing.T) {
	dir := useTempConfDir(t)
	invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"example.com","root":"/srv/one"}`))

	t.Setenv(nginxReloadCmdEnv, `sh -c 'echo "nginx: [emerg] reload failed" >&2; exit 1'`)
	response := invoke(t, "site.create", "r-2", json.RawMessage(`{"domain":"example.com","root":"/srv/two"}`))
	if response.Error == nil || response.Error.Code != pluginapi.E_UPSTREAM {
		t.Fatalf("reload 失败应 E_UPSTREAM，实际 error=%v result=%s", response.Error, response.Result)
	}
	if len(response.Result) != 0 && string(response.Result) != "null" {
		t.Fatalf("reload 失败不应回成功载荷: %s", response.Result)
	}
	for _, want := range []string{"已写入", "未生效", "reload failed"} {
		if !strings.Contains(response.Error.Message, want) {
			t.Fatalf("错误信息应含 %q: %q", want, response.Error.Message)
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, "kore-site-example.com.conf"))
	if err != nil || !strings.Contains(string(content), "/srv/two") {
		t.Fatalf("reload 失败时新配置应保留在磁盘: %q err=%v", content, err)
	}
	if _, ok := readManifestFile(t, dir).Sites["example.com"]; !ok {
		t.Fatal("reload 失败时清单应保留站点")
	}
}

// TestDeleteRollbackOnFailures 验证 delete 后 test/reload 失败都会恢复 vhost 与清单，
// 只有全链路成功才真正删除。
func TestDeleteRollbackOnFailures(t *testing.T) {
	dir := useTempConfDir(t)
	invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"example.com"}`))
	path := filepath.Join(dir, "kore-site-example.com.conf")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "test boom" >&2; exit 1'`)
	badTest := invoke(t, "site.delete", "r-2", json.RawMessage(`{"domain":"example.com"}`))
	if badTest.Error == nil || badTest.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("删除后校验失败应 E_INVALID，实际 error=%v", badTest.Error)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(original) {
		t.Fatalf("校验失败后应恢复 vhost: %q err=%v", after, err)
	}
	if _, ok := readManifestFile(t, dir).Sites["example.com"]; !ok {
		t.Fatal("校验失败后清单应恢复站点")
	}

	t.Setenv(nginxTestCmdEnv, "/bin/true")
	t.Setenv(nginxReloadCmdEnv, `sh -c 'echo "reload boom" >&2; exit 1'`)
	badReload := invoke(t, "site.delete", "r-3", json.RawMessage(`{"domain":"example.com"}`))
	if badReload.Error == nil || badReload.Error.Code != pluginapi.E_UPSTREAM {
		t.Fatalf("删除后重载失败应 E_UPSTREAM，实际 error=%v", badReload.Error)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(original) {
		t.Fatalf("重载失败后应恢复 vhost: %q err=%v", after, err)
	}

	t.Setenv(nginxReloadCmdEnv, "/bin/true")
	ok := invoke(t, "site.delete", "r-4", json.RawMessage(`{"domain":"example.com"}`))
	var result siteDeleteResult
	if err := json.Unmarshal(ok.Result, &result); err != nil {
		t.Fatalf("解析 delete 结果失败: %v", err)
	}
	if !result.Deleted || !result.Applied || !result.TestPassed || !result.Reloaded {
		t.Fatalf("delete 成功应 deleted/applied/test_passed/reloaded 全 true: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("delete 成功后 vhost 应消失: %v", err)
	}
}

// TestSiteStatusSignals 验证 status 的四个独立信号：conf/测试/站点目录/已加载。
func TestSiteStatusSignals(t *testing.T) {
	dir := useTempConfDir(t)
	root := filepath.Join(dir, "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "nginx: configuration file test is successful" >&2'`)
	t.Setenv(nginxDumpCmdEnv, `sh -c 'printf "server_name example.com;\n"'`)
	invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"example.com","root":`+quoteJSON(root)+`}`))

	response := invoke(t, "site.status", "r-2", json.RawMessage(`{"domain":"example.com"}`))
	var status siteStatusResult
	if err := json.Unmarshal(response.Result, &status); err != nil {
		t.Fatalf("解析 status 失败: %v (%s)", err, response.Result)
	}
	if !status.ConfExists || !status.Managed || !status.TestPassed || !status.SiteDirExists || !status.Loaded || !status.LoadChecked {
		t.Fatalf("status 应全绿: %+v", status)
	}
	if !strings.Contains(status.TestOutput, "test is successful") {
		t.Fatalf("status 应带 nginx -t 输出，实际 %q", status.TestOutput)
	}

	missing := invoke(t, "site.status", "r-3", json.RawMessage(`{"domain":"missing.example.com"}`))
	var missingStatus siteStatusResult
	if err := json.Unmarshal(missing.Result, &missingStatus); err != nil {
		t.Fatal(err)
	}
	if missingStatus.ConfExists || missingStatus.Managed || missingStatus.SiteDirExists || missingStatus.Loaded {
		t.Fatalf("不存在的站点不应有存在/加载信号: %+v", missingStatus)
	}
	if !missingStatus.TestPassed {
		t.Fatalf("全局 nginx -t 仍应通过: %+v", missingStatus)
	}
}

// TestSiteListReportsRealStatus 验证 list 每条带 exists/test_ok/reloaded，
// 且 test 失败时 test_ok 跟随为 false。
func TestSiteListReportsRealStatus(t *testing.T) {
	useTempConfDir(t)
	t.Setenv(nginxDumpCmdEnv, `sh -c 'printf "server_name one.example.com;\nserver_name two.example.com;\n"'`)
	invoke(t, "site.create", "r-1", json.RawMessage(`{"domain":"one.example.com"}`))
	invoke(t, "site.create", "r-2", json.RawMessage(`{"domain":"two.example.com"}`))

	response := invoke(t, "site.list", "r-3", nil)
	var result siteListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("解析 list 失败: %v", err)
	}
	if result.Count != 2 || !result.Applied {
		t.Fatalf("list 结果=%+v，期望 2 条且 applied", result)
	}
	for _, site := range result.Sites {
		if !site.Exists || !site.TestOK || !site.Reloaded {
			t.Fatalf("站点 %s 应 exists/test_ok/reloaded 全 true: %+v", site.Domain, site)
		}
	}

	t.Setenv(nginxTestCmdEnv, `sh -c 'exit 1'`)
	failing := invoke(t, "site.list", "r-4", nil)
	var failingResult siteListResult
	if err := json.Unmarshal(failing.Result, &failingResult); err != nil {
		t.Fatal(err)
	}
	if failingResult.Applied {
		t.Fatalf("nginx -t 失败时 list.applied 应为 false: %+v", failingResult)
	}
	for _, site := range failingResult.Sites {
		if site.TestOK {
			t.Fatalf("nginx -t 失败时 test_ok 应为 false: %+v", site)
		}
	}
}

// TestSiteListReportsSkipped 验证"没测"返回 skipped 而非 failed：
// nginx 未配置时 test_skipped/reload_skipped 为 true，且不携带失败摘要。
func TestSiteListReportsSkipped(t *testing.T) {
	useTempConfDir(t)
	invoke(t, "site.create", "s-1", json.RawMessage(`{"domain":"skip.example.com"}`))
	t.Setenv(nginxTestCmdEnv, "off")
	t.Setenv(nginxDumpCmdEnv, "off")

	response := invoke(t, "site.list", "s-2", nil)
	var result siteListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("解析 list 失败: %v", err)
	}
	if len(result.Sites) != 1 {
		t.Fatalf("期望 1 个站点: %+v", result)
	}
	site := result.Sites[0]
	if !site.TestSkipped || !site.ReloadSkipped {
		t.Fatalf("未配置 nginx 时应 skipped: %+v", site)
	}
	if site.TestOK || site.Reloaded || site.TestOutput != "" {
		t.Fatalf("skipped 不应被当成通过或失败: %+v", site)
	}

	statusResponse := invoke(t, "site.status", "s-3", json.RawMessage(`{"domain":"skip.example.com"}`))
	var status siteStatusResult
	if err := json.Unmarshal(statusResponse.Result, &status); err != nil {
		t.Fatal(err)
	}
	if !status.TestSkipped || !status.ReloadSkipped || status.TestOutput != "" {
		t.Fatalf("status 也应区分 skipped 与失败: %+v", status)
	}
}
