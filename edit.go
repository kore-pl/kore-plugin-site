package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// editMu 串行化所有「读 vhost → 改 → 写」的编辑，避免并发请求互相覆盖状态。
var editMu sync.Mutex

// applyResult 是 L2 所有写方法共用的结果字段（被各结果结构体内嵌提升）。
type applyResult struct {
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	Changed       bool   `json:"changed"`
	Applied       bool   `json:"applied"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	BackupPath    string `json:"backup_path,omitempty"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
}

// applyEdit 是 L2 全部写能力共用的编排（复用 v1 create/delete 的三段式）：
//
//	读现状 → 解析状态 → 回调改状态 → 重渲染 → 备份 → 原子写
//	→ nginx -t（失败回滚 + E_INVALID）→ nginx -s reload（失败回滚 + E_UPSTREAM）
//
// 目标状态与现状字节一致时短路：不写盘、不跑 nginx，回 changed=false。
func (s *siteService) applyEdit(domain string, mutate func(*siteState) *pluginapi.Error) (applyResult, siteState, *pluginapi.Error) {
	editMu.Lock()
	defer editMu.Unlock()

	path := vhostPath(domain)
	snap, err := snapshotFile(path)
	if err != nil {
		return applyResult{}, siteState{}, internalErr("读取 vhost 失败: " + err.Error())
	}
	if !snap.exists {
		return applyResult{}, siteState{}, siteNotFound(domain)
	}

	st := parseSiteState(domain, string(snap.data))
	if perr := mutate(&st); perr != nil {
		return applyResult{}, st, perr
	}
	content := renderVHost(st)
	// 生成后自检：绝不产出 default_server 指令（红队 R6）。
	if strings.Contains(stripComments(content), "default_server") {
		return applyResult{}, st, internalErr("生成的 vhost 含 default_server，拒绝落盘")
	}

	result := applyResult{Domain: domain, VHostPath: path}
	if string(content) == string(snap.data) {
		result.Applied, result.TestSkipped, result.ReloadSkipped = true, true, true
		return result, st, nil
	}

	backup, perr := s.writeBackup(domain, snap.data)
	if perr != nil {
		return applyResult{}, st, perr
	}
	if err := atomicWrite(path, content); err != nil {
		return applyResult{}, st, internalErr("写入 vhost 失败: " + err.Error())
	}

	testOutcome := s.runner.runTest()
	if testOutcome.ran && testOutcome.err != nil {
		if rbErr := snap.restore(); rbErr != nil {
			return applyResult{}, st, internalErr(fmt.Sprintf(
				"nginx 配置校验失败且回滚失败: %v；校验输出: %s", rbErr, describeCommand(testOutcome)))
		}
		return applyResult{}, st, invalidParam("nginx 配置校验失败，已回滚: " + describeCommand(testOutcome))
	}

	reloadOutcome := s.runner.runReload()
	if reloadOutcome.ran && reloadOutcome.err != nil {
		if rbErr := snap.restore(); rbErr != nil {
			return applyResult{}, st, internalErr(fmt.Sprintf(
				"nginx 重载失败且回滚失败: %v；重载输出: %s", rbErr, describeCommand(reloadOutcome)))
		}
		return applyResult{}, st, &pluginapi.Error{
			Code:    pluginapi.E_UPSTREAM,
			Message: "nginx 重载失败，已回滚到改动前配置: " + describeCommand(reloadOutcome),
		}
	}

	result.Changed = true
	result.BackupPath = backup
	result.TestPassed = testOutcome.ran && testOutcome.err == nil
	result.TestSkipped = !testOutcome.ran
	result.Reloaded = reloadOutcome.ran && reloadOutcome.err == nil
	result.ReloadSkipped = !reloadOutcome.ran
	result.Applied = testOutcome.err == nil && reloadOutcome.err == nil
	return result, st, nil
}

// writeBackup 在改动前留一份带时间戳的同目录备份（非 .conf 后缀，不会被 include）。
func (s *siteService) writeBackup(domain string, content []byte) (string, *pluginapi.Error) {
	backup := s.store.backupPath(domain)
	if err := atomicWrite(backup, content); err != nil {
		return "", internalErr("备份 vhost 失败: " + err.Error())
	}
	return backup, nil
}

// decodeParams 解析 L2 方法入参：必须是 JSON 对象；长度/字符校验由各 handler 负责。
func decodeParams[T any](raw json.RawMessage) (T, *pluginapi.Error) {
	var p T
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return p, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, invalidParam("params 解析失败: " + err.Error())
		}
	}
	return p, nil
}

// siteNotFound 区分「站点不存在」与「站点被禁用」，后者给出可执行的下一步。
func siteNotFound(domain string) *pluginapi.Error {
	if _, err := os.Stat(disabledVHostPath(domain)); err == nil {
		return &pluginapi.Error{
			Code:    pluginapi.E_CONFLICT,
			Message: "站点已禁用，请先 site.enable（domain: " + domain + "）",
		}
	}
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "站点不存在：" + domain}
}

// readSiteState 读取站点当前配置（禁用站点读 .disabled 副本），不跑任何 nginx 命令。
func readSiteState(domain string) (siteState, bool, *pluginapi.Error) {
	content, err := os.ReadFile(vhostPath(domain))
	enabled := true
	if errors.Is(err, os.ErrNotExist) {
		content, err = os.ReadFile(disabledVHostPath(domain))
		enabled = false
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return siteState{}, false, siteNotFound(domain)
		}
		return siteState{}, false, internalErr("读取 vhost 失败: " + err.Error())
	}
	return parseSiteState(domain, string(content)), enabled, nil
}
