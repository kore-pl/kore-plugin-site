package boundary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 清单并发（判据① 第 2 类的补充断言：**丢更新**）。
//
// 背景（cf0cd25 实测缺口）：该提交的 TestBoundaryConcurrencyParallelInstances 只断言
// 「每个 vhost 文件存在、server_name 唯一、无跨域名串扰、无 .tmp 残留、无数据竞争」，
// 但没有断言 `kore-site-manifest.json` 的**条目数**。清单是「读-改-写」：
// loadManifest → 拷 map → atomicWrite。atomicWrite 只保证单次写入的原子性
// （.tmp + fsync + rename），**不提供跨进程的互斥**——8 个实例读到同一份旧清单，
// 各自写回「旧清单 + 自己那条」，后写者覆盖先写者，于是丢条目。
// 文件层面全绿，`site.list` 却漏站点：这是断言覆盖不到的地方。
//
// 本用例补齐三条断言（不修改、不放宽 cf0cd25 既有断言，只新增）：
//  1. 8 实例并发建站后清单条目数 == 8（每条域名都在、路径正确）；
//  2. 每个站点的 server_name 只出现一次，且无交叉串扰；
//  3. 并发下无 .tmp 残留、无数据竞争（实例由 -race 插桩二进制启动）。
// 另加一条可观测症状：事后新实例 site.list 的条目数也必须 == 8（修复前只剩 1）。

// manifestFileName 与 sites.go 的 manifestName 保持一致（manifestName 未导出，
// 测试与实现各持一份字面量；不一致会让本用例以「清单不存在」失败，而不是假绿）。
const manifestFileName = "kore-site-manifest.json"

// manifestDoc 是清单的最小测试视图，字段与 siteManifest 对齐。
type manifestDoc struct {
	Version int               `json:"version"`
	Sites   map[string]string `json:"sites"`
}

// TestBoundaryConcurrencyManifestNoLostUpdate 断言并发建站不丢清单条目。
func TestBoundaryConcurrencyManifestNoLostUpdate(t *testing.T) {
	confDir := healthyConfDir(t)
	const instances = 8

	domains := make([]string, instances)
	for i := range domains {
		domains[i] = fmt.Sprintf("manifest-%02d.example.com", i)
	}

	// 与 cf0cd25 同一手法：go func + WaitGroup 拉起 8 个真实插件进程（-race 插桩），
	// 共享同一 conf.d，每个实例只写自己的域名。不用 t.Parallel 子测试，
	// 因为聚合断言必须在全部实例退出后执行。
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	fail := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, msg)
	}

	for i := 0; i < instances; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, cleanup, err := startPluginNoFatal(raceBin, confDir, nil)
			if err != nil {
				fail(fmt.Sprintf("实例 %d 启动失败: %v", i, err))
				return
			}
			defer cleanup()
			if err := p.handshakeErr(); err != nil {
				fail(fmt.Sprintf("实例 %d 握手失败: %v", i, err))
				return
			}
			response, err := p.callErr("site.create",
				`{"domain":"`+domains[i]+`","port":18080}`, fmt.Sprintf("r-manifest-%d", i))
			if err != nil {
				fail(fmt.Sprintf("实例 %d 请求失败: %v", i, err))
				return
			}
			if response.Error != nil {
				fail(fmt.Sprintf("实例 %d create 失败: code=%s msg=%s", i, response.Error.Code, response.Error.Message))
				return
			}
			if err := p.shutdownErr(); err != nil {
				fail(fmt.Sprintf("实例 %d 非优雅退出: %v（stderr=%s）", i, err, p.stderr.String()))
				return
			}
			// -race 插桩进程检测到竞争时以退出码 66 结束并在 stderr 打 "DATA RACE"。
			if strings.Contains(p.stderr.String(), "DATA RACE") {
				fail(fmt.Sprintf("实例 %d 报告数据竞争:\n%s", i, p.stderr.String()))
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("并发多实例失败 %d/%d:\n%s", len(errs), instances, strings.Join(errs, "\n"))
	}

	// —— 断言 1：清单条目数 == 8 ——
	manifestPath := filepath.Join(confDir, manifestFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读清单失败（并发后清单应存在）: %v", err)
	}
	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("清单不是合法 JSON: %v\n实际清单: %s", err, raw)
	}
	if doc.Version != 1 {
		t.Fatalf("清单 version=%d，期望 1\n实际清单: %s", doc.Version, raw)
	}
	if len(doc.Sites) != instances {
		var present []string
		for domain := range doc.Sites {
			present = append(present, domain)
		}
		t.Fatalf("并发建站后清单只有 %d/%d 条（读-改-写丢更新）\n已登记域名: %v\n实际清单: %s",
			len(doc.Sites), instances, present, raw)
	}
	for _, domain := range domains {
		gotPath, ok := doc.Sites[domain]
		if !ok {
			t.Errorf("清单漏掉域名 %s\n实际清单: %s", domain, raw)
			continue
		}
		wantPath := filepath.Join(confDir, "kore-site-"+domain+".conf")
		if gotPath != wantPath {
			t.Errorf("清单中 %s 的路径=%q，期望 %q", domain, gotPath, wantPath)
		}
	}

	// —— 断言 2：每个站点 server_name 只出现一次、无交叉串扰 ——
	for i, domain := range domains {
		path := filepath.Join(confDir, "kore-site-"+domain+".conf")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("实例 %d 的 vhost 不存在: %v", i, err)
		}
		if got := strings.Count(string(content), "server_name "); got != 1 {
			t.Fatalf("实例 %d 的 vhost 有 %d 条 server_name，期望 1 条:\n%s", i, got, content)
		}
		if !strings.Contains(string(content), "server_name "+domain+";") {
			t.Fatalf("实例 %d 的 vhost 串到了别的域名（期望 %s）:\n%s", i, domain, content)
		}
		if !strings.HasSuffix(string(content), "}\n") {
			t.Fatalf("实例 %d 的 vhost 内容不完整（末尾未闭合）:\n%s", i, content)
		}
		for j, other := range domains {
			if j != i && strings.Contains(string(content), other) {
				t.Fatalf("实例 %d 的 vhost 混入实例 %d 的域名 %s", i, j, other)
			}
		}
	}

	// —— 断言 3：无 .tmp 残留（数据竞争已由各实例 -race stderr 断言）——
	assertNoTempResidue(t, confDir)

	// 可观测症状：事后新实例 site.list 必须能看到全部 8 条，而不是只有最后写者那条。
	p := startPlugin(t, raceBin, confDir, nil)
	p.handshake(t)
	response := p.call(t, "site.list", `{}`, "r-manifest-list")
	if response.Error != nil {
		t.Fatalf("site.list 失败: code=%s msg=%s", response.Error.Code, response.Error.Message)
	}
	var listed struct {
		Count int `json:"count"`
		Sites []struct {
			Domain string `json:"domain"`
		} `json:"sites"`
	}
	if err := unmarshalResult(response.Result, &listed); err != nil {
		t.Fatalf("解析 site.list 结果失败: %v result=%s", err, response.Result)
	}
	if listed.Count != instances || len(listed.Sites) != instances {
		t.Fatalf("site.list 漏站点: count=%d，sites=%d，期望 %d\n实际清单: %s",
			listed.Count, len(listed.Sites), instances, raw)
	}
	seen := make(map[string]bool, len(listed.Sites))
	for _, site := range listed.Sites {
		seen[site.Domain] = true
	}
	for _, domain := range domains {
		if !seen[domain] {
			t.Errorf("site.list 结果缺少域名 %s", domain)
		}
	}
	_ = p.shutdown(t)
	p.assertNoDataRace(t, "manifest-no-lost-update")

	t.Logf("并发实例=%d，清单条目=%d，site.list=%d，无丢更新", instances, len(doc.Sites), listed.Count)
}
