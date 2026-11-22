package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// dockerMount 是 docker inspect 暴露的挂载点；只取校验需要的字段。
type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// preflightDockerWrite 在容器模式写盘前确认「写下去的目录真的会被容器 nginx 读到」：
// 宿主 conf.d 存在且可写 → 容器存在 → 容器内 conf.d 确实由该宿主目录提供。
// 只读方法不调用：容器故障由 nginx -t 的真实失败呈现，不阻断读取。
func preflightDockerWrite(settings pluginSettings) *pluginapi.Error {
	if settings.NginxMode != nginxModeDocker {
		return nil
	}
	if perr := checkWritableConfDir(settings.HostConfDir); perr != nil {
		return perr
	}
	mounts, perr := inspectContainerMounts(settings.DockerContainer)
	if perr != nil {
		return perr
	}
	actual, ok := mountHostPath(mounts, settings.DockerConfDir)
	if !ok {
		return invalidParam(fmt.Sprintf(
			"容器 %s 没有把 %s 挂载到宿主目录；请用 -v <宿主目录>:%s 启动容器（当前 host_conf_dir=%s）",
			settings.DockerContainer, settings.DockerConfDir, settings.DockerConfDir, settings.HostConfDir))
	}
	if !sameHostPath(actual, settings.HostConfDir) {
		return invalidParam(fmt.Sprintf(
			"容器 %s 的 %s 实际来自宿主 %s，与 host_conf_dir=%s 不一致；请把 host_conf_dir 改为 %s",
			settings.DockerContainer, settings.DockerConfDir, actual, settings.HostConfDir, actual))
	}
	return nil
}

// preflightWrite 是 siteService 的写路径闸门：host/off 直接放行，docker 模式必须过校验。
func (s *siteService) preflightWrite() *pluginapi.Error {
	return preflightDockerWrite(s.settings)
}

// checkWritableConfDir 校验宿主 conf.d：存在、是目录、可写（在目录内建临时文件探测）。
func checkWritableConfDir(dir string) *pluginapi.Error {
	st, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return invalidParam("宿主的 conf.d 目录 " + dir + " 不存在，请确认容器把 conf.d 挂载到了这个目录")
	}
	if err != nil {
		return invalidParam("宿主的 conf.d 目录 " + dir + " 无法访问: " + err.Error())
	}
	if !st.IsDir() {
		return invalidParam("宿主的 conf.d 路径不是目录: " + dir)
	}
	probe, err := os.CreateTemp(dir, ".kore-site-probe-*")
	if err != nil {
		return invalidParam("宿主的 conf.d 目录 " + dir + " 不可写: " + err.Error())
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// inspectContainerMounts 读取容器挂载表；容器不存在/不可访问统一回 E_UPSTREAM + 明确文案。
func inspectContainerMounts(container string) ([]dockerMount, *pluginapi.Error) {
	ctx, cancel := context.WithTimeout(context.Background(), nginxCommandTimeout)
	defer cancel()
	// #nosec G204 -- argv 固定，容器名已通过正则校验。
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container",
		"--format", "{{json .Mounts}}", container).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return nil, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: fmt.Sprintf("容器 %s 不存在或 docker 不可用（docker inspect 失败）：%s", container, truncateCommandOutput(detail)),
		}
	}
	var mounts []dockerMount
	if err := json.Unmarshal(out, &mounts); err != nil {
		return nil, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "解析容器挂载信息失败: " + err.Error(),
		}
	}
	return mounts, nil
}

// mountHostPath 在挂载表里找覆盖 containerDir 的最深挂载，返回对应的宿主路径。
// 支持 `-v /etc/nginx:/etc/nginx` 这类父目录挂载（按 Destination 最长匹配）。
func mountHostPath(mounts []dockerMount, containerDir string) (string, bool) {
	bestDst, bestHost := "", ""
	for _, m := range mounts {
		dst := filepath.Clean(m.Destination)
		if containerDir != dst && !strings.HasPrefix(containerDir, dst+"/") {
			continue
		}
		if len(dst) <= len(bestDst) || m.Source == "" {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(containerDir, dst), "/")
		host := m.Source
		if rel != "" {
			host = filepath.Join(host, rel)
		}
		bestDst, bestHost = dst, host
	}
	return bestHost, bestDst != ""
}

// sameHostPath 比较两个宿主路径；best-effort 解析符号链接（/var → /private/var 之类）。
func sameHostPath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}
