package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callOK 发一个业务请求并断言无错误，返回原始 result。
func callOK(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("序列化 %s 入参: %v", method, err)
	}
	response := invoke(t, method, "r-"+method, raw)
	if response.Error != nil {
		t.Fatalf("%s 失败: code=%s message=%s", method, response.Error.Code, response.Error.Message)
	}
	return response.Result
}

// callFail 发一个业务请求并断言错误码与消息片段。
func callFail(t *testing.T, method string, params any, code, contains string) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("序列化 %s 入参: %v", method, err)
	}
	response := invoke(t, method, "r-"+method, raw)
	if response.Error == nil {
		t.Fatalf("%s 应失败（期望 %s），实际 result=%s", method, code, response.Result)
	}
	if response.Error.Code != code {
		t.Fatalf("%s 错误码=%s，期望 %s（message=%s）", method, response.Error.Code, code, response.Error.Message)
	}
	if contains != "" && !strings.Contains(response.Error.Message, contains) {
		t.Fatalf("%s 错误消息=%q，期望包含 %q", method, response.Error.Message, contains)
	}
}

// decodeResult 把结果载荷解析成具体类型。
func decodeResult[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解析结果失败: %v（%s）", err, raw)
	}
	return v
}

// createTestSite 在临时 conf.d 建站并返回 vhost 路径与站点根目录。
func createTestSite(t *testing.T, dir, domain string) (string, string) {
	t.Helper()
	root := filepath.Join(dir, "www-"+domain)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根目录: %v", err)
	}
	callOK(t, "site.create", map[string]any{"domain": domain, "root": root})
	return filepath.Join(dir, "kore-site-"+domain+".conf"), root
}

// fakeCertFile 写一个占位证书/私钥文件（校验只要求存在且是普通文件）。
func fakeCertFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("-----BEGIN PLACEHOLDER-----\nMIIB\n-----END PLACEHOLDER-----\n"), 0o600); err != nil {
		t.Fatalf("写占位证书: %v", err)
	}
	return path
}

// readVHost 读取 vhost 文本，读取失败即终止用例。
func readVHost(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 vhost %s: %v", path, err)
	}
	return string(b)
}

// mustContain 断言文本包含全部片段。
func mustContain(t *testing.T, content string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Fatalf("vhost 缺少 %q，实际内容:\n%s", want, content)
		}
	}
}

// mustNotContain 断言文本不含任何被禁止片段。
func mustNotContain(t *testing.T, content string, forbidden ...string) {
	t.Helper()
	for _, item := range forbidden {
		if strings.Contains(content, item) {
			t.Fatalf("vhost 不应包含 %q，实际内容:\n%s", item, content)
		}
	}
}
