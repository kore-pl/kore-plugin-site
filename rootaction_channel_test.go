package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是「非 root 时的降级」在**代码层**的证据：不依赖真机权限，
// 用注入的假通道与纯判定函数证明三条行为——
//
//  1. channel 可用时 host 模式走通道（不 exec），且动作 id 与声明一致；
//  2. 内核回 E_NEED_ROOT 时，上层拿到的是 E_NEED_ROOT + 可操作 hint（不是 panic、不是 E_INTERNAL）；
//  3. conf.d 不可写只在「属主是 root 且自己非 root」时才升级成 E_NEED_ROOT，
//     其它权限问题保持 E_INTERNAL（不把所有失败都归因到 root，掩盖真实原因）。

// fakeChannel 是 rootActionFunc 的内存实现：记录被调用的 action id，返回预设结果。
type fakeChannel struct {
	calls  []string
	output rootActionOutput
	err    error
}

// call 满足 rootActionFunc 签名。
func (f *fakeChannel) call(actionID string) (rootActionOutput, error) {
	f.calls = append(f.calls, actionID)
	return f.output, f.err
}

// hostRunnerWithChannel 构造 host 模式 runner（固定前缀 nginx），注入假通道。
func hostRunnerWithChannel(ch rootActionFunc) *nginxRunner {
	return &nginxRunner{
		test:    commandSpec{prefix: []string{"nginx"}},
		reload:  commandSpec{prefix: []string{"nginx"}},
		dump:    commandSpec{prefix: []string{"nginx"}},
		mode:    nginxModeHost,
		channel: ch,
	}
}

// TestHostRunnerUsesPrivilegeChannel 断言 host 模式优先走内核通道，且三条命令
// 分别对应三条已声明动作——id 拼错会在这里被抓住，而不是在内核 404 时才发现。
func TestHostRunnerUsesPrivilegeChannel(t *testing.T) {
	fake := &fakeChannel{output: rootActionOutput{
		Stdout: "nginx: configuration file /etc/nginx/nginx.conf test is successful\n",
	}}
	runner := hostRunnerWithChannel(fake.call)

	if out := runner.runTest(); out.err != nil || !out.ran {
		t.Fatalf("通道成功时 runTest 仍报错: err=%v ran=%v", out.err, out.ran)
	}
	if out := runner.runReload(); out.err != nil || !out.ran {
		t.Fatalf("通道成功时 runReload 仍报错: err=%v ran=%v", out.err, out.ran)
	}
	if out := runner.runDump(); out.err != nil || !out.ran {
		t.Fatalf("通道成功时 runDump 仍报错: err=%v ran=%v", out.err, out.ran)
	}

	want := []string{rootActionNginxTest, rootActionNginxReload, rootActionNginxDump}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("通道被调用的动作=%v 期望=%v（id 必须与 info.json 声明逐字一致）", fake.calls, want)
	}
	// 通道返回的输出要能被下游解析：nginx -t 成功判据依赖这句原文。
	if got := runner.runTest(); !strings.Contains(got.output, "test is successful") {
		t.Fatalf("通道输出未透传到 commandOutcome.output: %q", got.output)
	}
}

// TestHostRunnerChannelExitCode 断言非零退出码被翻译成错误（不能当成功吞掉）。
func TestHostRunnerChannelExitCode(t *testing.T) {
	fake := &fakeChannel{output: rootActionOutput{
		Stderr:   "nginx: [emerg] unknown directive \"nope\" in /etc/nginx/conf.d/x.conf:3\n",
		ExitCode: 1,
	}}
	out := hostRunnerWithChannel(fake.call).runTest()
	if out.err == nil {
		t.Fatalf("内核回退出码 1 时 runTest 未报错")
	}
	if !strings.Contains(out.output, "unknown directive") {
		t.Fatalf("stderr 未进入可诊断输出: %q", out.output)
	}
	if out.needRoot() {
		t.Fatalf("语法错误被误判成「需要 root」：归因错误会掩盖真实配置问题")
	}
}

// TestNeedRootFromChannel 断言内核 E_NEED_ROOT 被结构化升级成本插件自己的
// E_NEED_ROOT + hint（不是 panic，也不是 E_INTERNAL）。
func TestNeedRootFromChannel(t *testing.T) {
	fake := &fakeChannel{err: &rootActionFailure{
		Code:    pluginapi.E_NEED_ROOT,
		Message: "当前进程不是 root，无法执行特权动作",
		Hint:    "用 sudo kored 启动",
	}}
	out := hostRunnerWithChannel(fake.call).runReload()
	if out.err == nil {
		t.Fatalf("通道报 E_NEED_ROOT 时 runReload 未返回错误")
	}
	if !out.needRoot() {
		t.Fatalf("outcome.needRoot() 未识别内核 E_NEED_ROOT")
	}
	perr := needRootCommandErr(out, "nginx 热重载")
	if perr == nil {
		t.Fatalf("needRootCommandErr 未把 E_NEED_ROOT 升级成协议错误")
	}
	if perr.Code != pluginapi.E_NEED_ROOT {
		t.Fatalf("错误码=%q 期望 %q", perr.Code, pluginapi.E_NEED_ROOT)
	}
	var detail struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(perr.Detail, &detail); err != nil || detail.Hint == "" {
		t.Fatalf("E_NEED_ROOT 缺少可操作 hint: detail=%s err=%v", perr.Detail, err)
	}
	if !strings.Contains(detail.Hint, "root") {
		t.Fatalf("hint 未说明怎么办（必须能指导小白下一步）: %q", detail.Hint)
	}
}

// TestOtherChannelErrorsNotAttributedToRoot 断言非权限类通道错误不被误归因。
func TestOtherChannelErrorsNotAttributedToRoot(t *testing.T) {
	fake := &fakeChannel{err: &rootActionFailure{
		Code:    pluginapi.E_INVALID,
		Message: "动作未声明",
	}}
	out := hostRunnerWithChannel(fake.call).runTest()
	if out.err == nil {
		t.Fatalf("通道报错时 runTest 未返回错误")
	}
	if out.needRoot() {
		t.Fatalf("E_INVALID 被误判成需要 root")
	}
	if perr := needRootCommandErr(out, "nginx 配置校验"); perr != nil {
		t.Fatalf("非权限错误不应升级成 E_NEED_ROOT: %v", perr)
	}
}

// TestNeedRootForConfDirDecision 是纯判定：属主 root + 自己非 root ⇒ E_NEED_ROOT；
// 其它三种情形一律 nil（保持 E_INTERNAL）。
func TestNeedRootForConfDirDecision(t *testing.T) {
	dir := t.TempDir()
	cause := &fs.PathError{Op: "open", Path: filepath.Join(dir, "kore-site-manifest.json.lock"), Err: fs.ErrPermission}

	// 本机临时目录属主是当前用户：不是 root ⇒ 不升级（这也是边界用例 chmod 0000 的场景）。
	if isRoot, known := dirOwnerIsRoot(dir); known && isRoot {
		t.Skip("测试目录属主是 root（以 root 运行时才会出现）")
	}
	if perr := needRootForConfDir(dir, cause); perr != nil {
		t.Fatalf("属主非 root 时不应报 E_NEED_ROOT，实际: %v", perr)
	}

	// root 自己不会被 DAC 拦：EACCES 另有原因，不归因权限。
	if perr := needRootForConfDirDecision(dir, cause, 0); perr != nil {
		t.Fatalf("euid=0 时不应报 E_NEED_ROOT，实际: %v", perr)
	}
	// 非权限错误（只读挂载等之外的普通错误）不升级。
	if perr := needRootForConfDirDecision(dir, errors.New("disk full"), 1000); perr != nil {
		t.Fatalf("非权限错误不应报 E_NEED_ROOT，实际: %v", perr)
	}
}

// TestConfDirWritableNotNeedRoot 是本机（非 root）真机证据：
// 目录可写时不应产生任何 E_NEED_ROOT——证明降级只在该报的时候报。
func TestConfDirWritableNotNeedRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.conf"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("临时目录不可写，环境异常: %v", err)
	}
	if perr := needRootForConfDir(dir, &fs.PathError{Err: fs.ErrPermission}); perr != nil {
		t.Fatalf("可写目录不应报 E_NEED_ROOT: %v", perr)
	}
}
