package boundary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 权限不足（判据① 第 3 类）。
//
// 现状核实：仓内此前**没有**权限类用例（grep chmod/000/EACCES 只命中 traffic.go 的
// 日志解析分支，不是权限注入），plog 与 ulimit 也都不覆盖"目标目录不可写"。
//
// 造真实无权限场景的方式：把 conf.d 目录 chmod 0000。测试进程是普通用户（非 root，
// 无 CAP_DAC_OVERRIDE），插件子进程同样非 root，因此 CreateTemp/WriteFile 必定 EACCES。
// 若是 root 运行，chmod 000 不构成权限壁垒，用例会显式 Skip 并说明原因——
// 这是唯一允许的 Skip：它证明的是"测不了"，不是"不想测"。
//
// 断言：明确错误码 E_INTERNAL + 文案含可操作的权限线索；不 panic（进程活着、
// 会话循环继续可服务）、不静默成功、不落半个文件。

// TestBoundaryPermissionConfDirDenied 断言 conf.d 不可写时返回明确错误码而非 panic/静默成功。
func TestBoundaryPermissionConfDirDenied(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 不受 DAC 权限位约束，chmod 0000 造不出 EACCES；本机需非 root 运行")
	}
	confDir := healthyConfDir(t)
	p := startPlugin(t, plainBin, confDir, nil)
	p.handshake(t)
	// 反证：权限还没收紧时写入是成功的。
	p.sanityCreate(t, "perm-ok.example.com")

	if err := os.Chmod(confDir, 0o000); err != nil {
		t.Fatalf("chmod conf.d 0000 失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(confDir, 0o755) })

	response := p.call(t, "site.create", `{"domain":"perm-denied.example.com","port":18080}`, "r-perm")
	if response.Error == nil {
		t.Fatalf("conf.d 不可写时 create 静默成功: result=%s", response.Result)
	}
	if response.Error.Code != pluginapi.E_INTERNAL {
		t.Fatalf("权限失败错误码=%q 期望 %q（msg=%s）", response.Error.Code, pluginapi.E_INTERNAL, response.Error.Message)
	}
	if !strings.Contains(strings.ToLower(response.Error.Message), "permission") &&
		!strings.Contains(response.Error.Message, "权限") {
		t.Fatalf("错误文案缺少可操作的权限线索: %q", response.Error.Message)
	}

	// 不 panic、不半死：进程必须仍然活着并继续按协议应答（会话循环可服务）。
	ping := p.call(t, "kore.ping", `{}`, "r-perm-ping")
	if ping.Error != nil {
		t.Fatalf("权限失败后进程不再服务 kore.ping: %s", ping.Error.Message)
	}
	// 权限恢复后必须能继续写入（不是"一次失败就永久坏掉"）。
	if err := os.Chmod(confDir, 0o755); err != nil {
		t.Fatalf("恢复 conf.d 权限失败: %v", err)
	}
	recovered := p.call(t, "site.create", `{"domain":"perm-recovered.example.com","port":18080}`, "r-perm-recover")
	if recovered.Error != nil {
		t.Fatalf("权限恢复后 create 仍失败: code=%s msg=%s", recovered.Error.Code, recovered.Error.Message)
	}

	_ = p.shutdown(t)
	p.assertNoDataRace(t, "permission-confdir")
}

// TestBoundaryPermissionSiteRootUnreadable 断言站点根不可读时 status 给出
// **unknown + 可操作 reason**，而不是把它误报成 absent（ADR-0023 §3.5 #6）。
//
// 造法：chmod 0000 站点根的父目录 → os.Stat(root) 返回 EACCES（非 ENOENT）。
func TestBoundaryPermissionSiteRootUnreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 不受 DAC 权限位约束，chmod 0000 造不出 EACCES；本机需非 root 运行")
	}
	confDir := healthyConfDir(t)
	parent := filepath.Join(t.TempDir(), "site-parent")
	root := filepath.Join(parent, "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根失败: %v", err)
	}

	p := startPlugin(t, plainBin, confDir, nil)
	p.handshake(t)
	created := p.call(t, "site.create", `{"domain":"perm-root.example.com","root":"`+root+`","port":18080}`, "r-root-create")
	if created.Error != nil {
		t.Fatalf("建站失败: code=%s msg=%s", created.Error.Code, created.Error.Message)
	}

	// 先证明正常状态下是 present（反证：下面的 unknown 不是"永远 unknown"）。
	before := p.call(t, "site.status", `{"domain":"perm-root.example.com"}`, "r-root-status-ok")
	if state := siteDirStateOf(t, before.Result); state != "present" {
		t.Fatalf("权限收紧前 site_dir_state=%q 期望 present", state)
	}

	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod 站点根父目录 0000 失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	after := p.call(t, "site.status", `{"domain":"perm-root.example.com"}`, "r-root-status-denied")
	if after.Error != nil {
		t.Fatalf("status 不应因站点根不可读而整体报错: code=%s msg=%s", after.Error.Code, after.Error.Message)
	}
	var payload struct {
		SiteDirState  string `json:"site_dir_state"`
		SiteDirReason string `json:"site_dir_reason"`
	}
	if err := unmarshalResult(after.Result, &payload); err != nil {
		t.Fatalf("解析 status 结果失败: %v result=%s", err, after.Result)
	}
	if payload.SiteDirState == "absent" {
		t.Fatalf("权限不足被误报成 absent（应为 unknown）: reason=%q", payload.SiteDirReason)
	}
	if payload.SiteDirState != "unknown" {
		t.Fatalf("site_dir_state=%q 期望 unknown", payload.SiteDirState)
	}
	if strings.TrimSpace(payload.SiteDirReason) == "" {
		t.Fatal("unknown 必须带可操作 reason（G-16 不静默降级），实际为空")
	}
	_ = p.shutdown(t)
}
