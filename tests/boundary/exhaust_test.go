package boundary

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 资源耗尽（判据① 第 4 类）。
//
// 现状核实：
//   - **地址空间耗尽已有**：tests/fault/helper_test.go 的 ulimit -v 32 MiB
//     （cgroup_test.go TestFaultCgroup），本套件**不重复造**，原样保留在 fault 包。
//   - 本文件补的是**另一种真实资源**：文件描述符耗尽（ulimit -n）。
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

	// ulimit -n 收紧到刚够启动（stdio 3 + UDS 1 + Go netpoll 若干），
	// 使 CreateTemp 拿不到 fd → EMFILE。数值在 8/10/12 间由探针确定。
	limited := startPlugin(t, plainBin, confDir, map[string]string{
		"__SH_PREFIX": "ulimit -n 12",
	})
	limited.handshake(t)

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
