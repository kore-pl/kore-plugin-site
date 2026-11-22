//go:build e2e

package e2e

import (
	"bytes"
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

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const (
	containerPort = "18888"
	siteMarker    = "kore-site-e2e-ok"
)

// TestDockerNginxLifecycle 用真实 nginx:alpine 容器验证建站闭环四步：
// create 生效（curl 命中）→ 非法配置被 nginx -t 拦下 → 插件校验失败回滚 →
// delete 后重载且不再命中站点。
func TestDockerNginxLifecycle(t *testing.T) {
	requireDocker(t)
	assertPortFree(t, containerPort)

	workspace := t.TempDir()
	confDir := filepath.Join(workspace, "conf.d")
	wwwDir := filepath.Join(workspace, "www")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("建 conf.d: %v", err)
	}
	if err := os.MkdirAll(wwwDir, 0o755); err != nil {
		t.Fatalf("建 www: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wwwDir, "index.html"), []byte(siteMarker+"\n"), 0o644); err != nil {
		t.Fatalf("写 index.html: %v", err)
	}

	container := startNginx(t, confDir, wwwDir)
	binary := buildPlugin(t)

	// 主实例：真实 docker exec nginx -t / nginx -s reload。
	main := startPlugin(t, binary, confDir, map[string]string{
		"KORE_NGINX_TEST_CMD":   "docker exec " + container + " nginx -t",
		"KORE_NGINX_RELOAD_CMD": "docker exec " + container + " nginx -s reload",
	})
	defer main.stop(t)

	// --- 1. create example.test：conf 落盘 + test_passed + reloaded ---
	created := main.call(t, "site.create", `{"domain":"example.test","root":`+jsonQuote(wwwDir)+`,"port":80}`)
	if created.Error != nil {
		t.Fatalf("site.create 失败: %s/%s", created.Error.Code, created.Error.Message)
	}
	var createResult struct {
		VHostPath  string `json:"vhost_path"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
		Applied    bool   `json:"applied"`
		Created    bool   `json:"created"`
	}
	if err := json.Unmarshal(created.Result, &createResult); err != nil {
		t.Fatalf("解析 create: %v (%s)", err, created.Result)
	}
	if !createResult.TestPassed || !createResult.Reloaded || !createResult.Applied || !createResult.Created {
		t.Fatalf("create 应 test_passed/reloaded/applied/created 全 true: %+v", createResult)
	}
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-example.test.conf")); err != nil {
		t.Fatalf("vhost 未落盘: %v", err)
	}
	t.Logf("步骤1 create: %s", created.Result)

	// --- 2. curl -H Host: example.test 应命中站点（200 + 标记）---
	code, body := waitHTTP(t, "example.test", func(code int, body string) bool {
		return code == http.StatusOK && strings.Contains(body, siteMarker)
	}, 10*time.Second)
	t.Logf("步骤2 curl Host: example.test -> HTTP %d, body=%q", code, snippet(body))

	// status / list 也必须反映真实加载状态。
	status := main.call(t, "site.status", `{"domain":"example.test"}`)
	var statusResult struct {
		ConfExists    bool `json:"conf_exists"`
		TestPassed    bool `json:"test_passed"`
		SiteDirExists bool `json:"site_dir_exists"`
		Loaded        bool `json:"loaded"`
		Managed       bool `json:"managed"`
	}
	if err := json.Unmarshal(status.Result, &statusResult); err != nil || status.Error != nil {
		t.Fatalf("site.status 失败: err=%v result=%s", status.Error, status.Result)
	}
	if !statusResult.ConfExists || !statusResult.TestPassed || !statusResult.SiteDirExists || !statusResult.Loaded || !statusResult.Managed {
		t.Fatalf("status 应全绿: %+v, raw=%s", statusResult, status.Result)
	}
	t.Logf("步骤2 status: %s", status.Result)

	list := main.call(t, "site.list", "")
	var listResult struct {
		Sites []struct {
			Domain   string `json:"domain"`
			Exists   bool   `json:"exists"`
			TestOK   bool   `json:"test_ok"`
			Reloaded bool   `json:"reloaded"`
		} `json:"sites"`
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(list.Result, &listResult); err != nil || list.Error != nil {
		t.Fatalf("site.list 失败: err=%v result=%s", list.Error, list.Result)
	}
	if len(listResult.Sites) != 1 || !listResult.Sites[0].Exists || !listResult.Sites[0].TestOK || !listResult.Sites[0].Reloaded {
		t.Fatalf("list 条目状态不对: %+v", listResult)
	}
	t.Logf("步骤2 list: %s", list.Result)

	// --- 3. 故意写坏 conf：nginx -t 必须失败；移除后恢复通过 ---
	broken := filepath.Join(confDir, "kore-site-broken.test.conf")
	brokenConf := "server {\n    listen 80;\n    server_name broken.test;\n    root /srv/www\n}\n"
	if err := os.WriteFile(broken, []byte(brokenConf), 0o600); err != nil {
		t.Fatalf("写坏 conf: %v", err)
	}
	brokenOut, brokenErr := dockerExec(container, "nginx", "-t")
	if brokenErr == nil {
		t.Fatalf("缺分号的 conf 竟然通过 nginx -t: %s", brokenOut)
	}
	t.Logf("步骤3 坏 conf nginx -t 退出非零: %v, 输出=%q", brokenErr, snippet(brokenOut))
	if err := os.Remove(broken); err != nil {
		t.Fatalf("移除坏 conf: %v", err)
	}
	if okOut, okErr := dockerExec(container, "nginx", "-t"); okErr != nil {
		t.Fatalf("移除坏 conf 后 nginx -t 仍失败: %v %s", okErr, okOut)
	}

	// 插件侧回滚：用"永远失败"的校验包装器，验证 create 会撤销自己写入的 conf。
	failing := startPlugin(t, binary, confDir, map[string]string{
		"KORE_NGINX_TEST_CMD":   `sh -c 'echo "nginx: [emerg] simulated invalid config" >&2; exit 1'`,
		"KORE_NGINX_RELOAD_CMD": "docker exec " + container + " nginx -s reload",
	})
	defer failing.stop(t)
	rolled := failing.call(t, "site.create", `{"domain":"rollback.test","root":`+jsonQuote(wwwDir)+`,"port":80}`)
	if rolled.Error == nil || rolled.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("校验失败应 E_INVALID，实际 error=%v result=%s", rolled.Error, rolled.Result)
	}
	if !strings.Contains(rolled.Error.Message, "simulated invalid config") {
		t.Fatalf("错误信息应带 nginx 输出: %q", rolled.Error.Message)
	}
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-rollback.test.conf")); !os.IsNotExist(err) {
		t.Fatalf("回滚后 vhost 应不存在: %v", err)
	}
	if okOut, okErr := dockerExec(container, "nginx", "-t"); okErr != nil {
		t.Fatalf("插件回滚后真实 nginx -t 仍失败: %v %s", okErr, okOut)
	}
	t.Logf("步骤3 插件回滚: E_INVALID=%q; 回滚后真实 nginx -t 通过", rolled.Error.Message)

	// 覆盖场景：先建合法站点，再让失败校验覆盖，原 conf 必须字节级恢复。
	if resp := main.call(t, "site.create", `{"domain":"rollback.test","root":`+jsonQuote(wwwDir)+`,"port":80}`); resp.Error != nil {
		t.Fatalf("预置合法站点失败: %s", resp.Error.Message)
	}
	rollbackPath := filepath.Join(confDir, "kore-site-rollback.test.conf")
	before, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if resp := failing.call(t, "site.create", `{"domain":"rollback.test","root":`+jsonQuote(wwwDir+"/other")+`,"port":80}`); resp.Error == nil {
		t.Fatal("覆盖时校验失败应报错")
	}
	after, err := os.ReadFile(rollbackPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("覆盖失败后未原样恢复: err=%v", err)
	}
	if okOut, okErr := dockerExec(container, "nginx", "-t"); okErr != nil {
		t.Fatalf("覆盖回滚后真实 nginx -t 仍失败: %v %s", okErr, okOut)
	}
	t.Logf("步骤3 覆盖回滚: 原 conf 字节级恢复，真实 nginx -t 通过")

	// 先删预置的 rollback.test：nginx 会把第一个 server 块当作隐式默认站点，
	// 若它还在且 root 相同，删除 example.test 后 Host 仍会命中同一个根目录。
	if resp := main.call(t, "site.delete", `{"domain":"rollback.test"}`); resp.Error != nil {
		t.Fatalf("清理 rollback.test 失败: %s", resp.Error.Message)
	}

	// --- 4. delete：conf 消失、reload 成功、curl 不再命中站点 ---
	deleted := main.call(t, "site.delete", `{"domain":"example.test"}`)
	var deleteResult struct {
		Deleted    bool `json:"deleted"`
		TestPassed bool `json:"test_passed"`
		Reloaded   bool `json:"reloaded"`
		Applied    bool `json:"applied"`
	}
	if err := json.Unmarshal(deleted.Result, &deleteResult); err != nil || deleted.Error != nil {
		t.Fatalf("site.delete 失败: err=%v result=%s", deleted.Error, deleted.Result)
	}
	if !deleteResult.Deleted || !deleteResult.TestPassed || !deleteResult.Reloaded || !deleteResult.Applied {
		t.Fatalf("delete 应 deleted/test_passed/reloaded/applied 全 true: %+v", deleteResult)
	}
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-example.test.conf")); !os.IsNotExist(err) {
		t.Fatalf("删除后 vhost 应消失: %v", err)
	}
	code, body = waitHTTP(t, "example.test", func(code int, body string) bool {
		return code == 0 || !strings.Contains(body, siteMarker)
	}, 10*time.Second)
	t.Logf("步骤4 delete: %s", deleted.Result)
	t.Logf("步骤4 curl Host: example.test -> HTTP %d（0=连接被拒/无响应）, body=%q（不再命中站点）", code, snippet(body))

}

// requireDocker 检查 docker CLI 与 daemon 可用。
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker 不可用，跳过 e2e: %v", err)
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon 不可用，跳过 e2e: %v %s", err, out)
	}
}

// assertPortFree 确认 18888 未被占用。
func assertPortFree(t *testing.T, port string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("端口 %s 已被占用", port)
	}
}

// startNginx 拉起 nginx:alpine，映射 conf.d 与站点根目录，等待容器可执行 nginx -t。
func startNginx(t *testing.T, confDir, wwwDir string) string {
	t.Helper()
	name := fmt.Sprintf("kore-site-e2e-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{
		"run", "-d", "--name", name,
		"-p", containerPort + ":80",
		"-v", confDir + ":/etc/nginx/conf.d:ro",
		"-v", wwwDir + ":" + wwwDir + ":ro",
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

// dockerExec 在容器内执行命令，返回合并输出与退出错误。
func dockerExec(container string, args ...string) (string, error) {
	full := append([]string{"exec", container}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	return string(out), err
}

// buildPlugin 构建当前插件二进制到临时目录（不改仓内 bin/dist）。
func buildPlugin(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "site")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建插件失败: %v\n%s", err, buildOut)
	}
	return out
}

// repoRoot 从测试目录向上找到本仓根。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录: %v", err)
	}
	for i := 0; i < 8; i++ {
		mod, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && strings.Contains(string(mod), "module github.com/pengtang1024/kore-plugin-site") {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("未找到仓根")
	return ""
}

// pluginProc 是真实插件进程 + 已完成的 hello 会话。
type pluginProc struct {
	conn    net.Conn
	cmd     *exec.Cmd
	reqSeq  int
	logs    bytes.Buffer
	stopped bool
}

// startPlugin 在临时 UDS 上拉起真二进制并完成 kore.hello 握手。
func startPlugin(t *testing.T, binary, confDir string, extraEnv map[string]string) *pluginProc {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "kore.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听 UDS: %v", err)
	}
	process := &pluginProc{logs: bytes.Buffer{}}
	process.cmd = exec.Command(binary)
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KORE_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"KORE_PLUGIN_SOCK="+sock,
		"KORE_LOG_LEVEL=error",
		"KORE_SITE_CONF_DIR="+confDir,
	)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	process.cmd.Env = env
	process.cmd.Stderr = &process.logs
	if err := process.cmd.Start(); err != nil {
		_ = ln.Close()
		t.Fatalf("启动插件: %v", err)
	}
	t.Cleanup(func() {
		process.stop(t)
		_ = ln.Close()
	})
	if unixLn, ok := ln.(*net.UnixListener); ok {
		_ = unixLn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("接受插件连接: %v\n%s", err, process.logs.String())
	}
	process.conn = conn
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读 kore.hello: %v", err)
	}
	envFrame, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码 kore.hello: %v", err)
	}
	hello, ok := envFrame.(*pluginapi.Request)
	if !ok || hello.Method != "kore.hello" {
		t.Fatalf("首帧不是 kore.hello: %T", envFrame)
	}
	response := &pluginapi.Response{
		Type: pluginapi.TypeResponse, ID: hello.ID, ReqID: hello.ReqID,
		Result: json.RawMessage(`{"ok":true,"protocol":"` + pluginapi.Protocol + `"}`),
	}
	if err := pluginapi.WriteJSON(conn, response); err != nil {
		t.Fatalf("回 kore.hello: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return process
}

// call 发一个业务请求并读回响应。
func (p *pluginProc) call(t *testing.T, method, params string) *pluginapi.Response {
	t.Helper()
	p.reqSeq++
	reqID := fmt.Sprintf("r-%d", p.reqSeq)
	var raw json.RawMessage
	if params != "" {
		raw = json.RawMessage(params)
	}
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: fmt.Sprintf("id-%d", p.reqSeq), ReqID: reqID,
		Method: method, Params: raw,
	}
	_ = p.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := pluginapi.WriteJSON(p.conn, request); err != nil {
		t.Fatalf("%s 写请求失败: %v", method, err)
	}
	payload, err := pluginapi.ReadFrame(p.conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("%s 读响应失败: %v\n插件日志:\n%s", method, err, p.logs.String())
	}
	frame, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("%s 解码响应失败: %v", method, err)
	}
	response, ok := frame.(*pluginapi.Response)
	if !ok {
		t.Fatalf("%s 响应帧类型 %T", method, frame)
	}
	_ = p.conn.SetDeadline(time.Time{})
	return response
}

// stop 发 kore.shutdown 配对读回响应，再回收进程。
func (p *pluginProc) stop(t *testing.T) {
	if p == nil || p.stopped {
		return
	}
	p.stopped = true
	if p.conn != nil {
		p.reqSeq++
		request := &pluginapi.Request{
			Type: pluginapi.TypeRequest, ID: fmt.Sprintf("id-%d", p.reqSeq), ReqID: "r-shutdown",
			Method: "kore.shutdown",
		}
		_ = p.conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := pluginapi.WriteJSON(p.conn, request); err == nil {
			_, _ = pluginapi.ReadFrame(p.conn, pluginapi.MaxFrameSize)
		}
		_ = p.conn.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
}

// waitHTTP 轮询直到条件满足；返回最后一次状态码与正文。
func waitHTTP(t *testing.T, host string, ok func(code int, body string) bool, timeout time.Duration) (int, string) {
	t.Helper()
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	deadline := time.Now().Add(timeout)
	var lastCode int
	var lastBody string
	for {
		request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+containerPort+"/", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			lastCode, lastBody = response.StatusCode, string(body)
			if ok(lastCode, lastBody) {
				return lastCode, lastBody
			}
		} else {
			lastCode, lastBody = 0, err.Error()
			if ok(lastCode, lastBody) {
				return lastCode, lastBody
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时：最后一次 code=%d body=%q", timeout, lastCode, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// jsonQuote 把字符串编码成 JSON 字面量，便于拼 params。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// snippet 截断日志正文。
func snippet(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= 160 {
		return s
	}
	return string(runes[:160]) + "…"
}
