package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// configField 是 PLUGIN-CONFIG §2 的字段描述：类型枚举固定，Default 用
// json.RawMessage 承载标量（G-24 允许的「方法结果载荷」受控例外）。
type configField struct {
	Key         string          `json:"key"`
	Label       string          `json:"label"`
	Type        string          `json:"type"`
	Default     json.RawMessage `json:"default"`
	Required    bool            `json:"required"`
	Help        string          `json:"help"`
	Options     []string        `json:"options,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
}

// configSchema 是 config.get 的表单描述（固定 title + fields 两字段）。
type configSchema struct {
	Title  string        `json:"title"`
	Fields []configField `json:"fields"`
}

// configGetResult / configSetParams / configSetResult 是配置契约的响应形状。
type configGetResult struct {
	Schema configSchema    `json:"schema"`
	Values json.RawMessage `json:"values"`
	Source string          `json:"source"`
}

type configSetParams struct {
	Values json.RawMessage `json:"values"`
}

type configSetResult struct {
	Saved           bool     `json:"saved"`
	Applied         bool     `json:"applied"`
	RestartRequired bool     `json:"restart_required"`
	Warnings        []string `json:"warnings"`
}

// nginxConfigSchema 描述容器模式与 PHP-FPM 的 5 个配置项；help 必须能指导小白操作。
func nginxConfigSchema() configSchema {
	return configSchema{Title: "nginx 集成", Fields: []configField{
		{
			Key: cfgNginxMode, Label: "nginx 运行方式", Type: "select",
			Options: []string{nginxModeAuto, nginxModeHost, nginxModeDocker, nginxModeOff},
			Default: json.RawMessage(`"auto"`), Required: true,
			Help: "auto：先找宿主 nginx，找不到再找运行中的 nginx 容器；也可强制 host / docker / off",
		},
		{
			Key: cfgDockerContainer, Label: "Docker 容器名", Type: "string",
			Default: json.RawMessage(`""`), Required: false,
			Help:        "容器模式下执行 nginx 的容器名（例如 kore-nginx）；nginx_mode=docker 时必填",
			Placeholder: "kore-nginx",
		},
		{
			Key: cfgDockerConfDir, Label: "容器内 conf.d 路径", Type: "string",
			Default: json.RawMessage(`"/etc/nginx/conf.d"`), Required: false,
			Help: "容器内 nginx 的 conf.d 目录；插件用它核对宿主目录是否真的挂载进容器",
		},
		{
			Key: cfgHostConfDir, Label: "宿主 conf.d 路径", Type: "string",
			Default: json.RawMessage(`"/etc/nginx/conf.d"`), Required: false,
			Help: "站点配置写入的宿主目录；容器模式下它必须已挂载到容器内的 conf.d",
		},
		{
			Key: cfgPHPFpmSocket, Label: "PHP-FPM socket 路径", Type: "string",
			Default: json.RawMessage(`""`), Required: false,
			Help:        "留空则自动探测 /run/php/php<版本>-fpm.sock；nginx 在容器内时填容器内路径（例如 /run/php/php8.3-fpm.sock）",
			Placeholder: "/run/php/php8.3-fpm.sock",
		},
	}}
}

// handleConfigGet 返回 schema、当前生效值与来源（file > env > default）。
func handleConfigGet() (any, *pluginapi.Error) {
	settings, source, err := loadPluginSettings()
	if err != nil {
		return nil, internalErr("读取插件配置失败: " + err.Error())
	}
	values, err := json.Marshal(settings)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	return configGetResult{Schema: nginxConfigSchema(), Values: values, Source: source}, nil
}

// handleConfigSet 执行「校验 → 原子写 → 生效」：写盘成功才返回 applied，
// 下一次业务调用重新读取 config.json，因此 5 个字段全部热生效（restart_required=false）。
func handleConfigSet(raw json.RawMessage) (any, *pluginapi.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, invalidParam("缺少 values 对象")
	}
	var params configSetParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParam("params 解析失败: " + err.Error())
	}
	if len(params.Values) == 0 || string(params.Values) == "null" {
		return nil, invalidParam("缺少 values 对象")
	}
	var patch settingsPatch
	if err := json.Unmarshal(params.Values, &patch); err != nil {
		return nil, invalidParam("values 解析失败: " + err.Error())
	}
	current, _, err := loadPluginSettings()
	if err != nil {
		// 旧配置已损坏时允许用一次完整提交覆盖修复，而不是把设置面板锁死。
		current = defaultSettings()
	}
	merged, err := validateSettings(applyPatch(current, patch))
	if err != nil {
		return nil, invalidParam(err.Error())
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return nil, internalErr("序列化配置失败: " + err.Error())
	}
	if err := writePluginConfig(payload); err != nil {
		return nil, internalErr("保存插件配置失败: " + err.Error())
	}
	return configSetResult{Saved: true, Applied: true, Warnings: []string{}}, nil
}

// writePluginConfig 原子写 <KORE_STATE_DIR>/config.json（0600 + fsync 文件与目录）。
func writePluginConfig(payload []byte) error {
	path := pluginConfigPath()
	if path == "" {
		return errors.New("KORE_STATE_DIR 未设置，无法保存插件配置")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建插件状态目录 %s: %w", dir, err)
	}
	if err := atomicWrite(path, payload); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
