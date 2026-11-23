package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// cloneParams 是 site.clone 的入参：复制源站点的 vhost 配置到新域名，
// 可选复制站点根目录（仅允许插件 state 目录内，遵守 ADR-0023 零侵入 / G-31）。
type cloneParams struct {
	SourceDomain string `json:"source_domain"`
	NewDomain    string `json:"new_domain"`
	// Root 是克隆后站点的根目录；空表示沿用源站点根目录（测试环境可显式传新路径）。
	Root string `json:"root,omitempty"`
	// CopyRoot 为 true 时把源站点根目录递归复制到 Root（两者都必须位于 KORE_STATE_DIR 内）。
	CopyRoot bool `json:"copy_root,omitempty"`
}

// siteCloneResult 是 site.clone 的结果：配置复制、目录复制与生效状态分开如实披露。
type siteCloneResult struct {
	nginxStatus
	SourceDomain  string `json:"source_domain"`
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	SourceVHost   string `json:"source_vhost_path"`
	Root          string `json:"root"`
	RootCopied    bool   `json:"root_copied"`
	CopiedFiles   int    `json:"copied_files"`
	CopiedBytes   int64  `json:"copied_bytes"`
	SSLCopied     bool   `json:"ssl_copied"`
	RewriteCopied bool   `json:"rewrite_copied"`
	ProxyCopied   bool   `json:"proxy_copied"`
	RedirectCount int    `json:"redirect_count"`
	IndexCopied   bool   `json:"index_copied"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	Applied       bool   `json:"applied"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// clone 复制一个已托管站点：先按既有 create 路径落 vhost 与清单（冲突保护 + 备份），
// 再用源 vhost 的完整状态（SSL/伪静态/反代/重定向/index）覆盖渲染，
// 复用 applyEdit 的「原子写 → nginx -t → reload → 失败回滚」闭环。
func (s *siteService) clone(p cloneParams) (siteCloneResult, *pluginapi.Error) {
	sourceDomain := strings.TrimSpace(p.SourceDomain)
	newDomain := strings.TrimSpace(p.NewDomain)
	if perr := validateDomain(sourceDomain); perr != nil {
		return siteCloneResult{}, invalidParam("source_domain 非法: " + perr.Message)
	}
	if perr := validateDomain(newDomain); perr != nil {
		return siteCloneResult{}, perr
	}
	if sourceDomain == newDomain {
		return siteCloneResult{}, invalidParam("source_domain 与 new_domain 不能相同")
	}
	manifest, perr := s.store.loadManifest()
	if perr != nil {
		return siteCloneResult{}, perr
	}
	sourcePath := vhostPath(sourceDomain)
	if manifest.Sites[sourceDomain] != sourcePath {
		return siteCloneResult{}, &pluginapi.Error{
			Code:    pluginapi.E_NOTFOUND,
			Message: "源站点 " + sourceDomain + " 不在本插件清单中，无法克隆",
		}
	}
	sourceState, sourceEnabled, perr := readSiteState(sourceDomain)
	if perr != nil {
		return siteCloneResult{}, perr
	}
	if _, exists := manifest.Sites[newDomain]; exists {
		return siteCloneResult{}, &pluginapi.Error{
			Code:    pluginapi.E_CONFLICT,
			Message: "目标站点 " + newDomain + " 已存在（克隆不覆盖已有站点）",
		}
	}
	if perr := s.preflightWrite(); perr != nil {
		return siteCloneResult{}, perr
	}

	newRoot := strings.TrimSpace(p.Root)
	if newRoot == "" {
		newRoot = sourceState.Root
	}
	if err := validateRootPath(newRoot); err != nil {
		return siteCloneResult{}, invalidParam("root 非法: " + err.Error())
	}
	copiedFiles, copiedBytes := 0, int64(0)
	if p.CopyRoot {
		files, bytes, perr := copySiteTree(sourceState.Root, newRoot)
		if perr != nil {
			return siteCloneResult{}, perr
		}
		copiedFiles, copiedBytes = files, bytes
	}

	// 1) 用既有 create 路径落新 vhost + 清单（含冲突/备份/回滚语义）。
	if _, _, _, perr := s.store.create(siteParams{Domain: newDomain, Root: newRoot, Port: sourceState.Port}); perr != nil {
		return siteCloneResult{}, perr
	}
	// 2) 用源站点的完整状态覆盖新 vhost，复用 applyEdit 的 test/reload/回滚闭环。
	cloned := sourceState
	cloned.Domain, cloned.Root = newDomain, newRoot
	apply, _, perr := s.applyEdit(newDomain, func(st *siteState) *pluginapi.Error {
		*st = cloned
		return nil
	})
	if perr != nil {
		// create 已落盘但状态覆盖失败：删除新站点，避免留下半成品。
		_, _, _ = s.store.delete(newDomain)
		return siteCloneResult{}, perr
	}

	result := siteCloneResult{
		nginxStatus:   s.runner.status(),
		SourceDomain:  sourceDomain,
		Domain:        newDomain,
		VHostPath:     vhostPath(newDomain),
		SourceVHost:   sourcePath,
		Root:          newRoot,
		RootCopied:    p.CopyRoot,
		CopiedFiles:   copiedFiles,
		CopiedBytes:   copiedBytes,
		SSLCopied:     cloned.SSL != nil,
		RewriteCopied: cloned.Rewrite != "" && cloned.Rewrite != rewriteNone,
		ProxyCopied:   cloned.Proxy != nil,
		RedirectCount: len(cloned.Redirects),
		IndexCopied:   len(cloned.Index) > 0,
		TestPassed:    apply.TestPassed,
		Reloaded:      apply.Reloaded,
		Applied:       apply.Applied,
		TestSkipped:   apply.TestSkipped,
		ReloadSkipped: apply.ReloadSkipped,
	}
	if !sourceEnabled {
		result.Hint = "源站点当前处于停用状态；克隆出的站点按启用状态创建，配置已完整复制。"
	}
	if p.CopyRoot {
		result.Hint = strings.TrimSpace(result.Hint + " 站点根目录已复制到 " + newRoot + "。")
	} else if newRoot == sourceState.Root {
		result.Hint = strings.TrimSpace(result.Hint + " 未复制目录：新站点与源站点共用根目录 " + newRoot +
			"（需要独立目录请传 root 且设置 copy_root=true，目录必须在 KORE_STATE_DIR 内）。")
	}
	return result, nil
}

// copySiteTree 递归复制站点根目录，仅允许 KORE_STATE_DIR 内的路径（ADR-0023 零侵入）：
// 拒绝符号链接与设备文件，限制条目/字节上限，避免克隆把插件变成任意文件复制器。
func copySiteTree(source, target string) (int, int64, *pluginapi.Error) {
	stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	if stateDir == "" {
		return 0, 0, &pluginapi.Error{
			Code:    pluginapi.E_UNSUPPORTED,
			Message: "copy_root 需要 KORE_STATE_DIR：插件只能复制 state 目录内的站点根（ADR-0023 零侵入）",
			Detail:  encode(map[string]string{"hint": "把站点根放到 KORE_STATE_DIR 下，或只克隆配置（copy_root=false）后手工迁移文件。"}),
		}
	}
	if !withinDir(stateDir, source) || !withinDir(stateDir, target) {
		return 0, 0, &pluginapi.Error{
			Code:    pluginapi.E_INVALID,
			Message: "copy_root 只允许复制 KORE_STATE_DIR 内的目录（宿主路径写入违反 ADR-0023 / G-31）",
			Detail: encode(map[string]string{
				"hint":   "source/target 都必须位于 " + stateDir + " 下；宿主 /var/www 请用 root 参数指向新路径后由运维自行迁移。",
				"source": source, "target": target,
			}),
		}
	}
	if _, err := os.Stat(source); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, invalidParam("源站点根目录不存在: " + source + "（可传 copy_root=false 只克隆配置）")
		}
		return 0, 0, internalErr("读取源站点根目录失败: " + err.Error())
	}
	const maxFiles, maxBytes = 100000, int64(2 << 30)
	files, bytes := 0, int64(0)
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			return nil // 不跟随符号链接，避免复制到白名单外
		case entry.IsDir():
			return os.MkdirAll(dst, info.Mode().Perm()|0o700)
		case entry.Type().IsRegular():
			files++
			bytes += info.Size()
			if files > maxFiles || bytes > maxBytes {
				return fmt.Errorf("克隆超过上限（%d 文件 / %d 字节），请手工迁移", maxFiles, maxBytes)
			}
			return copyFile(path, dst, info.Mode().Perm())
		default:
			return nil // 设备/管道文件不复制
		}
	})
	if err != nil {
		return 0, 0, internalErr("复制站点根目录失败: " + err.Error())
	}
	return files, bytes, nil
}

// withinDir 判断 path 是否位于 dir 内（Clean 后前缀匹配，防 ../ 逃逸）。
func withinDir(dir, path string) bool {
	cleanDir := filepath.Clean(dir)
	cleanPath := filepath.Clean(path)
	return cleanPath == cleanDir || strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator))
}

// copyFile 复制普通文件，保留权限位。
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// cloneHandler 是 site.clone 的 verb 入口。
func cloneHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	var p cloneParams
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return nil, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalidParam("params 解析失败: " + err.Error())
		}
	}
	return svc.clone(p)
}
