package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是 ADR-0053 在插件侧的**行为证据**：删站/停用/启用不再由插件进程
// os.Remove / os.Rename 直接动 conf.d（装 root 后 kore-site 对 conf.d 没有目录
// 写权限，三条路径全 DAC 失败），而是：
//
//  1. 删站 → 内核受控删除 file_delete（不是 os.Remove）；
//  2. 停用 → 内容备份到插件自有目录（KORE_STATE_DIR）+ 受控删除，conf.d 不留 .disabled 孤儿；
//  3. 启用 → 从插件自有目录读回 + 受控写 file_write 写回，成功后删备份；
//  4. 非 root → E_NEED_ROOT + 可操作 hint（不是 E_INTERNAL，也不是静默成功）。

// startFakeKernel 在临时目录起一个只服务本用例的 UDS HTTP 内核替身。
func startFakeKernel(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "kore.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听 UDS: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return sock
}

// TestDeleteVHostWireShape 断言删除请求的**线上形状**：action_id=vhost-delete、
// 只带单层文件名 kore-site-<domain>.conf、不带 content。
//
// 这三条任何一条变了，内核都会拒绝或删错对象——所以它是协议层的硬断言，
// 不是实现细节。
func TestDeleteVHostWireShape(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		got     struct {
			ActionID string   `json:"action_id"`
			Args     []string `json:"args"`
			Content  *string  `json:"content"`
		}
	)
	sock := startFakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"executed":true,"exit_code":0}`)
	})
	t.Setenv("KORE_KERNEL_SOCK", sock)
	t.Setenv("KORE_PLUGIN_TOKEN", "tok-delete")
	t.Setenv("KORE_PLUGIN_ID", "site")

	deleteVHost := vhostDeleteFuncFromEnv()
	if deleteVHost == nil {
		t.Fatalf("通道三要素齐备时受控删除调用面不应为 nil")
	}
	if err := deleteVHost("wire.example.com"); err != nil {
		t.Fatalf("受控删除调用失败: %v", err)
	}
	if gotPath != "/api/v1/plugins/site/root-action" {
		t.Fatalf("请求路径=%q", gotPath)
	}
	if gotAuth != "Bearer tok-delete" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if got.ActionID != rootActionVHostDelete {
		t.Fatalf("action_id=%q 期望 %q", got.ActionID, rootActionVHostDelete)
	}
	if len(got.Args) != 1 || got.Args[0] != "kore-site-wire.example.com.conf" {
		t.Fatalf("args=%v 期望单层文件名 kore-site-wire.example.com.conf", got.Args)
	}
	if got.Content != nil {
		t.Fatalf("删除请求不得携带 content（内核会以 E_INVALID 拒绝）: %q", *got.Content)
	}
}

// TestDeleteGoesThroughDeleteChannel 断言 deleteWithBackup 调的是受控删除通道，
// **没有**在本地 os.Remove——若退回目录内删除，本用例立刻红。
func TestDeleteGoesThroughDeleteChannel(t *testing.T) {
	dir := useTempConfDir(t)
	store := &vhostStore{dir: dir, writer: func(domain string, content []byte) error {
		return os.WriteFile(filepath.Join(dir, vhostFileName(domain)), content, 0o644)
	}}
	path, _, _, perr := store.create(siteParams{Domain: "chan-del.example.com", Port: 18091}, nil)
	if perr != nil {
		t.Fatalf("建站失败: %v", perr)
	}

	var deleted []string
	store.deleter = func(domain string) error {
		deleted = append(deleted, domain)
		return nil
	}
	gotPath, backup, wasDeleted, perr := store.deleteWithBackup("chan-del.example.com")
	if perr != nil {
		t.Fatalf("走受控删除失败: %v", perr)
	}
	if gotPath != path || !wasDeleted {
		t.Fatalf("返回值不对: path=%q deleted=%v", gotPath, wasDeleted)
	}
	if len(deleted) != 1 || deleted[0] != "chan-del.example.com" {
		t.Fatalf("受控删除通道调用=%v，期望恰好一次且 domain 正确", deleted)
	}
	if backup != "" {
		t.Fatalf("走受控删除时备份由内核强制完成，插件不应再写第二份: %q", backup)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("通道替身没动磁盘时文件应仍在——插件自己 os.Remove 了（%v）", err)
	}
}

// TestDeleteNeedRootFromChannel 断言非 root 降级：内核回 E_NEED_ROOT 时
// 插件升级成同码 + 受控删除口径 hint，而不是 E_INTERNAL。
func TestDeleteNeedRootFromChannel(t *testing.T) {
	dir := useTempConfDir(t)
	store := &vhostStore{dir: dir}
	if _, _, _, perr := store.create(siteParams{Domain: "needroot-del.example.com", Port: 18092}, nil); perr != nil {
		t.Fatalf("建站失败: %v", perr)
	}
	store.deleter = func(string) error {
		return &rootActionFailure{
			Code:    pluginapi.E_NEED_ROOT,
			Message: "需要 root 权限：内核需以系统服务形态运行",
			Hint:    "按 ADR-0042 以系统服务形态运行内核",
		}
	}
	_, _, _, perr := store.deleteWithBackup("needroot-del.example.com")
	if perr == nil {
		t.Fatalf("通道回 E_NEED_ROOT 时删除静默成功")
	}
	if perr.Code != pluginapi.E_NEED_ROOT {
		t.Fatalf("错误码=%q 期望 %q（message=%s）", perr.Code, pluginapi.E_NEED_ROOT, perr.Message)
	}
	var detail struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(perr.Detail, &detail); err != nil || detail.Hint == "" {
		t.Fatalf("缺少可操作 hint: detail=%s err=%v", perr.Detail, err)
	}
	if !strings.Contains(detail.Hint, "受控删除") || !strings.Contains(detail.Hint, "root") {
		t.Fatalf("hint 未说明正确出路（受控删除 / root 启动）: %s", detail.Hint)
	}
	if strings.Contains(detail.Hint, "chmod") || strings.Contains(detail.Hint, "chown") {
		t.Fatalf("hint 仍在建议给目录授权: %s", detail.Hint)
	}
}

// TestDisableBacksUpToPluginDirThenEnableRestores 是停用/启用迁移的核心行为断言：
// 停用后 conf.d 里片段消失、插件自有目录有逐字节一致的备份、conf.d 不留 .disabled 孤儿；
// 启用后片段原样写回、备份消失。写/删都经注入的通道替身，等价于内核受控写/删。
func TestDisableBacksUpToPluginDirThenEnableRestores(t *testing.T) {
	confDir := useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)

	fakeWriter := func(domain string, content []byte) error {
		return os.WriteFile(filepath.Join(confDir, vhostFileName(domain)), content, 0o644)
	}
	fakeDeleter := func(domain string) error {
		return os.Remove(filepath.Join(confDir, vhostFileName(domain)))
	}
	store := &vhostStore{dir: confDir, writer: fakeWriter, deleter: fakeDeleter}
	svc := &siteService{store: store, runner: hostRunnerWithChannel((&fakeChannel{}).call)}

	domain := "migrate.example.com"
	path, _, _, perr := store.create(siteParams{Domain: domain, Port: 18093}, nil)
	if perr != nil {
		t.Fatalf("建站失败: %v", perr)
	}
	before := readVHost(t, path)

	disabled, perr := svc.toggle(domain, false)
	if perr != nil {
		t.Fatalf("停用失败: %v", perr)
	}
	if !disabled.Changed || !disabled.Applied || disabled.Enabled {
		t.Fatalf("停用结果不对: %+v", disabled)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("停用后 conf.d 片段应消失（受控删除）: %v", err)
	}
	backupPath := filepath.Join(stateDir, vhostFileName(domain)+disabledSuffix)
	if readVHost(t, backupPath) != before {
		t.Fatalf("停用后插件自有目录的备份应与原配置逐字节一致")
	}
	if _, err := os.Stat(path + disabledSuffix); !os.IsNotExist(err) {
		t.Fatalf("conf.d 里不应再留 .conf.disabled 孤儿: %v", err)
	}
	if disabled.DisabledPath != backupPath {
		t.Fatalf("DisabledPath=%q 期望插件自有目录备份 %q", disabled.DisabledPath, backupPath)
	}

	enabled, _ := svc.toggle(domain, true)
	if !enabled.Changed || !enabled.Applied || !enabled.Enabled {
		t.Fatalf("启用结果不对: %+v", enabled)
	}
	if readVHost(t, path) != before {
		t.Fatalf("启用后 conf.d 片段应与停用前逐字节一致")
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("启用后备份应消失: %v", err)
	}

	// 重复停用仍要幂等短路（不写盘、不跑 nginx）。
	if _, perr := svc.toggle(domain, false); perr != nil {
		t.Fatalf("再次停用失败: %v", perr)
	}
	again, perr := svc.toggle(domain, false)
	if perr != nil {
		t.Fatalf("重复停用失败: %v", perr)
	}
	if again.Changed || !again.TestSkipped || !again.ReloadSkipped || !again.Applied {
		t.Fatalf("重复停用应幂等短路: %+v", again)
	}
}

// TestDisableRollbackRestoresThroughChannel 断言停用过程中 nginx 校验失败时，
// conf.d 片段经受控写**写回**（不是 os.Rename 回滚——那条路同样 DAC 失败）。
func TestDisableRollbackRestoresThroughChannel(t *testing.T) {
	confDir := useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)

	var writes int
	fakeWriter := func(domain string, content []byte) error {
		writes++
		return os.WriteFile(filepath.Join(confDir, vhostFileName(domain)), content, 0o644)
	}
	fakeDeleter := func(domain string) error {
		return os.Remove(filepath.Join(confDir, vhostFileName(domain)))
	}
	store := &vhostStore{dir: confDir, writer: fakeWriter, deleter: fakeDeleter}
	fake := &fakeChannel{output: rootActionOutput{ExitCode: 1, Stderr: "test boom"}}
	runner := hostRunnerWithChannel(fake.call)
	svc := &siteService{store: store, runner: runner}

	domain := "rollback-del.example.com"
	path, _, _, _ := store.create(siteParams{Domain: domain, Port: 18094}, nil)
	before := readVHost(t, path)
	writes = 0

	if _, perr := svc.toggle(domain, false); perr == nil {
		t.Fatalf("nginx 校验失败时停用应失败")
	}
	if readVHost(t, path) != before {
		t.Fatalf("校验失败后 conf.d 片段应经受控写恢复为原内容")
	}
	if writes != 1 {
		t.Fatalf("回滚应走一次受控写，实际 %d 次", writes)
	}
	if _, err := os.Stat(filepath.Join(stateDir, vhostFileName(domain)+disabledSuffix)); !os.IsNotExist(err) {
		t.Fatalf("回滚后不应残留停用备份: %v", err)
	}
}
