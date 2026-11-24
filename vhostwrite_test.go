package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是 K4（ADR-0052）在插件侧的**行为证据**：写 vhost 必须走内核受控写
// （file_write），不再由插件进程自己 os.CreateTemp + rename；非 root 的降级口径
// 必须改成「以 root 启动内核 / 受控写通道」而不是「给 conf.d 目录授权」。

// TestCreateWritesThroughChannel 断言 create 调的是受控写通道（file_write 语义），
// 且**没有**在本地落盘——若退回 os.CreateTemp/rename，本用例立刻红。
func TestCreateWritesThroughChannel(t *testing.T) {
	dir := useTempConfDir(t)
	var (
		calls      int
		gotDomain  string
		gotContent []byte
	)
	store := &vhostStore{dir: dir, writer: func(domain string, content []byte) error {
		calls++
		gotDomain = domain
		gotContent = append([]byte(nil), content...)
		return nil
	}}
	path, _, created, perr := store.create(siteParams{Domain: "chan.example.com", Port: 18081}, nil)
	if perr != nil {
		t.Fatalf("create 走通道失败: %v", perr)
	}
	if calls != 1 {
		t.Fatalf("受控写通道被调用 %d 次，期望 1 次（不能退回本地写）", calls)
	}
	if gotDomain != "chan.example.com" {
		t.Fatalf("通道 domain=%q", gotDomain)
	}
	if !strings.Contains(string(gotContent), "server_name chan.example.com") {
		t.Fatalf("通道正文不是渲染后的 vhost: %q", string(gotContent))
	}
	if !created {
		t.Fatalf("首次创建应 created=true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("走通道时插件不应在本地落盘（%s）", path)
	}
}

// TestCreateNeedRootHintFromChannel 断言非 root 降级：内核回 E_NEED_ROOT 时
// 插件升级成同码 + 可操作 hint，且 hint **不再提** chgrp/chmod/setfacl（ADR-0052 已否决目录授权）。
func TestCreateNeedRootHintFromChannel(t *testing.T) {
	dir := useTempConfDir(t)
	store := &vhostStore{dir: dir, writer: func(string, []byte) error {
		return &rootActionFailure{
			Code:    pluginapi.E_NEED_ROOT,
			Message: "当前进程不是 root，无法执行特权动作",
			Hint:    "以 root 启动 Kore",
		}
	}}
	_, _, _, perr := store.create(siteParams{Domain: "needroot.example.com", Port: 18082}, nil)
	if perr == nil {
		t.Fatalf("通道回 E_NEED_ROOT 时 create 静默成功")
	}
	if perr.Code != pluginapi.E_NEED_ROOT {
		t.Fatalf("错误码=%q 期望 %q（msg=%s）", perr.Code, pluginapi.E_NEED_ROOT, perr.Message)
	}
	var detail struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(perr.Detail, &detail); err != nil || detail.Hint == "" {
		t.Fatalf("缺少可操作 hint: detail=%s err=%v", perr.Detail, err)
	}
	for _, banned := range []string{"chgrp", "chmod", "setfacl"} {
		if strings.Contains(detail.Hint, banned) {
			t.Fatalf("hint 仍在建议目录授权 %q（ADR-0052 已否决）: %s", banned, detail.Hint)
		}
	}
	if !strings.Contains(detail.Hint, "受控写") && !strings.Contains(detail.Hint, "root") {
		t.Fatalf("hint 未说明正确出路（受控写 / root 启动）: %s", detail.Hint)
	}
}

// TestManifestMovesToStateDir 断言生产形态下清单落 KORE_STATE_DIR：
// conf.d 由 root 持有 0755 时，清单只能写进插件私有状态目录，否则 create 必失败。
func TestManifestMovesToStateDir(t *testing.T) {
	confDir := useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)

	store := newVHostStore()
	if _, _, _, perr := store.create(siteParams{Domain: "state.example.com", Port: 18083}, nil); perr != nil {
		t.Fatalf("create 失败: %v", perr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, manifestName)); err != nil {
		t.Fatalf("清单未落在 KORE_STATE_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(confDir, manifestName)); !os.IsNotExist(err) {
		t.Fatalf("清单不应再写进 conf.d（生产目录对插件不可写）: %v", err)
	}
	if _, err := os.Stat(filepath.Join(confDir, vhostFileName("state.example.com"))); err != nil {
		t.Fatalf("vhost 应落在 conf.d: %v", err)
	}
}
