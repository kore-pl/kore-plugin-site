package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const sampleAccessLog = `203.0.113.7 - - [23/Nov/2026:10:00:01 +0800] "GET /index.html HTTP/1.1" 200 1024 "-" "curl/8"
203.0.113.7 - - [23/Nov/2026:10:00:02 +0800] "GET /static/app.js HTTP/1.1" 200 2048 "https://example.com/" "Mozilla/5.0"
198.51.100.9 - - [23/Nov/2026:10:00:03 +0800] "POST /api/login HTTP/1.1" 401 128 "-" "curl/8"
198.51.100.9 - - [23/Nov/2026:10:00:04 +0800] "GET /index.html?from=nav HTTP/1.1" 304 - "-" "Mozilla/5.0"
192.0.2.42 - - [23/Nov/2026:10:00:05 +0800] "GET /missing HTTP/1.1" 404 64 "-" "curl/8"
garbage line without format
`

// TestTrafficParsesAccessLog 只读解析日志：统计总量/字节/状态/热门路径与 IP，跳过无法解析行。
func TestTrafficParsesAccessLog(t *testing.T) {
	confDir := useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)
	callOK(t, "site.create", map[string]any{"domain": "traffic.example.com"})
	logPath := filepath.Join(stateDir, "access.log")
	if err := os.WriteFile(logPath, []byte(sampleAccessLog), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = confDir
	raw := callOK(t, "site.traffic", map[string]any{"domain": "traffic.example.com", "log_path": logPath})
	var result siteTrafficResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.Parsed != 5 || result.Skipped != 1 {
		t.Fatalf("解析统计不合格: %+v", result)
	}
	if result.Total != 5 || result.UniqueIPs != 3 || result.BytesSent != 3264 {
		t.Fatalf("汇总不合格: total=%d ips=%d bytes=%d", result.Total, result.UniqueIPs, result.BytesSent)
	}
	if len(result.Status) != 3 {
		t.Fatalf("状态分类应有 3 类（2xx/3xx/4xx）: %+v", result.Status)
	}
	if result.TopPaths[0].Path != "/index.html" || result.TopPaths[0].Count != 2 {
		t.Fatalf("热门路径不合格: %+v", result.TopPaths)
	}
	if len(result.TopIPs) < 2 || result.TopIPs[0].Count != 2 || result.TopIPs[1].Count != 2 {
		t.Fatalf("热门 IP 应为两个并列计数 2: %+v", result.TopIPs)
	}
	if result.TopIPs[0].IP+result.TopIPs[1].IP != "198.51.100.9203.0.113.7" {
		t.Fatalf("热门 IP 集合不合格: %+v", result.TopIPs)
	}

	// 非法 log_path（不在 /var/log 或 state 内）必须拒绝，避免任意文件读取。
	callFail(t, "site.traffic", map[string]any{"domain": "traffic.example.com", "log_path": "/etc/shadow"}, pluginapi.E_INVALID, "log_path")
	// 站点不存在。
	callFail(t, "site.traffic", map[string]any{"domain": "missing.example.com"}, pluginapi.E_NOTFOUND, "")
}

// TestTrafficEmptyState 没有可读日志时返回 available=false + 明确 hint，不伪造数据。
func TestTrafficEmptyState(t *testing.T) {
	useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "empty.example.com"})
	raw := callOK(t, "site.traffic", map[string]any{"domain": "empty.example.com"})
	var result siteTrafficResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Available || result.Total != 0 {
		t.Fatalf("无数据时不得返回统计: %+v", result)
	}
	if result.Reason == "" || result.Hint == "" {
		t.Fatalf("空态必须给原因与 hint: %+v", result)
	}
	if len(result.Tried) == 0 {
		t.Fatal("应列出尝试过的日志路径")
	}
}

// TestTrafficEmptyLogFile 文件存在但没有可解析行 → no_entries 空态。
func TestTrafficEmptyLogFile(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)
	useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "noentry.example.com"})
	logPath := filepath.Join(stateDir, "empty.log")
	if err := os.WriteFile(logPath, []byte("not a combined line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := callOK(t, "site.traffic", map[string]any{"domain": "noentry.example.com", "log_path": logPath})
	var result siteTrafficResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Available || result.Reason != "no_entries" {
		t.Fatalf("应返回 no_entries 空态: %+v", result)
	}
}
