package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// siteService 把 vhost 落盘与 nginx 校验/重载串成建站闭环：
//
//	create: 快照 → 写 vhost/清单 → nginx -t → nginx -s reload
//	delete: 快照 → 备份并删 vhost/清单 → nginx -t → nginx -s reload
//
// 校验失败回滚到调用前状态并回 E_INVALID；重载失败时 create 保留已写配置
// （站点已写入但不生效）回 E_UPSTREAM，delete 恢复备份后回 E_UPSTREAM。
type siteService struct {
	store  *vhostStore
	runner *nginxRunner
}

// newSiteService 用当前环境解析 conf.d 与 nginx 命令配置。
func newSiteService() (*siteService, *pluginapi.Error) {
	runner, perr := loadNginxRunner()
	if perr != nil {
		return nil, perr
	}
	return &siteService{store: newVHostStore(), runner: runner}, nil
}

// create 执行完整建站链路并返回真实生效状态。
func (s *siteService) create(p siteParams) (siteCreateResult, *pluginapi.Error) {
	confBefore, err := snapshotFile(vhostPath(p.Domain))
	if err != nil {
		return siteCreateResult{}, internalErr("读取 vhost 原状态失败: " + err.Error())
	}
	manifestBefore, err := snapshotFile(s.store.manifestPath())
	if err != nil {
		return siteCreateResult{}, internalErr("读取站点清单原状态失败: " + err.Error())
	}

	path, backup, created, perr := s.store.create(p)
	if perr != nil {
		return siteCreateResult{}, perr
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := restoreSnapshots(confBefore, manifestBefore); rbErr != nil {
			return siteCreateResult{}, internalErr(fmt.Sprintf(
				"nginx 配置校验失败且回滚失败: %v；校验输出: %s", rbErr, describeCommand(testOutcome)))
		}
		return siteCreateResult{}, invalidParam("nginx 配置校验失败，已回滚 vhost: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		// 不回滚：站点已经写入，只是尚未被 nginx 加载；明确告知调用方当前未生效。
		return siteCreateResult{}, &pluginapi.Error{
			Code: pluginapi.E_UPSTREAM,
			Message: "站点已写入但 nginx 重载失败，当前未生效（vhost: " + path + "）: " +
				describeCommand(reloadOutcome),
		}
	}

	return siteCreateResult{
		Domain:        p.Domain,
		VHostPath:     path,
		DefaultServer: false,
		TestPassed:    testOutcome.ran && testOutcome.err == nil,
		Reloaded:      reloadOutcome.ran && reloadOutcome.err == nil,
		Applied:       true,
		BackupPath:    backup,
		Created:       created,
		TestSkipped:   !testOutcome.ran,
		ReloadSkipped: !reloadOutcome.ran,
	}, nil
}

// delete 删除 vhost 后重新校验并重载；任一失败都恢复删除前状态。
func (s *siteService) delete(domain string) (siteDeleteResult, *pluginapi.Error) {
	confBefore, err := snapshotFile(vhostPath(domain))
	if err != nil {
		return siteDeleteResult{}, internalErr("读取 vhost 原状态失败: " + err.Error())
	}
	manifestBefore, err := snapshotFile(s.store.manifestPath())
	if err != nil {
		return siteDeleteResult{}, internalErr("读取站点清单原状态失败: " + err.Error())
	}

	path, backup, deleted, perr := s.store.deleteWithBackup(domain)
	if perr != nil {
		return siteDeleteResult{}, perr
	}
	if !deleted {
		// 幂等：没有可删的托管站点，不改配置自然也不需要 reload。
		return siteDeleteResult{
			Domain: domain, VHostPath: path, Applied: true, Deleted: false,
			TestSkipped: true, ReloadSkipped: true,
		}, nil
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := restoreSnapshots(confBefore, manifestBefore); rbErr != nil {
			return siteDeleteResult{}, internalErr(fmt.Sprintf(
				"删除后 nginx 配置校验失败且恢复备份失败: %v；校验输出: %s", rbErr, describeCommand(testOutcome)))
		}
		return siteDeleteResult{}, invalidParam("删除后 nginx 配置校验失败，已恢复备份: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if rbErr := restoreSnapshots(confBefore, manifestBefore); rbErr != nil {
			return siteDeleteResult{}, internalErr(fmt.Sprintf(
				"删除后 nginx 重载失败且恢复备份失败: %v；重载输出: %s", rbErr, describeCommand(reloadOutcome)))
		}
		return siteDeleteResult{}, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "删除后 nginx 重载失败，已恢复备份: " + describeCommand(reloadOutcome),
		}
	}

	return siteDeleteResult{
		Domain:        domain,
		VHostPath:     path,
		Applied:       true,
		Deleted:       true,
		BackupPath:    backup,
		TestPassed:    testOutcome.ran && testOutcome.err == nil,
		Reloaded:      reloadOutcome.ran && reloadOutcome.err == nil,
		TestSkipped:   !testOutcome.ran,
		ReloadSkipped: !reloadOutcome.ran,
	}, nil
}

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
		sites[i].Exists = sites[i].Enabled
		sites[i].TestOK = testOK
		sites[i].TestSkipped = testSkipped
		sites[i].ReloadSkipped = reloadSkipped
		sites[i].TestOutput = testOutput
		sites[i].Reloaded = dumpOK && configHasServerName(dumpOutcome.output, sites[i].Domain)
		if !sites[i].Exists {
			allExists = false
		}
		if !sites[i].Reloaded {
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
	switch {
	case err == nil:
		result.ConfExists = true
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

// restoreSnapshots 按"先 vhost 后清单"恢复改动前状态，两者错误合并上报。
func restoreSnapshots(conf, manifest fileSnapshot) error {
	var problems []string
	if err := conf.restore(); err != nil {
		problems = append(problems, err.Error())
	}
	if err := manifest.restore(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
