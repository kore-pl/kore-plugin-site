package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// nginx 运行模式：auto 自动探测 / host 宿主命令 / docker 容器内执行 / off 显式关闭。
const (
	nginxModeAuto   = "auto"
	nginxModeHost   = "host"
	nginxModeDocker = "docker"
	nginxModeOff    = "off"
)

// 配置键、默认值与插件配置文件名（与 docs/PLUGIN-CONFIG.md、README 一致）。
const (
	pluginConfigName   = "config.json"
	cfgNginxMode       = "nginx_mode"
	cfgDockerContainer = "docker_container"
	cfgDockerConfDir   = "docker_conf_dir"
	cfgHostConfDir     = "host_conf_dir"

	defaultNginxMode     = nginxModeAuto
	defaultDockerConfDir = "/etc/nginx/conf.d"
	// defaultHostConfDir 复用 defaultVHostDir，避免两份默认路径漂移。
	defaultHostConfDir = defaultVHostDir
)

// 环境变量是 config.json 的兼容入口：config.json > 环境变量 > 代码默认值。
const (
	envNginxMode       = "KORE_NGINX_MODE"
	envDockerContainer = "KORE_DOCKER_CONTAINER"
	envDockerConfDir   = "KORE_DOCKER_CONF_DIR"
	envHostConfDir     = "KORE_HOST_CONF_DIR"
)

// containerNameRE 约束容器名：字母或数字开头，后接 [A-Za-z0-9_.-]，最长 128。
// 容器名会作为独立 argv 进入 `docker exec <name> nginx`，绝不允许空白、斜杠、分号。
var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// pluginSettings 是站点插件的可配置项（config.get 的 values 形状，G-24 显式字段）。
type pluginSettings struct {
	NginxMode       string `json:"nginx_mode"`
	DockerContainer string `json:"docker_container"`
	DockerConfDir   string `json:"docker_conf_dir"`
	HostConfDir     string `json:"host_conf_dir"`
}

// settingsPatch 是 config.set 的增量入参：指针区分「未提交」与「显式清空」。
type settingsPatch struct {
	NginxMode       *string `json:"nginx_mode"`
	DockerContainer *string `json:"docker_container"`
	DockerConfDir   *string `json:"docker_conf_dir"`
	HostConfDir     *string `json:"host_conf_dir"`
}

// defaultSettings 返回代码默认值。
func defaultSettings() pluginSettings {
	return pluginSettings{
		NginxMode:     defaultNginxMode,
		DockerConfDir: defaultDockerConfDir,
		HostConfDir:   defaultHostConfDir,
	}
}

// pluginConfigPath 返回 <KORE_STATE_DIR>/config.json；状态目录缺省时返回空串。
func pluginConfigPath() string {
	dir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(dir), pluginConfigName)
}

// loadPluginSettings 按 file > env > default 解析并校验配置。
func loadPluginSettings() (pluginSettings, string, error) {
	settings := defaultSettings()
	source := "default"
	if path := pluginConfigPath(); path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			var fromFile pluginSettings
			if err := json.Unmarshal(raw, &fromFile); err != nil {
				return pluginSettings{}, "", fmt.Errorf("解析插件配置 %s: %w", path, err)
			}
			settings = mergeSettings(settings, fromFile)
			source = "file"
		case errors.Is(err, os.ErrNotExist):
			// 未配置：继续按环境变量/默认值解析。
		default:
			return pluginSettings{}, "", fmt.Errorf("读取插件配置 %s: %w", path, err)
		}
	}
	if source != "file" {
		if fromEnv, any := settingsFromEnv(); any {
			settings = mergeSettings(settings, fromEnv)
			source = "env"
		}
	}
	normalized, err := validateSettings(settings)
	if err != nil {
		return pluginSettings{}, "", err
	}
	return normalized, source, nil
}

// settingsFromEnv 读取 4 个环境变量；KORE_SITE_CONF_DIR 作为宿主目录的历史别名保留。
func settingsFromEnv() (pluginSettings, bool) {
	var out pluginSettings
	any := false
	assign := func(dst *string, keys ...string) {
		for _, key := range keys {
			if value := strings.TrimSpace(os.Getenv(key)); value != "" {
				*dst = value
				any = true
				return
			}
		}
	}
	assign(&out.NginxMode, envNginxMode)
	assign(&out.DockerContainer, envDockerContainer)
	assign(&out.DockerConfDir, envDockerConfDir)
	assign(&out.HostConfDir, envHostConfDir, vhostDirEnv)
	return out, any
}

// mergeSettings 用 over 的非空字段覆盖 base；空值表示未提交，保留 base。
func mergeSettings(base, over pluginSettings) pluginSettings {
	set := func(dst *string, value string) {
		if strings.TrimSpace(value) != "" {
			*dst = strings.TrimSpace(value)
		}
	}
	set(&base.NginxMode, over.NginxMode)
	set(&base.DockerContainer, over.DockerContainer)
	set(&base.DockerConfDir, over.DockerConfDir)
	set(&base.HostConfDir, over.HostConfDir)
	return base
}

// applyPatch 把增量入参合并到当前生效配置；显式空串可清空容器名。
func applyPatch(current pluginSettings, patch settingsPatch) pluginSettings {
	if patch.NginxMode != nil {
		current.NginxMode = *patch.NginxMode
	}
	if patch.DockerContainer != nil {
		current.DockerContainer = strings.TrimSpace(*patch.DockerContainer)
	}
	if patch.DockerConfDir != nil {
		current.DockerConfDir = *patch.DockerConfDir
	}
	if patch.HostConfDir != nil {
		current.HostConfDir = *patch.HostConfDir
	}
	return current
}
