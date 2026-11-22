package main

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// siteToggleResult 是 site.disable / site.enable 的结果。
// 启停不做删除：只把 vhost 改名成 .conf.disabled，配置与备份全部保留。
type siteToggleResult struct {
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	DisabledPath  string `json:"disabled_path"`
	Enabled       bool   `json:"enabled"`
	Changed       bool   `json:"changed"`
	Applied       bool   `json:"applied"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
}

// disableHandler 停用站点：vhost 改名为 .conf.disabled 后校验并重载。
func disableHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	return svc.toggle(p.Domain, false)
}

// enableHandler 重新启用站点：.conf.disabled 改回 .conf 后校验并重载。
func enableHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	return svc.toggle(p.Domain, true)
}

// toggle 用改名实现启用/停用，并复用 test → reload 编排；任一步失败都改回原名，
// 绝不留下"文件半改名但 nginx 未确认"的中间态。
func (s *siteService) toggle(domain string, enable bool) (siteToggleResult, *pluginapi.Error) {
	editMu.Lock()
	defer editMu.Unlock()

	path := vhostPath(domain)
	disabledPath := disabledVHostPath(domain)
	m, perr := s.store.loadManifest()
	if perr != nil {
		return siteToggleResult{}, perr
	}
	if managed, ok := m.Sites[domain]; !ok || managed != path {
		return siteToggleResult{}, &pluginapi.Error{
			Code:    pluginapi.E_NOTFOUND,
			Message: "站点不在本插件清单中，拒绝改名：" + domain,
		}
	}

	from, to := path, disabledPath
	if enable {
		from, to = disabledPath, path
	}
	result := siteToggleResult{Domain: domain, VHostPath: path, DisabledPath: disabledPath, Enabled: enable}
	if _, err := os.Stat(from); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return siteToggleResult{}, internalErr("读取 vhost 失败: " + err.Error())
		}
		if _, statErr := os.Stat(to); statErr == nil {
			// 已处于目标状态：幂等，不写盘也不跑 nginx。
			result.Applied, result.TestSkipped, result.ReloadSkipped = true, true, true
			return result, nil
		}
		return siteToggleResult{}, &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "站点配置不存在：" + domain}
	}

	if err := os.Rename(from, to); err != nil {
		return siteToggleResult{}, internalErr("重命名 vhost 失败: " + err.Error())
	}
	rollback := func() error { return os.Rename(to, from) }

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := rollback(); rbErr != nil {
			return siteToggleResult{}, internalErr("nginx 配置校验失败且回滚失败: " + rbErr.Error() +
				"；校验输出: " + describeCommand(testOutcome))
		}
		return siteToggleResult{}, invalidParam("nginx 配置校验失败，已恢复原状态: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if rbErr := rollback(); rbErr != nil {
			return siteToggleResult{}, internalErr("nginx 重载失败且回滚失败: " + rbErr.Error() +
				"；重载输出: " + describeCommand(reloadOutcome))
		}
		return siteToggleResult{}, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "nginx 重载失败，已恢复原状态: " + describeCommand(reloadOutcome),
		}
	}

	result.Changed = true
	result.Applied = testOutcome.err == nil && reloadOutcome.err == nil
	result.TestPassed = testOutcome.ran && testOutcome.err == nil
	result.Reloaded = reloadOutcome.ran && reloadOutcome.err == nil
	result.TestSkipped = !testOutcome.ran
	result.ReloadSkipped = !reloadOutcome.ran
	return result, nil
}
