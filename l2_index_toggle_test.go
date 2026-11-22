package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetIndex 覆盖默认文档设置与默认值回退。
func TestSetIndex(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "index.example.com"
	path, _ := createTestSite(t, dir, domain)

	result := decodeResult[indexResult](t, callOK(t, "site.setIndex", map[string]any{
		"domain": domain, "files": []string{"index.php", "index.html"},
	}))
	if len(result.Index) != 2 || !result.Applied || !result.Changed {
		t.Fatalf("setIndex 结果不对: %+v", result)
	}
	mustContain(t, readVHost(t, path), "index index.php index.html;")

	// files 省略时回落标准三件套。
	callOK(t, "site.setIndex", map[string]any{"domain": domain})
	mustContain(t, readVHost(t, path), "index index.html index.htm index.php;")

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if len(summary.Index) != 3 || summary.Index[0] != "index.html" || summary.Index[2] != "index.php" {
		t.Fatalf("site.get 的 index 列表不对: %+v", summary.Index)
	}
}

// TestSetIndexInvalidFiles 验证文件名白名单与条目上限。
func TestSetIndexInvalidFiles(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "indexbad.example.com"
	createTestSite(t, dir, domain)

	tooMany := make([]string, 0, maxIndexFiles+1)
	for i := 0; i <= maxIndexFiles; i++ {
		tooMany = append(tooMany, "index"+itoa(i)+".html")
	}
	cases := []struct {
		name     string
		files    any
		contains string
	}{
		{"路径穿越", []string{"../etc/passwd"}, "index 文件名非法"},
		{"空格", []string{"index file.html"}, "index 文件名非法"},
		{"分号注入", []string{"a;b"}, "index 文件名非法"},
		{"点点", []string{".."}, "index 文件名非法"},
		{"超上限", tooMany, "超过上限 16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callFail(t, "site.setIndex", map[string]any{"domain": domain, "files": tc.files}, "E_INVALID", tc.contains)
		})
	}
}

// TestDisableEnable 验证停用只改名不删配置，启用后原样恢复。
func TestDisableEnable(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "toggle.example.com"
	path, _ := createTestSite(t, dir, domain)
	before := readVHost(t, path)
	disabledPath := path + disabledSuffix

	disabled := decodeResult[siteToggleResult](t, callOK(t, "site.disable", map[string]any{"domain": domain}))
	if disabled.Enabled || !disabled.Changed || !disabled.Applied || !disabled.TestPassed || !disabled.Reloaded {
		t.Fatalf("disable 结果不对: %+v", disabled)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("停用后主 vhost 应不存在: %v", err)
	}
	if _, err := os.Stat(disabledPath); err != nil {
		t.Fatalf("停用后 .disabled 副本应存在: %v", err)
	}
	if readVHost(t, disabledPath) != before {
		t.Fatalf("停用不应改动配置内容")
	}

	list := decodeResult[siteListResult](t, callOK(t, "site.list", nil))
	if len(list.Sites) != 1 {
		t.Fatalf("list 应仍有 1 条: %+v", list)
	}
	entry := list.Sites[0]
	if entry.Enabled || !entry.Disabled || !entry.Exists {
		t.Fatalf("list 条目应 enabled=false/disabled=true/exists=true: %+v", entry)
	}
	if !list.Applied {
		t.Fatalf("停用是设计内状态，list.applied 不应为 false: %+v", list)
	}

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if !summary.Disabled || summary.Enabled || summary.Root == "" {
		t.Fatalf("site.get 应可见禁用站点配置: %+v", summary)
	}

	// 停用态不允许改配置，错误信息必须给出下一步。
	callFail(t, "site.setRewrite", map[string]any{"domain": domain, "template": "wordpress"}, "E_CONFLICT", "请先 site.enable")

	enabled := decodeResult[siteToggleResult](t, callOK(t, "site.enable", map[string]any{"domain": domain}))
	if !enabled.Enabled || !enabled.Changed || !enabled.Applied {
		t.Fatalf("enable 结果不对: %+v", enabled)
	}
	if readVHost(t, path) != before {
		t.Fatalf("启用后配置应与停用前逐字节一致")
	}
	if _, err := os.Stat(disabledPath); !os.IsNotExist(err) {
		t.Fatalf("启用后 .disabled 副本应消失: %v", err)
	}

	// 重复停用幂等。
	callOK(t, "site.disable", map[string]any{"domain": domain})
	again := decodeResult[siteToggleResult](t, callOK(t, "site.disable", map[string]any{"domain": domain}))
	if again.Changed || !again.TestSkipped || !again.ReloadSkipped || !again.Applied {
		t.Fatalf("重复停用应幂等短路: %+v", again)
	}
}

// TestDisableRollbackOnFailure 验证停用过程中 test/reload 失败都会改回原名。
func TestDisableRollbackOnFailure(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "toggleroll.example.com"
	path, _ := createTestSite(t, dir, domain)

	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "test boom" >&2; exit 1'`)
	callFail(t, "site.disable", map[string]any{"domain": domain}, "E_INVALID", "已恢复原状态")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("校验失败后主 vhost 应恢复: %v", err)
	}
	if _, err := os.Stat(path + disabledSuffix); !os.IsNotExist(err) {
		t.Fatalf("校验失败后不应残留 .disabled: %v", err)
	}

	t.Setenv(nginxTestCmdEnv, "/bin/true")
	t.Setenv(nginxReloadCmdEnv, `sh -c 'echo "reload boom" >&2; exit 1'`)
	callFail(t, "site.disable", map[string]any{"domain": domain}, "E_UPSTREAM", "已恢复原状态")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("重载失败后主 vhost 应恢复: %v", err)
	}
}

// TestGetReportsAllL2Features 验证单站点可同时携带 SSL/反代/重定向/index。
func TestGetReportsAllL2Features(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "full.example.com"
	path, root := createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "full.crt")
	key := fakeCertFile(t, dir, "full.key")

	callOK(t, "site.bindSSL", map[string]any{"domain": domain, "cert_path": cert, "key_path": key, "force_https": false})
	callOK(t, "site.setProxy", map[string]any{"domain": domain, "upstream": "http://127.0.0.1:7000"})
	callOK(t, "site.setRedirect", map[string]any{"domain": domain, "from_path": "/old", "to_url": "/new", "code": 302})
	callOK(t, "site.setIndex", map[string]any{"domain": domain, "files": []string{"app.html"}})

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if !summary.SSL.Bound || summary.SSL.ForceHTTPS {
		t.Fatalf("SSL 摘要不对: %+v", summary.SSL)
	}
	if !summary.Proxy.Enabled || summary.Proxy.Upstream != "http://127.0.0.1:7000" {
		t.Fatalf("反代摘要不对: %+v", summary.Proxy)
	}
	if len(summary.Redirects) != 1 || summary.Redirects[0].Code != 302 {
		t.Fatalf("重定向摘要不对: %+v", summary.Redirects)
	}
	if len(summary.Index) != 1 || summary.Index[0] != "app.html" {
		t.Fatalf("index 摘要不对: %+v", summary.Index)
	}
	if summary.Root != root || !summary.SiteDirExists || summary.Port != defaultPort {
		t.Fatalf("根目录/端口摘要不对: %+v", summary)
	}
	// 四类配置共存且 nginx 结构合法：只有一个 location /（反代），重定向是精确匹配。
	mustContain(t, readVHost(t, path),
		"listen 80;", "listen 443 ssl;", "proxy_pass http://127.0.0.1:7000;",
		"location = /old {", "index app.html;",
	)
	if entries, _ := filepath.Glob(filepath.Join(dir, "kore-site-*.conf")); len(entries) != 1 {
		t.Fatalf("conf.d 应只有一个 vhost，实际 %v", entries)
	}
}
