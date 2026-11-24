package boundary

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 异常输入（判据① 第 1 类）。
//
// 现状核实：仓内已有 17 个异常输入断言（methods_test.go 的 TestSiteInvalidParams /
// TestUnknownMethod / TestRootInjectionRejected 等），本文件**不重复造**——
// 只把最有代表性的 5 条搬到判据① 的边界目录，作为 boundary 套件的自包含基线，
// 其余留在原位（它们同样跑在 `make check` 的 go test ./... 里）。
//
// 搬过来的判据：入参畸形必须回**明确错误码**（E_INVALID / E_NOTFOUND），
// 且不落盘、不 panic。

// invalidCase 是一条异常输入用例：入参 → 期望错误码 + 期望文案片段。
type invalidCase struct {
	name    string
	params  string
	wantErr string
	wantMsg string
}

// TestBoundaryInvalidInput 断言畸形入参返回明确错误码且不写盘。
func TestBoundaryInvalidInput(t *testing.T) {
	t.Parallel()
	confDir := healthyConfDir(t)
	p := startPlugin(t, plainBin, confDir, nil)
	p.handshake(t)
	p.sanityCreate(t, "boundary-invalid.example.com")

	cases := []invalidCase{
		{name: "空 domain", params: `{}`, wantErr: pluginapi.E_INVALID, wantMsg: "domain"},
		{name: "大写 domain", params: `{"domain":"Boundary.Example.COM"}`, wantErr: pluginapi.E_INVALID, wantMsg: "小写"},
		{name: "路径穿越 domain", params: `{"domain":"../../etc/passwd"}`, wantErr: pluginapi.E_INVALID, wantMsg: "非法"},
		{name: "单标签 domain", params: `{"domain":"localhost"}`, wantErr: pluginapi.E_INVALID, wantMsg: "非法"},
		{name: "params 不是对象", params: `[]`, wantErr: pluginapi.E_INVALID, wantMsg: "JSON 对象"},
		{name: "抢占 default_server", params: `{"domain":"d.example.com","default_server":true}`, wantErr: pluginapi.E_INVALID, wantMsg: "default_server"},
		{name: "端口越界", params: `{"domain":"p.example.com","port":70000}`, wantErr: pluginapi.E_INVALID, wantMsg: "port"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			response := p.call(t, "site.create", tc.params, "r-invalid-"+tc.name)
			if response.Error == nil {
				t.Fatalf("%s: 期望明确错误码，实际成功: result=%s", tc.name, response.Result)
			}
			if response.Error.Code != tc.wantErr {
				t.Fatalf("%s: 错误码=%q 期望 %q（msg=%s）", tc.name, response.Error.Code, tc.wantErr, response.Error.Message)
			}
			if !strings.Contains(response.Error.Message, tc.wantMsg) {
				t.Fatalf("%s: 错误文案 %q 未包含 %q", tc.name, response.Error.Message, tc.wantMsg)
			}
			// 畸形入参绝不落盘：conf.d 里只应存在 sanity 站点。
			assertNoStrayVHost(t, confDir, tc.name)
		})
	}

	// 未知方法：必须回 E_NOTFOUND，不是 panic、不是空响应。
	response := p.call(t, "site.noSuchVerb", `{}`, "r-unknown")
	if response.Error == nil || response.Error.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("未知方法期望 %s，实际 error=%+v", pluginapi.E_NOTFOUND, response.Error)
	}

	// 收尾：优雅退出，证明上面所有错误路径都没有把会话循环搞死。
	_ = p.shutdown(t)
}

// assertNoStrayVHost 断言 conf.d 里没有除 sanity 站点之外的 vhost 文件。
func assertNoStrayVHost(t *testing.T, confDir, label string) {
	t.Helper()
	entries, err := os.ReadDir(confDir)
	if err != nil {
		t.Fatalf("%s: 读 conf.d 失败: %v", label, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "kore-site-") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if name == "kore-site-boundary-invalid.example.com.conf" {
			continue
		}
		t.Fatalf("%s: 畸形入参落盘了非预期 vhost: %s", label, filepath.Join(confDir, name))
	}
}

// TestBoundaryInvalidInputShape 断言 params 为 null / 缺字段时的响应结构稳定：
// error 与 result 不能同时给（协议 §9：错误响应不带 result）。
func TestBoundaryInvalidInputShape(t *testing.T) {
	t.Parallel()
	p := startPlugin(t, plainBin, healthyConfDir(t), nil)
	p.handshake(t)
	response := p.call(t, "site.create", `null`, "r-shape")
	if response.Error == nil {
		t.Fatalf("params=null 期望错误码，实际成功: %s", response.Result)
	}
	if len(response.Result) != 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(response.Result, &probe); err == nil && len(probe) > 0 {
			t.Fatalf("错误响应不应携带 result，实际: %s", response.Result)
		}
	}
	_ = p.shutdown(t)
}
