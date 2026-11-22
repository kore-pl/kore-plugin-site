package main

import "testing"

// TestRedirectSetGetUnset 覆盖多重重定向的增删与 get 摘要。
func TestRedirectSetGetUnset(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "redir.example.com"
	path, _ := createTestSite(t, dir, domain)

	result := decodeResult[redirectSetResult](t, callOK(t, "site.setRedirect", map[string]any{
		"domain": domain, "from_path": "/old", "to_url": "/new", "code": 302,
	}))
	if result.FromPath != "/old" || result.ToURL != "/new" || result.Code != 302 || result.Redirects != 1 {
		t.Fatalf("setRedirect 结果不对: %+v", result)
	}
	if !result.Applied || !result.TestPassed || !result.Reloaded || !result.Changed {
		t.Fatalf("setRedirect 应全绿: %+v", result)
	}
	mustContain(t, readVHost(t, path), "location = /old {", "return 302 /new;", beginMarker(blockKeyRedirectPrefix+"/old"))

	// 第二条：省略 code 默认 301。
	second := decodeResult[redirectSetResult](t, callOK(t, "site.setRedirect", map[string]any{
		"domain": domain, "from_path": "/legacy", "to_url": "https://example.com/new",
	}))
	if second.Code != 301 || second.Redirects != 2 {
		t.Fatalf("第二条重定向默认码/计数不对: %+v", second)
	}

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if len(summary.Redirects) != 2 {
		t.Fatalf("site.get 应回两条重定向: %+v", summary.Redirects)
	}
	if summary.Redirects[0].FromPath != "/old" || summary.Redirects[1].FromPath != "/legacy" {
		t.Fatalf("重定向顺序应保持写入顺序: %+v", summary.Redirects)
	}

	removed := decodeResult[redirectUnsetResult](t, callOK(t, "site.unsetRedirect", map[string]any{
		"domain": domain, "from_path": "/old",
	}))
	if !removed.Removed || removed.Redirects != 1 {
		t.Fatalf("unsetRedirect 结果不对: %+v", removed)
	}
	mustNotContain(t, readVHost(t, path), "/old")

	again := decodeResult[redirectUnsetResult](t, callOK(t, "site.unsetRedirect", map[string]any{
		"domain": domain, "from_path": "/old",
	}))
	if again.Removed || again.Changed {
		t.Fatalf("重复删除应幂等: %+v", again)
	}
}

// TestRedirectInvalidParams 覆盖 from_path / to_url / code 三处校验。
func TestRedirectInvalidParams(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "redirbad.example.com"
	createTestSite(t, dir, domain)

	cases := []struct {
		name     string
		params   map[string]any
		contains string
	}{
		{"缺 from_path", map[string]any{"domain": domain, "to_url": "/new"}, "缺少必填字段 from_path"},
		{"from 不以斜杠开头", map[string]any{"domain": domain, "from_path": "old", "to_url": "/new"}, "必须以 / 开头"},
		{"from 含分号", map[string]any{"domain": domain, "from_path": "/a;b", "to_url": "/new"}, "必须以 / 开头"},
		{"from 含点点", map[string]any{"domain": domain, "from_path": "/a/../b", "to_url": "/new"}, "不允许包含 .."},
		{"缺 to_url", map[string]any{"domain": domain, "from_path": "/old"}, "缺少必填字段 to_url"},
		{"javascript 伪协议", map[string]any{"domain": domain, "from_path": "/old", "to_url": "javascript:alert(1)"}, "只允许 http:// 或 https://"},
		{"data 伪协议", map[string]any{"domain": domain, "from_path": "/old", "to_url": "data:text/html,x"}, "只允许 http:// 或 https://"},
		{"协议相对地址", map[string]any{"domain": domain, "from_path": "/old", "to_url": "//evil.com/x"}, "协议相对地址"},
		{"ftp 协议", map[string]any{"domain": domain, "from_path": "/old", "to_url": "ftp://example.com/x"}, "只允许 http:// 或 https://"},
		{"to_url 带分号", map[string]any{"domain": domain, "from_path": "/old", "to_url": "https://example.com/a;b"}, "只允许 http:// 或 https://"},
		{"code 非法", map[string]any{"domain": domain, "from_path": "/old", "to_url": "/new", "code": 307}, "只允许 301 或 302"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callFail(t, "site.setRedirect", tc.params, "E_INVALID", tc.contains)
		})
	}
}

// TestRedirectTestFailureRollsBack 验证 nginx -t 失败时重定向改动被回滚。
func TestRedirectTestFailureRollsBack(t *testing.T) {
	dir := useTempConfDir(t)
	domain := "redirroll.example.com"
	path, _ := createTestSite(t, dir, domain)
	before := readVHost(t, path)

	t.Setenv(nginxTestCmdEnv, `sh -c 'echo "nginx: [emerg] simulated redirect error" >&2; exit 1'`)
	callFail(t, "site.setRedirect", map[string]any{
		"domain": domain, "from_path": "/old", "to_url": "/new",
	}, "E_INVALID", "已回滚")

	if after := readVHost(t, path); after != before {
		t.Fatalf("校验失败后 vhost 未恢复:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
