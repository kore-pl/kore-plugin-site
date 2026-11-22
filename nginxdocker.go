package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// autoOffReason 是 auto 三分支都落空时的明确跳过原因（不再静默 skipped）。
const autoOffReason = "未检测到 nginx（宿主与容器都没有）"

// dockerExecPrefix 返回固定的 argv 前缀：容器名与 nginx 各是独立 argv，
// 不经过 shell，也不接受任何 RPC 入参。操作旗标由调用点固定追加。
func dockerExecPrefix(container string) []string {
	return []string{"docker", "exec", container, "nginx"}
}

// buildRunner 把生效配置翻译成命令层：auto 在这里完成三分支探测，
// 同时返回「生效设置」（auto 探测到容器时回填 container），供写路径做容器校验。
func buildRunner(settings pluginSettings) (*nginxRunner, pluginSettings, error) {
	switch settings.NginxMode {
	case nginxModeOff:
		return &nginxRunner{
			test: commandSpec{disabled: true}, reload: commandSpec{disabled: true},
			dump: commandSpec{disabled: true}, mode: nginxModeOff,
			reason: "nginx_mode=off：已关闭 nginx 校验与重载",
		}, settings, nil
	case nginxModeHost:
		return hostRunner(settings)
	case nginxModeDocker:
		return dockerRunner(settings)
	case nginxModeAuto:
		return autoRunner(settings)
	default:
		return nil, settings, fmt.Errorf("未知 nginx_mode %q", settings.NginxMode)
	}
}

// loadNginxRunner 是命令层对外的唯一构造入口（单测与故障注入同样走这里）。
func loadNginxRunner() (*nginxRunner, *pluginapi.Error) {
	settings, _, err := loadPluginSettings()
	if err != nil {
		return nil, internalErr("插件配置非法: " + err.Error())
	}
	runner, _, err := buildRunner(settings)
	if err != nil {
		return nil, internalErr(err.Error())
	}
	return runner, nil
}

// hostRunner 从 KORE_NGINX_*_CMD 解析命令前缀；未配置时就是宿主 nginx。
// 显式命令前缀若是 `docker exec <容器> nginx`，如实上报为 docker 模式（命令原样使用）。
func hostRunner(settings pluginSettings) (*nginxRunner, pluginSettings, error) {
	testSpec, err := commandSpecFromEnv(nginxTestCmdEnv, defaultNginxPrefix, "-t")
	if err != nil {
		return nil, settings, fmt.Errorf("%s 非法: %w", nginxTestCmdEnv, err)
	}
	reloadSpec, err := commandSpecFromEnv(nginxReloadCmdEnv, defaultNginxPrefix, "-s", "reload")
	if err != nil {
		return nil, settings, fmt.Errorf("%s 非法: %w", nginxReloadCmdEnv, err)
	}
	dumpSpec, err := dumpSpecFromEnv(testSpec, reloadSpec)
	if err != nil {
		return nil, settings, fmt.Errorf("%s 非法: %w", nginxDumpCmdEnv, err)
	}
	runner := &nginxRunner{test: testSpec, reload: reloadSpec, dump: dumpSpec, mode: nginxModeHost}
	if container, ok := containerFromSpec(testSpec); ok {
		runner.mode = nginxModeDocker
		runner.container = container
	}
	runner.reason = disabledReason(testSpec, reloadSpec, dumpSpec)
	return runner, settings, nil
}

// dockerRunner 生成固定 argv：docker exec <容器> nginx + 插件注入的操作旗标。
func dockerRunner(settings pluginSettings) (*nginxRunner, pluginSettings, error) {
	if err := validateContainerName(settings.DockerContainer); err != nil {
		return nil, settings, err
	}
	spec := commandSpec{prefix: dockerExecPrefix(settings.DockerContainer)}
	return &nginxRunner{
		test: spec, reload: spec, dump: spec,
		mode: nginxModeDocker, container: settings.DockerContainer,
	}, settings, nil
}

// autoRunner 实现 auto 三分支：宿主命令 > 运行中的 nginx 容器 > off。
// 环境变量显式给了命令前缀时优先尊重它，保证老部署升级后行为不变。
func autoRunner(settings pluginSettings) (*nginxRunner, pluginSettings, error) {
	if hasExplicitCommandEnv() {
		return hostRunner(settings)
	}
	if _, err := exec.LookPath("nginx"); err == nil {
		return hostRunner(settings)
	}
	if container := detectNginxContainer(); container != "" {
		detected := settings
		detected.NginxMode = nginxModeDocker
		detected.DockerContainer = container
		return dockerRunner(detected)
	}
	return &nginxRunner{
		test: commandSpec{disabled: true}, reload: commandSpec{disabled: true},
		dump: commandSpec{disabled: true}, mode: nginxModeOff, reason: autoOffReason,
	}, settings, nil
}

// hasExplicitCommandEnv 判断是否有人显式提供了命令前缀（含 off 显式关闭）。
func hasExplicitCommandEnv() bool {
	for _, key := range []string{nginxTestCmdEnv, nginxReloadCmdEnv, nginxDumpCmdEnv} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// detectNginxContainer 用 `docker ps` 找镜像名含 nginx 的运行中容器；不经过 shell。
func detectNginxContainer() string {
	if _, err := exec.LookPath("docker"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), nginxCommandTimeout)
	defer cancel()
	// #nosec G204 -- argv 固定，无用户输入。
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}} {{.Image}}").Output()
	if err != nil {
		return ""
	}
	return firstNginxContainer(string(out))
}

// firstNginxContainer 从 `名字 镜像` 每行一条的输出里挑第一个 nginx 容器。
func firstNginxContainer(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !strings.Contains(strings.ToLower(fields[1]), "nginx") {
			continue
		}
		if validateContainerName(fields[0]) != nil {
			continue
		}
		return fields[0]
	}
	return ""
}

// containerFromSpec 从显式前缀识别 `docker exec <容器> nginx ...` 形态，仅用于如实上报模式。
func containerFromSpec(spec commandSpec) (string, bool) {
	prefix := spec.prefix
	if spec.disabled || len(prefix) < 4 {
		return "", false
	}
	if prefix[0] != "docker" || prefix[1] != "exec" || prefix[3] != "nginx" {
		return "", false
	}
	if validateContainerName(prefix[2]) != nil {
		return "", false
	}
	return prefix[2], true
}

// disabledReason 把被显式关闭的环节拼成一句可读原因。
func disabledReason(specs ...commandSpec) string {
	names := []string{"配置校验", "热重载", "加载检测"}
	var parts []string
	for i, spec := range specs {
		if spec.disabled && i < len(names) {
			parts = append(parts, names[i])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "已通过 KORE_NGINX_*_CMD=off 关闭" + strings.Join(parts, "/")
}
