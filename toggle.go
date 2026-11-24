package main

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// siteToggleResult 是 site.disable / site.enable 的结果。
// 启停不丢配置：停用把内容备份到插件自有目录（DisabledPath）后摘掉 conf.d 片段，
// 启用再把备份经受控写写回——配置与备份全程保留，conf.d 里不留 .disabled 副本。
type siteToggleResult struct {
	nginxStatus
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

// disableHandler 停用站点：内容备份到插件目录 + 受控删除 conf.d 片段后校验并重载。
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

// enableHandler 重新启用站点：备份经受控写写回 conf.d 后校验并重载。
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

// toggle 用「备份 + 受控删除 / 受控写回」实现启用/停用，并复用 test → reload 编排；
// 任一步失败都把 conf.d 恢复成动作执行前的状态，绝不留下「文件半删半写但 nginx 未确认」的中间态。
//
// 为什么不再是 os.Rename（ADR-0053）：装 root 后插件以 kore-site 运行，对 /etc/nginx/conf.d
// 只有读权限，rename / unlink 都需要目录写权限，三条路径（删站、停用、启用）都会 DAC 失败。
// 新口径：
//   - 停用 = 内容备份到插件自有目录（KORE_STATE_DIR）+ 内核受控删除 conf.d 片段；
//   - 启用 = 从插件自有目录读回 + 内核受控写 file_write 写回，成功后删除备份；
//   - conf.d 里**不再**留 *.conf.disabled 副本（旧副本不会被 *.conf 匹配，随删除尽力清理）。
func (s *siteService) toggle(domain string, enable bool) (siteToggleResult, *pluginapi.Error) {
	editMu.Lock()
	defer editMu.Unlock()

	path := vhostPath(domain)
	disabledPath := s.store.disabledPath(domain)
	m, perr := s.store.loadManifest()
	if perr != nil {
		return siteToggleResult{}, perr
	}
	if managed, ok := m.Sites[domain]; !ok || managed != path {
		return siteToggleResult{}, &pluginapi.Error{
			Code:    pluginapi.E_NOTFOUND,
			Message: "站点不在本插件清单中，拒绝启停：" + domain,
		}
	}

	result := siteToggleResult{
		nginxStatus: s.runner.status(), Domain: domain, VHostPath: path,
		DisabledPath: disabledPath, Enabled: enable,
	}

	live, liveErr := os.ReadFile(path)
	if liveErr != nil && !errors.Is(liveErr, os.ErrNotExist) {
		return siteToggleResult{}, internalErr("读取 vhost 失败: " + liveErr.Error())
	}
	backup, backupErr := os.ReadFile(disabledPath)
	if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
		return siteToggleResult{}, internalErr("读取停用备份失败: " + backupErr.Error())
	}

	// 状态由「conf.d 片段 + 插件目录备份」两个事实共同决定：
	// 只有片段 = 启用态；只有备份 = 停用态；两者都在 = 上次失败留下的中间态，按目标态收敛。
	var content []byte
	if enable {
		if backupErr != nil { // 没有备份 → 本来就没停用过
			if liveErr == nil {
				// 已处于启用态：幂等短路，不写盘也不跑 nginx。
				result.Applied, result.TestSkipped, result.ReloadSkipped = true, true, true
				return result, nil
			}
			return siteToggleResult{}, &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "站点配置不存在：" + domain}
		}
		content = backup
	} else {
		if liveErr != nil { // 片段已不在 conf.d
			if backupErr == nil {
				// 已处于停用态：幂等短路。
				result.Applied, result.TestSkipped, result.ReloadSkipped = true, true, true
				return result, nil
			}
			return siteToggleResult{}, &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "站点配置不存在：" + domain}
		}
		content = live
	}

	if perr := s.preflightWrite(); perr != nil {
		return siteToggleResult{}, perr
	}

	if enable {
		// 先受控写回 conf.d；备份留到 test/reload 成功后删（失败时只能靠它回到停用态）。
		if err := s.store.writeVHost(domain, content); err != nil {
			return siteToggleResult{}, vhostWriteErr(err)
		}
	} else {
		// 先落备份（插件自有目录可写），再受控删除 conf.d 片段。
		if err := atomicWrite(disabledPath, content); err != nil {
			return siteToggleResult{}, internalErr("备份停用站点配置失败: " + err.Error())
		}
		if err := s.store.deleteVHost(domain); err != nil {
			_ = os.Remove(disabledPath)
			return siteToggleResult{}, vhostDeleteErr(err)
		}
	}

	rollback := func() error {
		if enable {
			// 启用回滚 = 再摘掉 conf.d 片段；备份原地保留，站点回到停用态。
			return s.store.deleteVHost(domain)
		}
		// 停用回滚 = 把内容写回 conf.d，成功后再删备份。
		if err := s.store.writeVHost(domain, content); err != nil {
			return err
		}
		if err := os.Remove(disabledPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := rollback(); rbErr != nil {
			return siteToggleResult{}, internalErr("nginx 配置校验失败且回滚失败: " + rbErr.Error() +
				"；校验输出: " + describeCommand(testOutcome))
		}
		if perr := needRootCommandErr(testOutcome, "nginx 配置校验"); perr != nil {
			return siteToggleResult{}, perr
		}
		return siteToggleResult{}, invalidParam("nginx 配置校验失败，已恢复原状态: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if rbErr := rollback(); rbErr != nil {
			return siteToggleResult{}, internalErr("nginx 重载失败且回滚失败: " + rbErr.Error() +
				"；重载输出: " + describeCommand(reloadOutcome))
		}
		if perr := needRootCommandErr(reloadOutcome, "nginx 重载"); perr != nil {
			return siteToggleResult{}, perr
		}
		return siteToggleResult{}, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "nginx 重载失败，已恢复原状态: " + describeCommand(reloadOutcome),
		}
	}

	// 启用成功：停用态正式退出，删掉插件目录里的备份（conf.d 里本来就没有副本）。
	if enable {
		if err := os.Remove(disabledPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return siteToggleResult{}, internalErr("启用成功但清理停用备份失败: " + err.Error())
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
