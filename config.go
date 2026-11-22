package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单 manifest.json（只含握手字段，不含 size_bytes）：version / requires_kore /
// capabilities 与安装期 info.json 的握手字段保持一致，由 make dist 前置校验防漂移。
//
//go:embed manifest.json
var embeddedInfo []byte

// manifest 是 info.json 中插件运行期真正需要的字段，其余字段归内核校验。
type manifest struct {
	// ID 插件 id，缺省用于补 KORE_PLUGIN_ID。
	ID string `json:"id"`
	// Version 握手声明的插件版本。
	Version string `json:"version"`
	// Protocol 协议主版本，必须与 SDK 常量一致。
	Protocol string `json:"protocol"`
	// RequiresKore 握手声明的内核版本区间。
	RequiresKore string `json:"requires_kore"`
	// Capabilities 握手声明的能力，必须是清单声明的子集。
	Capabilities []string `json:"capabilities"`
}

// config 是插件运行期配置：环境变量 + 内嵌清单，不读任何配置文件。
type config struct {
	// socket 主壳创建的 UDS 路径（KORE_PLUGIN_SOCK）。
	socket string
	// id 内核登记 id（KORE_PLUGIN_ID），缺省取清单 id。
	id string
	// token 内核注入的可选令牌（KORE_PLUGIN_TOKEN），M1 仅随 hello 上报。
	token string
	// version 插件版本，取内嵌清单。
	version string
	// requiresKore 内核版本区间，取内嵌清单。
	requiresKore string
	// capabilities 能力声明，取内嵌清单。
	capabilities []string
	// startedAt 进程启动时刻，用于 kore.ping 的 uptime_s。
	startedAt time.Time
}

// loadConfig 读取 3 个协议环境变量并解析内嵌 manifest.json。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 manifest.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		startedAt:    time.Now(),
	}
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}
