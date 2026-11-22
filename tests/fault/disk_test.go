// Package fault 的 disk 用例：conf.d 路径不可写/不是目录，site.create 落盘失败。
//
// 观测语义：写盘失败是插件内部执行错误，site.create 返回 E_INTERNAL
// （"内核自身错误/写库失败/序列化失败"类）；错误必须结构化（code+message），
// 且不能 panic、不能返回 applied=true 的假成功。
// sanity 反证：可写 conf.d 下 site.create 成功。
package fault

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestFaultDisk 把 KORE_SITE_CONF_DIR 指向一个普通文件（ENOTDIR），
// 断言 site.create 返回 E_INTERNAL 且带写盘失败语义。
func TestFaultDisk(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	blocked := blockedConfDir(t)
	cmd, accept := startPlugin(t, binary, blocked, 0)
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	response := callMethod(t, conn, "site.create", json.RawMessage(faultParams), "r-disk")
	if response.Error == nil {
		t.Fatalf("磁盘故障下 site.create 不应成功: result=%s", response.Result)
	}
	if response.Error.Code != pluginapi.E_INTERNAL {
		t.Fatalf("错误码=%q，期望 E_INTERNAL（message=%q）", response.Error.Code, response.Error.Message)
	}
	if !strings.Contains(strings.ToLower(response.Error.Message), "conf.d") &&
		!strings.Contains(strings.ToLower(response.Error.Message), "目录") &&
		!strings.Contains(strings.ToLower(response.Error.Message), "conf") {
		t.Fatalf("错误信息 %q 未含 conf.d 写盘失败语义", response.Error.Message)
	}
	if len(response.Result) != 0 && string(response.Result) != "null" {
		t.Fatalf("故障下不应带结果载荷: %s", response.Result)
	}
	shutdownWithReply(t, conn)
	_ = cmd.Process.Kill()
	t.Logf("disk 故障观测: site.create 返回 %s/%s", response.Error.Code, response.Error.Message)
}

// TestFaultDiskNoPanic 确认 ENOTDIR 不会让插件进程崩溃（仍可处理 shutdown）。
func TestFaultDiskNoPanic(t *testing.T) {
	binary := findBinary(t)
	cmd, accept := startPlugin(t, binary, blockedConfDir(t), 0)
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)

	_ = callMethod(t, conn, "site.create", json.RawMessage(faultParams), "r-disk-panic")
	// 还能正常应答 kore.shutdown → 进程没被磁盘故障打崩。
	reply := callMethod(t, conn, "kore.shutdown", nil, "r-shutdown-check")
	if reply.Error != nil {
		t.Fatalf("磁盘故障后插件不应崩，shutdown 报错: %s", reply.Error.Message)
	}
	_ = cmd.Process.Kill()
}
