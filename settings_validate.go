package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// validateSettings 校验并归一化全部字段；配置非法一律给出「哪一项错 + 怎么改」。
func validateSettings(settings pluginSettings) (pluginSettings, error) {
	mode, err := normalizeNginxMode(settings.NginxMode)
	if err != nil {
		return pluginSettings{}, err
	}
	settings.NginxMode = mode
	if settings.DockerContainer != "" {
		if err := validateContainerName(settings.DockerContainer); err != nil {
			return pluginSettings{}, err
		}
	}
	if settings.DockerConfDir, err = normalizeAbsDir(cfgDockerConfDir, settings.DockerConfDir); err != nil {
		return pluginSettings{}, err
	}
	if settings.HostConfDir, err = normalizeAbsDir(cfgHostConfDir, settings.HostConfDir); err != nil {
		return pluginSettings{}, err
	}
	if settings.PHPFpmSocket != "" {
		if err := validatePHPSocketPath(settings.PHPFpmSocket); err != nil {
			return pluginSettings{}, fmt.Errorf("%s 非法: %w（示例 /run/php/php8.3-fpm.sock）", cfgPHPFpmSocket, err)
		}
	}
	if mode == nginxModeDocker && settings.DockerContainer == "" {
		return pluginSettings{}, errors.New("nginx_mode=docker 必须同时配置 docker_container（例如 kore-nginx）")
	}
	return settings, nil
}

// normalizeNginxMode 校验模式枚举；空值回落到默认 auto。
func normalizeNginxMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return defaultNginxMode, nil
	}
	switch mode {
	case nginxModeAuto, nginxModeHost, nginxModeDocker, nginxModeOff:
		return mode, nil
	default:
		return "", fmt.Errorf("nginx_mode 只能是 auto/host/docker/off（当前 %q）", raw)
	}
}

// validateContainerName 硬校验容器名，拒绝任何注入面（空白、斜杠、分号、引号、$ 等）。
func validateContainerName(name string) error {
	if containerNameRE.MatchString(name) {
		return nil
	}
	return fmt.Errorf("docker_container 非法：必须字母或数字开头，只允许字母、数字、下划线、点和连字符，最长 128 字符（当前 %q）", name)
}

// normalizeAbsDir 校验目录配置：必须绝对路径、Clean 后无冗余分量、不含控制字符。
func normalizeAbsDir(key, raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", fmt.Errorf("%s 不能为空", key)
	}
	if strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("%s 不能包含 NUL 字节", key)
	}
	for _, r := range dir {
		if r < 0x20 {
			return "", fmt.Errorf("%s 不能包含控制字符", key)
		}
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s 必须是绝对路径（当前 %q）", key, dir)
	}
	if filepath.Clean(dir) != dir {
		return "", fmt.Errorf("%s 不能包含冗余路径分量（当前 %q）", key, dir)
	}
	if filepath.Base(dir) == ".." {
		return "", fmt.Errorf("%s 不能指向上级目录（当前 %q）", key, dir)
	}
	return dir, nil
}
