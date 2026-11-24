package boundary

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 非 root 下的降级（P0：0 条 root_actions 时建站不可用）。
//
// 真机造法：把 conf.d 指到一个**属主是 root、当前进程不可写**的目录（本机用 /etc）。
// 插件降权后以 kore-site 运行，vhost 写走内核受控写通道（ADR-0052）；
// 通道不可用时仍必须以 E_NEED_ROOT 降级（而不是 E_INTERNAL 或静默成功）。断言：
//
//  1. 错误码是 `E_NEED_ROOT`（不是 E_INTERNAL——用户需要知道"这是权限问题、要怎么办"）；
//  2. detail.hint 非空且给出可执行动作（chgrp/chmod/setfacl 或 root 启动 kore）；
//  3. **不 panic**：失败后进程仍能应答 kore.ping（会话循环未被带崩）；
//  4. **不落半个文件**：目标 vhost 与清单锁都不存在于该目录。
//
// root 运行时 DAC 拦不住任何写，本用例显式 Skip 并说明"测不了"，不假装通过。
// 本用例只覆盖写路径的降级；`nginx -t` / `-s reload` 的通道缺失由单元用例
// （rootaction_channel_test.go 的 TestNeedRootFromChannel）覆盖——那里注入假通道。

// TestBoundaryNeedRootWhenConfDirRootOwned 断言 conf.d 属 root 时返回 E_NEED_ROOT + hint。
func TestBoundaryNeedRootWhenConfDirRootOwned(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 不受 DAC 权限位约束，造不出 EACCES；本机需非 root 运行")
	}
	// 选一个属主 root、对当前用户不可写的既有目录：/etc 在常规发行版恒满足。
	confDir := "/etc"
	info, err := os.Stat(confDir)
	if err != nil {
		t.Skipf("环境无 %s，无法造 root 属主目录: %v", confDir, err)
	}
	if !info.IsDir() {
		t.Skipf("%s 不是目录", confDir)
	}
	if probeErr := os.WriteFile(filepath.Join(confDir, ".kore-site-probe"), []byte("x"), 0o600); probeErr == nil {
		_ = os.Remove(filepath.Join(confDir, ".kore-site-probe"))
		t.Skipf("%s 对当前用户可写（可能以 root 运行），测不了权限降级", confDir)
	}

	const domain = "needroot-boundary.example.com"
	vhostPath := filepath.Join(confDir, domain+".conf")
	lockPath := filepath.Join(confDir, "kore-site-manifest.json.lock")
	// 前置：目标文件本就不存在，避免把别人的文件误删/误判。
	for _, path := range []string{vhostPath, lockPath} {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("前置条件不成立：%s 已存在，换一个干净的属 root 目录", path)
		}
	}

	p := startPlugin(t, plainBin, confDir, nil)
	p.handshake(t)

	response := p.call(t, "site.create", `{"domain":"`+domain+`","port":18080}`, "r-needroot")
	if response.Error == nil {
		t.Fatalf("conf.d 属 root 不可写时 create 静默成功: result=%s", response.Result)
	}
	if response.Error.Code != pluginapi.E_NEED_ROOT {
		t.Fatalf("错误码=%q 期望 %q（msg=%s）",
			response.Error.Code, pluginapi.E_NEED_ROOT, response.Error.Message)
	}
	if !strings.Contains(response.Error.Message, "root") {
		t.Fatalf("错误文案未说明是 root 权限问题: %q", response.Error.Message)
	}
	var detail struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(response.Error.Detail, &detail); err != nil || detail.Hint == "" {
		t.Fatalf("E_NEED_ROOT 缺少 detail.hint（用户不知道下一步该做什么）: detail=%s err=%v",
			response.Error.Detail, err)
	}
	// ADR-0052 否决目录授权：hint 必须指向「以 root 启动 Kore / 受控写通道」，
	// 且**不得**再建议 chgrp/chmod/setfacl（access_log 注入 = root RCE）。
	for _, banned := range []string{"chgrp", "chmod", "setfacl"} {
		if strings.Contains(detail.Hint, banned) {
			t.Fatalf("hint 仍在建议目录授权 %q（ADR-0052 已否决）: %q", banned, detail.Hint)
		}
	}
	if !strings.Contains(detail.Hint, "受控写") && !strings.Contains(detail.Hint, "root") {
		t.Fatalf("hint 未给出可执行出路（受控写 / root 启动）: %q", detail.Hint)
	}

	// 把实测响应打进 CI 日志：这条错误就是"非 root 下正确报错"的证据原文。
	t.Logf("E_NEED_ROOT 实测: code=%s message=%s hint=%s",
		response.Error.Code, response.Error.Message, detail.Hint)

	// 不 panic、不半死：失败后仍按协议应答。
	if ping := p.call(t, "kore.ping", `{}`, "r-needroot-ping"); ping.Error != nil {
		t.Fatalf("E_NEED_ROOT 后进程不再服务 kore.ping: %s", ping.Error.Message)
	}
	// 不落半个文件：失败必须是"什么都没写"，不是"写了一半"。
	for _, path := range []string{vhostPath, lockPath} {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("失败路径落下了文件（半成品）: %s", path)
		}
	}
	// 临时文件也不许残留。
	if matches, _ := filepath.Glob(filepath.Join(confDir, ".kore-site-*.tmp")); len(matches) > 0 {
		t.Fatalf("失败路径残留临时文件: %v", matches)
	}

	_ = p.shutdown(t)
}
