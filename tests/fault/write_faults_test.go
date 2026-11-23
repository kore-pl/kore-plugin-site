// Package fault 的写方法全覆盖 + 读方法抽样用例（G-20 / V1-GOALS §29 缺口补齐）。
//
// site 共 20 个方法，create / status 已有直接覆盖；本文件补 14 个写方法与 4 个读方法中的
// 抽样覆盖（list / get）。测试仍是黑盒：真拉二进制、真 UDS、真帧协议，不 mock、不 t.Skip。
//
// 两类故障注入，互补而不是重复：
//
//	A. conf.d 被 KORE_SITE_CONF_DIR 指向普通文件（ENOTDIR，沿用 TestFaultDisk 手法）。
//	   覆盖全部 14 个写方法 + 抽样读方法：断言结构化错误码、无 panic、协议循环仍活。
//	   局限是它卡在「读 vhost/清单」之前，写路径本身没被触达——所以只当"错误映射"用例。
//
//	B. nginx 命令注入失败（KORE_NGINX_TEST_CMD=false / KORE_NGINX_RELOAD_CMD=false）。
//	   这时原子写已经发生，插件必须走自己的回滚分支。这才是"无半成品"的真正观测点：
//	   回滚后 vhost / 清单 / htpasswd / .disabled 的**字节与条目集合**必须与故障前一致，
//	   且失败后 site.get 的输出与故障前逐字节相同（语义级证明，不只比文件）。
//
// 目录快照刻意排除 *.bak.*：带时间戳的备份是非 .conf 的审计产物（nginx 不加载），
// 属设计内留痕，不计为半成品；会被 nginx include 的 .conf / .disabled / .htpasswd /
// 清单以及临时文件全部纳入比对。
package fault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// writeDomain 是写方法用例统一使用的站点域名。
const writeDomain = "faultw.example.com"

// pluginCall 是一次业务调用（method + 原始 JSON 入参）。
type pluginCall struct {
	method string
	params string
}

// withFaultPlugin 拉起真插件 → 握手 → 执行 fn → 配对 shutdown → 回收进程。
func withFaultPlugin(t *testing.T, binary, confDir string, env map[string]string, fn func(t *testing.T, conn net.Conn)) {
	t.Helper()
	cmd, accept := startPluginWithEnv(t, binary, confDir, 0, env)
	conn := accept()
	defer func() { _ = conn.Close() }()
	handshakeWith(t, conn)
	fn(t, conn)
	shutdownWithReply(t, conn)
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// callOK 调用一个预期成功的方法，失败即 Fatal，返回结果载荷。
func callOK(t *testing.T, conn net.Conn, call pluginCall, reqID string) json.RawMessage {
	t.Helper()
	response := callMethod(t, conn, call.method, json.RawMessage(call.params), reqID)
	if response.Error != nil {
		t.Fatalf("%s 预期成功，实际 %s/%s", call.method, response.Error.Code, response.Error.Message)
	}
	return response.Result
}

// assertAlive 断言协议循环仍活：kore.ping 不依赖 conf.d，进程未被打崩。
func assertAlive(t *testing.T, conn net.Conn, hint string) {
	t.Helper()
	reply := callMethod(t, conn, "kore.ping", nil, "r-ping-"+hint)
	if reply.Error != nil {
		t.Fatalf("%s 后插件不应失活，kore.ping 报错: %s", hint, reply.Error.Message)
	}
}

// snapshotDir 对 conf.d 内会被 nginx 加载或属于插件受管产物的文件做内容指纹。
// 排除 *.bak.*（审计备份，设计内留痕）。
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 conf.d 失败: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.Contains(name, ".bak.") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatalf("读取 %s 失败: %v", name, readErr)
		}
		sum := sha256.Sum256(content)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}

// assertDirUnchanged 比对故障前后的 conf.d 指纹：不允许少条目、多条目或内容漂移。
func assertDirUnchanged(t *testing.T, before, after map[string]string, hint string) {
	t.Helper()
	for name := range after {
		if strings.HasSuffix(name, ".tmp") {
			t.Fatalf("%s: 残留临时文件 %s（原子写未清理）", hint, name)
		}
	}
	for name, sum := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("%s: 故障后丢失 %s（回滚未还原）", hint, name)
		}
		if got != sum {
			t.Fatalf("%s: 故障后 %s 内容被改动（回滚未还原到字节级）", hint, name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Fatalf("%s: 故障后新增残留 %s（半成品）", hint, name)
		}
	}
}

// tempCertFiles 造一对可读的证书/私钥占位文件（内容不参与执行：nginx 命令是桩）。
func tempCertFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatalf("写证书占位失败: %v", err)
	}
	if err := os.WriteFile(key, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("写私钥占位失败: %v", err)
	}
	return cert, key
}

// seedSite 用正常工作流（TEST/RELOAD=true 桩）建好站点并执行前置调用，
// 返回 (site.get 结果字节, conf.d 快照)。get 结果用于故障后的逐字节语义比对。
func seedSite(t *testing.T, binary, confDir, domain, root string, prep []pluginCall) ([]byte, map[string]string) {
	t.Helper()
	var getResult []byte
	withFaultPlugin(t, binary, confDir, nil, func(t *testing.T, conn net.Conn) {
		create := pluginCall{
			method: "site.create",
			params: fmt.Sprintf(`{"domain":%q,"root":%q,"port":18080}`, domain, root),
		}
		callOK(t, conn, create, "r-seed-create")
		for i, call := range prep {
			callOK(t, conn, call, fmt.Sprintf("r-seed-prep-%d", i))
		}
		getResult = callOK(t, conn, pluginCall{"site.get", fmt.Sprintf(`{"domain":%q}`, domain)}, "r-seed-get")
	})
	return getResult, snapshotDir(t, confDir)
}

// writeMethodsENOTDIR 是 14 个写方法在 ENOTDIR 下的入参表。
// 期望错误码全部为 E_INTERNAL：conf.d 不可用时读 vhost/清单本身就是内部错误，
// 不允许伪装成 E_NOTFOUND（那会让调用方误以为"站点不存在"而去重建）。
func writeMethodsENOTDIR(certPath, keyPath string) []pluginCall {
	base := fmt.Sprintf(`{"domain":%q}`, writeDomain)
	withCert := fmt.Sprintf(`{"domain":%q,"cert_path":%q,"key_path":%q}`, writeDomain, certPath, keyPath)
	return []pluginCall{
		{"site.delete", base},
		{"site.bindSSL", withCert},
		{"site.unbindSSL", base},
		{"site.setRewrite", fmt.Sprintf(`{"domain":%q,"template":"wordpress"}`, writeDomain)},
		{"site.setProxy", fmt.Sprintf(`{"domain":%q,"upstream":"http://127.0.0.1:8080"}`, writeDomain)},
		{"site.unsetProxy", base},
		{"site.setRedirect", fmt.Sprintf(`{"domain":%q,"from_path":"/old","to_url":"https://example.com/new"}`, writeDomain)},
		{"site.unsetRedirect", fmt.Sprintf(`{"domain":%q,"from_path":"/old"}`, writeDomain)},
		{"site.disable", base},
		{"site.enable", base},
		{"site.setIndex", fmt.Sprintf(`{"domain":%q,"files":["index.html","index.htm"]}`, writeDomain)},
		{"site.clone", fmt.Sprintf(`{"source_domain":%q,"new_domain":"faultw-clone.example.com"}`, writeDomain)},
		{"site.setPassword", fmt.Sprintf(`{"domain":%q,"username":"admin","password":"fault-pass-123"}`, writeDomain)},
		{"site.clearPassword", base},
	}
}

// TestFaultWriteMethodsConfDirENOTDIR 覆盖全部 14 个写方法：conf.d 是普通文件（ENOTDIR）。
// 断言：E_INTERNAL + 非空 message + 无结果载荷 + 协议循环仍活 + 阻塞路径未被改写。
func TestFaultWriteMethodsConfDirENOTDIR(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	blocked := blockedConfDir(t)
	certPath, keyPath := tempCertFiles(t)
	before, err := os.ReadFile(blocked)
	if err != nil {
		t.Fatalf("读取阻塞路径失败: %v", err)
	}

	for _, call := range writeMethodsENOTDIR(certPath, keyPath) {
		t.Run(call.method, func(t *testing.T) {
			withFaultPlugin(t, binary, blocked, nil, func(t *testing.T, conn net.Conn) {
				response := callMethod(t, conn, call.method, json.RawMessage(call.params), "r-enotdir")
				if response.Error == nil {
					t.Fatalf("conf.d 不可用时 %s 不应成功: result=%s", call.method, response.Result)
				}
				if response.Error.Code != pluginapi.E_INTERNAL {
					t.Fatalf("%s 错误码=%q，期望 E_INTERNAL（message=%q）",
						call.method, response.Error.Code, response.Error.Message)
				}
				if strings.TrimSpace(response.Error.Message) == "" {
					t.Fatalf("%s 返回了空错误信息，违反结构化错误约定", call.method)
				}
				if len(response.Result) != 0 && string(response.Result) != "null" {
					t.Fatalf("%s 故障下不应带结果载荷: %s", call.method, response.Result)
				}
				// 服务仍可用：失败之后协议循环必须继续应答。
				assertAlive(t, conn, "ENOTDIR/"+call.method)
			})
		})
	}

	after, err := os.ReadFile(blocked)
	if err != nil {
		t.Fatalf("复读阻塞路径失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("阻塞路径内容被改写: %q → %q", before, after)
	}
}

// rollbackCase 是一个「写盘后校验失败 → 必须回滚」的用例。
type rollbackCase struct {
	name string
	// prep 是故障注入前必须成功的调用（构造被改动的前置状态）。
	prep []pluginCall
	// call 是在故障进程里执行的写方法（预期 E_INVALID + 回滚）。
	call pluginCall
	// authFile 期望 htpasswd 的最终状态："" 不检查 / "absent" 不存在 / "present" 存在。
	authFile string
}

// rollbackCases 是 12 个走 applyEdit 编排的写方法。
// 说明：unbindSSL / unsetProxy / unsetRedirect / enable / clearPassword 必须先把目标状态
// 造出来，否则是幂等短路（changed=false）而不会触达写盘分支。
func rollbackCases(certPath, keyPath string) []rollbackCase {
	base := fmt.Sprintf(`{"domain":%q}`, writeDomain)
	bindSSL := pluginCall{"site.bindSSL", fmt.Sprintf(
		`{"domain":%q,"cert_path":%q,"key_path":%q}`, writeDomain, certPath, keyPath)}
	setProxy := pluginCall{"site.setProxy", fmt.Sprintf(
		`{"domain":%q,"upstream":"http://127.0.0.1:8080","ws":true}`, writeDomain)}
	unsetProxy := pluginCall{"site.unsetProxy", base}
	setRedirect := pluginCall{"site.setRedirect", fmt.Sprintf(
		`{"domain":%q,"from_path":"/old","to_url":"https://example.com/new"}`, writeDomain)}
	setPassword := pluginCall{"site.setPassword", fmt.Sprintf(
		`{"domain":%q,"username":"admin","password":"fault-pass-123"}`, writeDomain)}
	return []rollbackCase{
		{name: "bindSSL", call: bindSSL},
		{name: "unbindSSL", prep: []pluginCall{bindSSL}, call: pluginCall{"site.unbindSSL", base}},
		{name: "setRewrite", call: pluginCall{"site.setRewrite", fmt.Sprintf(
			`{"domain":%q,"template":"wordpress"}`, writeDomain)}},
		{name: "setProxy", call: setProxy},
		{name: "unsetProxy", prep: []pluginCall{setProxy}, call: unsetProxy},
		{name: "setRedirect", call: setRedirect},
		{name: "unsetRedirect", prep: []pluginCall{setRedirect}, call: pluginCall{"site.unsetRedirect",
			fmt.Sprintf(`{"domain":%q,"from_path":"/old"}`, writeDomain)}},
		{name: "setIndex", call: pluginCall{"site.setIndex", fmt.Sprintf(
			`{"domain":%q,"files":["index.html","index.htm","index.php"]}`, writeDomain)}},
		{name: "disable", call: pluginCall{"site.disable", base}},
		{name: "enable", prep: []pluginCall{{"site.disable", base}}, call: pluginCall{"site.enable", base}},
		{name: "setPassword", call: setPassword, authFile: "absent"},
		{name: "clearPassword", prep: []pluginCall{setPassword}, call: pluginCall{"site.clearPassword", base},
			authFile: "present"},
	}
}

// TestFaultWriteRollbackOnTestFailure 覆盖 12 个写方法在「原子写已发生、nginx -t 失败」时的回滚。
// 断言：E_INVALID + 校验失败语义 + conf.d 指纹逐字节不变 + site.get 输出逐字节不变 +
// htpasswd 无残留 + 服务仍可用。
func TestFaultWriteRollbackOnTestFailure(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)
	certPath, keyPath := tempCertFiles(t)
	faultEnv := map[string]string{"KORE_NGINX_TEST_CMD": "false"}

	for _, c := range rollbackCases(certPath, keyPath) {
		t.Run(c.name, func(t *testing.T) {
			confDir := healthyConfDir(t)
			root := filepath.Join(t.TempDir(), "www")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("建站点根失败: %v", err)
			}
			beforeGet, before := seedSite(t, binary, confDir, writeDomain, root, c.prep)

			withFaultPlugin(t, binary, confDir, faultEnv, func(t *testing.T, conn net.Conn) {
				response := callMethod(t, conn, c.call.method, json.RawMessage(c.call.params), "r-rollback")
				if response.Error == nil {
					t.Fatalf("nginx -t 失败时 %s 不应成功: result=%s", c.call.method, response.Result)
				}
				if response.Error.Code != pluginapi.E_INVALID {
					t.Fatalf("%s 错误码=%q，期望 E_INVALID（message=%q）",
						c.call.method, response.Error.Code, response.Error.Message)
				}
				if !strings.Contains(response.Error.Message, "校验失败") {
					t.Fatalf("%s 错误信息 %q 未含校验失败语义", c.call.method, response.Error.Message)
				}
				// 语义级证明：失败后读回的配置必须与故障前完全一致。
				afterGet := callOK(t, conn, pluginCall{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)}, "r-get")
				if string(afterGet) != string(beforeGet) {
					t.Fatalf("%s 回滚后 site.get 与故障前不一致:\n before=%s\n after =%s",
						c.call.method, beforeGet, afterGet)
				}
				assertAlive(t, conn, c.name)
			})

			after := snapshotDir(t, confDir)
			assertDirUnchanged(t, before, after, c.name)

			if c.authFile != "" {
				_, statErr := os.Stat(filepath.Join(confDir, "kore-site-"+writeDomain+".htpasswd"))
				switch c.authFile {
				case "absent":
					if statErr == nil {
						t.Fatal("setPassword 失败后不应留下 htpasswd（半成品凭据文件）")
					}
				case "present":
					if statErr != nil {
						t.Fatalf("clearPassword 失败后原有 htpasswd 必须保留: %v", statErr)
					}
				}
			}
		})
	}
}

// TestFaultDeleteRollbackOnTestFailure 删除是最危险的写方法（先移除再校验）。
// 断言：E_INVALID + 「已恢复备份」语义 + vhost 与清单逐字节还原 + 无 .disabled 残留 + 服务可用。
func TestFaultDeleteRollbackOnTestFailure(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	confDir := healthyConfDir(t)
	root := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根失败: %v", err)
	}
	beforeGet, before := seedSite(t, binary, confDir, writeDomain, root, nil)
	vhostPath := filepath.Join(confDir, "kore-site-"+writeDomain+".conf")

	withFaultPlugin(t, binary, confDir, map[string]string{"KORE_NGINX_TEST_CMD": "false"},
		func(t *testing.T, conn net.Conn) {
			response := callMethod(t, conn, "site.delete",
				json.RawMessage(fmt.Sprintf(`{"domain":%q}`, writeDomain)), "r-del")
			if response.Error == nil {
				t.Fatal("nginx -t 失败时 site.delete 不应成功")
			}
			if response.Error.Code != pluginapi.E_INVALID {
				t.Fatalf("错误码=%q，期望 E_INVALID（message=%q）", response.Error.Code, response.Error.Message)
			}
			if !strings.Contains(response.Error.Message, "恢复备份") {
				t.Fatalf("错误信息 %q 未说明已恢复备份", response.Error.Message)
			}
			afterGet := callOK(t, conn, pluginCall{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)}, "r-get")
			if string(afterGet) != string(beforeGet) {
				t.Fatalf("删除回滚后 site.get 与故障前不一致:\n before=%s\n after =%s", beforeGet, afterGet)
			}
			// 显式验证优雅退出仍正常（服务仍可用）。
			if reply := callMethod(t, conn, "kore.shutdown", nil, "r-shutdown"); reply.Error != nil {
				t.Fatalf("删除回滚后 shutdown 报错: %s", reply.Error.Message)
			}
		})

	assertDirUnchanged(t, before, snapshotDir(t, confDir), "delete")
	if _, err := os.Stat(vhostPath); err != nil {
		t.Fatalf("删除回滚后 vhost 必须存在: %v", err)
	}
	if _, err := os.Stat(vhostPath + ".disabled"); err == nil {
		t.Fatal("删除回滚后不应出现 .disabled 副本")
	}
}

// TestFaultCloneRollbackOnTestFailure 克隆失败不得留下新站点（孤儿 vhost + 清单条目）。
// 断言：E_INVALID + 目标 vhost 不存在 + 清单/源站点逐字节不变 + 服务可用。
func TestFaultCloneRollbackOnTestFailure(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	confDir := healthyConfDir(t)
	root := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根失败: %v", err)
	}
	// 源站点必须带一条 L2 状态（index 非默认）：否则 clone 的 store.create 模板与
	// renderVHost(源状态) 逐字节相同，applyEdit 会短路（不写盘、不跑 nginx），故障打不中。
	seedPrep := []pluginCall{{"site.setIndex", fmt.Sprintf(`{"domain":%q,"files":["source.html"]}`, writeDomain)}}
	beforeGet, before := seedSite(t, binary, confDir, writeDomain, root, seedPrep)
	const target = "faultw-clone.example.com"

	withFaultPlugin(t, binary, confDir, map[string]string{"KORE_NGINX_TEST_CMD": "false"},
		func(t *testing.T, conn net.Conn) {
			params := fmt.Sprintf(`{"source_domain":%q,"new_domain":%q}`, writeDomain, target)
			response := callMethod(t, conn, "site.clone", json.RawMessage(params), "r-clone")
			if response.Error == nil {
				t.Fatal("nginx -t 失败时 site.clone 不应成功")
			}
			if response.Error.Code != pluginapi.E_INVALID {
				t.Fatalf("错误码=%q，期望 E_INVALID（message=%q）", response.Error.Code, response.Error.Message)
			}
			// 源站点不受影响，插件仍可服务。
			sourceGet := callOK(t, conn, pluginCall{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)}, "r-get")
			if string(sourceGet) != string(beforeGet) {
				t.Fatal("克隆失败后源站点配置发生了变化")
			}
			assertAlive(t, conn, "clone")
		})

	assertDirUnchanged(t, before, snapshotDir(t, confDir), "clone")
	if _, err := os.Stat(filepath.Join(confDir, "kore-site-"+target+".conf")); err == nil {
		t.Fatal("克隆失败后不应留下目标站点 vhost（孤儿配置）")
	}
}

// TestFaultWriteRollbackOnReloadFailure 重载失败是另一条回滚分支（校验已过、reload 挂）。
// 断言：E_UPSTREAM + 已回滚语义 + conf.d 指纹不变。选 delete（最危险）与 setIndex（普通改动）。
func TestFaultWriteRollbackOnReloadFailure(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)
	faultEnv := map[string]string{"KORE_NGINX_RELOAD_CMD": "false"}

	cases := []pluginCall{
		{"site.delete", fmt.Sprintf(`{"domain":%q}`, writeDomain)},
		{"site.setIndex", fmt.Sprintf(`{"domain":%q,"files":["main.html"]}`, writeDomain)},
	}
	for _, call := range cases {
		t.Run(call.method, func(t *testing.T) {
			confDir := healthyConfDir(t)
			root := filepath.Join(t.TempDir(), "www")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("建站点根失败: %v", err)
			}
			beforeGet, before := seedSite(t, binary, confDir, writeDomain, root, nil)

			withFaultPlugin(t, binary, confDir, faultEnv, func(t *testing.T, conn net.Conn) {
				response := callMethod(t, conn, call.method, json.RawMessage(call.params), "r-reload")
				if response.Error == nil {
					t.Fatalf("nginx reload 失败时 %s 不应成功", call.method)
				}
				if response.Error.Code != pluginapi.E_UPSTREAM {
					t.Fatalf("%s 错误码=%q，期望 E_UPSTREAM（message=%q）",
						call.method, response.Error.Code, response.Error.Message)
				}
				if !strings.Contains(response.Error.Message, "滚") && !strings.Contains(response.Error.Message, "恢复") {
					t.Fatalf("%s 错误信息 %q 未说明已回滚/已恢复", call.method, response.Error.Message)
				}
				afterGet := callOK(t, conn, pluginCall{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)}, "r-get")
				if string(afterGet) != string(beforeGet) {
					t.Fatalf("%s 重载失败后 site.get 与故障前不一致", call.method)
				}
				assertAlive(t, conn, "reload/"+call.method)
			})

			assertDirUnchanged(t, before, snapshotDir(t, confDir), "reload/"+call.method)
		})
	}
}

// TestFaultReadListAndGetConfDirENOTDIR 读方法抽样：list + get。
// 抽样理由：list 是全部前端入口且聚合「清单 + 逐站点 stat」，get 是解析树最深的一个
// （SSL/伪静态/反代/重定向/index + site_dir 三态）。getRewrite 与 get 共用 parseSiteState，
// traffic 的 vhost 读取也走 readSiteState，两者的读路径已被这两个用例覆盖，故不重复取样。
// 断言：E_INTERNAL + 结构化错误 + 无载荷 + 协议循环仍活。
func TestFaultReadListAndGetConfDirENOTDIR(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	blocked := blockedConfDir(t)
	before, err := os.ReadFile(blocked)
	if err != nil {
		t.Fatalf("读取阻塞路径失败: %v", err)
	}
	calls := []pluginCall{
		{"site.list", `{}`},
		{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)},
	}
	for _, call := range calls {
		t.Run(call.method, func(t *testing.T) {
			withFaultPlugin(t, binary, blocked, nil, func(t *testing.T, conn net.Conn) {
				response := callMethod(t, conn, call.method, json.RawMessage(call.params), "r-read")
				if response.Error == nil {
					t.Fatalf("conf.d 不可用时 %s 不应成功: result=%s", call.method, response.Result)
				}
				if response.Error.Code != pluginapi.E_INTERNAL {
					t.Fatalf("%s 错误码=%q，期望 E_INTERNAL（message=%q）",
						call.method, response.Error.Code, response.Error.Message)
				}
				if strings.TrimSpace(response.Error.Message) == "" {
					t.Fatalf("%s 返回了空错误信息", call.method)
				}
				if len(response.Result) != 0 && string(response.Result) != "null" {
					t.Fatalf("%s 故障下不应带结果载荷: %s", call.method, response.Result)
				}
				assertAlive(t, conn, "ENOTDIR-read/"+call.method)
			})
		})
	}
	after, err := os.ReadFile(blocked)
	if err != nil {
		t.Fatalf("复读阻塞路径失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("读方法故障下不应改写阻塞路径")
	}
}

// TestFaultReadCorruptManifestNoOverwrite 清单损坏（数据静止态故障）：
// list / get 必须报 E_INTERNAL 且**不得**做破坏性覆盖（覆盖会把运维可恢复的数据抹掉）。
func TestFaultReadCorruptManifestNoOverwrite(t *testing.T) {
	binary := findBinary(t)
	sanityHealthy(t, binary)

	confDir := healthyConfDir(t)
	root := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建站点根失败: %v", err)
	}
	seedSite(t, binary, confDir, writeDomain, root, nil)

	manifestPath := filepath.Join(confDir, "kore-site-manifest.json")
	corrupt := []byte("{ this is not valid json ")
	if err := os.WriteFile(manifestPath, corrupt, 0o600); err != nil {
		t.Fatalf("写损坏清单失败: %v", err)
	}

	calls := []pluginCall{
		{"site.list", `{}`},
		{"site.get", fmt.Sprintf(`{"domain":%q}`, writeDomain)},
	}
	withFaultPlugin(t, binary, confDir, nil, func(t *testing.T, conn net.Conn) {
		for _, call := range calls {
			response := callMethod(t, conn, call.method, json.RawMessage(call.params), "r-corrupt")
			if response.Error == nil {
				t.Fatalf("清单损坏时 %s 不应成功", call.method)
			}
			if response.Error.Code != pluginapi.E_INTERNAL {
				t.Fatalf("%s 错误码=%q，期望 E_INTERNAL", call.method, response.Error.Code)
			}
			if !strings.Contains(response.Error.Message, "清单") {
				t.Fatalf("%s 错误信息 %q 未点名站点清单", call.method, response.Error.Message)
			}
		}
		assertAlive(t, conn, "corrupt-manifest")
	})

	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("复读清单失败: %v", err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("损坏清单被静默覆盖（应保持原样待运维恢复）: %q", after)
	}
}
