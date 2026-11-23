package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// 站点目录三态（§3.5 #6）：present / absent / unknown。
//
// 三态而不是 bool 的理由：容器模式下 vhost 里的 root 是**容器内路径**，宿主
// os.Stat 会误报 false；而「查不到（docker 不可用 / 容器异常 / 路径无法解析）」
// 与「确实不存在」是两件事，混成 false 会诱导用户误删正常站点。
const (
	siteDirPresent = "present"
	siteDirAbsent  = "absent"
	siteDirUnknown = "unknown"
)

// siteDirProbeTimeout 是容器内目录探测的执行预算；与 nginx 命令同一量级。
// 声明为变量只为单测覆盖超时分支，生产路径不修改。
var siteDirProbeTimeout = nginxCommandTimeout

// probeSiteDir 判定站点目录状态：docker 模式在容器内 test -d，其余用宿主 os.Stat。
// 契约：unknown 必须带可操作 reason（G-16 不静默降级）；present / absent 的 reason 为空。
func probeSiteDir(runner *nginxRunner, root string) (string, string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return siteDirUnknown, "vhost 中未解析到 root 指令，无法判断站点目录"
	}
	if !strings.HasPrefix(root, "/") {
		return siteDirUnknown, "root=" + root + " 不是绝对路径，无法判断站点目录；请在 vhost 中写绝对路径"
	}
	if runner != nil && runner.mode == nginxModeDocker {
		return dockerSiteDirState(runner.container, root)
	}
	return hostSiteDirState(root)
}

// hostSiteDirState 用宿主文件系统判定：ENOENT 与「存在但不是目录」都是 absent，
// 其它读取失败（权限、ENOTDIR、I/O）是 unknown——那是「查不到」，不是「不存在」。
func hostSiteDirState(root string) (string, string) {
	st, err := os.Stat(root)
	switch {
	case err == nil && st.IsDir():
		return siteDirPresent, ""
	case err == nil:
		return siteDirAbsent, ""
	case errors.Is(err, os.ErrNotExist):
		return siteDirAbsent, ""
	default:
		return siteDirUnknown, "宿主无法读取站点目录 " + root + "：" + err.Error() +
			"；请检查目录权限，或把 nginx_mode 改为 docker 让插件在容器内判断"
	}
}

// dockerSiteDirState 在容器内执行 `docker exec <container> test -d <root>`。
//
// 判定依据是「退出码 + 是否无输出」：
//   - 0                          → present
//   - 1 且无任何输出              → absent（test -d 唯一的无输出失败形态）
//   - 其它（1 带 stderr / 125 / 启动失败 / 超时）→ unknown + 可操作原因
//
// 只看退出码不够：docker daemon 级错误（容器不存在 / 未运行）同样返回 1，但会往
// stderr 写 "Error response from daemon: ..."；把它当 absent 正是 #6 要修的误报。
func dockerSiteDirState(container, root string) (string, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return siteDirUnknown, "容器模式需要 docker 命令但当前环境找不到：" + err.Error() +
			"；请安装 docker 或把它加入 PATH，或把 nginx_mode 改为 host"
	}
	ctx, cancel := context.WithTimeout(context.Background(), siteDirProbeTimeout)
	defer cancel()
	// #nosec G204 -- argv 固定，容器名已正则校验，root 是独立 argv（不经 shell 且要求绝对路径）。
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "test", "-d", root).CombinedOutput()
	if err == nil {
		return siteDirPresent, ""
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return siteDirUnknown, fmt.Sprintf(
			"docker exec 在 %s 内没有返回，无法判断容器 %s 内的站点目录 %s；请确认容器在运行且 docker daemon 正常",
			siteDirProbeTimeout, container, root)
	}
	detail := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code == 1 && detail == "" {
			return siteDirAbsent, ""
		}
		message := fmt.Sprintf("容器 %s 内无法判断站点目录 %s（docker exec test -d 退出码 %d）", container, root, code)
		if detail != "" {
			message += "：" + truncateCommandOutput(detail)
		}
		return siteDirUnknown, message + "；请确认容器在运行、docker 可用，或改用 nginx_mode=host"
	}
	return siteDirUnknown, "无法执行 docker exec 判断容器 " + container + " 内的站点目录 " + root +
		"：" + err.Error() + "；请确认 docker 已安装且当前用户可访问 docker"
}
