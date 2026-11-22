package main

import (
	"strings"
	"testing"
)

// TestConfigHasServerNameExact 验证加载检测是精确 server_name 匹配，
// 而不是子串命中——a.example.com 不能因为 a.example.com.cn 而假加载。
func TestConfigHasServerNameExact(t *testing.T) {
	dump := "# configuration file /etc/nginx/conf.d/x.conf:\n" +
		"server {\n" +
		"    server_name a.example.com.cn;\n" +
		"    server_name one.example.com two.example.com;\n" +
		"    # server_name commented.example.com;\n" +
		"}\n"
	cases := []struct {
		domain string
		want   bool
	}{
		{domain: "one.example.com", want: true},
		{domain: "two.example.com", want: true},
		{domain: "a.example.com", want: false},
		{domain: "commented.example.com", want: false},
	}
	for _, tc := range cases {
		if got := configHasServerName(dump, tc.domain); got != tc.want {
			t.Fatalf("configHasServerName(%q)=%v，期望 %v", tc.domain, got, tc.want)
		}
	}
}

// TestNginxCommandsCanBeDisabled 验证 off 是显式关闭而不是"空命令"。
func TestNginxCommandsCanBeDisabled(t *testing.T) {
	t.Setenv(nginxTestCmdEnv, "off")
	t.Setenv(nginxReloadCmdEnv, "disable")
	t.Setenv(nginxDumpCmdEnv, "none")
	runner, perr := loadNginxRunner()
	if perr != nil {
		t.Fatalf("off 不应被当作配置错误: %v", perr.Message)
	}
	if !runner.test.disabled || !runner.reload.disabled || !runner.dump.disabled {
		t.Fatalf("三条命令都应 disabled: %+v", runner)
	}
	for name, outcome := range map[string]commandOutcome{
		"test":   runner.runTest(),
		"reload": runner.runReload(),
		"dump":   runner.runDump(),
	} {
		if outcome.ran {
			t.Fatalf("%s 被关闭后不应执行", name)
		}
	}
}

// TestSplitCommandLineQuoting 验证引号/转义只影响 argv 切分，不引入 shell。
func TestSplitCommandLineQuoting(t *testing.T) {
	argv, err := splitCommandLine(`sh -c 'echo "a b" >&2; exit 1'`)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	want := []string{"sh", "-c", `echo "a b" >&2; exit 1`}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%q，期望 %q", argv, want)
	}
	for _, bad := range []string{`sh -c 'unterminated`, "sh -c \\", ""} {
		if _, err := splitCommandLine(bad); err == nil {
			t.Fatalf("非法命令 %q 应报错", bad)
		}
	}
}

// TestTruncateCommandOutput 验证输出截断按 rune 处理且带提示。
func TestTruncateCommandOutput(t *testing.T) {
	long := strings.Repeat("配", maxCommandOutput+10)
	got := truncateCommandOutput(long)
	if !strings.HasSuffix(got, "（输出截断）") {
		t.Fatalf("截断后应带提示: %q", got[len(got)-20:])
	}
	if len([]rune(got)) > maxCommandOutput+len([]rune("…（输出截断）")) {
		t.Fatalf("截断后长度仍超限: %d", len([]rune(got)))
	}
}
