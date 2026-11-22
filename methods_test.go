package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// invoke 发一个请求并读回响应与 serve 结束状态（每个用例独立连接）。
func invoke(t *testing.T, method, reqID string, params json.RawMessage) *pluginapi.Response {
	t.Helper()
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := startServe(pluginConn)
	writeRequest(t, kernelConn, method, reqID, params)
	response := readResponse(t, kernelConn)
	writeRequest(t, kernelConn, "kore.shutdown", "r-shutdown", nil)
	if shutdown := readResponse(t, kernelConn); string(shutdown.Result) != `{"ok":true}` {
		t.Fatalf("shutdown 结果=%s", shutdown.Result)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
	return response
}

// TestSiteMethods 覆盖三个站点方法的成功路径与 req_id 回显。
func TestSiteMethods(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		params     json.RawMessage
		wantResult string
	}{
		{name: "list 裸名", method: "list", wantResult: `{"sites":[],"count":0,"applied":false}`},
		{name: "list 全名", method: "site.list", wantResult: `{"sites":[],"count":0,"applied":false}`},
		{
			name: "create 成功", method: "site.create", params: json.RawMessage(`{"domain":"example.com"}`),
			wantResult: `{"domain":"example.com","vhost_path":"conf.d/example.com.conf","default_server":false,"applied":false,"note":"M2-A 第一批：仅参数校验与结构化返回，未改动 nginx；独立 conf.d vhost + include 留待 M2-A 第二批"}`,
		},
		{
			name: "delete 成功", method: "site.delete", params: json.RawMessage(`{"domain":"a.example.com"}`),
			wantResult: `{"domain":"a.example.com","vhost_path":"conf.d/a.example.com.conf","applied":false,"note":"M2-A 第一批：仅参数校验与结构化返回，未改动 nginx；独立 conf.d vhost + include 留待 M2-A 第二批"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := invoke(t, tc.method, "r-1", tc.params)
			if response.ReqID != "r-1" {
				t.Fatalf("req_id=%q，期望回显 r-1", response.ReqID)
			}
			if response.Error != nil {
				t.Fatalf("意外错误: %v", response.Error)
			}
			if string(response.Result) != tc.wantResult {
				t.Fatalf("result=%s\n期望 %s", response.Result, tc.wantResult)
			}
		})
	}
}

// TestSiteInvalidParams 覆盖非法参数：缺字段、路径穿越、大写、default_server。
func TestSiteInvalidParams(t *testing.T) {
	cases := []struct {
		name     string
		params   json.RawMessage
		contains string
	}{
		{name: "缺少 domain", params: json.RawMessage(`{}`), contains: "缺少必填字段 domain"},
		{name: "params 非对象", params: json.RawMessage(`"x"`), contains: "params 必须是 JSON 对象"},
		{name: "路径穿越", params: json.RawMessage(`{"domain":"../etc/nginx"}`), contains: "domain 非法"},
		{name: "大写域名", params: json.RawMessage(`{"domain":"Example.COM"}`), contains: "小写"},
		{name: "单标签", params: json.RawMessage(`{"domain":"localhost"}`), contains: "domain 非法"},
		{name: "默认站点", params: json.RawMessage(`{"domain":"example.com","default_server":true}`), contains: "default_server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := invoke(t, "site.create", "r-1", tc.params)
			if response.Error == nil {
				t.Fatalf("期望 E_INVALID，实际 result=%s", response.Result)
			}
			if response.Error.Code != "E_INVALID" {
				t.Fatalf("error code=%s，期望 E_INVALID", response.Error.Code)
			}
			if !strings.Contains(response.Error.Message, tc.contains) {
				t.Fatalf("error message=%q，期望包含 %q", response.Error.Message, tc.contains)
			}
		})
	}
}

// TestUnknownMethod 未知方法回 E_NOTFOUND。
func TestUnknownMethod(t *testing.T) {
	response := invoke(t, "site.nope", "r-1", nil)
	if response.Error == nil || response.Error.Code != "E_NOTFOUND" {
		t.Fatalf("error=%v，期望 E_NOTFOUND", response.Error)
	}
}
