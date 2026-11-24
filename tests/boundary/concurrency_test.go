package boundary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 并发（判据① 第 2 类）。
//
// 现状核实：`grep -rn "t\\.Parallel" --include='*.go' .` = 0 命中，
// 仓内没有任何并发断言。本文件补两类，都用 **-race 插桩二进制**（raceBin）跑：
// 没插桩的进程，说"无数据竞争"只是口号。
//
//   1. TestBoundaryConcurrencyAtomicVisibility：单实例持续改写同一域名 vhost，
//      4 个读协程并发读 conf.d —— 断言任何时刻读到的内容都**整体等于某个已提交版本**，
//      绝不出现半截内容（写路径 atomicWrite 的临时文件+rename 契约）。
//   2. TestBoundaryConcurrencyParallelInstances：8 个真实插件进程（t.Parallel 子测试）
//      并发写同一 conf.d，每个写自己的域名 —— 断言互不串扰、无残留临时文件、
//      无数据竞争、优雅退出。
//
// 为什么不是"多连接打一个插件"：site 是**主动连内核**的单 UDS 会话（main.go: run →
// serve 单连接顺序循环），一个进程只有一个会话，所以进程内并发的真实面就是
// "写者 vs 读者"与"多实例共享 conf.d"，而不是并发请求分发。

// TestBoundaryConcurrencyAtomicVisibility 断言并发读写不出现撕裂内容。
func TestBoundaryConcurrencyAtomicVisibility(t *testing.T) {
	t.Parallel()
	confDir := healthyConfDir(t)
	domain := "conc-atomic.example.com"
	vhost := filepath.Join(confDir, "kore-site-"+domain+".conf")

	p := startPlugin(t, raceBin, confDir, nil)
	p.handshake(t)

	rootA := filepath.Join(t.TempDir(), "www-a")
	rootB := filepath.Join(t.TempDir(), "www-b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("建站点根失败: %v", err)
		}
	}

	// 先各自提交一次，取得两个**完整已提交版本**的字节，作为读协程的合法值集合。
	version := map[string][]byte{}
	for label, root := range map[string]string{"A": rootA, "B": rootB} {
		response := p.call(t, "site.create",
			`{"domain":"`+domain+`","root":"`+root+`","port":18080}`, "r-atomic-init-"+label)
		if response.Error != nil {
			t.Fatalf("初始化 %s 版本失败: code=%s msg=%s", label, response.Error.Code, response.Error.Message)
		}
		content, err := os.ReadFile(vhost)
		if err != nil {
			t.Fatalf("读已提交 vhost 失败: %v", err)
		}
		version[label] = content
	}
	if strings.EqualFold(string(version["A"]), string(version["B"])) {
		t.Fatal("两个版本的 vhost 内容相同，用例失去判据（root 未生效）")
	}

	const (
		readers = 4
		writes  = 120
	)
	var (
		stop     = make(chan struct{})
		readerWG sync.WaitGroup
		badMu    sync.Mutex
		bad      []string
		observed int
	)
	record := func(msg string) {
		badMu.Lock()
		defer badMu.Unlock()
		if len(bad) < 5 {
			bad = append(bad, msg)
		}
	}
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func(id int) {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				content, err := os.ReadFile(vhost)
				if err != nil {
					if os.IsNotExist(err) {
						continue // 首版提交前的空窗
					}
					record(fmt.Sprintf("reader-%d 读失败: %v", id, err))
					continue
				}
				badMu.Lock()
				observed++
				badMu.Unlock()
				if equal(content, version["A"]) || equal(content, version["B"]) {
					continue
				}
				record(fmt.Sprintf("reader-%d 读到撕裂/混合内容（%d 字节）", id, len(content)))
			}
		}(i)
	}

	// 写协程（主协程）交替提交 A/B 两个版本。
	for i := 0; i < writes; i++ {
		label := "A"
		if i%2 == 0 {
			label = "B"
		}
		root := map[string]string{"A": rootA, "B": rootB}[label]
		response := p.call(t, "site.create",
			`{"domain":"`+domain+`","root":"`+root+`","port":18080}`, fmt.Sprintf("r-atomic-%d", i))
		if response.Error != nil {
			close(stop)
			readerWG.Wait()
			t.Fatalf("第 %d 次提交失败: code=%s msg=%s", i, response.Error.Code, response.Error.Message)
		}
	}
	close(stop)
	readerWG.Wait()

	if len(bad) > 0 {
		t.Fatalf("并发读写违反原子可见性:\n%s", strings.Join(bad, "\n"))
	}
	if observed == 0 {
		t.Fatal("读协程一次都没读到文件，断言没有真正执行")
	}

	// 终态必须等于最后一次提交的版本，且没有临时文件残留。
	// 循环从 i=0 的 B 开始，writes-1 决定最后一次是 A 还是 B。
	lastLabel := "A"
	if (writes-1)%2 == 0 {
		lastLabel = "B"
	}
	final, err := os.ReadFile(vhost)
	if err != nil {
		t.Fatalf("读终态 vhost 失败: %v", err)
	}
	if !equal(final, version[lastLabel]) {
		t.Fatalf("终态 vhost 与最后一次提交（%s 版）不一致（终态 %d 字节，期望 %d 字节）",
			lastLabel, len(final), len(version[lastLabel]))
	}
	assertNoTempResidue(t, confDir)

	_ = p.shutdown(t)
	p.assertNoDataRace(t, "concurrency-atomic-visibility")
	t.Logf("并发读观察次数=%d，写提交=%d，参与者=%d", observed, writes, readers)
}

// TestBoundaryConcurrencyParallelInstances 断言多实例并发写同一 conf.d 互不串扰。
func TestBoundaryConcurrencyParallelInstances(t *testing.T) {
	confDir := healthyConfDir(t)
	const instances = 8

	domains := make([]string, instances)
	for i := range domains {
		domains[i] = fmt.Sprintf("conc-%02d.example.com", i)
	}

	// 真并发：go func + WaitGroup，每个 goroutine 拉起一个真实插件进程。
	// 这里不用 t.Parallel 子测试，因为聚合断言必须在**全部**实例退出后执行。
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
			domain := domains[i]
			response, err := p.callErr("site.create",
				`{"domain":"`+domain+`","port":18080}`, fmt.Sprintf("r-conc-%d", i))
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
			if p.stderr.String() != "" && strings.Contains(p.stderr.String(), "DATA RACE") {
				fail(fmt.Sprintf("实例 %d 报告数据竞争:\n%s", i, p.stderr.String()))
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("并发多实例失败 %d/%d:\n%s", len(errs), instances, strings.Join(errs, "\n"))
	}

	// 无状态串扰：每个域名只出现在自己的 vhost 里，且内容完整。
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
	assertNoTempResidue(t, confDir)
	t.Logf("并发实例=%d，域名单一归属校验通过", instances)
}

// assertNoTempResidue 断言 conf.d 里没有 atomicWrite 遗留的临时文件。
func assertNoTempResidue(t *testing.T, confDir string) {
	t.Helper()
	entries, err := os.ReadDir(confDir)
	if err != nil {
		t.Fatalf("读 conf.d 失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("conf.d 残留临时文件（原子写未清理）: %s", entry.Name())
		}
	}
}

// equal 是字节切片比较（避免额外引入 bytes 别名遮蔽）。
func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
