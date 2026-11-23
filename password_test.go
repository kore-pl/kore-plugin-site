package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestApr1MatchesOpenSSL 固定向量来自 `openssl passwd -apr1 -salt testsalt secret123`。
func TestApr1MatchesOpenSSL(t *testing.T) {
	got := apr1Crypt("secret123", "testsalt")
	want := "$apr1$testsalt$nULVWJ.yTC71jzax6PfXK0"
	if got != want {
		t.Fatalf("apr1 与 openssl 不一致: got=%s want=%s", got, want)
	}
}

// TestSetPasswordWritesAuthBlock 设置密码：vhost 出现 auth 段、htpasswd 是 apr1 哈希、返回无明文。
func TestSetPasswordWritesAuthBlock(t *testing.T) {
	confDir := useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "auth.example.com"})

	raw := callOK(t, "site.setPassword", map[string]any{
		"domain": "auth.example.com", "username": "ops", "password": "s3cret-pass",
	})
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result["enabled"] != true || result["username"] != "ops" || result["changed"] != true {
		t.Fatalf("结果不合格: %v", result)
	}
	if _, leaked := result["password"]; leaked {
		t.Fatal("返回体不得含 password 字段")
	}
	if strings.Contains(string(raw), "s3cret-pass") || strings.Contains(string(raw), "$apr1$") {
		t.Fatalf("返回体不得回显明文或哈希: %s", raw)
	}
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-auth.example.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "auth_basic \"kore site auth.example.com\";") ||
		!strings.Contains(text, "auth_basic_user_file "+filepath.Join(confDir, "kore-site-auth.example.com.htpasswd")+";") {
		t.Fatalf("vhost 缺少 auth 段:\n%s", text)
	}
	entry, err := os.ReadFile(filepath.Join(confDir, "kore-site-auth.example.com.htpasswd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(entry), "ops:$apr1$") || strings.Contains(string(entry), "s3cret-pass") {
		t.Fatalf("htpasswd 内容不合格: %q", entry)
	}
}

// TestSetPasswordIdempotent 同用户同密码不重复改写；换密码则 changed=true。
func TestSetPasswordIdempotent(t *testing.T) {
	useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "idem.example.com"})
	callOK(t, "site.setPassword", map[string]any{"domain": "idem.example.com", "username": "ops", "password": "same-pass-1"})
	raw := callOK(t, "site.setPassword", map[string]any{"domain": "idem.example.com", "username": "ops", "password": "same-pass-1"})
	var result sitePasswordResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("相同密码应幂等: %+v", result)
	}
	raw = callOK(t, "site.setPassword", map[string]any{"domain": "idem.example.com", "username": "ops", "password": "new-pass-22"})
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatalf("换密码应 changed=true: %+v", result)
	}
}

// TestClearPasswordRestoresVhost 清除后 auth 段与密码文件都消失；再次清除幂等。
func TestClearPasswordRestoresVhost(t *testing.T) {
	confDir := useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "clear.example.com"})
	callOK(t, "site.setPassword", map[string]any{"domain": "clear.example.com", "username": "ops", "password": "clear-pass-9"})
	raw := callOK(t, "site.clearPassword", map[string]any{"domain": "clear.example.com"})
	var result sitePasswordResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Enabled || !result.Changed {
		t.Fatalf("清除结果不合格: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(confDir, "kore-site-clear.example.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "auth_basic") {
		t.Fatalf("清除后 vhost 仍有 auth 段:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-clear.example.com.htpasswd")); !os.IsNotExist(err) {
		t.Fatalf("清除后密码文件应删除: %v", err)
	}
	raw = callOK(t, "site.clearPassword", map[string]any{"domain": "clear.example.com"})
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Enabled {
		t.Fatalf("重复清除应幂等: %+v", result)
	}
}

// TestSetPasswordValidation 弱口令/非法用户名/不存在站点要明确报错。
func TestSetPasswordValidation(t *testing.T) {
	useTempConfDir(t)
	callOK(t, "site.create", map[string]any{"domain": "valid.example.com"})
	callFail(t, "site.setPassword", map[string]any{"domain": "valid.example.com", "username": "ops", "password": "short"}, pluginapi.E_INVALID, "至少")
	callFail(t, "site.setPassword", map[string]any{"domain": "valid.example.com", "username": "bad user", "password": "long-enough-1"}, pluginapi.E_INVALID, "用户名")
	callFail(t, "site.setPassword", map[string]any{"domain": "nope.example.com", "username": "ops", "password": "long-enough-1"}, pluginapi.E_NOTFOUND, "")
}
