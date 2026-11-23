package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultSiteRootUsesPluginData ADR-0023 阶段 C：注入 KORE_PLUGIN_DATA 后默认根落在安装目录内。
func TestDefaultSiteRootUsesPluginData(t *testing.T) {
	data := t.TempDir()
	t.Setenv(pluginDataEnv, data)
	root, inData := defaultSiteRoot("a.example.com")
	if !inData || root != filepath.Join(data, "sites", "a.example.com") {
		t.Fatalf("默认根应为 $KORE_PLUGIN_DATA/sites/<domain>: %q inData=%v", root, inData)
	}
}

// TestDefaultSiteRootFallsBackWithoutEnv 缺变量时回退 /var/www 并标记非零侵入模式。
func TestDefaultSiteRootFallsBackWithoutEnv(t *testing.T) {
	t.Setenv(pluginDataEnv, "")
	root, inData := defaultSiteRoot("b.example.com")
	if inData || root != "/var/www/b.example.com" {
		t.Fatalf("缺变量应回退 /var/www/<domain>: %q inData=%v", root, inData)
	}
}

// TestCreateUsesPluginDataRoot 新建站点（未显式 root）落插件数据根。
func TestCreateUsesPluginDataRoot(t *testing.T) {
	confDir := useTempConfDir(t)
	data := t.TempDir()
	t.Setenv(pluginDataEnv, data)
	callOK(t, "site.create", map[string]any{"domain": "data.example.com"})
	want := filepath.Join(data, "sites", "data.example.com")
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-data.example.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "root "+want+";") {
		t.Fatalf("vhost 未写入插件数据根:\n%s", content)
	}
}

// TestExplicitRootWins 显式 root 永远优先（阶段 C 不动用户写的路径）。
func TestExplicitRootWins(t *testing.T) {
	confDir := useTempConfDir(t)
	data := t.TempDir()
	t.Setenv(pluginDataEnv, data)
	explicit := filepath.Join(t.TempDir(), "custom-root")
	callOK(t, "site.create", map[string]any{"domain": "explicit.example.com", "root": explicit})
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-explicit.example.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "root "+explicit+";") {
		t.Fatalf("显式 root 必须原样保留:\n%s", content)
	}
}

// TestExistingSiteRootNotMigrated 既有站点重写时不因默认值变化被搬走（关键回归）。
func TestExistingSiteRootNotMigrated(t *testing.T) {
	confDir := useTempConfDir(t)
	t.Setenv(pluginDataEnv, "")
	callOK(t, "site.create", map[string]any{"domain": "legacy.example.com", "root": "/var/www/legacy.example.com"})
	data := t.TempDir()
	t.Setenv(pluginDataEnv, data)
	callOK(t, "site.create", map[string]any{"domain": "legacy.example.com"})
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-legacy.example.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "root /var/www/legacy.example.com;") {
		t.Fatalf("既有站点根不得迁移:\n%s", content)
	}
}

// TestOutsideInstallMarked 安装目录外的站点在 status/list 里被标记（只读提示，不搬迁）。
func TestOutsideInstallMarked(t *testing.T) {
	useTempConfDir(t)
	install := t.TempDir()
	t.Setenv(installDirEnv, install)
	t.Setenv(pluginDataEnv, "")
	callOK(t, "site.create", map[string]any{"domain": "outside.example.com", "root": "/var/www/outside.example.com"})

	raw := callOK(t, "site.status", map[string]any{"domain": "outside.example.com"})
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status["outside_install"] != true {
		t.Fatalf("status 应标记 outside_install: %v", status)
	}
	if hint, _ := status["outside_install_hint"].(string); !strings.Contains(hint, "site.move") {
		t.Fatalf("hint 应提示 site.move: %q", hint)
	}
	raw = callOK(t, "site.list", map[string]any{})
	var list siteListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, site := range list.Sites {
		if site.Domain == "outside.example.com" {
			found = true
			if !site.OutsideInstall || !strings.Contains(site.OutsideInstallHint, "site.move") {
				t.Fatalf("list 应标记 outside_install: %+v", site)
			}
		}
	}
	if !found {
		t.Fatal("列表里没有 outside.example.com")
	}

	// 安装目录内的站点不标记。
	inside := filepath.Join(install, "data", "plugin-data", "site", "sites", "inside.example.com")
	callOK(t, "site.create", map[string]any{"domain": "inside.example.com", "root": inside})
	raw = callOK(t, "site.status", map[string]any{"domain": "inside.example.com"})
	var insideStatus map[string]any
	if err := json.Unmarshal(raw, &insideStatus); err != nil {
		t.Fatal(err)
	}
	if insideStatus["outside_install"] == true {
		t.Fatalf("安装目录内不应标记: %v", insideStatus)
	}
}
