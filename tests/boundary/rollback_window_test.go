package boundary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestRollbackDoesNotClobberConcurrentWriter 覆盖 R6-3：apply 在 nginx -t 失败窗口内的
// 回滚不得覆盖同期并发写者对共享清单的改动。
//
// 时序（确定性，不靠 goroutine 调度运气）：
//  1. A 进程用可控的 nginx -t 桩：touch ENTERED → 等 RELEASE → exit 1（强制触发回滚）；
//  2. 等到 ENTERED——此时 A 已把域名 A 写进清单、且已释放清单锁，正卡在配置校验里，
//     这正是「回滚窗口」；
//  3. B 进程并发创建域名 B（校验用 true，成功写入清单 {A,B}）；
//  4. 放 RELEASE，A 的校验失败触发回滚；
//  5. 断言：回滚后清单仍含 B（且已移除 A），B 的 vhost 仍在。
//
// 修复前：A 用锁外整文件快照回滚，把清单恢复成「既没有 A 也没有 B」的旧值（本例里
// 快照时清单还不存在 → 直接删除清单文件）→ B 丢失 → FAIL。
// 修复后：A 用持锁采集的本域名快照做定向撤销，只删 A 的条目 → B 保留 → PASS。
func TestRollbackDoesNotClobberConcurrentWriter(t *testing.T) {
	confDir := healthyConfDir(t)
	tmp := t.TempDir()
	entered := filepath.Join(tmp, "entered")
	release := filepath.Join(tmp, "release")
	script := filepath.Join(tmp, "nginx-test-block.sh")

	// 可控校验桩：先落 ENTERED 告知测试「已进入回滚窗口」，再阻塞等 RELEASE，最后退出 1。
	// 追加的 -t 参数由 argv(fixed...) 传入，脚本忽略它。
	body := fmt.Sprintf("#!/bin/sh\ntouch %q\nwhile [ ! -f %q ]; do sleep 0.02; done\nexit 1\n",
		entered, release)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写 nginx -t 桩失败: %v", err)
	}

	domainA := "rollback-a.example.com"
	domainB := "rollback-b.example.com"

	// A：nginx -t 走阻塞桩。
	a, cleanupA, err := startPluginNoFatal(raceBin, confDir, map[string]string{
		"KORE_NGINX_TEST_CMD": script,
	})
	if err != nil {
		t.Fatalf("拉起 A 失败: %v", err)
	}
	defer cleanupA()
	if err := a.handshakeErr(); err != nil {
		t.Fatalf("A 握手失败: %v", err)
	}

	type callResult struct {
		resp *pluginapi.Response
		err  error
	}
	resultCh := make(chan callResult, 1)
	go func() {
		resp, callErr := a.callErr("site.create",
			`{"domain":"`+domainA+`","port":18080}`, "r-rollback-a")
		resultCh <- callResult{resp: resp, err: callErr}
	}()

	// 等 A 进入校验桩——这一刻 A 已落盘、已释放清单锁，正处于回滚窗口。
	waitForFile(t, entered, 15*time.Second)

	// B：并发写者，校验/重载用 true，正常把域名 B 写进共享清单。
	b, cleanupB, err := startPluginNoFatal(raceBin, confDir, nil)
	if err != nil {
		t.Fatalf("拉起 B 失败: %v", err)
	}
	defer cleanupB()
	if err := b.handshakeErr(); err != nil {
		t.Fatalf("B 握手失败: %v", err)
	}
	respB, err := b.callErr("site.create",
		`{"domain":"`+domainB+`","port":18080}`, "r-rollback-b")
	if err != nil {
		t.Fatalf("B 请求失败: %v", err)
	}
	if respB.Error != nil {
		t.Fatalf("B 创建失败: code=%s msg=%s", respB.Error.Code, respB.Error.Message)
	}

	// 放 A 的校验桩返回失败 → A 触发回滚。
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatalf("放行 A 校验桩失败: %v", err)
	}

	var got callResult
	select {
	case got = <-resultCh:
	case <-time.After(15 * time.Second):
		t.Fatal("A 未在 15s 内返回")
	}
	if got.err != nil {
		t.Fatalf("A 请求失败: %v", got.err)
	}
	if got.resp == nil || got.resp.Error == nil {
		t.Fatalf("A 的 create 应在 nginx -t 失败后返回错误，实际 resp=%+v", got.resp)
	}

	// —— 断言：B 必须活着，A 必须被自己撤销 ——
	manifestPath := filepath.Join(confDir, manifestFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("回滚覆盖了并发写者 B：清单文件被删/读不到（%v）。修复前应在此 FAIL", err)
	}
	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("清单不是合法 JSON: %v\n清单=%s", err, raw)
	}
	if _, ok := doc.Sites[domainB]; !ok {
		t.Fatalf("回滚覆盖了并发写者 B：清单中已无 %s（锁外旧快照回滚）。清单=%s\n修复前应在此 FAIL",
			domainB, raw)
	}
	if _, ok := doc.Sites[domainA]; ok {
		t.Fatalf("A 的回滚未清除自身条目 %s（定向撤销失效）。清单=%s", domainA, raw)
	}
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-"+domainB+".conf")); err != nil {
		t.Fatalf("回滚误删了 B 的 vhost: %v", err)
	}

	_ = b.shutdownErr()
	_ = a.shutdownErr()
	a.assertNoDataRace(t, "rollback-window-a")
	b.assertNoDataRace(t, "rollback-window-b")
	t.Logf("回滚窗口内并发写者 B 存活，A 自身条目已定向撤销，清单=%s", raw)
}

// waitForFile 轮询等待文件出现，避免用 sleep 拼运气。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待文件 %s 出现超时（%s）", path, timeout)
}
