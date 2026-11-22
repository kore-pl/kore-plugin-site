package main

import (
	"testing"
)

// TestSetRewriteAppliesTemplate 覆盖伪静态模板切换与注释块解析。
func TestSetRewriteAppliesTemplate(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "wp.example.com"
	path, _ := createTestSite(t, dir, domain)

	result := decodeResult[rewriteSetResult](t, callOK(t, "site.setRewrite", map[string]any{
		"domain": domain, "template": "wordpress",
	}))
	if result.Template != "wordpress" || !result.Applied || !result.TestPassed || !result.Reloaded || !result.Changed {
		t.Fatalf("setRewrite 结果不对: %+v", result)
	}
	mustContain(t, readVHost(t, path),
		beginMarker(blockKeyRewrite),
		"# template: wordpress",
		"try_files $uri $uri/ /index.php?$query_string;",
	)

	got := decodeResult[rewriteGetResult](t, callOK(t, "site.getRewrite", map[string]any{"domain": domain}))
	if got.Template != "wordpress" || !got.Enabled {
		t.Fatalf("getRewrite 应回 wordpress: %+v", got)
	}

	// 切换模板必须整体替换旧块，而不是叠加。
	callOK(t, "site.setRewrite", map[string]any{"domain": domain, "template": "thinkphp"})
	content := readVHost(t, path)
	mustContain(t, content, "rewrite ^(.*)$ /index.php?s=$1 last;")
	mustNotContain(t, content, "$query_string")

	// 回到 none：恢复默认静态处理，注释块整体消失。
	callOK(t, "site.setRewrite", map[string]any{"domain": domain, "template": "none"})
	content = readVHost(t, path)
	mustContain(t, content, "try_files $uri $uri/ =404;")
	mustNotContain(t, content, beginMarker(blockKeyRewrite))

	if got := decodeResult[rewriteGetResult](t, callOK(t, "site.getRewrite", map[string]any{"domain": domain})); got.Template != "none" {
		t.Fatalf("none 后 getRewrite 应为 none: %+v", got)
	}
}

// TestSetRewriteInvalidParams 验证模板枚举与必填校验。
func TestSetRewriteInvalidParams(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "wpbad.example.com"
	createTestSite(t, dir, domain)

	callFail(t, "site.setRewrite", map[string]any{"domain": domain}, "E_INVALID", "缺少必填字段 template")
	callFail(t, "site.setRewrite", map[string]any{"domain": domain, "template": "raw-nginx"}, "E_INVALID", "template 必须是")
	callFail(t, "site.setRewrite", map[string]any{"domain": domain, "template": "none; include /etc/passwd"}, "E_INVALID", "template 必须是")
}

// TestSetProxyWritesLocation 验证反代落盘（含 WebSocket 升级头）与 get 摘要。
func TestSetProxyWritesLocation(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "proxy.example.com"
	path, _ := createTestSite(t, dir, domain)

	result := decodeResult[proxySetResult](t, callOK(t, "site.setProxy", map[string]any{
		"domain": domain, "upstream": "http://127.0.0.1:8080", "ws": true,
	}))
	if !result.Proxy || !result.WS || !result.Applied || !result.Changed {
		t.Fatalf("setProxy 结果不对: %+v", result)
	}
	content := readVHost(t, path)
	mustContain(t, content,
		beginMarker(blockKeyProxy),
		"proxy_pass http://127.0.0.1:8080;",
		"proxy_set_header Host $host;",
		"proxy_http_version 1.1;",
		"proxy_set_header Upgrade $http_upgrade;",
		`proxy_set_header Connection "upgrade";`,
	)

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if !summary.Proxy.Enabled || summary.Proxy.Upstream != "http://127.0.0.1:8080" || !summary.Proxy.WS {
		t.Fatalf("site.get 反代摘要不对: %+v", summary.Proxy)
	}

	callOK(t, "site.unsetProxy", map[string]any{"domain": domain})
	content = readVHost(t, path)
	mustNotContain(t, content, "proxy_pass", beginMarker(blockKeyProxy))
	summary = decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if summary.Proxy.Enabled {
		t.Fatalf("unsetProxy 后仍报启用: %+v", summary.Proxy)
	}
}

// TestSetProxyInvalidUpstream 覆盖上游地址正则：只允许 http://host:port。
func TestSetProxyInvalidUpstream(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "proxybad.example.com"
	createTestSite(t, dir, domain)

	cases := []struct {
		name     string
		upstream string
		contains string
	}{
		{"缺 upstream", "", "缺少必填字段 upstream"},
		{"https 上游", "https://127.0.0.1:443", "只支持 http://"},
		{"缺端口", "http://127.0.0.1", "必须是 http://host:port"},
		{"端口越界", "http://127.0.0.1:99999", "端口必须在 1..65535"},
		{"带路径", "http://127.0.0.1:80/admin", "必须是 http://host:port"},
		{"带凭据", "http://user:pass@127.0.0.1:80", "必须是 http://host:port"},
		{"注入分号", "http://127.0.0.1:80;rewrite ^ /x", "必须是 http://host:port"},
		{"IPv6 非法写", "http://[::1]:80", "必须是 http://host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callFail(t, "site.setProxy", map[string]any{"domain": domain, "upstream": tc.upstream}, "E_INVALID", tc.contains)
		})
	}
}

// TestRewriteProxyMutuallyExclusive 验证反代与伪静态互斥（都占用 location /），
// 互相设置时清掉对方并在结果里明确告知。
func TestRewriteProxyMutuallyExclusive(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "exclusive.example.com"
	path, _ := createTestSite(t, dir, domain)

	callOK(t, "site.setRewrite", map[string]any{"domain": domain, "template": "wordpress"})
	proxyResult := decodeResult[proxySetResult](t, callOK(t, "site.setProxy", map[string]any{
		"domain": domain, "upstream": "http://127.0.0.1:9000",
	}))
	if !proxyResult.RewriteCleared {
		t.Fatalf("设置反代应清掉伪静态并回 rewrite_cleared=true: %+v", proxyResult)
	}
	mustNotContain(t, readVHost(t, path), "# template: wordpress")
	if got := decodeResult[rewriteGetResult](t, callOK(t, "site.getRewrite", map[string]any{"domain": domain})); got.Template != "none" {
		t.Fatalf("伪静态应被清为 none: %+v", got)
	}

	rewriteResult := decodeResult[rewriteSetResult](t, callOK(t, "site.setRewrite", map[string]any{
		"domain": domain, "template": "nextjs",
	}))
	if !rewriteResult.ProxyCleared {
		t.Fatalf("设置伪静态应清掉反代并回 proxy_cleared=true: %+v", rewriteResult)
	}
	mustNotContain(t, readVHost(t, path), "proxy_pass")
}
