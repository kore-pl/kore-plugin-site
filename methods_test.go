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
func useTempConfDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(vhostDirEnv, dir)
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
			if got := resultString(t, response); got != `{"sites":[],"count":0,"applied":true}` {
				t.Fatalf("result=%s", got)
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

// TestVHostCreateWrite 验证 create 真写独立 vhost：server/listen/server_name/location
// 齐全，且不含 default_server；清单登记该文件。
func TestVHostCreateWrite(t *testing.T) {
	dir := useTempConfDir(t)
	params := json.RawMessage(`{"domain":"example.com","port":8080,"root":"/srv/example"}`)
	response := invoke(t, "site.create", "r-create", params)
	got := resultString(t, response)
	want := `{"domain":"example.com","vhost_path":"` + filepath.Join(dir, "kore-site-example.com.conf") + `","default_server":false,"applied":true,"created":true}`
	if got != want {
		t.Fatalf("result=%s\n期望 %s", got, want)
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
