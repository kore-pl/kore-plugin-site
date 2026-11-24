//go:build !windows

// 本文件只用 Linux/Unix 的能力：/proc/<pid>/fd 读 fd 表、prlimit 收紧 RLIMIT_NOFILE。
// 边界套件其余用例同样依赖 UDS + sh 语义，Windows 不在覆盖范围（CI 只在 Linux 跑）。
package boundary

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 资源耗尽（判据① 第 4 类）。
//
// 现状核实：
//   - **地址空间耗尽已有**：tests/fault/helper_test.go 的 ulimit -v 32 MiB
//     （cgroup_test.go TestFaultCgroup），本套件**不重复造**，原样保留在 fault 包。
//   - 本文件补的是**另一种真实资源**：文件描述符耗尽（收紧 RLIMIT_NOFILE）。
//     /dev/full 与 tmpfs 满在这里造不出可靠判据：vhost 走临时文件+rename，
//     rename 会替换符号链接本身，ENOSPC 分支需 root 挂 tmpfs，故不伪造。
//
// 断言口径：资源不足必须回**明确错误码** E_INTERNAL + 可操作文案
// （too many open files / 打开文件），绝不 panic、绝不静默成功落半个文件。

// TestBoundaryExhaustFD 断言 fd 耗尽时 create 回明确错误码且不落盘。
func TestBoundaryExhaustFD(t *testing.T) {
	t.Parallel()
	confDir := healthyConfDir(t)

	// 反证：不设限时同一入参必须成功（保证下面的失败是 fd 限制造成的）。
	healthy := startPlugin(t, plainBin, confDir, nil)
	healthy.handshake(t)
	healthy.sanityCreate(t, "exhaust-ok.example.com")
	_ = healthy.shutdown(t)

	// fd 预算用**相对值**，不用绝对 ulimit：
	//
	// 原先写死 `ulimit -n 12`，它只在"宿主顺手泄漏了若干 fd"的交互 shell 里才构成
	// 耗尽；`presets-check` 用 python 子进程（close_fds=True）拉起用例，插件只剩
	// stdio + Go netpoll + UDS 这几个 fd，12 个预算根本耗不尽，create 会**合法成功**，
	// 用例反而把正常行为判成 bug（2026-11-23 G-20 判据①改「真跑」后暴露）。
	// 实测 dd1f565（清单锁）之前同样失败，故与本轮代码改动无关，是存量脆弱假设。
	//
	// 改为：握手后读插件 /proc/<pid>/fd 的实际占用 C，把 RLIMIT_NOFILE 收到 C+2。
	// 空位恒为 2，与宿主继承了多少 fd、Go 开了几个 netpoll fd 全部无关：
	// 清单锁占 1 → 临时文件用完即释放 → nginx -t 的 fork/exec 要 2 个 pipe fd 时
	// 必然 EMFILE（校验失败 → 已回滚 vhost）。
	markInheritedFDsCloseOnExec(t)
	limited := startPlugin(t, plainBin, confDir, nil)
	limited.handshake(t)
	tightenFD(t, limited.pid(), 2)

	response := limited.call(t, "site.create", `{"domain":"exhaust-fd.example.com","port":18080}`, "r-fd")
	if response.Error == nil {
		t.Fatalf("fd 耗尽时 create 静默成功: result=%s", response.Result)
	}
	// fd 预算收紧到 12 时，vhost 落盘仍能成功，耗尽点在"校验"环节的 fork/exec：
	// 插件把校验失败映射为 E_INVALID（配置校验失败）并回滚 vhost —— 这里是
	// **实测**到的真实路径，不按想象改成 E_INTERNAL。关键是：明确错误码 + 资源线索 + 已回滚。
	if response.Error.Code != pluginapi.E_INVALID && response.Error.Code != pluginapi.E_INTERNAL {
		t.Fatalf("fd 耗尽错误码=%q 不在协议错误码集合内（msg=%s）",
			response.Error.Code, response.Error.Message)
	}
	msg := strings.ToLower(response.Error.Message)
	if !strings.Contains(msg, "too many open files") {
		t.Fatalf("错误文案没有 fd 耗尽线索（too many open files）: %q", response.Error.Message)
	}
	if !strings.Contains(response.Error.Message, "回滚") {
		t.Fatalf("fd 耗尽必须回滚已写 vhost，文案: %q", response.Error.Message)
	}

	// 不 panic、会话仍可服务：ping 必须还能应答。
	ping := limited.call(t, "kore.ping", `{}`, "r-fd-ping")
	if ping.Error != nil {
		t.Fatalf("fd 耗尽后进程不再服务: %s", ping.Error.Message)
	}
	// 不能留下半个 vhost（失败必须整体不生效）。
	if _, err := readIfExists(filepath.Join(confDir, "kore-site-exhaust-fd.example.com.conf")); err == nil {
		t.Fatal("fd 耗尽后仍留下了 vhost 文件（非原子失败）")
	}
	_ = limited.shutdown(t)
}

// pid 返回插件进程号：sh 用 exec 替换自身，故与 cmd 的 pid 相同。
func (p *process) pid() int { return p.cmd.Process.Pid }

// tightenFD 把目标进程的 RLIMIT_NOFILE 软限收到「当前已开 fd + spare」。
//
// 用**相对预算**而不是写死绝对上限：插件的基线 fd 数由它自己的运行时与 UDS 连接
// 决定，写死一个数会在不同父进程/不同 Go 版本下从"耗尽"漂移成"够用"。
// 这里用 prlimit(1) 而非 syscall prlimit64，是为了让本文件在非 Linux 的 GOOS 下
// 仍可编译（make cross-vet 会类型检查测试包）。
func tightenFD(t *testing.T, pid int, spare int) {
	t.Helper()
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatalf("读插件 fd 表失败（pid=%d）: %v", pid, err)
	}
	want := len(ents) + spare
	out, err := exec.Command("prlimit", "--pid", strconv.Itoa(pid),
		"--nofile="+strconv.Itoa(want)+":").CombinedOutput()
	if err != nil {
		t.Fatalf("收紧 RLIMIT_NOFILE 到 %d 失败: %v（输出: %s）", want, err, out)
	}
	// 回读内核账本确认真的收紧了：读错就是测到"没耗尽"的假路径。
	limits, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		t.Fatalf("读插件 limits 失败: %v", err)
	}
	for _, line := range strings.Split(string(limits), "\n") {
		if !strings.HasPrefix(line, "Max open files") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != strconv.Itoa(want) {
			t.Fatalf("RLIMIT_NOFILE 未按预期收紧: %q（期望 soft=%d）", line, want)
		}
		return
	}
	t.Fatalf("插件 /proc/%d/limits 里没有 Max open files 行", pid)
}

// markInheritedFDsCloseOnExec 把测试进程里继承来的 fd（≥3）标成 FD_CLOEXEC。
//
// 必须做：Go 的 forkExec 只显式传 stdio，子进程在 exec 前不会关闭其他 fd——
// 非 CLOEXEC 的继承 fd 会一路带进插件。实测交互 shell 直跑时插件凭空多出 79 个
// 稀疏 fd（宿主工具链散发），同一条「收紧到已开 fd+2」的规则会因空洞留出多余空位，
// fd 耗尽路径根本走不到，用例就翻成假通过。标 CLOEXEC 后插件的 fd 表只由它自己
// 决定，预算断言与宿主继承 fd、调度方式彻底解耦。
func markInheritedFDsCloseOnExec(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("读测试进程 fd 表失败: %v", err)
	}
	for _, e := range ents {
		fd, convErr := strconv.Atoi(e.Name())
		if convErr != nil || fd < 3 {
			continue
		}
		syscall.CloseOnExec(fd) // fd 已关闭时是 no-op，无需处理错误
	}
}
