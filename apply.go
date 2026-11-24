package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// siteService 把 vhost 落盘与 nginx 校验/重载串成建站闭环：
//
//	create: 锁内写 vhost/清单（采集本域名撤销元信息）→ nginx -t → nginx -s reload
//	delete: 锁内备份并删 vhost/清单（采集本域名撤销元信息）→ nginx -t → nginx -s reload
//
// 校验失败时**在清单锁内**做定向回滚（只撤销本域名，不碰并发写者的其他站点），
// 回 E_INVALID；重载失败时 create 保留已写配置（站点已写入但不生效）回 E_UPSTREAM，
// delete 恢复本域名备份后回 E_UPSTREAM。
//
// 为什么回滚必须在锁内且定向：撤销元信息由 createLocked / deleteLocked 在持锁时采集，
// 若在锁外用「操作前的整文件快照」回滚，会把 nginx -t 期间并发写者新增的站点抹掉
// （R6-3 跨锁竞态，回归用例 tests/boundary/rollback_window_test.go）。
type siteService struct {
	store  *vhostStore
	runner *nginxRunner
	// settings 是本次调用生效的配置（auto 探测到容器时已回填 container）。
	settings pluginSettings
}

// newSiteService 用当前环境解析 conf.d 与 nginx 命令配置。
func newSiteService() (*siteService, *pluginapi.Error) {
	settings, _, err := loadPluginSettings()
	if err != nil {
		return nil, internalErr("插件配置非法: " + err.Error())
	}
	runner, effective, err := buildRunner(settings)
	if err != nil {
		return nil, internalErr(err.Error())
	}
	return &siteService{store: newVHostStore(), runner: runner, settings: effective}, nil
}

// create 执行完整建站链路并返回真实生效状态。
func (s *siteService) create(p siteParams) (siteCreateResult, *pluginapi.Error) {
	// PHP 站点先解析出真实 FPM socket：解析失败直接 E_INVALID，**一个字节都不落盘**。
	// 静默退化成静态站是本缺陷的原形（配置写着 PHP、nginx 不解析 .php）。
	var php *phpState
	if strings.TrimSpace(p.PHPVersion) != "" {
		resolution, perr := resolvePHPState(s.settings, p.PHPVersion, "")
		if perr != nil {
			return siteCreateResult{}, perr
		}
		resolved := resolution.State
		php = &resolved
	}
	if perr := s.preflightWrite(); perr != nil {
		return siteCreateResult{}, perr
	}

	// 快照不再在锁外拍：create 在持锁临界区内采集本域名撤销所需的元信息
	// （backup / created），回滚时凭它在锁内做定向撤销，不碰并发写者的清单条目。
	path, backup, created, perr := s.store.create(p, php)
	if perr != nil {
		return siteCreateResult{}, perr
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := s.store.rollbackCreate(p.Domain, path, backup, created); rbErr != nil {
			return siteCreateResult{}, internalErr(fmt.Sprintf(
				"nginx 配置校验失败且回滚失败: %v；校验输出: %s", rbErr, describeCommand(testOutcome)))
		}
		if perr := needRootCommandErr(testOutcome, "nginx 配置校验"); perr != nil {
			return siteCreateResult{}, perr
		}
		return siteCreateResult{}, invalidParam("nginx 配置校验失败，已回滚 vhost: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if perr := needRootCommandErr(reloadOutcome, "nginx 重载"); perr != nil {
			perr.Message = "站点已写入但 nginx 重载失败，当前未生效（vhost: " + path + "）：" + perr.Message
			return siteCreateResult{}, perr
		}
		// 不回滚：站点已经写入，只是尚未被 nginx 加载；明确告知调用方当前未生效。
		return siteCreateResult{}, &pluginapi.Error{
			Code: pluginapi.E_UPSTREAM,
			Message: "站点已写入但 nginx 重载失败，当前未生效（vhost: " + path + "）: " +
				describeCommand(reloadOutcome),
		}
	}

	result := siteCreateResult{
		nginxStatus:   s.runner.status(),
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
	}
	// PHP 状态从**实际落盘的 vhost** 解析，而不是回显入参意图：
	// 万一渲染层没写出 PHP 块，这里必须如实报 php_enabled=false（否则就是本缺陷同款自嗨）。
	if content, readErr := os.ReadFile(path); readErr == nil {
		if written := parsePHPState(scanBlocks(string(content))); written != nil {
			result.PHPEnabled = true
			result.PHPVersion = written.Version
			result.PHPSocket = written.Socket
		}
	}
	return result, nil
}

// delete 删除 vhost 后重新校验并重载；任一失败都恢复删除前状态。
func (s *siteService) delete(domain string) (siteDeleteResult, *pluginapi.Error) {
	if perr := s.preflightWrite(); perr != nil {
		return siteDeleteResult{}, perr
	}

	// 与 create 同理：撤销所需元信息由 deleteWithBackup 在持锁临界区内采集，
	// 回滚时凭它在锁内只恢复本域名，不碰并发写者新增的其他站点。
	path, backup, deleted, perr := s.store.deleteWithBackup(domain)
	if perr != nil {
		return siteDeleteResult{}, perr
	}
	if !deleted {
		// 幂等：没有可删的托管站点，不改配置自然也不需要 reload。
		return siteDeleteResult{
			nginxStatus: s.runner.status(),
			Domain:      domain, VHostPath: path, Applied: true, Deleted: false,
			TestSkipped: true, ReloadSkipped: true,
		}, nil
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := s.store.rollbackDelete(domain, path, backup); rbErr != nil {
			return siteDeleteResult{}, internalErr(fmt.Sprintf(
				"删除后 nginx 配置校验失败且恢复备份失败: %v；校验输出: %s", rbErr, describeCommand(testOutcome)))
		}
		if perr := needRootCommandErr(testOutcome, "nginx 配置校验"); perr != nil {
			return siteDeleteResult{}, perr
		}
		return siteDeleteResult{}, invalidParam("删除后 nginx 配置校验失败，已恢复备份: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if rbErr := s.store.rollbackDelete(domain, path, backup); rbErr != nil {
			return siteDeleteResult{}, internalErr(fmt.Sprintf(
				"删除后 nginx 重载失败且恢复备份失败: %v；重载输出: %s", rbErr, describeCommand(reloadOutcome)))
		}
		if perr := needRootCommandErr(reloadOutcome, "nginx 重载"); perr != nil {
			return siteDeleteResult{}, perr
		}
		return siteDeleteResult{}, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "删除后 nginx 重载失败，已恢复备份: " + describeCommand(reloadOutcome),
		}
	}

	return siteDeleteResult{
		nginxStatus:   s.runner.status(),
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
