package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestCloneCopiesConfigAndRoot 克隆带伪静态的站点并复制 state 内的站点根。
func TestCloneCopiesConfigAndRoot(t *testing.T) {
	confDir := useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)
	sourceRoot := filepath.Join(stateDir, "src")
	targetRoot := filepath.Join(stateDir, "dst")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "index.html"), []byte("<h1>src</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	callOK(t, "site.create", map[string]any{"domain": "src.example.com", "root": sourceRoot})
	callOK(t, "site.setRewrite", map[string]any{"domain": "src.example.com", "template": "wordpress"})

	var result siteCloneResult
	raw := callOK(t, "site.clone", map[string]any{
		"source_domain": "src.example.com", "new_domain": "clone.example.com",
		"root": targetRoot, "copy_root": true,
	})
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析 clone 结果: %v", err)
	}
	if !result.Applied || !result.RootCopied || result.CopiedFiles < 1 {
		t.Fatalf("克隆结果不合格: %+v", result)
	}
	if !result.RewriteCopied || result.SSLCopied || result.ProxyCopied {
		t.Fatalf("配置复制标记不合格: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-clone.example.com.conf"))
	if err != nil {
		t.Fatalf("读取克隆 vhost: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "server_name clone.example.com;") || strings.Contains(text, "server_name src.example.com;") {
		t.Fatalf("克隆 vhost 的域名未替换:\n%s", text)
	}
	if !strings.Contains(text, "root "+targetRoot+";") {
		t.Fatalf("克隆 vhost 的 root 未替换:\n%s", text)
	}
	if !strings.Contains(text, "kore-site:rewrite") {
		t.Fatalf("伪静态块未复制:\n%s", text)
	}
	copied, err := os.ReadFile(filepath.Join(targetRoot, "index.html"))
	if err != nil || string(copied) != "<h1>src</h1>" {
		t.Fatalf("站点根未复制: %v %q", err, copied)
	}
}

// TestCloneConflictsAndValidation 冲突/不存在/同域名/白名单外复制都要明确报错。
func TestCloneConflictsAndValidation(t *testing.T) {
	useTempConfDir(t)
	stateDir := t.TempDir()
	t.Setenv("KORE_STATE_DIR", stateDir)
	callOK(t, "site.create", map[string]any{"domain": "src.example.com", "root": filepath.Join(stateDir, "src")})
	callOK(t, "site.create", map[string]any{"domain": "taken.example.com", "root": filepath.Join(stateDir, "taken")})

	callFail(t, "site.clone", map[string]any{"source_domain": "src.example.com", "new_domain": "taken.example.com"}, pluginapi.E_CONFLICT, "已存在")
	callFail(t, "site.clone", map[string]any{"source_domain": "missing.example.com", "new_domain": "new.example.com"}, pluginapi.E_NOTFOUND, "不在本插件清单")
	callFail(t, "site.clone", map[string]any{"source_domain": "src.example.com", "new_domain": "src.example.com"}, pluginapi.E_INVALID, "不能相同")
	// copy_root 指向 state 外：ADR-0023 零侵入，必须拒绝。
	callFail(t, "site.clone", map[string]any{
		"source_domain": "src.example.com", "new_domain": "outside.example.com",
		"root": "/tmp/outside-state", "copy_root": true,
	}, pluginapi.E_INVALID, "KORE_STATE_DIR")
}

// TestCloneWithoutCopySharesRoot 默认只克隆配置，不复制目录；结果与提示如实说明。
func TestCloneWithoutCopySharesRoot(t *testing.T) {
	useTempConfDir(t)
	var result siteCloneResult
	callOK(t, "site.create", map[string]any{"domain": "share.example.com"})
	raw := callOK(t, "site.clone", map[string]any{
		"source_domain": "share.example.com", "new_domain": "share2.example.com",
	})
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.RootCopied || result.CopiedFiles != 0 {
		t.Fatalf("未请求 copy_root 时不得复制目录: %+v", result)
	}
	if !strings.Contains(result.Hint, "未复制目录") {
		t.Fatalf("应如实提示共用根目录: %q", result.Hint)
	}
}
