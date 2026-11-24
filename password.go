package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 站点级 basic auth：密码只以 Apache apr1（MD5-crypt）哈希写入 htpasswd 文件，
// 明文既不落盘也不进 vhost、不进任何返回体；nginx 原生支持 $apr1$。
// 与既有 L2 特性一致，auth 段作为受管注释块渲染进 vhost，走 applyEdit 的
// 「原子写 -> nginx -t -> reload -> 失败回滚」闭环；htpasswd 文件与 vhost 同目录
// （既有 host_conf_dir 写路径内，零侵入 G-31）。

const (
	blockKeyAuth    = "auth"
	authHashScheme  = "apr1"
	authFilePrefix  = "kore-site-"
	authFileSuffix  = ".htpasswd"
	authMinPassword = 8
	authMaxPassword = 128
)

// authState 是 vhost 里 auth 段的解析结果；用户名/哈希在 htpasswd 文件里，不重复存。
type authState struct {
	HashFile string
}

// sitePasswordParams 是 setPassword 的入参。
type sitePasswordParams struct {
	Domain   string `json:"domain"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// sitePasswordResult 是设置/清除密码的结果：只回显用户名与状态，永不回显密码或哈希。
type sitePasswordResult struct {
	nginxStatus
	Domain        string `json:"domain"`
	Username      string `json:"username,omitempty"`
	Enabled       bool   `json:"enabled"`
	Changed       bool   `json:"changed"`
	AuthFile      string `json:"auth_file,omitempty"`
	HashScheme    string `json:"hash_scheme"`
	Applied       bool   `json:"applied"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// randomSalt 生成 8 位 itoa64 盐（apr1 约定），用 crypto/rand 避免可预测。
func randomSalt() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况下退回固定盐：哈希仍有效，只是盐不可预测性降低。
		return "koresalt"
	}
	for i := range buf {
		buf[i] = apr1Alphabet[int(buf[i])%len(apr1Alphabet)]
	}
	return string(buf)
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

func authFilePath(domain string) string {
	return filepath.Join(filepath.Dir(vhostPath(domain)), authFilePrefix+domain+authFileSuffix)
}

// writeAuthBlock 在 server 块内写出 basic auth 两行；realm 用域名便于浏览器提示辨认。
func writeAuthBlock(b *strings.Builder, st siteState) {
	if st.Auth == nil || st.Auth.HashFile == "" {
		return
	}
	b.WriteString("    " + beginMarker(blockKeyAuth) + "\n")
	b.WriteString("    auth_basic \"kore site " + st.Domain + "\";\n")
	b.WriteString("    auth_basic_user_file " + st.Auth.HashFile + ";\n")
	b.WriteString("    " + endMarker(blockKeyAuth) + "\n")
}

// parseAuthState 从 vhost 中还原 auth 段；未启用返回 nil。
func parseAuthState(content string) *authState {
	file := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "auth_basic_user_file ") {
			file = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "auth_basic_user_file ")), ";")
			file = strings.Trim(file, "\"")
		}
	}
	if file == "" {
		return nil
	}
	return &authState{HashFile: file}
}

// validatePassword 拒绝空弱口令与控制字符（basic auth 明文过网络，必须够长）。
func validatePassword(password string) *pluginapi.Error {
	if len(password) < authMinPassword {
		return invalidParam(fmt.Sprintf("密码至少 %d 个字符（basic auth 会以明文经过网络，建议配合 HTTPS）", authMinPassword))
	}
	if len(password) > authMaxPassword {
		return invalidParam(fmt.Sprintf("密码最长 %d 个字符", authMaxPassword))
	}
	if strings.ContainsAny(password, "\x00\n\r") {
		return invalidParam("密码不能包含换行或 NUL 字符")
	}
	return nil
}

func validateUsername(username string) *pluginapi.Error {
	if !usernamePattern.MatchString(username) {
		return invalidParam("用户名只允许字母、数字、点、下划线、连字符，长度 1..32，且以字母或数字开头")
	}
	return nil
}

// apr1Crypt 生成 Apache MD5-crypt（$apr1$）哈希，nginx/auth_basic 原生支持。
// 实现严格按 apr_md5_encode 规格；单测用 openssl passwd -apr1 的输出做对照。
func apr1Crypt(password, salt string) string {
	const magic = "$apr1$"
	salt = strings.TrimPrefix(salt, magic)
	if i := strings.IndexByte(salt, '$'); i >= 0 {
		salt = salt[:i]
	}
	if len(salt) > 8 {
		salt = salt[:8]
	}
	if salt == "" {
		salt = "koresalt"
	}
	pw := []byte(password)
	ctx := md5.New()
	ctx.Write(pw)
	ctx.Write([]byte(magic))
	ctx.Write([]byte(salt))
	ctx1 := md5.New()
	ctx1.Write(pw)
	ctx1.Write([]byte(salt))
	ctx1.Write(pw)
	final := ctx1.Sum(nil)
	for pl := len(pw); pl > 0; pl -= 16 {
		n := pl
		if n > 16 {
			n = 16
		}
		ctx.Write(final[:n])
	}
	for i := len(pw); i != 0; i >>= 1 {
		if i&1 != 0 {
			ctx.Write([]byte{0})
		} else {
			ctx.Write(pw[:1])
		}
	}
	sum := ctx.Sum(nil)
	for i := 0; i < 1000; i++ {
		c := md5.New()
		if i&1 != 0 {
			c.Write(pw)
		} else {
			c.Write(sum)
		}
		if i%3 != 0 {
			c.Write([]byte(salt))
		}
		if i%7 != 0 {
			c.Write(pw)
		}
		if i&1 != 0 {
			c.Write(sum)
		} else {
			c.Write(pw)
		}
		sum = c.Sum(nil)
	}
	return magic + salt + "$" + apr1Encode(sum)
}

const apr1Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// apr1Encode 是 crypt 的 itoa64 变体：低位在前、4 字符一组，末尾 2 字符。
func apr1Encode(d []byte) string {
	out := make([]byte, 0, 22)
	step := func(a, b, c byte, n int) {
		w := uint32(a)<<16 | uint32(b)<<8 | uint32(c)
		for i := 0; i < n; i++ {
			out = append(out, apr1Alphabet[w&0x3f])
			w >>= 6
		}
	}
	step(d[0], d[6], d[12], 4)
	step(d[1], d[7], d[13], 4)
	step(d[2], d[8], d[14], 4)
	step(d[3], d[9], d[15], 4)
	step(d[4], d[10], d[5], 4)
	step(0, 0, d[11], 2)
	return string(out)
}

// readAuthEntry 读取 htpasswd 的首行（本插件只管理单用户），返回用户名与哈希。
func readAuthEntry(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			return line[:i], line[i+1:], nil
		}
	}
	return "", "", errors.New("htpasswd 文件里没有有效条目")
}

// writeAuthFile 以 0644 原子写入 htpasswd（nginx worker 需要可读；内容只有哈希）。
func writeAuthFile(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kore-auth-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// setPassword 为站点启用 basic auth：先生成哈希文件，再走 applyEdit 写 vhost 并校验 reload；
// 任一步失败都回滚 vhost 与 htpasswd，不留下"有 auth 段但无密码文件"的半成品。
func (s *siteService) setPassword(p sitePasswordParams) (sitePasswordResult, *pluginapi.Error) {
	domain := strings.TrimSpace(p.Domain)
	if perr := validateDomain(domain); perr != nil {
		return sitePasswordResult{}, perr
	}
	username := strings.TrimSpace(p.Username)
	if perr := validateUsername(username); perr != nil {
		return sitePasswordResult{}, perr
	}
	if perr := validatePassword(p.Password); perr != nil {
		return sitePasswordResult{}, perr
	}
	if perr := s.preflightWrite(); perr != nil {
		return sitePasswordResult{}, perr
	}
	st, _, perr := readSiteState(domain)
	if perr != nil {
		return sitePasswordResult{}, perr
	}
	file := authFilePath(domain)
	previous, prevErr := os.ReadFile(file)
	changed := true
	if prevErr == nil {
		if oldUser, oldHash, err := readAuthEntry(file); err == nil && oldUser == username {
			// 同用户：用旧盐重算，密码没变就不产生无谓 reload（幂等）。
			if strings.HasPrefix(oldHash, "$apr1$") {
				parts := strings.Split(oldHash, "$")
				if len(parts) >= 4 && apr1Crypt(p.Password, parts[2]) == oldHash {
					changed = false
				}
			}
		}
	}
	salt := randomSalt()
	entry := username + ":" + apr1Crypt(p.Password, salt) + "\n"
	if err := writeAuthFile(file, entry); err != nil {
		if perr := needRootForConfDir(s.store.dir, err); perr != nil {
			return sitePasswordResult{}, perr
		}
		return sitePasswordResult{}, internalErr("写入密码文件失败: " + err.Error())
	}
	restoreAuth := func() {
		if prevErr == nil {
			_ = writeAuthFile(file, string(previous))
		} else {
			_ = os.Remove(file)
		}
	}
	st.Auth = &authState{HashFile: file}
	apply, _, perr := s.applyEdit(domain, func(state *siteState) *pluginapi.Error {
		state.Auth = &authState{HashFile: file}
		return nil
	})
	if perr != nil {
		restoreAuth()
		return sitePasswordResult{}, perr
	}
	result := sitePasswordResult{
		nginxStatus:   s.runner.status(),
		Domain:        domain,
		Username:      username,
		Enabled:       true,
		Changed:       changed,
		AuthFile:      file,
		HashScheme:    authHashScheme,
		Applied:       apply.Applied,
		TestPassed:    apply.TestPassed,
		Reloaded:      apply.Reloaded,
		TestSkipped:   apply.TestSkipped,
		ReloadSkipped: apply.ReloadSkipped,
	}
	if !changed {
		result.Hint = "用户名与密码未变化，vhost 无需改写（未触发 reload）。"
	}
	return result, nil
}

// clearPassword 关闭 basic auth：移除 vhost 里的 auth 段并删除 htpasswd；
// 本就没有时幂等返回 changed=false。
func (s *siteService) clearPassword(domain string) (sitePasswordResult, *pluginapi.Error) {
	domain = strings.TrimSpace(domain)
	if perr := validateDomain(domain); perr != nil {
		return sitePasswordResult{}, perr
	}
	if perr := s.preflightWrite(); perr != nil {
		return sitePasswordResult{}, perr
	}
	st, _, perr := readSiteState(domain)
	if perr != nil {
		return sitePasswordResult{}, perr
	}
	if st.Auth == nil {
		return sitePasswordResult{
			nginxStatus: s.runner.status(), Domain: domain, Enabled: false, Changed: false,
			HashScheme: authHashScheme, Applied: true, TestSkipped: true, ReloadSkipped: true,
			Hint: "该站点本来就没有开启密码访问。",
		}, nil
	}
	file := st.Auth.HashFile
	previous, prevErr := os.ReadFile(file)
	apply, _, perr := s.applyEdit(domain, func(state *siteState) *pluginapi.Error {
		state.Auth = nil
		return nil
	})
	if perr != nil {
		return sitePasswordResult{}, perr
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		if prevErr == nil {
			_ = writeAuthFile(file, string(previous))
		}
		return sitePasswordResult{}, internalErr("vhost 已关闭密码访问，但删除密码文件失败: " + err.Error())
	}
	return sitePasswordResult{
		nginxStatus:   s.runner.status(),
		Domain:        domain,
		Enabled:       false,
		Changed:       true,
		AuthFile:      file,
		HashScheme:    authHashScheme,
		Applied:       apply.Applied,
		TestPassed:    apply.TestPassed,
		Reloaded:      apply.Reloaded,
		TestSkipped:   apply.TestSkipped,
		ReloadSkipped: apply.ReloadSkipped,
		Hint:          "已移除密码访问；之前设置的密码文件已删除。",
	}, nil
}

// passwordHandlers 是 setPassword / clearPassword 的 verb 入口。
func setPasswordHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	var p sitePasswordParams
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return nil, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalidParam("params 解析失败: " + err.Error())
		}
	}
	return svc.setPassword(p)
}

func clearPasswordHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	var p siteDomainParams
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return nil, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalidParam("params 解析失败: " + err.Error())
		}
	}
	return svc.clearPassword(p.Domain)
}
