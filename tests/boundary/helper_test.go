// Package boundary 覆盖 PLUGIN-CATALOG 判据① 要求的四类边界用例：
// 异常输入 / 并发 / 权限不足 / 资源耗尽，并随 `make check`（go test ./...）进 CI。
//
// 测试口径与 tests/fault 一致：site 是单 main 包，无法 import 内部执行面，
// 因此全部用例都是**黑盒真进程**——拉真实二进制、走真实 UDS 与 kore.plugin.v1
// 帧协议，不 mock、不写"永远为真"的断言。
//
// 与 tests/fault 的分工（避免重复造）：
//   - tests/fault   : 四类故障注入（kill / timeout / 磁盘写失败 / ulimit -v 地址空间）
//   - tests/boundary: 判据① 的边界维度，并发用例用 -race 插桩二进制跑
//
// 本文件只做二进制构建与协议客户端；四类断言分别在
// invalid_input_test.go / concurrency_test.go / permission_test.go / exhaust_test.go。
package boundary

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const (
	// boundaryTimeout 是单次协议交互的超时预算。
	boundaryTimeout = 10 * time.Second
	// helloGrace 覆盖插件 2s 握手预算 + 进程启动余量。
	helloGrace = 6 * time.Second
	// shPrefixKey 是"用 sh -c 前缀施加 rlimit"的内部环境键，不落进插件环境。
	shPrefixKey = "__SH_PREFIX"
)

// 构建产物在 TestMain 里一次性生成，供全部用例复用。
var (
	rootDir  string
	plainBin string
	raceBin  string
)

// TestMain 定位仓根并构建两个真实二进制：
//   - plainBin：与 make build 同参（CGO_ENABLED=0），用于异常输入/权限/资源用例
//   - raceBin ：CGO_ENABLED=1 + -race，用于并发用例——只有插桩过的进程，
//     "并发下无数据竞争"才是被验证过的断言，而不是一句口号。
func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "取工作目录失败: %v\n", err)
			return 1
		}
		rootDir = findRoot(dir)
		if rootDir == "" {
			fmt.Fprintln(os.Stderr, "未找到仓根（含 go.mod）")
			return 1
		}
		outDir, err := os.MkdirTemp("", "kore-site-boundary-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "建临时构建目录失败: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(outDir) }()

		// 必须**每次**从当前源码重建：复用仓内 bin/site 会让"改了源码的变异实验"
		// 测到旧二进制（实测踩过：变异后用例照样通过 = 假绿灯）。
		plainBin = filepath.Join(outDir, "site")
		if out, buildErr := buildBinary(plainBin, false); buildErr != nil {
			fmt.Fprintf(os.Stderr, "构建 site 失败: %v\n%s\n", buildErr, out)
			return 1
		}
		raceBin = filepath.Join(outDir, "site-race")
		if out, buildErr := buildBinary(raceBin, true); buildErr != nil {
			fmt.Fprintf(os.Stderr, "构建 -race 插桩 site 失败: %v\n%s\n", buildErr, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// findRoot 从起点向上找含 go.mod 的仓根。
func findRoot(start string) string {
	dir := start
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// buildBinary 在仓根构建插件；race=true 时开启 -race 插桩（需 CGO）。
func buildBinary(out string, race bool) ([]byte, error) {
	args := []string{"build", "-trimpath", "-buildvcs=false", "-o", out}
	if race {
		args = append(args, "-race")
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = rootDir
	if race {
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOWORK=off")
	} else {
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	}
	return cmd.CombinedOutput()
}

// process 是一个真实插件进程 + 测试扮演内核侧的连接。
type process struct {
	cmd     *exec.Cmd
	conn    net.Conn
	stderr  *syncBuffer
	waitErr error
}

// syncBuffer 是并发安全的 stderr 收集器：-race 报告由插件进程写到 stderr，
// 必须在进程退出后完整读回，否则"无数据竞争"的断言就是空的。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startPluginNoFatal 在临时 UDS 上拉起真实插件；错误以返回值给出而不是 Fatal，
// 以便在 goroutine 里使用（t.Fatalf 只能在测试主 goroutine 调用）。
// 返回的 cleanup 必须 defer，负责杀进程与关监听。
func startPluginNoFatal(binary, confDir string, extraEnv map[string]string) (*process, func(), error) {
	dir, err := os.MkdirTemp("", "kore-site-run-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("建临时运行目录失败: %w", err)
	}
	sock := filepath.Join(dir, "kore.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, func() {}, fmt.Errorf("监听临时 UDS 失败: %w", err)
	}

	shPrefix := extraEnv[shPrefixKey]
	var cmd *exec.Cmd
	if shPrefix != "" {
		// rlimit 只在子 shell 生效，不污染测试进程自身。
		cmd = exec.Command("sh", "-c", shPrefix+"; exec \"$0\"", binary)
	} else {
		cmd = exec.Command(binary)
	}
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if key, value, ok := strings.Cut(kv, "="); ok {
			merged[key] = value
		}
	}
	merged["KORE_PLUGIN_SOCK"] = sock
	merged["KORE_LOG_LEVEL"] = "error"
	merged["KORE_SITE_CONF_DIR"] = confDir
	// 不依赖宿主是否装了 nginx：命令层用 true 桩，保留 create 的完整
	// "校验 → 重载"链路，只把外部 web server 换成恒定成功。
	merged["KORE_NGINX_TEST_CMD"] = "true"
	merged["KORE_NGINX_RELOAD_CMD"] = "true"
	merged["KORE_NGINX_DUMP_CMD"] = "true"
	for key, value := range extraEnv {
		if key == shPrefixKey {
			continue
		}
		merged[key] = value
	}
	cmd.Env = make([]string, 0, len(merged))
	for key, value := range merged {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, func() {}, fmt.Errorf("启动插件失败: %w", err)
	}
	p := &process{cmd: cmd, stderr: stderr}
	cleanup := func() {
		if p.conn != nil {
			_ = p.conn.Close()
		}
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}

	// net.Listener 没有 SetDeadline，用带超时的 Accept 代替，避免插件起不来时挂死。
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		ch <- accepted{conn: conn, err: acceptErr}
	}()
	select {
	case got := <-ch:
		_ = ln.Close()
		if got.err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("接受插件连接失败: %w", got.err)
		}
		p.conn = got.conn
		return p, cleanup, nil
	case <-time.After(helloGrace):
		_ = ln.Close()
		cleanup()
		return nil, func() {}, fmt.Errorf("%s 内未收到插件连接（stderr=%s）", helloGrace, stderr.String())
	}
}

// startPlugin 是 startPluginNoFatal 的 Fatal 包装（测试主 goroutine 使用）。
func startPlugin(t *testing.T, binary, confDir string, extraEnv map[string]string) *process {
	t.Helper()
	p, cleanup, err := startPluginNoFatal(binary, confDir, extraEnv)
	if err != nil {
		t.Fatalf("拉起插件失败: %v", err)
	}
	t.Cleanup(cleanup)
	return p
}

// handshakeErr 扮演内核侧完成握手，错误以返回值给出（goroutine 安全）。
func (p *process) handshakeErr() error {
	_ = p.conn.SetDeadline(time.Now().Add(boundaryTimeout))
	defer func() { _ = p.conn.SetDeadline(time.Time{}) }()
	payload, err := pluginapi.ReadFrame(p.conn, pluginapi.MaxFrameSize)
	if err != nil {
		return fmt.Errorf("读 kore.hello 失败: %w", err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		return fmt.Errorf("解码 kore.hello 失败: %w", err)
	}
	req, ok := env.(*pluginapi.Request)
	if !ok || req.Method != "kore.hello" {
		return fmt.Errorf("首帧不是 kore.hello: %T", env)
	}
	response := &pluginapi.Response{
		Type:   pluginapi.TypeResponse,
		ID:     req.ID,
		ReqID:  req.ReqID,
		Result: json.RawMessage(`{"ok":true,"protocol":"` + pluginapi.Protocol + `"}`),
	}
	if err := pluginapi.WriteJSON(p.conn, response); err != nil {
		return fmt.Errorf("回 kore.hello 响应失败: %w", err)
	}
	return nil
}

// handshake 是 handshakeErr 的 Fatal 包装。
func (p *process) handshake(t *testing.T) {
	t.Helper()
	if err := p.handshakeErr(); err != nil {
		t.Fatalf("%v", err)
	}
}

// callErr 发一个业务请求并读回响应；reqID 用于并发场景区分归属（goroutine 安全）。
func (p *process) callErr(method, params, reqID string) (*pluginapi.Response, error) {
	_ = p.conn.SetDeadline(time.Now().Add(boundaryTimeout))
	defer func() { _ = p.conn.SetDeadline(time.Time{}) }()
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-" + reqID, ReqID: reqID,
		Method: method, Params: json.RawMessage(params),
	}
	if err := pluginapi.WriteJSON(p.conn, request); err != nil {
		return nil, fmt.Errorf("写 %s 请求失败: %w", method, err)
	}
	payload, err := pluginapi.ReadFrame(p.conn, pluginapi.MaxFrameSize)
	if err != nil {
		return nil, fmt.Errorf("读 %s 响应失败: %w", method, err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		return nil, fmt.Errorf("解码 %s 响应失败: %w", method, err)
	}
	response, ok := env.(*pluginapi.Response)
	if !ok {
		return nil, fmt.Errorf("%s 响应帧类型是 %T，不是 response", method, env)
	}
	if response.ReqID != reqID {
		return nil, fmt.Errorf("%s 响应 req_id=%q 与请求 %q 不一致（状态串扰）", method, response.ReqID, reqID)
	}
	return response, nil
}

// call 是 callErr 的 Fatal 包装。
func (p *process) call(t *testing.T, method, params, reqID string) *pluginapi.Response {
	t.Helper()
	response, err := p.callErr(method, params, reqID)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return response
}

// shutdownErr 发 kore.shutdown 并配对读回响应帧，回收进程退出码（goroutine 安全）。
// 连接不读回响应会让插件写阻塞，进而掩盖真实的 -race 退出路径。
func (p *process) shutdownErr() error {
	_ = p.conn.SetDeadline(time.Now().Add(boundaryTimeout))
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-shutdown", ReqID: "r-shutdown",
		Method: "kore.shutdown",
	}
	_ = pluginapi.WriteJSON(p.conn, request)
	_, _ = pluginapi.ReadFrame(p.conn, pluginapi.MaxFrameSize)
	_ = p.conn.SetDeadline(time.Time{})
	_ = p.conn.Close()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		p.waitErr = err
		return err
	case <-time.After(boundaryTimeout):
		return fmt.Errorf("插件在 %s 内未退出", boundaryTimeout)
	}
}

// shutdown 是 shutdownErr 的 Fatal 包装。
func (p *process) shutdown(t *testing.T) error {
	t.Helper()
	if err := p.shutdownErr(); err != nil {
		t.Fatalf("%v", err)
	}
	return nil
}

// assertNoDataRace 断言 -race 插桩进程没有报告数据竞争。
// go 运行时检测到竞争时默认以退出码 66 结束并往 stderr 打印 "DATA RACE"。
func (p *process) assertNoDataRace(t *testing.T, label string) {
	t.Helper()
	if report := p.stderr.String(); strings.Contains(report, "DATA RACE") {
		t.Fatalf("%s: 插件在并发下报告数据竞争\n%s", label, report)
	}
}

// sanityCreate 是所有边界用例的反证基线：合法入参必须成功落盘。
// 用例若连这一条都过不了，说明环境/桩有问题，而不是插件行为有问题。
func (p *process) sanityCreate(t *testing.T, domain string) {
	t.Helper()
	response := p.call(t, "site.create", `{"domain":"`+domain+`","port":18080}`, "r-sanity")
	if response.Error != nil {
		t.Fatalf("sanity: 正常 site.create 意外报错: code=%s msg=%s", response.Error.Code, response.Error.Message)
	}
	var created struct {
		Applied bool `json:"applied"`
	}
	if err := unmarshalResult(response.Result, &created); err != nil || !created.Applied {
		t.Fatalf("sanity: site.create 未落盘: result=%s err=%v", response.Result, err)
	}
}

// healthyConfDir 建一个可写临时 conf.d。
func healthyConfDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conf.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建临时 conf.d 失败: %v", err)
	}
	return dir
}

// unmarshalResult 解析响应载荷（结果统一是 JSON 对象）。
func unmarshalResult(raw json.RawMessage, out any) error {
	return json.Unmarshal(raw, out)
}

// siteDirStateOf 从 site.status 结果里取 site_dir_state。
func siteDirStateOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var payload struct {
		SiteDirState string `json:"site_dir_state"`
	}
	if err := unmarshalResult(raw, &payload); err != nil {
		t.Fatalf("解析 status 结果失败: %v result=%s", err, raw)
	}
	return payload.SiteDirState
}

// readIfExists 读文件；不存在时返回错误。
func readIfExists(path string) ([]byte, error) {
	return os.ReadFile(path)
}
