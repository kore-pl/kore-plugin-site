// Package main 是 kore M2 的 site 插件：nginx 站点管理。
//
// 当前版本（0.1.0）只完成协议握手与 site.list / site.create / site.delete
// 的参数校验和结构化返回，不实际改动 nginx 配置；真实 conf.d vhost 写入
// 留待 M2-A 第二批。
//
// 协议面（kore.plugin.v1）：连接内核注入的 UDS → kore.hello 握手 → 会话循环
// 读 request、按 method 分发并回 response；stdout 留给协议，日志一律写 stderr。
package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
)

// main 按退出码区分失败：0 优雅退出、1 启动或协议失败。
func main() {
	logger := newLogger(os.Getenv("KORE_LOG_LEVEL"))
	if err := run(logger); err != nil {
		logger.Error("插件退出", "err", err)
		os.Exit(1)
	}
}

// run 依次完成建连、hello 握手与会话循环；收到 kore.shutdown 后返回 nil。
func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", cfg.socket)
	if err != nil {
		return fmt.Errorf("连接主壳 socket %s: %w", cfg.socket, err)
	}
	defer func() { _ = conn.Close() }()
	if err := handshake(conn, cfg); err != nil {
		return err
	}
	logger.Info("hello 完成", "plugin_id", cfg.id, "version", cfg.version)
	return serve(conn, cfg, logger)
}
