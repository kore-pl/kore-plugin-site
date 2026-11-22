package main

import (
	"strings"
	"testing"
)

// TestBindSSLWritesVhost 验证 bindSSL 的落盘结果：443 监听 + 证书 + TLS1.2+
// + 80→443 跳转，并检查 site.get 能读回同一份事实。
func TestBindSSLWritesVhost(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "ssl.example.com"
	path, _ := createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "site.crt")
	key := fakeCertFile(t, dir, "site.key")

	result := decodeResult[sslBindResult](t, callOK(t, "site.bindSSL", map[string]any{
		"domain": domain, "cert_path": cert, "key_path": key,
	}))
	if !result.SSLBound || !result.ForceHTTPS || !result.TestPassed || !result.Reloaded || !result.Applied || !result.Changed {
		t.Fatalf("bindSSL 结果不完整: %+v", result)
	}
	if result.BackupPath == "" {
		t.Fatalf("bindSSL 应返回 backup_path: %+v", result)
	}

	content := readVHost(t, path)
	mustContain(t, content,
		"listen 443 ssl;",
		`ssl_certificate "`+cert+`";`,
		`ssl_certificate_key "`+key+`";`,
		"ssl_protocols TLSv1.2 TLSv1.3;",
		"ssl_prefer_server_ciphers on;",
		"return 301 https://$host$request_uri;",
		beginMarker(blockKeySSL),
		beginMarker(blockKeyHTTPRedirect),
	)
	// force_https 默认开：80 只出现在跳转 server 块里，主块只监听 443。
	if got := strings.Count(content, "listen 80;"); got != 1 {
		t.Fatalf("force_https 时应有且仅有一个 listen 80（跳转块），实际 %d:\n%s", got, content)
	}
	mustNotContain(t, content, "default_server")

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if !summary.SSL.Bound || !summary.SSL.ForceHTTPS || summary.SSL.CertPath != cert || summary.SSL.KeyPath != key {
		t.Fatalf("site.get 的 SSL 摘要不对: %+v", summary.SSL)
	}
	if summary.SSL.Protocols != sslProtocols || !summary.Enabled || !summary.Managed {
		t.Fatalf("site.get 摘要不对: %+v", summary)
	}
}

// TestBindSSLForceHTTPSFalse 验证 force_https=false：同时监听 80/443 且无跳转块。
func TestBindSSLForceHTTPSFalse(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "dual.example.com"
	path, _ := createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "dual.crt")
	key := fakeCertFile(t, dir, "dual.key")

	result := decodeResult[sslBindResult](t, callOK(t, "site.bindSSL", map[string]any{
		"domain": domain, "cert_path": cert, "key_path": key, "force_https": false,
	}))
	if result.ForceHTTPS {
		t.Fatalf("force_https=false 应回写 false: %+v", result)
	}
	content := readVHost(t, path)
	mustContain(t, content, "listen 80;", "listen 443 ssl;")
	mustNotContain(t, content, beginMarker(blockKeyHTTPRedirect), "return 301")
}

// TestBindSSLInvalidParams 覆盖证书/私钥参数校验（配置注入面与文件存在性）。
func TestBindSSLInvalidParams(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "bad.example.com"
	createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "ok.crt")
	key := fakeCertFile(t, dir, "ok.key")

	cases := []struct {
		name     string
		params   map[string]any
		contains string
	}{
		{"缺 cert_path", map[string]any{"domain": domain, "key_path": key}, "缺少必填字段 cert_path"},
		{"缺 key_path", map[string]any{"domain": domain, "cert_path": cert}, "缺少必填字段 key_path"},
		{"相对路径", map[string]any{"domain": domain, "cert_path": "certs/a.crt", "key_path": key}, "绝对路径"},
		{"注入字符", map[string]any{"domain": domain, "cert_path": "/tmp/a.crt;include /etc/passwd", "key_path": key}, "仅允许"},
		{"文件不存在", map[string]any{"domain": domain, "cert_path": "/tmp/kore-no-such.crt", "key_path": key}, "不可读"},
		{"不是普通文件", map[string]any{"domain": domain, "cert_path": dir, "key_path": key}, "不是普通文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callFail(t, "site.bindSSL", tc.params, "E_INVALID", tc.contains)
		})
	}

	callFail(t, "site.unbindSSL", map[string]any{"domain": "missing.example.com"}, "E_NOTFOUND", "站点不存在")
}

// TestBindSSLTestFailureRollsBack 验证 nginx -t 失败时 vhost 字节级回滚。
func TestBindSSLTestFailureRollsBack(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "rollback-ssl.example.com"
	path, _ := createTestSite(t, dir, domain)
	before := readVHost(t, path)
	cert := fakeCertFile(t, dir, "r.crt")
	key := fakeCertFile(t, dir, "r.key")

	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "nginx: [emerg] simulated tls error" >&2; exit 1'`)
	callFail(t, "site.bindSSL", map[string]any{
		"domain": domain, "cert_path": cert, "key_path": key,
	}, "E_INVALID", "已回滚")

	if after := readVHost(t, path); after != before {
		t.Fatalf("校验失败后 vhost 未原样恢复:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestBindSSLIdempotent 验证重复绑定同一参数不写盘、不跑 nginx（changed=false）。
func TestBindSSLIdempotent(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "idem.example.com"
	createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "i.crt")
	key := fakeCertFile(t, dir, "i.key")
	params := map[string]any{"domain": domain, "cert_path": cert, "key_path": key}

	callOK(t, "site.bindSSL", params)
	second := decodeResult[sslBindResult](t, callOK(t, "site.bindSSL", params))
	if second.Changed || !second.TestSkipped || !second.ReloadSkipped || !second.Applied {
		t.Fatalf("重复绑定应幂等短路: %+v", second)
	}
}

// TestUnbindSSLRemovesBlock 验证解绑后站点回到纯 HTTP。
func TestUnbindSSLRemovesBlock(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "unbind.example.com"
	path, _ := createTestSite(t, dir, domain)
	cert := fakeCertFile(t, dir, "u.crt")
	key := fakeCertFile(t, dir, "u.key")
	callOK(t, "site.bindSSL", map[string]any{"domain": domain, "cert_path": cert, "key_path": key})

	result := decodeResult[sslUnbindResult](t, callOK(t, "site.unbindSSL", map[string]any{"domain": domain}))
	if result.SSLBound || !result.Changed || !result.TestPassed || !result.Reloaded {
		t.Fatalf("unbindSSL 结果不对: %+v", result)
	}
	content := readVHost(t, path)
	mustNotContain(t, content, "ssl_certificate", "listen 443", beginMarker(blockKeyHTTPRedirect))
	mustContain(t, content, "listen 80;")

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if summary.SSL.Bound {
		t.Fatalf("解绑后 site.get 仍报 ssl bound: %+v", summary.SSL)
	}
}
