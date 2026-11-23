package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// expectedBusinessMethods 是本仓业务方法总数（0.3.x 的 16 个 + 0.4.0 clone + 0.4.1 setPassword/clearPassword = 20）。
const expectedBusinessMethods = 20

// expectedConfigMethods 是配置面板通用契约的两个方法：由 methods.go 的 dispatch 直接分发
// （`<id>.config.get` / `<id>.config.set`），**不走 verbRegistry**。
//
// 因此它们单独成组断言（必须存在、读=read、写=admin），而不是与 verb 做双向对齐：
// 把 config.* 塞进 verbRegistry 只为了让「双向断言」通过，会让分发 switch 与注册表
// 变成两个事实来源，且 config 方法并不实现 verbHandler 签名——那是错的修法。
var expectedConfigMethods = map[string]string{
	"config.get": "system:read",
	"config.set": "system:admin",
}

// expectedTotalMethods 是 info.json 应有的声明条数：16 业务 + 2 config。
const expectedTotalMethods = expectedBusinessMethods + 2

// expectedReadMethods 是本仓只读业务方法清单；其余业务方法必须是 system:admin。
// 这张表是"只读 / 写"分类的验收依据：把写方法误降为 read 会让只读会话获得写权限。
var expectedReadMethods = map[string]struct{}{
	"list": {}, "status": {}, "get": {}, "getRewrite": {}, "traffic": {},
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

// TestMethodCapabilitiesMatchVerbRegistry 防止「info.json 声明」与「实现」漂移。
//
// 断言分两组，对应插件里两条真实分发路径：
//  1. 业务方法（16 条）：与 siteverbs.go 的 verbRegistry 双向对齐（声明 ⊆ 实现、实现 ⊆ 声明），
//     并锁定只读 / 写分类；
//  2. config.*（2 条）：不与 verbRegistry 对齐，只断言「必须存在」且「get=read、set=admin」。
func TestMethodCapabilitiesMatchVerbRegistry(t *testing.T) {
	info := loadManifestDecl(t, "info.json")
	if info.ID != "site" {
		t.Fatalf("info.json id=%q，期望 site", info.ID)
	}
	if len(verbRegistry) != expectedBusinessMethods {
		t.Fatalf("verbRegistry 有 %d 个 verb，期望 %d：新增/删除方法必须同步 info.json 与能力分类",
			len(verbRegistry), expectedBusinessMethods)
	}
	if len(info.MethodCapabilities) != expectedTotalMethods {
		t.Fatalf("method_capabilities 声明 %d 条，期望 %d 条（%d 业务方法 + %d config 契约方法）",
			len(info.MethodCapabilities), expectedTotalMethods, expectedBusinessMethods, len(expectedConfigMethods))
	}

	declaredBusiness := make(map[string]string, expectedBusinessMethods)
	declaredConfig := make(map[string]string, len(expectedConfigMethods))
	prefix := info.ID + "."
	for method, capability := range info.MethodCapabilities {
		action, ok := strings.CutPrefix(method, prefix)
		if !ok || action == "" {
			t.Fatalf("方法键 %q 必须以插件自身 id 为域（应为 %s<action>）", method, prefix)
		}
		if _, ok := privilegedCapabilitySet[capability]; !ok {
			t.Fatalf("%s 的能力值 %q 不是 6 项特权能力之一", method, capability)
		}
		// 归类只看实现来源：verbRegistry 里有的算业务方法，否则必须落在 config 契约白名单内。
		if _, isVerb := verbRegistry[action]; isVerb {
			declaredBusiness[action] = capability
			continue
		}
		if _, isConfig := expectedConfigMethods[action]; isConfig {
			declaredConfig[action] = capability
			continue
		}
		t.Fatalf("info.json 声明了 %q，但它既不在 siteverbs.go 的 verbRegistry，也不在 config 契约方法清单（声明漂移）", method)
	}

	// 组 1（业务方法）：双向对齐 + 只读 / 写分类。
	if len(declaredBusiness) != expectedBusinessMethods {
		t.Fatalf("业务方法声明 %d 条，期望 %d 条（每个 verb 都必须有方法级能力）",
			len(declaredBusiness), expectedBusinessMethods)
	}
	for verb := range verbRegistry {
		if _, ok := declaredBusiness[verb]; !ok {
			t.Fatalf("siteverbs.go 注册了 verb %q，但 info.json 的 method_capabilities 未声明（声明漂移）", verb)
		}
	}
	for verb, capability := range declaredBusiness {
		want := "system:admin"
		if _, readOnly := expectedReadMethods[verb]; readOnly {
			want = "system:read"
		}
		if capability != want {
			t.Fatalf("verb %q 的能力=%q，期望 %q（只读方法只能 read，写方法必须 admin）", verb, capability, want)
		}
	}

	// 组 2（config 契约）：必须存在且读=read、写=admin。
	if len(declaredConfig) != len(expectedConfigMethods) {
		t.Fatalf("config 契约方法声明 %d 条，期望 %d 条（config.get / config.set 必须逐条声明）",
			len(declaredConfig), len(expectedConfigMethods))
	}
	for action, want := range expectedConfigMethods {
		got, ok := declaredConfig[action]
		if !ok {
			t.Fatalf("info.json 缺少 %s%s 的方法级声明：未声明会回退插件级 capabilities（system:admin），配置读取会按 admin 进审计", prefix, action)
		}
		if got != want {
			t.Fatalf("config 方法 %q 的能力=%q，期望 %q（读=read，写=admin）", action, got, want)
		}
	}
}

// TestMethodCapabilitiesDomainScoped 防回归：每个声明的键都必须以插件自身 id 为域
// （`<id>.<action>`）。内核 validate.go 对该条 fail-closed——写错一个键，整份清单加载失败、
// 插件不可安装，因此在提交前用单测拦住。
func TestMethodCapabilitiesDomainScoped(t *testing.T) {
	info := loadManifestDecl(t, "info.json")
	prefix := info.ID + "."
	for method := range info.MethodCapabilities {
		if !strings.HasPrefix(method, prefix) {
			t.Fatalf("方法键 %q 未以插件自身 id 为域：必须以 %q 开头", method, prefix)
		}
		if action := strings.TrimPrefix(method, prefix); action == "" || strings.HasPrefix(action, ".") {
			t.Fatalf("方法键 %q 的动作段为空或以点开头", method)
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
