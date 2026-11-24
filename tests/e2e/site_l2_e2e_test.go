//go:build e2e

package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L2 端到端使用独立端口，避免与 v1 用例（18888）互相抢占。
const (
	l2HTTPPort    = "18889"
	l2HTTPSPort   = "18443"
	l2BackendPort = "18990"
	l2Domain      = "l2.kore.test"
	l2SiteMarker  = "kore-site-l2-ok"
	l2BackendMark = "kore-site-l2-backend-ok"
)

// TestDockerNginxL2 用真实 nginx:alpine 验证 L2 六步：
// create+bindSSL（https 200 / http 301）→ 伪静态 → 反代 → 重定向 → 启停 → index。
func TestDockerNginxL2(t *testing.T) {
	requireDocker(t)
	assertPortFreeL2(t, l2HTTPPort)
	assertPortFreeL2(t, l2HTTPSPort)

	workspace := t.TempDir()
	confDir := filepath.Join(workspace, "conf.d")
	wwwDir := filepath.Join(workspace, "www")
	certDir := filepath.Join(workspace, "certs")
	backendDir := filepath.Join(workspace, "backend")
	for _, dir := range []string{confDir, wwwDir, certDir, backendDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", dir, err)
		}
	}
	writeText(t, filepath.Join(wwwDir, "index.html"), l2SiteMarker+"\n")
	writeText(t, filepath.Join(backendDir, "index.html"), l2BackendMark+"\n")
	certPath, keyPath := makeSelfSignedCert(t, certDir, l2Domain)

	container := startNginxL2(t, confDir, wwwDir, certDir, backendDir)
	binary := buildPlugin(t)
	main := startPlugin(t, binary, confDir, map[string]string{
		"KORE_NGINX_TEST_CMD":   "docker exec " + container + " nginx -t",
		"KORE_NGINX_RELOAD_CMD": "docker exec " + container + " nginx -s reload",
		// wordpress 是 PHP 伪静态模板：插件会写入 fastcgi_pass 块。容器里没有 PHP-FPM，
		// nginx -t 不校验 socket 是否存在（只有真正请求 .php 时才连），因此显式声明 socket
		// 即可让"伪静态 + nginx -t"这条链仍然成立；静态页请求不会走到 PHP。
		"KORE_SITE_PHP_FPM_SOCKET": "/run/php/php8.3-fpm.sock",
	})
	defer main.stop(t)

	vhostPath := filepath.Join(confDir, "kore-site-"+l2Domain+".conf")

	// --- 步骤 1a：create 后 HTTP 200 ---
	if resp := main.call(t, "site.create", `{"domain":"`+l2Domain+`","root":`+jsonQuote(wwwDir)+`,"port":80}`); resp.Error != nil {
		t.Fatalf("site.create 失败: %s/%s", resp.Error.Code, resp.Error.Message)
	}
	code, _, body := waitResponse(t, "http", l2HTTPPort, "/", func(c int, _ http.Header, b string) bool {
		return c == http.StatusOK && strings.Contains(b, l2SiteMarker)
	})
	t.Logf("步骤1a create: curl -H Host:%s http://127.0.0.1:%s/ -> HTTP %d body=%q", l2Domain, l2HTTPPort, code, snippet(body))

	// --- 步骤 1b：bindSSL 后 HTTPS 200、HTTP 301 ---
	sslResp := main.call(t, "site.bindSSL", `{"domain":"`+l2Domain+`","cert_path":`+jsonQuote(certPath)+`,"key_path":`+jsonQuote(keyPath)+`}`)
	if sslResp.Error != nil {
		t.Fatalf("site.bindSSL 失败: %s/%s", sslResp.Error.Code, sslResp.Error.Message)
	}
	var sslResult struct {
		SSLBound   bool   `json:"ssl_bound"`
		ForceHTTPS bool   `json:"force_https"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
		BackupPath string `json:"backup_path"`
	}
	if err := json.Unmarshal(sslResp.Result, &sslResult); err != nil {
		t.Fatalf("解析 bindSSL: %v (%s)", err, sslResp.Result)
	}
	if !sslResult.SSLBound || !sslResult.ForceHTTPS || !sslResult.TestPassed || !sslResult.Reloaded {
		t.Fatalf("bindSSL 应全绿: %+v", sslResult)
	}
	vhost := readText(t, vhostPath)
	t.Logf("bindSSL 后 vhost 片段（脱敏）:\n%s", vhost)

	httpsCode, _, httpsBody := waitResponse(t, "https", l2HTTPSPort, "/", func(c int, _ http.Header, b string) bool {
		return c == http.StatusOK && strings.Contains(b, l2SiteMarker)
	})
	t.Logf("步骤1b bindSSL: curl -k https://127.0.0.1:%s/ -> HTTP %d body=%q", l2HTTPSPort, httpsCode, snippet(httpsBody))

	httpCode, httpHeader, _ := waitResponse(t, "http", l2HTTPPort, "/", func(c int, _ http.Header, _ string) bool {
		return c == http.StatusMovedPermanently
	})
	location := httpHeader.Get("Location")
	t.Logf("步骤1b bindSSL: curl http://127.0.0.1:%s/ -> HTTP %d Location=%q", l2HTTPPort, httpCode, location)
	if !strings.HasPrefix(location, "https://"+l2Domain) {
		t.Fatalf("80→443 跳转目标不对: %q", location)
	}

	// --- 步骤 2：伪静态 ---
	rewriteResp := main.call(t, "site.setRewrite", `{"domain":"`+l2Domain+`","template":"wordpress"}`)
	if rewriteResp.Error != nil {
		t.Fatalf("site.setRewrite 失败: %s/%s", rewriteResp.Error.Code, rewriteResp.Error.Message)
	}
	var rewriteResult struct {
		Template   string `json:"template"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
	}
	if err := json.Unmarshal(rewriteResp.Result, &rewriteResult); err != nil || !rewriteResult.TestPassed {
		t.Fatalf("setRewrite 应 nginx -t 通过: err=%v result=%s", err, rewriteResp.Result)
	}
	gotRewrite := main.call(t, "site.getRewrite", `{"domain":"`+l2Domain+`"}`)
	var rewriteGet struct {
		Template string `json:"template"`
	}
	if err := json.Unmarshal(gotRewrite.Result, &rewriteGet); err != nil || rewriteGet.Template != "wordpress" {
		t.Fatalf("getRewrite 应回 wordpress: err=%v result=%s", err, gotRewrite.Result)
	}
	waitResponse(t, "https", l2HTTPSPort, "/", func(c int, _ http.Header, b string) bool {
		return c == http.StatusOK && strings.Contains(b, l2SiteMarker)
	})
	t.Logf("步骤2 setRewrite: nginx -t 通过, getRewrite=%q, https 仍 200", rewriteGet.Template)

	// --- 步骤 3：反向代理到容器内 127.0.0.1:18990 ---
	startStaticBackend(t, container, backendDir)
	proxyResp := main.call(t, "site.setProxy", `{"domain":"`+l2Domain+`","upstream":"http://127.0.0.1:`+l2BackendPort+`","ws":true}`)
	if proxyResp.Error != nil {
		t.Fatalf("site.setProxy 失败: %s/%s", proxyResp.Error.Code, proxyResp.Error.Message)
	}
	var proxyResult struct {
		Proxy      bool   `json:"proxy"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
		Upstream   string `json:"upstream"`
	}
	if err := json.Unmarshal(proxyResp.Result, &proxyResult); err != nil || !proxyResult.Proxy || !proxyResult.TestPassed {
		t.Fatalf("setProxy 应生效: err=%v result=%s", err, proxyResp.Result)
	}
	proxyCode, _, proxyBody := waitResponse(t, "https", l2HTTPSPort, "/", func(c int, _ http.Header, b string) bool {
		return c == http.StatusOK && strings.Contains(b, l2BackendMark)
	})
	t.Logf("步骤3 setProxy: https -> HTTP %d body=%q（命中后端 127.0.0.1:%s）", proxyCode, snippet(proxyBody), l2BackendPort)

	// --- 步骤 4：重定向（先撤反代，隔离变量） ---
	if resp := main.call(t, "site.unsetProxy", `{"domain":"`+l2Domain+`"}`); resp.Error != nil {
		t.Fatalf("site.unsetProxy 失败: %s", resp.Error.Message)
	}
	redirectResp := main.call(t, "site.setRedirect", `{"domain":"`+l2Domain+`","from_path":"/old","to_url":"/new","code":302}`)
	if redirectResp.Error != nil {
		t.Fatalf("site.setRedirect 失败: %s/%s", redirectResp.Error.Code, redirectResp.Error.Message)
	}
	redirectCode, redirectHeader, _ := waitResponse(t, "https", l2HTTPSPort, "/old", func(c int, _ http.Header, _ string) bool {
		return c == http.StatusFound
	})
	t.Logf("步骤4 setRedirect: curl -I https://127.0.0.1:%s/old -> HTTP %d Location=%q", l2HTTPSPort, redirectCode, redirectHeader.Get("Location"))

	// --- 步骤 5：停用 → 站点不再命中；启用 → 恢复 ---
	disableResp := main.call(t, "site.disable", `{"domain":"`+l2Domain+`"}`)
	if disableResp.Error != nil {
		t.Fatalf("site.disable 失败: %s/%s", disableResp.Error.Code, disableResp.Error.Message)
	}
	var toggle struct {
		Enabled bool `json:"enabled"`
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(disableResp.Result, &toggle); err != nil || toggle.Enabled || !toggle.Applied {
		t.Fatalf("disable 结果不对: err=%v result=%s", err, disableResp.Result)
	}
	if _, err := os.Stat(vhostPath); !os.IsNotExist(err) {
		t.Fatalf("disable 后主 vhost 应消失: %v", err)
	}
	if _, err := os.Stat(vhostPath + ".disabled"); err != nil {
		t.Fatalf("disable 后 .disabled 副本应存在: %v", err)
	}
	// nginx -s reload 只发 SIGHUP 就返回，旧 worker 会短暂继续服务：
	// 判据是"轮询到不再命中" + nginx -T 已加载配置里不再有该 server_name。
	disabledCode, disabledErr := waitSiteGone(t, "https", l2HTTPSPort, "/", 15*time.Second)
	dump, dumpErr := dockerExec(container, "nginx", "-T")
	if dumpErr != nil {
		t.Fatalf("nginx -T 失败: %v %s", dumpErr, dump)
	}
	if strings.Contains(dump, "server_name "+l2Domain+";") {
		t.Fatalf("disable 后 nginx 已加载配置里仍有 %s", l2Domain)
	}
	t.Logf("步骤5 disable: curl -k https://127.0.0.1:%s/ -> HTTP %d err=%q；nginx -T 已无 %s",
		l2HTTPSPort, disabledCode, disabledErr, l2Domain)
	if resp := main.call(t, "site.enable", `{"domain":"`+l2Domain+`"}`); resp.Error != nil {
		t.Fatalf("site.enable 失败: %s/%s", resp.Error.Code, resp.Error.Message)
	}
	enabledCode, _, _ := waitResponse(t, "https", l2HTTPSPort, "/", func(c int, _ http.Header, b string) bool {
		return c == http.StatusOK && strings.Contains(b, l2SiteMarker)
	})
	t.Logf("步骤5 enable: curl -k https://127.0.0.1:%s/ -> HTTP %d（站点恢复）", l2HTTPSPort, enabledCode)

	// --- 步骤 6：默认文档 ---
	indexResp := main.call(t, "site.setIndex", `{"domain":"`+l2Domain+`","files":["index.html","index.htm","index.php"]}`)
	if indexResp.Error != nil {
		t.Fatalf("site.setIndex 失败: %s/%s", indexResp.Error.Code, indexResp.Error.Message)
	}
	vhost = readText(t, vhostPath)
	if !strings.Contains(vhost, "index index.html index.htm index.php;") {
		t.Fatalf("vhost 缺少 index 指令:\n%s", vhost)
	}
	t.Logf("步骤6 setIndex: vhost 含 %q", "index index.html index.htm index.php;")
}

// makeSelfSignedCert 用 openssl 现场签一张自签证书（docker 内 nginx 直接引用）。
func makeSelfSignedCert(t *testing.T, dir, domain string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skipf("openssl 不可用，跳过 L2 e2e: %v", err)
	}
	certPath := filepath.Join(dir, "site.crt")
	keyPath := filepath.Join(dir, "site.key")
	cmd := exec.Command("openssl", "req", "-x509", "-nodes", "-newkey", "rsa:2048",
		"-days", "1", "-subj", "/CN="+domain, "-keyout", keyPath, "-out", certPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成自签证书失败: %v\n%s", err, out)
	}
	return certPath, keyPath
}

// startNginxL2 拉起 nginx:alpine 并映射 80/443、conf.d、站点根、证书与后端目录。
func startNginxL2(t *testing.T, confDir, wwwDir, certDir, backendDir string) string {
	t.Helper()
	name := fmt.Sprintf("kore-site-l2-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{
		"run", "-d", "--name", name,
		"-p", l2HTTPPort + ":80", "-p", l2HTTPSPort + ":443",
		"-v", confDir + ":/etc/nginx/conf.d:ro",
		"-v", wwwDir + ":" + wwwDir + ":ro",
		"-v", certDir + ":" + certDir + ":ro",
		"-v", backendDir + ":" + backendDir + ":ro",
		"nginx:alpine",
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run 失败: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if out, err := dockerExec(name, "nginx", "-t"); err == nil {
			t.Logf("容器就绪: %s", strings.TrimSpace(out))
			return name
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("容器 20s 内未就绪，logs:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startStaticBackend 构建静态后端二进制、docker cp 进容器并常驻在 127.0.0.1:18990。
func startStaticBackend(t *testing.T, container, backendDir string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "kore-l2-backend")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".")
	cmd.Dir = filepath.Join(repoRoot(t), "tests", "e2e", "testdata", "backend")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建后端失败: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "cp", binary, container+":/tmp/kore-l2-backend").CombinedOutput(); err != nil {
		t.Fatalf("复制后端进容器失败: %v %s", err, out)
	}
	if out, err := exec.Command("docker", "exec", "-d", container, "/tmp/kore-l2-backend",
		"-addr", "127.0.0.1:"+l2BackendPort, "-dir", backendDir).CombinedOutput(); err != nil {
		t.Fatalf("启动后端失败: %v %s", err, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if out, err := dockerExec(container, "wget", "-q", "-O", "-", "http://127.0.0.1:"+l2BackendPort+"/"); err == nil && strings.Contains(out, l2BackendMark) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("后端 15s 内未就绪")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitResponse 轮询直到条件满足，返回最后一次状态码/头/正文。
func waitResponse(t *testing.T, scheme, port, path string, ok func(int, http.Header, string) bool) (int, http.Header, string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastCode int
	var lastBody, lastErr string
	for {
		code, header, body, err := doRequest(scheme, port, l2Domain, path)
		lastCode = code
		if err != nil {
			lastBody, lastErr = "", err.Error()
		} else {
			lastBody, lastErr = body, ""
		}
		if ok(code, header, body) {
			return code, header, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s://127.0.0.1:%s%s 超时: code=%d err=%s body=%q", scheme, port, path, lastCode, lastErr, snippet(lastBody))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitSiteGone 轮询直到站点不再返回 200（reload 异步生效），超时才判失败。
func waitSiteGone(t *testing.T, scheme, port, path string, timeout time.Duration) (int, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastCode int
	var lastErr string
	for {
		lastCode, lastErr = curlStatus(t, scheme, port, path)
		if lastErr != "" || lastCode != http.StatusOK {
			return lastCode, lastErr
		}
		if time.Now().After(deadline) {
			t.Fatalf("disable 后 %v 内站点仍返回 200", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// curlStatus 发一次请求，返回状态码与错误（用于断言"不再命中"）。
func curlStatus(t *testing.T, scheme, port, path string) (int, string) {
	t.Helper()
	code, _, _, err := doRequest(scheme, port, l2Domain, path)
	if err != nil {
		return 0, err.Error()
	}
	return code, ""
}

// doRequest 用固定 Host 头发请求；自签证书跳过校验，禁止跟随跳转以观察 30x。
func doRequest(scheme, port, host, path string) (int, http.Header, string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- 自签证书仅本地 e2e
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequest(http.MethodGet, scheme+"://127.0.0.1:"+port+path, nil)
	if err != nil {
		return 0, nil, "", err
	}
	request.Host = host
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return response.StatusCode, response.Header, string(body), nil
}

// assertPortFreeL2 确认测试端口未被占用。
func assertPortFreeL2(t *testing.T, port string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("端口 %s 已被占用", port)
	}
}

// writeText 写测试用文本文件。
func writeText(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

// readText 读文件内容。
func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
