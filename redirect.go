package main

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 重定向三处硬约束（全部为了挡配置注入与伪协议）：
//   - from_path 只允许以 / 开头的路径安全字符，且不含 ".."；
//   - to_url 只允许 http:// / https:// 绝对地址或 / 开头的站内路径，
//     因此 javascript:/data:/vbscript: 等伪协议在正则层就被拒绝；
//   - 字符集排除引号、分号、井号与空白——它们会截断 nginx return 指令。
var (
	fromPathRE = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
	toURLRE    = regexp.MustCompile(`^(https?://|/)[A-Za-z0-9._~:/?&=%-]*$`)
)

const (
	// maxRedirects 限制单站点重定向条数，避免配置无限膨胀。
	maxRedirects = 32
	// maxFromPathLen / maxToURLLen 限制单条重定向长度。
	maxFromPathLen = 256
	maxToURLLen    = 512
)

// redirectParams 是 site.setRedirect 入参；code 省略时默认 301。
type redirectParams struct {
	Domain   string `json:"domain"`
	FromPath string `json:"from_path"`
	ToURL    string `json:"to_url"`
	Code     int    `json:"code,omitempty"`
}

// redirectUnsetParams 是 site.unsetRedirect 入参。
type redirectUnsetParams struct {
	Domain   string `json:"domain"`
	FromPath string `json:"from_path"`
}

// redirectSetResult 是 site.setRedirect 结果。
type redirectSetResult struct {
	applyResult
	FromPath  string `json:"from_path"`
	ToURL     string `json:"to_url"`
	Code      int    `json:"code"`
	Redirects int    `json:"redirects"`
}

// redirectUnsetResult 是 site.unsetRedirect 结果。
type redirectUnsetResult struct {
	applyResult
	FromPath  string `json:"from_path"`
	Removed   bool   `json:"removed"`
	Redirects int    `json:"redirects"`
}

// setRedirectHandler 新增/覆盖一条精确匹配重定向（同 from_path 幂等替换）。
func setRedirectHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[redirectParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	fromPath, perr := validateFromPath(p.FromPath)
	if perr != nil {
		return nil, perr
	}
	toURL, perr := validateToURL(p.ToURL)
	if perr != nil {
		return nil, perr
	}
	code := p.Code
	if code == 0 {
		code = 301
	}
	if code != 301 && code != 302 {
		return nil, invalidParam("code 只允许 301 或 302")
	}

	var count int
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		next := make([]redirectRule, 0, len(st.Redirects)+1)
		replaced := false
		for _, rule := range st.Redirects {
			if rule.FromPath == fromPath {
				next = append(next, redirectRule{FromPath: fromPath, ToURL: toURL, Code: code})
				replaced = true
				continue
			}
			next = append(next, rule)
		}
		if !replaced {
			if len(next) >= maxRedirects {
				return invalidParam("重定向条数已达上限 32，请先 unsetRedirect 清理")
			}
			next = append(next, redirectRule{FromPath: fromPath, ToURL: toURL, Code: code})
		}
		st.Redirects = next
		count = len(next)
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return redirectSetResult{
		applyResult: res,
		FromPath:    fromPath,
		ToURL:       toURL,
		Code:        code,
		Redirects:   count,
	}, nil
}

// unsetRedirectHandler 删除指定 from_path 的重定向；不存在时为幂等空操作。
func unsetRedirectHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[redirectUnsetParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	fromPath, perr := validateFromPath(p.FromPath)
	if perr != nil {
		return nil, perr
	}
	var count int
	removed := false
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		next := make([]redirectRule, 0, len(st.Redirects))
		for _, rule := range st.Redirects {
			if rule.FromPath == fromPath {
				removed = true
				continue
			}
			next = append(next, rule)
		}
		if next == nil {
			next = []redirectRule{}
		}
		st.Redirects = next
		count = len(next)
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return redirectUnsetResult{
		applyResult: res,
		FromPath:    fromPath,
		Removed:     removed,
		Redirects:   count,
	}, nil
}

// validateFromPath 校验重定向来源路径。
func validateFromPath(raw string) (string, *pluginapi.Error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", invalidParam("缺少必填字段 from_path")
	}
	if len(path) > maxFromPathLen {
		return "", invalidParam("from_path 超过 256 字符")
	}
	if !fromPathRE.MatchString(path) {
		return "", invalidParam("from_path 必须以 / 开头，仅允许字母数字与 ._~/-")
	}
	if strings.Contains(path, "..") {
		return "", invalidParam("from_path 不允许包含 ..")
	}
	return path, nil
}

// validateToURL 校验跳转目标：只允许 http(s) 绝对地址或站内 / 路径。
func validateToURL(raw string) (string, *pluginapi.Error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", invalidParam("缺少必填字段 to_url")
	}
	if len(target) > maxToURLLen {
		return "", invalidParam("to_url 超过 512 字符")
	}
	if strings.HasPrefix(target, "//") {
		return "", invalidParam("to_url 不允许协议相对地址（//host）")
	}
	if !toURLRE.MatchString(target) {
		return "", invalidParam("to_url 只允许 http:// 或 https:// 开头，或以 / 开头的站内路径")
	}
	return target, nil
}
