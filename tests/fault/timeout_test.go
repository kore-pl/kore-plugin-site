// Package fault 的 timeout 用例：内核接受连接后不回 kore.hello 响应。
//
// 观测语义：插件握手等待 2s 预算超时后以退出码 1 退出并停止服务，
// 对应内核调用方收到 E_UPSTREAM（无响应）。sanity 反证证明正常握手下不退出。
package fault

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestFaultTimeout 真测插件握手超时行为：不永久挂起、预算内退出码 1。
func TestFaultTimeout(t *testing.T) {
	binary := findBinary(t)

	// 对照组：正常握手后插件应保持运行（排除"启动即失败"的假通过）。
	okCmd, okAccept := startPlugin(t, binary, healthyConfDir(t), 0)
	okConn := okAccept()
	handshakeWith(t, okConn)
	if okCmd.ProcessState != nil {
		t.Fatal("对照组：正常握手后插件不应退出")
	}
	shutdownWithReply(t, okConn) // 配对读帧，不留悬挂
	_ = okConn.Close()
	_ = okCmd.Process.Kill()
	_ = okCmd.Wait()

	// 实验组：接受连接但不回握手响应，插件必须在 helloTimeoutLimit 内超时退出。
	cmd, accept := startPlugin(t, binary, healthyConfDir(t), 0)
	conn := accept()
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(helloTimeoutLimit))

	waitCtx, waitCancel := context.WithTimeout(context.Background(), helloTimeoutLimit)
	defer waitCancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("握手超时后插件不应正常退出")
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("退出错误类型=%T，期望 *exec.ExitError", err)
		}
		if code := exitErr.ExitCode(); code != 1 {
			t.Fatalf("握手超时退出码=%d，期望 1（E_UPSTREAM 语义）", code)
		}
	case <-waitCtx.Done():
		_ = cmd.Process.Kill()
		t.Fatalf("插件握手超时后 %v 内未退出（疑似永久挂起）", helloTimeoutLimit)
	}
	t.Log("timeout 故障观测: 无 hello 响应时插件 2s 内退出码 1 → E_UPSTREAM 语义")
}
