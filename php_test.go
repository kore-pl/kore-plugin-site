package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFPMSocket 在临时目录里创建一个**真实** unix socket（php<version>-fpm.sock），
// 并把插件的 FPM 扫描目录指向该目录。
//
// 探测逻辑只认 unix socket（普通文件不算），因此用例不能用 os.WriteFile 造假；
// 这里用 net.Listen("unix") 造真实 socket，Close 时由 Go 负责删掉路径。
func fakeFPMSocket(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "php"+version+"-fpm.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("创建假 FPM socket %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv(envPHPSocketDirs, dir)
	return socket
}

// emptyFPMSocketDir 把扫描目录指到空目录：确定性验证"本机没有 PHP-FPM"的分支，
// 不会因为跑测试的机器真的装了 PHP 而偶发通过。
func emptyFPMSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envPHPSocketDirs, dir)
	return dir
}

// TestPHPSiteVhostHasFastcgiPass 是核心正向用例：PHP 站必须真的落到 FPM socket。
func TestPHPSiteVhostHasFastcgiPass(t *testing.T) {
	dir := useTempConfDir(t)
	socket := fakeFPMSocket(t, "8.3")
	domain := "php.example.com"
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根: %v", err)
	}

	result := decodeResult[siteCreateResult](t, callOK(t, "site.create", map[string]any{
		"domain": domain, "root": root, "php_version": "auto",
	}))
	if !result.PHPEnabled || result.PHPSocket != socket || result.PHPVersion != "8.3" {
		t.Fatalf("create 结果应披露真实 PHP 状态: %+v", result)
	}

	vhost := readVHost(t, filepath.Join(dir, "kore-site-"+domain+".conf"))
	mustContain(t, vhost,
		beginMarker(blockKeyPHP),
		"location ~ \\.php$ {",
		"fastcgi_pass unix:"+socket+";",
		"fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;",
		"fastcgi_param REDIRECT_STATUS 200;",
		"try_files $uri =404;",
		endMarker(blockKeyPHP),
	)
	// PHP 站也不该丢掉静态处理：location / 仍然在。
	mustContain(t, vhost, "location / {", "try_files $uri $uri/ =404;")

	got := decodeResult[phpGetResult](t, callOK(t, "site.getPHP", map[string]any{"domain": domain}))
	if !got.PHPEnabled || got.PHPSocket != socket || got.PHPVersion != "8.3" {
		t.Fatalf("getPHP 应回真实状态: %+v", got)
	}
	if len(got.Available) != 1 || got.Available[0].Version != "8.3" {
		t.Fatalf("getPHP 应披露可用候选: %+v", got.Available)
	}

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if !summary.PHP.Enabled || summary.PHP.Socket != socket {
		t.Fatalf("site.get 的 php 子视图应 enabled: %+v", summary.PHP)
	}
	status := decodeResult[siteStatusResult](t, callOK(t, "site.status", map[string]any{"domain": domain}))
	if !status.PHPEnabled || status.PHPVersion != "8.3" {
		t.Fatalf("site.status 应披露 php_enabled/php_version: %+v", status)
	}
}

// TestStaticSiteHasNoFastcgiPass 是核心反向用例：纯静态站多一行 PHP 处理面都是缺陷。
func TestStaticSiteHasNoFastcgiPass(t *testing.T) {
	dir := useTempConfDir(t)
	fakeFPMSocket(t, "8.3") // 即使本机有 PHP，不要求就不许加
	domain := "static.example.com"
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根: %v", err)
	}

	result := decodeResult[siteCreateResult](t, callOK(t, "site.create", map[string]any{
		"domain": domain, "root": root,
	}))
	if result.PHPEnabled {
		t.Fatalf("未请求 PHP 的站点不应启用 PHP: %+v", result)
	}
	vhost := readVHost(t, filepath.Join(dir, "kore-site-"+domain+".conf"))
	mustNotContain(t, vhost, "fastcgi_pass", "location ~ \\.php$", beginMarker(blockKeyPHP))

	summary := decodeResult[siteGetResult](t, callOK(t, "site.get", map[string]any{"domain": domain}))
	if summary.PHP.Enabled {
		t.Fatalf("静态站 site.get 的 php.enabled 必须为 false: %+v", summary.PHP)
	}
}

// TestCreatePHPWithoutFPMFails 验证"探测不到 FPM 就明确报错"：既不报成功，也不落半成品。
func TestCreatePHPWithoutFPMFails(t *testing.T) {
	dir := useTempConfDir(t)
	emptyFPMSocketDir(t)
	domain := "nophp.example.com"
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根: %v", err)
	}

	callFail(t, "site.create", map[string]any{
		"domain": domain, "root": root, "php_version": "auto",
	}, "E_INVALID", "PHP-FPM")
	// 关键：不做"静默生成静态站"——连 vhost 都不该出现。
	if _, err := os.Stat(filepath.Join(dir, "kore-site-"+domain+".conf")); !os.IsNotExist(err) {
		t.Fatalf("PHP 解析失败时不应落盘 vhost，实际 err=%v", err)
	}
}

// TestPHPSiteExplicitVersion 验证指定版本必须命中，否则列出已探测到的版本。
func TestPHPSiteExplicitVersion(t *testing.T) {
	dir := useTempConfDir(t)
	fakeFPMSocket(t, "8.3")
	domain := "ver.example.com"
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根: %v", err)
	}
	callFail(t, "site.create", map[string]any{
		"domain": domain, "root": root, "php_version": "8.2",
	}, "E_INVALID", "8.3")

	result := decodeResult[siteCreateResult](t, callOK(t, "site.create", map[string]any{
		"domain": domain, "root": root, "php_version": "8.3",
	}))
	if !result.PHPEnabled || result.PHPVersion != "8.3" {
		t.Fatalf("指定 8.3 应命中: %+v", result)
	}

	// 版本号是注入面：非 X.Y 形状必须拒绝。
	callFail(t, "site.create", map[string]any{
		"domain": domain, "php_version": "8.3; }",
	}, "E_INVALID", "php_version")
}

// TestSetRewriteWordpressWithoutPHPRejected 是本缺陷的直接回归：
// 选中 wordpress 但站点没有 PHP ⇒ 必须拒绝，且 vhost 一个字节都不变。
func TestSetRewriteWordpressWithoutPHPRejected(t *testing.T) {
	dir := useTempConfDir(t)
	emptyFPMSocketDir(t)
	domain := "wp-nophp.example.com"
	path, _ := createTestSite(t, dir, domain)
	before := readVHost(t, path)

	callFail(t, "site.setRewrite", map[string]any{
		"domain": domain, "template": "wordpress",
	}, "E_INVALID", "PHP-FPM")

	after := readVHost(t, path)
	if after != before {
		t.Fatalf("被拒绝的 setRewrite 不得改动 vhost\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	mustNotContain(t, after, "# template: wordpress", "fastcgi_pass")
}

// TestSetRewriteWordpressAutoEnablesPHP 验证正交设计：
// 伪静态 location / 与 PHP location ~ \.php$ **必须并存**（不是二选一）。
func TestSetRewriteWordpressAutoEnablesPHP(t *testing.T) {
	dir := useTempConfDir(t)
	socket := fakeFPMSocket(t, "8.3")
	domain := "wp.example.com"
	path, _ := createTestSite(t, dir, domain)

	result := decodeResult[rewriteSetResult](t, callOK(t, "site.setRewrite", map[string]any{
		"domain": domain, "template": "wordpress",
	}))
	if !result.Applied || !result.PHPEnabled || !result.PHPAutoEnabled || result.PHPVersion != "8.3" {
		t.Fatalf("wordpress 应自动补上 PHP 并生效: %+v", result)
	}
	vhost := readVHost(t, path)
	// 伪静态块（location / 美化链接）与 PHP 块必须同时存在且顺序正确。
	mustContain(t, vhost,
		"# template: wordpress",
		"try_files $uri $uri/ /index.php?$query_string;",
		"fastcgi_pass unix:"+socket+";",
	)
	if strings.Index(vhost, beginMarker(blockKeyRewrite)) > strings.Index(vhost, beginMarker(blockKeyPHP)) {
		t.Fatalf("PHP 块应在伪静态块之后（content 块在前，正交块追加在后）:\n%s", vhost)
	}

	// 幂等：再设一次同模板不得改写文件。
	again := decodeResult[rewriteSetResult](t, callOK(t, "site.setRewrite", map[string]any{
		"domain": domain, "template": "wordpress",
	}))
	if again.Changed {
		t.Fatalf("重复设置同模板不应产生改动: %+v", again)
	}
	if readVHost(t, path) != vhost {
		t.Fatal("重复设置同模板后 vhost 字节应完全一致")
	}
}

// TestUnsetPHPRefusedWhilePHPRewrite 验证不会留下"PHP 伪静态 + 无 FPM"的坏站点。
func TestUnsetPHPRefusedWhilePHPRewrite(t *testing.T) {
	dir := useTempConfDir(t)
	fakeFPMSocket(t, "8.3")
	domain := "unset.example.com"
	path, _ := createTestSite(t, dir, domain)
	callOK(t, "site.setRewrite", map[string]any{"domain": domain, "template": "wordpress"})

	callFail(t, "site.unsetPHP", map[string]any{"domain": domain}, "E_INVALID", "site.setRewrite")
	mustContain(t, readVHost(t, path), "fastcgi_pass")

	callOK(t, "site.setRewrite", map[string]any{"domain": domain, "template": "none"})
	result := decodeResult[phpSetResult](t, callOK(t, "site.unsetPHP", map[string]any{"domain": domain}))
	if result.PHPEnabled {
		t.Fatalf("unsetPHP 后 php_enabled 应为 false: %+v", result)
	}
	mustNotContain(t, readVHost(t, path), "fastcgi_pass", beginMarker(blockKeyPHP))

	// 幂等：再关一次不报错也不改文件。
	if again := decodeResult[phpSetResult](t, callOK(t, "site.unsetPHP", map[string]any{"domain": domain})); again.Changed {
		t.Fatalf("重复 unsetPHP 不应改动: %+v", again)
	}
}

// TestSetPHPExplicitSocket 验证显式 socket 的注入防线与"不可见如实披露"。
func TestSetPHPExplicitSocket(t *testing.T) {
	dir := useTempConfDir(t)
	emptyFPMSocketDir(t)
	domain := "socket.example.com"
	path, _ := createTestSite(t, dir, domain)

	// 注入面：socket 会原样进 nginx 配置，必须拒绝非法字符。
	callFail(t, "site.setPHP", map[string]any{
		"domain": domain, "socket": "/run/php/x.sock; }",
	}, "E_INVALID", "socket 非法")

	// 显式声明但宿主不可见（容器场景）：允许，但 verified=false + 给下一步提示。
	result := decodeResult[phpSetResult](t, callOK(t, "site.setPHP", map[string]any{
		"domain": domain, "socket": "/run/php/php8.3-fpm.sock",
	}))
	if !result.PHPEnabled || result.PHPVersion != "8.3" || result.PHPVerified {
		t.Fatalf("显式 socket 应启用且标记未验证: %+v", result)
	}
	if result.PHPHint == "" {
		t.Fatalf("未验证的 socket 必须给出提示: %+v", result)
	}
	mustContain(t, readVHost(t, path), "fastcgi_pass unix:/run/php/php8.3-fpm.sock;")
}

// TestPHPConfigSocketOverride 验证 config.php_fpm_socket 作为探测兜底，且版本冲突被拒。
func TestPHPConfigSocketOverride(t *testing.T) {
	dir := useTempConfDir(t)
	emptyFPMSocketDir(t)
	domain := "cfg.example.com"
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根: %v", err)
	}
	t.Setenv(envPHPFpmSocket, "/run/php/php8.2-fpm.sock")

	result := decodeResult[siteCreateResult](t, callOK(t, "site.create", map[string]any{
		"domain": domain, "root": root, "php_version": "auto",
	}))
	if !result.PHPEnabled || result.PHPVersion != "8.2" {
		t.Fatalf("配置 socket 应作为兜底并解析出版本: %+v", result)
	}
	// 请求版本与配置 socket 不一致必须拒绝，不能静默改写成另一个版本。
	callFail(t, "site.setPHP", map[string]any{
		"domain": domain, "php_version": "8.3",
	}, "E_INVALID", "8.2")
}

// TestCloneCopiesPHPState 验证克隆沿用源站点 PHP 配置（配置复制，不重新探测）。
func TestCloneCopiesPHPState(t *testing.T) {
	dir := useTempConfDir(t)
	socket := fakeFPMSocket(t, "8.3")
	src, _ := createTestSite(t, dir, "src-php.example.com")
	callOK(t, "site.setPHP", map[string]any{"domain": "src-php.example.com", "php_version": "auto"})

	callOK(t, "site.clone", map[string]any{"source_domain": "src-php.example.com", "new_domain": "dst-php.example.com"})
	mustContain(t, readVHost(t, filepath.Join(dir, "kore-site-dst-php.example.com.conf")),
		"fastcgi_pass unix:"+socket+";")
	mustContain(t, readVHost(t, src), "fastcgi_pass unix:"+socket+";")
}

// TestPHPRenderParseRoundTrip 验证 render↔parse 互逆：解析出的状态重渲染后字节一致。
func TestPHPRenderParseRoundTrip(t *testing.T) {
	st := siteState{
		Domain:  "rt.example.com",
		Root:    "/var/www/rt.example.com",
		Port:    80,
		Index:   defaultIndexFiles(),
		Rewrite: "wordpress",
		PHP:     &phpState{Version: "8.3", Socket: "/run/php/php8.3-fpm.sock"},
	}
	first := string(renderVHost(st))
	parsed := parseSiteState(st.Domain, first)
	if parsed.PHP == nil || parsed.PHP.Socket != st.PHP.Socket || parsed.PHP.Version != "8.3" {
		t.Fatalf("parse 未还原 PHP 状态: %+v", parsed.PHP)
	}
	if second := string(renderVHost(parsed)); second != first {
		t.Fatalf("render→parse→render 不幂等\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
