// Package fault 的 cgroup 用例：内存地址空间受限（等价 cgroup 内存超限的进程侧表现）。
//
// 观测语义：地址空间不足时插件无法完成服务，内核侧观测为进程退出/连接中断，
// 对应 E_UPSTREAM（"插件进程无响应/崩溃/连接中断"）。
// 不依赖 root 权限、不写 /sys/fs/cgroup——用 ulimit -v 在子进程施加限制。
// sanity 反证：无限制时同一二进制能完成 site.create。
package fault

import (
	"context"
	"os/exec"
	"testing"
)

// cgroupMemLimitKB 是受限地址空间上限（32 MiB）。Go 运行时本身要几百 MiB
// 虚拟地址空间，此限制足以让进程无法正常运行，但不会触碰真实 cgroup 文件系统。
const cgroupMemLimitKB = 32 * 1024

// TestFaultCgroup 在 ulimit -v 限制下拉起插件，断言进程在预算内失败退出，
// 不永久挂起；同时 sanity 对照证明无限制时二进制正常。
func TestFaultCgroup(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	// 受限进程：地址空间不足，启动/握手阶段必然失败并在预算内退出。
	cmd, accept := startPlugin(t, binary, healthyConfDir(t), cgroupMemLimitKB)
	_ = accept // 受限进程可能根本不 accept；不强求连接，观测点是进程退出
	defer func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), faultTimeout)
	defer waitCancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("地址空间受限的插件不应正常退出")
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Logf("cgroup 故障观测: 受限进程退出码=%d → E_UPSTREAM 语义", exitErr.ExitCode())
		} else {
			t.Logf("cgroup 故障观测: 受限进程退出 err=%v（非 exec.ExitError）", err)
		}
	case <-waitCtx.Done():
		_ = cmd.Process.Kill()
		t.Fatalf("地址空间受限后 %v 内未退出（疑似永久挂起）", faultTimeout)
	}
}

// TestFaultCgroupNoHang 验证受限进程不悬挂后立即重启一个无限制进程仍可服务
// （资源限制不会污染后续实例），这是生产级故障恢复的最低要求。
func TestFaultCgroupNoHang(t *testing.T) {
	binary := findBinary(t)
	cmd, _ := startPlugin(t, binary, healthyConfDir(t), cgroupMemLimitKB)
	ctx, cancel := context.WithTimeout(context.Background(), faultTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		t.Fatal("受限进程未能在预算内退出")
	}
	// 恢复验证：无限制实例正常握手 + site.create。
	sanityHealthy(t, binary)
}
