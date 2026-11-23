package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// expectedBusinessMethods 是本仓业务方法总数（docs/PLUGIN-MATURITY.md §2 site 行：16）。
const expectedBusinessMethods = 16

// expectedReadMethods 是本仓只读方法清单；其余业务方法必须是 system:admin。
// 这张表是"只读 / 写"分类的验收依据：把写方法误降为 read 会让只读会话获得写权限。
var expectedReadMethods = map[string]struct{}{
	"list": {}, "status": {}, "get": {}, "getRewrite": {},
}

// privilegedCapabilitySet 是内核唯一白名单（internal/core/registry/validate.go：
// system:read|system:admin|net:read|net:write|proc:read|proc:write）。
var privilegedCapabilitySet = map[string]struct{}{
	"system:read": {}, "system:admin": {},
	"net:read": {}, "net:write": {},
	"proc:read": {}, "proc:write": {},
}

// manifestDeclFields 是 info.json / manifest.json 中版本同步与能力声明相关字段子集。
type manifestDeclFields struct {
	ID                 string            `json:"id"`
	Version            string            `json:"version"`
	Capabilities       []string          `json:"capabilities"`
	MethodCapabilities map[string]string `json:"method_capabilities"`
}

// loadManifestDecl 读取仓根 JSON（go test 的工作目录就是包目录 = 仓根）。
func loadManifestDecl(t *testing.T, path string) manifestDeclFields {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var decl manifestDeclFields
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return decl
}

// TestMethodCapabilitiesMatchVerbRegistry 防止「info.json 声明」与「siteverbs.go 实现」漂移：
// 双向断言（声明 ⊆ 实现、实现 ⊆ 声明），并锁定只读 / 写分类。
func TestMethodCapabilitiesMatchVerbRegistry(t *testing.T) {
	info := loadManifestDecl(t, "info.json")
	if info.ID != "site" {
		t.Fatalf("info.json id=%q，期望 site", info.ID)
	}
	if len(verbRegistry) != expectedBusinessMethods {
		t.Fatalf("verbRegistry 有 %d 个 verb，期望 %d：新增/删除方法必须同步 info.json 与能力分类",
			len(verbRegistry), expectedBusinessMethods)
	}
	if len(info.MethodCapabilities) != expectedBusinessMethods {
		t.Fatalf("method_capabilities 声明 %d 条，期望 %d 条（每个业务方法都必须有方法级能力）",
			len(info.MethodCapabilities), expectedBusinessMethods)
	}

	declared := make(map[string]string, len(info.MethodCapabilities))
	prefix := info.ID + "."
	for method, capability := range info.MethodCapabilities {
		action, ok := strings.CutPrefix(method, prefix)
		if !ok || action == "" {
			t.Fatalf("方法键 %q 必须以插件自身 id 为域（应为 %s<action>）", method, prefix)
		}
		if _, ok := verbRegistry[action]; !ok {
			t.Fatalf("info.json 声明了 %q，但 siteverbs.go 的 verbRegistry 没有对应 verb（声明漂移）", method)
		}
		declared[action] = capability
	}
	for verb := range verbRegistry {
		if _, ok := declared[verb]; !ok {
			t.Fatalf("siteverbs.go 注册了 verb %q，但 info.json 的 method_capabilities 未声明（声明漂移）", verb)
		}
	}

	for verb, capability := range declared {
		if _, ok := privilegedCapabilitySet[capability]; !ok {
			t.Fatalf("%s 的能力值 %q 不是 6 项特权能力之一", verb, capability)
		}
		want := "system:admin"
		if _, readOnly := expectedReadMethods[verb]; readOnly {
			want = "system:read"
		}
		if capability != want {
			t.Fatalf("verb %q 的能力=%q，期望 %q（只读方法只能 read，写方法必须 admin）", verb, capability, want)
		}
	}
}

// TestManifestVersionSynced 守住 G-17：info.json 与内嵌 manifest.json 的
// id/version/capabilities 必须一致（Makefile dist 也会比对，此处提前在单测拦住）。
func TestManifestVersionSynced(t *testing.T) {
	info := loadManifestDecl(t, "info.json")
	manifest := loadManifestDecl(t, "manifest.json")
	if manifest.ID != info.ID {
		t.Fatalf("manifest.id=%q 与 info.id=%q 不一致", manifest.ID, info.ID)
	}
	if manifest.Version != info.Version {
		t.Fatalf("manifest.version=%q 与 info.version=%q 不一致（发版必须同步 bump）", manifest.Version, info.Version)
	}
	if strings.Join(manifest.Capabilities, ",") != strings.Join(info.Capabilities, ",") {
		t.Fatalf("manifest.capabilities=%v 与 info.capabilities=%v 不一致", manifest.Capabilities, info.Capabilities)
	}
}
