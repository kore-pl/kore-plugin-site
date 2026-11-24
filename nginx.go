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
	// channel 是内核 root-action 通道（host 模式专用）；nil 表示通道不可用。
	//
	// 为什么 host 模式优先走通道：插件降权后以 kore-site 运行，`nginx -s reload` 要给
	// root master 发信号、`nginx -t`/`-T` 要读 root 才能读的配置——直连 exec 必然 EACCES。
	// docker 模式不走通道：命令是 `docker exec …`，在容器内以容器用户执行，与宿主 root 无关。
	channel rootActionFunc
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

// runTest 执行配置校验（host 模式优先经内核提权通道）。
func (r *nginxRunner) runTest() commandOutcome {
	return r.run(rootActionNginxTest, r.test, "-t")
}

// runReload 执行热重载（host 模式优先经内核提权通道）。
func (r *nginxRunner) runReload() commandOutcome {
	return r.run(rootActionNginxReload, r.reload, "-s", "reload")
}

// runDump 转储已加载配置（host 模式优先经内核提权通道）。
func (r *nginxRunner) runDump() commandOutcome {
	return r.run(rootActionNginxDump, r.dump, "-T")
}

// run 是三条命令的唯一分派点：通道可用且 host 模式 → 通道执行；否则直连 exec。
//
// 通道优先而不是「先直连、失败再重试通道」：降权进程直连 reload 会真的向 master
// 发信号失败并留下一条噪声错误，而判定「是否走通道」只依赖环境变量是否齐备，
// 不需要一次失败的尝试来试探。
func (r *nginxRunner) run(actionID string, spec commandSpec, fixed ...string) commandOutcome {
	if spec.disabled {
		return commandOutcome{ran: false}
	}
	if r.channel != nil && r.mode == nginxModeHost {
		return runViaRootAction(r.channel, actionID)
	}
	return runCommand(spec, fixed...)
}

// runViaRootAction 经内核通道执行一条声明过的固定动作，并把结果归一成 commandOutcome。
//
// 输出顺序：先 stdout 后 stderr——与 CombinedOutput 的「不保证顺序」相比，这里
// 顺序稳定，`nginx -t` 的 "syntax is ok" / "test is successful" 仍在同一串里可解析。
func runViaRootAction(channel rootActionFunc, actionID string) commandOutcome {
	out, err := channel(actionID)
	if err != nil {
		// 通道错误（含内核 E_NEED_ROOT / E_INVALID / 审计拒绝）原样保留，
		// 由 outcome.needRoot 做结构化归因；错误文案同时作为可诊断输出。
		return commandOutcome{output: err.Error(), err: err, ran: true}
	}
	combined := out.Stdout + out.Stderr
	if out.ExitCode != 0 {
		return commandOutcome{
			output: combined,
			err:    fmt.Errorf("命令退出码 %d（内核 root-action %s）", out.ExitCode, actionID),
			ran:    true,
		}
	}
	return commandOutcome{output: combined, ran: true}
}

// needRoot 报告该命令失败是否**因为缺宿主 root 权限**。
//
// 两条判据：① 内核通道明确回 E_NEED_ROOT（结构化，最准）；
// ② 直连 exec 的输出里出现权限被拒的判据（降权插件直连 nginx 的典型后果）。
// 第二条只在通道不可用时才有意义——通道可用时错误一定来自判据 ①。
func (o commandOutcome) needRoot() bool {
	if o.err == nil {
		return false
	}
	if isNeedRootFailure(o.err) {
		return true
	}
	return looksLikePermissionDenied(o.output + " " + o.err.Error())
}

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
