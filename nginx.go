package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// nginx 命令层的唯一职责：把"配置校验 / 热重载 / 已加载配置转储"变成
// 固定 argv 的 exec 调用。安全边界有三条硬约束：
//
//  1. 命令来自运维环境变量（受信配置），只按 shell-like 规则切分成 argv，
//     绝不经过 /bin/sh 解释；RPC 入参（domain/root/port）永远不进 argv。
//  2. 操作旗标由插件固定注入：校验固定 "-t"、重载固定 "-s","reload"、
//     转储固定 "-T"。环境变量只允许提供命令前缀，不允许自带操作旗标。
//  3. 每次执行都带超时，输出截断后才进错误消息，避免拖垮协议帧。
const (
	// nginxTestCmdEnv 配置校验命令前缀；未设置时默认 nginx -t。
	nginxTestCmdEnv = "KORE_NGINX_TEST_CMD"
	// nginxReloadCmdEnv 热重载命令前缀；未设置时默认 nginx -s reload。
	nginxReloadCmdEnv = "KORE_NGINX_RELOAD_CMD"
	// nginxDumpCmdEnv 已加载配置转储命令前缀；未设置时由校验前缀推导 -T。
	nginxDumpCmdEnv = "KORE_NGINX_DUMP_CMD"
	// defaultNginxPrefix 是未配置时的命令前缀，插件补上固定的操作旗标。
	defaultNginxPrefix = "nginx"
	// nginxCommandTimeout 是单条 nginx 命令的执行预算。
	nginxCommandTimeout = 20 * time.Second
	// maxCommandOutput 是进入错误消息的输出上限（按 rune 计）。
	maxCommandOutput = 4096
)

// commandSpec 是一条外部命令的已校验前缀；操作旗标在调用点固定拼接。
type commandSpec struct {
	prefix   []string
	disabled bool
}

// argv 返回"前缀 + 固定操作旗标"的完整 argv；调用方不得再拼接任何入参。
func (c commandSpec) argv(fixed ...string) []string {
	out := make([]string, 0, len(c.prefix)+len(fixed))
	out = append(out, c.prefix...)
	return append(out, fixed...)
}

// nginxRunner 持有三类命令的解析结果与实际生效模式。
// mode ∈ host/docker/off；reason 只在跳过（off 或显式关闭）时说明原因。
type nginxRunner struct {
	test      commandSpec
	reload    commandSpec
	dump      commandSpec
	mode      string
	reason    string
	container string
}

// status 返回结果里统一披露的命令层模式与跳过原因。
func (r *nginxRunner) status() nginxStatus {
	return nginxStatus{NginxMode: r.mode, NginxReason: r.reason}
}

// dumpSpecFromEnv 解析 KORE_NGINX_DUMP_CMD；未显式配置时复用校验前缀并补 -T。
// 这样 docker exec 场景只需配两条命令，-T 自动落在同一容器/同一 nginx 上。
func dumpSpecFromEnv(test, reload commandSpec) (commandSpec, error) {
	raw, ok := os.LookupEnv(nginxDumpCmdEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		// 优先复用校验前缀；校验被关闭时退回复载前缀，保证仍能探测"已加载"。
		base := test
		if base.disabled {
			base = reload
		}
		if base.disabled {
			return commandSpec{disabled: true}, nil
		}
		return commandSpec{prefix: append([]string(nil), base.prefix...)}, nil
	}
	return commandSpecFromEnv(nginxDumpCmdEnv, defaultNginxPrefix, "-T")
}

// commandSpecFromEnv 读取环境变量并归一成"无操作旗标的前缀"。
// 未设置/空串走默认值；off/disable/none 表示显式跳过该环节。
func commandSpecFromEnv(envName, defaultPrefix string, fixed ...string) (commandSpec, error) {
	raw, ok := os.LookupEnv(envName)
	if !ok || strings.TrimSpace(raw) == "" {
		raw = defaultPrefix
	}
	raw = strings.TrimSpace(raw)
	if isDisabledCommand(raw) {
		return commandSpec{disabled: true}, nil
	}
	argv, err := splitCommandLine(raw)
	if err != nil {
		return commandSpec{}, fmt.Errorf("%s 非法: %w", envName, err)
	}
	argv, err = stripFixedFlags(argv, fixed)
	if err != nil {
		return commandSpec{}, fmt.Errorf("%s 非法: %w", envName, err)
	}
	return commandSpec{prefix: argv}, nil
}

// isDisabledCommand 识别显式关闭标记。
func isDisabledCommand(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "disable", "disabled", "none":
		return true
	default:
		return false
	}
}

// stripFixedFlags 去掉用户已经在尾部写好的固定旗标，并拒绝前缀中出现
// 任何操作旗标——保证插件注入的 "-t" / "-s reload" / "-T" 永远唯一。
func stripFixedFlags(argv, fixed []string) ([]string, error) {
	if hasArgvSuffix(argv, fixed) {
		argv = argv[:len(argv)-len(fixed)]
	}
	if len(argv) == 0 {
		return nil, errors.New("命令前缀为空")
	}
	for _, token := range argv {
		switch token {
		case "-t", "-T", "-s":
			return nil, fmt.Errorf("命令前缀不得包含操作旗标 %q（由插件固定注入）", token)
		}
	}
	return argv, nil
}

// hasArgvSuffix 判断 argv 是否以 suffix 结尾。
func hasArgvSuffix(argv, suffix []string) bool {
	if len(suffix) == 0 || len(argv) < len(suffix) {
		return false
	}
	tail := argv[len(argv)-len(suffix):]
	for i := range suffix {
		if tail[i] != suffix[i] {
			return false
		}
	}
	return true
}

// splitCommandLine 把命令字符串切成 argv，支持单/双引号与反斜杠转义，
// 不调用 shell。任何未闭合引号、控制字符或空命令都判配置非法。
func splitCommandLine(raw string) ([]string, error) {
	var (
		args     []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
		hasToken bool
	)
	flush := func() {
		if hasToken {
			args = append(args, current.String())
			current.Reset()
			hasToken = false
		}
	}
	for _, r := range raw {
		if r != '\t' && r < 0x20 {
			return nil, errors.New("命令含控制字符")
		}
		if escaped {
			switch r {
			case '\'', '"', '\\':
				current.WriteRune(r)
			default:
				current.WriteRune('\\')
				current.WriteRune(r)
			}
			escaped, hasToken = false, true
			continue
		}
		if inSingle {
			if r == '\'' {
				inSingle = false
			} else {
				current.WriteRune(r)
			}
			hasToken = true
			continue
		}
		if inDouble {
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
			}
			hasToken = true
			continue
		}
		switch r {
		case ' ', '\t':
			flush()
		case '\'':
			inSingle, hasToken = true, true
		case '"':
			inDouble, hasToken = true, true
		case '\\':
			escaped, hasToken = true, true
		default:
			current.WriteRune(r)
			hasToken = true
		}
	}
	if escaped {
		return nil, errors.New("命令以未完成的转义符结尾")
	}
	if inSingle || inDouble {
		return nil, errors.New("命令引号未闭合")
	}
	flush()
	if len(args) == 0 {
		return nil, errors.New("命令为空")
	}
	for _, arg := range args {
		if arg == "" {
			return nil, errors.New("命令含空参数")
		}
	}
	return args, nil
}

// commandOutcome 是一条命令的观测结果；ran=false 表示该环节被显式关闭。
type commandOutcome struct {
	output string
	err    error
	ran    bool
}

// runTest 执行固定 argv 的配置校验。
func (r *nginxRunner) runTest() commandOutcome { return runCommand(r.test, "-t") }

// runReload 执行固定 argv 的热重载。
func (r *nginxRunner) runReload() commandOutcome { return runCommand(r.reload, "-s", "reload") }

// runDump 执行固定 argv 的已加载配置转储。
func (r *nginxRunner) runDump() commandOutcome { return runCommand(r.dump, "-T") }

// runCommand 以固定 argv 执行命令：不经过 shell，不拼接任何 RPC 入参。
func runCommand(spec commandSpec, fixed ...string) commandOutcome {
	if spec.disabled {
		return commandOutcome{ran: false}
	}
	argv := spec.argv(fixed...)
	ctx, cancel := context.WithTimeout(context.Background(), nginxCommandTimeout)
	defer cancel()
	// #nosec G204 -- argv 来自受信环境配置且操作旗标固定，RPC 入参不参与命令。
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	// 保留完整输出：nginx -T 的 server_name 可能远在 4 KiB 之后，
	// 截断只发生在进入错误消息/结果字段时（describeCommand）。
	if ctx.Err() == context.DeadlineExceeded {
		return commandOutcome{output: string(out), err: fmt.Errorf("命令超时（%s）", nginxCommandTimeout), ran: true}
	}
	return commandOutcome{output: string(out), err: err, ran: true}
}

// truncateCommandOutput 限制进入错误消息/结果的输出长度。
func truncateCommandOutput(s string) string {
	runes := []rune(s)
	if len(runes) <= maxCommandOutput {
		return s
	}
	return string(runes[:maxCommandOutput]) + "…（输出截断）"
}

// describeCommand 把命令结果整理成单行可诊断文本，并在这里做长度截断。
func describeCommand(out commandOutcome) string {
	text := truncateCommandOutput(strings.TrimSpace(out.output))
	switch {
	case out.err == nil:
		return text
	case text == "":
		return out.err.Error()
	default:
		return out.err.Error() + ": " + text
	}
}

// configHasServerName 在 nginx -T 的转储里精确匹配 server_name 指令，
// 避免 a.example.com 因子串命中 a.example.com.cn 造成假加载。
func configHasServerName(configText, domain string) bool {
	for _, line := range strings.Split(configText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "server_name" {
			continue
		}
		for _, value := range fields[1:] {
			if strings.TrimSuffix(value, ";") == domain {
				return true
			}
		}
	}
	return false
}
