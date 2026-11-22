package main

import (
	"errors"
	"os"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// list 返回清单内每条站点的 exists/test_ok/reloaded 真实状态。
// nginx -t 与 nginx -T 各只跑一次，按域名逐条比对 server_name。
func (s *siteService) list() (siteListResult, *pluginapi.Error) {
	sites, perr := s.store.list()
	if perr != nil {
		return siteListResult{}, perr
	}
	if len(sites) == 0 {
		return siteListResult{Sites: sites, Count: 0, Applied: true}, nil
	}
	testOutcome := s.runner.runTest()
	dumpOutcome := s.runner.runDump()
	testOK := testOutcome.ran && testOutcome.err == nil
	dumpOK := dumpOutcome.ran && dumpOutcome.err == nil
	// "没测"与"测挂"必须可区分：skipped 走灰色未检测，failed 才允许红色。
	testSkipped, reloadSkipped := !testOutcome.ran, !dumpOutcome.ran
	testOutput := ""
	if testOutcome.ran && testOutcome.err != nil {
		testOutput = describeCommand(testOutcome)
	}
	allExists, allLoaded := true, true
	for i := range sites {
		sites[i].TestOK = testOK
		sites[i].TestSkipped = testSkipped
		sites[i].ReloadSkipped = reloadSkipped
		sites[i].TestOutput = testOutput
		sites[i].Reloaded = dumpOK && configHasServerName(dumpOutcome.output, sites[i].Domain)
		if !sites[i].Exists {
			allExists = false
		}
		// 停用站点按设计就不在 nginx 配置里，不能因此判定整批未加载。
		if sites[i].Enabled && !sites[i].Reloaded {
			allLoaded = false
		}
	}
	applied := testOK && allExists && (!dumpOutcome.ran || allLoaded)
	return siteListResult{Sites: sites, Count: len(sites), Applied: applied}, nil
}

// status 检查单站点四个独立信号，不修改任何文件、不触发 reload。
func (s *siteService) status(domain string) (siteStatusResult, *pluginapi.Error) {
	manifest, perr := s.store.loadManifest()
	if perr != nil {
		return siteStatusResult{}, perr
	}
	path := vhostPath(domain)
	result := siteStatusResult{Domain: domain, VHostPath: path, Managed: manifest.Sites[domain] == path}

	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// 停用站点：读 .conf.disabled 副本，配置仍在，只是没被 include。
		if disabled, readErr := os.ReadFile(disabledVHostPath(domain)); readErr == nil {
			result.Disabled = true
			content, err = disabled, nil
		}
	}
	switch {
	case err == nil:
		result.ConfExists = !result.Disabled
		if root := parseRootDirective(string(content)); root != "" {
			if st, statErr := os.Stat(root); statErr == nil && st.IsDir() {
				result.SiteDirExists = true
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// conf 不存在是合法观测值，不是服务错误。
	default:
		return siteStatusResult{}, internalErr("读取 vhost 失败: " + err.Error())
	}

	testOutcome := s.runner.runTest()
	result.TestSkipped = !testOutcome.ran
	result.TestPassed = testOutcome.ran && testOutcome.err == nil
	if testOutcome.ran {
		result.TestOutput = strings.TrimSpace(describeCommand(testOutcome))
	}
	dumpOutcome := s.runner.runDump()
	result.ReloadSkipped = !dumpOutcome.ran
	result.LoadChecked = dumpOutcome.ran && dumpOutcome.err == nil
	if result.LoadChecked {
		result.Loaded = configHasServerName(dumpOutcome.output, domain)
	}
	return result, nil
}
