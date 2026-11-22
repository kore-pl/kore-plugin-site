// Package fault 对 site 插件做四类故障注入（G-20 / V1-GOALS §29）。
//
// 测试是黑盒：真拉 bin/site 二进制、走真实 UDS 与 kore.plugin.v1 帧协议，
// 不 mock、不 t.Skip。所有发往插件的 kore.shutdown 都配对读回响应帧，
// 避免连接断开时的悬挂（logs/dns 曾踩过这个坑）。
//
// site 是单 main 包，测试无法 import 内部执行面；同时 dist sha 已入索引，
// 禁止改生产代码 / go.mod / Makefile。因此四类故障全部从进程外部注入：
//   - kill      : SIGKILL 进程，断言连接断、无响应（E_UPSTREAM 语义）
//   - timeout   : 内核不回 hello，插件必须在 2s 握手预算内退出（E_UPSTREAM 语义）
//   - disk      : conf.d 指向普通文件，site.create 写盘失败 → E_INTERNAL
//   - cgroup    : ulimit -v 压低地址空间，进程无法服务 → E_UPSTREAM 语义
package fault

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// faultTimeout 是单类故障测试的整体超时预算。
const faultTimeout = 10 * time.Second

// helloTimeoutLimit 覆盖插件 2s 握手超时 + 退出余量。
const helloTimeoutLimit = 6 * time.Second

// siteBin 是仓内预期二进制的相对路径。
const siteBin = "bin/site"

// faultParams 是 site.create 的合法入参（conf.d 指向临时目录时不会污染系统）。
const faultParams = `{"domain":"fault.example.com","port":18080}`

// findBinary 从测试目录向上找仓根的 bin/site；若尚未构建，则临时构建真二进制。
// 两条路径都针对真进程，构建产物写到 t.TempDir，不污染仓内、不改 dist 输入。
func findBinary(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	root := ""
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, siteBin)
		if st, statErr := os.Stat(candidate); statErr == nil && !st.IsDir() {
			return candidate
		}
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			root = dir
			break
		}
		dir = filepath.Dir(dir)
	}
	if root == "" {
		t.Fatalf("未找到仓根（含 go.mod），起点目录 %s", dir)
	}
	built := filepath.Join(t.TempDir(), "site")
	cmd := exec.Command("go", "build", "-o", built, ".")
	cmd.Dir = root
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("临时构建 site 二进制失败: %v\n%s", buildErr, out)
	}
	return built
}

// startPlugin 在临时 UDS 上拉起真实插件进程，返回进程与连接函数。
// confDir 非空时通过 KORE_SITE_CONF_DIR 注入到子进程；memLimitKB>0 时用
// sh -c 'ulimit -v' 限制地址空间（等价 cgroup 内存超限的进程侧表现）。
func startPlugin(t *testing.T, binary, confDir string, memLimitKB int) (*exec.Cmd, func() net.Conn) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "kore.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听临时 UDS 失败: %v", err)
	}

	var cmd *exec.Cmd
	if memLimitKB > 0 {
		// ulimit 只在子 shell 生效，不污染测试进程自身。
		script := "ulimit -v " + itoa(memLimitKB) + "; exec \"$0\""
		cmd = exec.Command("sh", "-c", script, binary)
	} else {
		cmd = exec.Command(binary)
	}
	cmd.Env = append(os.Environ(),
		"KORE_PLUGIN_SOCK="+sock,
		"KORE_LOG_LEVEL=error",
		"KORE_SITE_CONF_DIR="+confDir,
	)
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		t.Fatalf("启动插件失败: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = ln.Close()
	})
	accept := func() net.Conn {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			t.Fatalf("接受插件连接失败: %v", acceptErr)
		}
		return conn
	}
	return cmd, accept
}

// handshakeWith 扮演内核侧：读插件主动发来的 kore.hello，回 ok 响应完成握手。
func handshakeWith(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(faultTimeout))
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读 kore.hello 失败: %v", err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码 kore.hello 失败: %v", err)
	}
	req, ok := env.(*pluginapi.Request)
	if !ok || req.Method != "kore.hello" {
		t.Fatalf("首帧不是 kore.hello: %T %+v", env, env)
	}
	response := &pluginapi.Response{
		Type:   pluginapi.TypeResponse,
		ID:     req.ID,
		ReqID:  req.ReqID,
		Result: json.RawMessage(`{"ok":true,"protocol":"` + pluginapi.Protocol + `"}`),
	}
	if err := pluginapi.WriteJSON(conn, response); err != nil {
		t.Fatalf("回 kore.hello 响应失败: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
}

// callMethod 发一个业务请求并读回响应（不在此处发 shutdown）。
func callMethod(t *testing.T, conn net.Conn, method string, params json.RawMessage, reqID string) *pluginapi.Response {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(faultTimeout))
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-" + reqID, ReqID: reqID,
		Method: method, Params: params,
	}
	if err := pluginapi.WriteJSON(conn, request); err != nil {
		t.Fatalf("写 %s 请求失败: %v", method, err)
	}
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读 %s 响应失败: %v", method, err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码 %s 响应失败: %v", method, err)
	}
	response, ok := env.(*pluginapi.Response)
	if !ok {
		t.Fatalf("%s 响应帧类型是 %T，不是 response", method, env)
	}
	_ = conn.SetDeadline(time.Time{})
	return response
}

// shutdownWithReply 发 kore.shutdown 并配对读回响应帧。
// net.Pipe 无缓冲且本测试用真实 UDS，不读回响应会让插件写阻塞在半途。
func shutdownWithReply(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(faultTimeout))
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-shutdown", ReqID: "r-shutdown",
		Method: "kore.shutdown",
	}
	if err := pluginapi.WriteJSON(conn, request); err != nil {
		return // 进程已故障时写失败属预期，不在此处 Fatal
	}
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		return // 同上：kill/timeout 场景下连接断开是预期
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		return
	}
	if response, ok := env.(*pluginapi.Response); ok && response.Error != nil {
		t.Logf("kore.shutdown 返回错误: %s", response.Error.Message)
	}
	_ = conn.SetDeadline(time.Time{})
}

// healthyConfDir 建一个可写临时 conf.d，返回路径。
func healthyConfDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conf.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建临时 conf.d 失败: %v", err)
	}
	return dir
}

// blockedConfDir 建一个"路径存在但不是目录"的 conf.d，制造真实 ENOTDIR 写盘失败。
func blockedConfDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("建阻塞路径失败: %v", err)
	}
	return path
}

// sanityHealthy 反证：正常握手 + site.create 成功 + shutdown 配帧。
// 每个 fault 文件都调用它，证明"故障断言不是永远失败的假通过"。
func sanityHealthy(t *testing.T, binary string) {
	t.Helper()
	cmd, accept := startPlugin(t, binary, healthyConfDir(t), 0)
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	response := callMethod(t, conn, "site.create", json.RawMessage(faultParams), "r-sanity")
	if response.Error != nil {
		t.Fatalf("sanity: 正常 site.create 意外报错: %s", response.Error.Message)
	}
	var created struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(response.Result, &created); err != nil || !created.Applied {
		t.Fatalf("sanity: site.create 未落盘: result=%s err=%v", response.Result, err)
	}
	shutdownWithReply(t, conn)
	_ = cmd.Process.Kill()
}

// isTimeoutOrClosed 判断错误是否为超时或连接关闭（二者都证明插件不再服务）。
func isTimeoutOrClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "closed") ||
		strings.Contains(msg, "broken pipe") || strings.Contains(msg, "reset") ||
		strings.Contains(msg, "eof")
}

// itoa 把正整数转十进制字符串（仅用于 ulimit 参数，避免 strconv import 扩散）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
