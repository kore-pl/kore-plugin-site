// Package fault 的 kill 用例：SIGKILL 插件进程，验证方法面不再有响应。
//
// 观测语义：进程崩溃后内核侧收到连接关闭/读超时，对应协议 E_UPSTREAM
// （"插件进程无响应/崩溃/连接中断"）。断言：进程确实被 SIGKILL、连接读不到
// 响应帧；sanity 反证证明正常握手下同一二进制能完成 site.create。
package fault

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestFaultKill 真起 bin/site → 握手 → SIGKILL → 断言进程被信号终止且连接断。
func TestFaultKill(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	cmd, accept := startPlugin(t, binary, healthyConfDir(t), 0)
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("发送 SIGKILL 失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), faultTimeout)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("被 SIGKILL 的进程不应正常退出")
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("退出错误类型=%T，期望 *exec.ExitError", err)
		}
		if code := exitErr.ExitCode(); code != -1 && code != 137 {
			t.Fatalf("退出码=%d，期望信号终止（-1/137）", code)
		}
	case <-ctx.Done():
		t.Fatal("SIGKILL 后进程未在预算内退出")
	}

	// 进程死后，向已建立的 UDS 发 site.create 不应拿到响应帧。
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-killed", ReqID: "r-killed",
		Method: "site.create", Params: []byte(faultParams),
	}
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	writeErr := pluginapi.WriteJSON(conn, request)
	_ = writeErr // 写失败是 kill 后的预期表现，关键是下面读不到响应
	_, readErr := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if readErr == nil {
		t.Fatal("插件被 kill 后仍读到响应帧，违背 E_UPSTREAM 故障隔离预期")
	}
	if !isTimeoutOrClosed(readErr) {
		t.Fatalf("读错误类型异常（非超时/关闭）: %v", readErr)
	}
	t.Logf("kill 故障观测: 进程 SIGKILL 生效，UDS 读错误=%v → E_UPSTREAM 语义", readErr)
}
