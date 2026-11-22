package main

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// indexFileRE 约束默认文档文件名：单段文件名，不允许路径与配置注入字符。
var indexFileRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxIndexFiles 限制 index 指令条目数。
const maxIndexFiles = 16

// indexParams 是 site.setIndex 入参；files 省略时用标准默认三件套。
type indexParams struct {
	Domain string   `json:"domain"`
	Files  []string `json:"files,omitempty"`
}

// indexResult 是 site.setIndex 结果。
type indexResult struct {
	applyResult
	Index []string `json:"index"`
}

// setIndexHandler 设置 index 指令（默认 index.html index.htm index.php）。
func setIndexHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[indexParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	files, perr := normalizeIndexFiles(p.Files)
	if perr != nil {
		return nil, perr
	}
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		st.Index = files
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return indexResult{applyResult: res, Index: files}, nil
}

// normalizeIndexFiles 去空白、去重并校验每个文件名；空列表回落到标准默认值。
func normalizeIndexFiles(raw []string) ([]string, *pluginapi.Error) {
	if len(raw) == 0 {
		return defaultIndexFiles(), nil
	}
	if len(raw) > maxIndexFiles {
		return nil, invalidParam("index 文件数超过上限 16")
	}
	seen := make(map[string]bool, len(raw))
	files := make([]string, 0, len(raw))
	for _, item := range raw {
		name := strings.TrimSpace(item)
		if name == "" || name == "." || name == ".." || !indexFileRE.MatchString(name) {
			return nil, invalidParam("index 文件名非法: " + name + "（只允许字母数字与 ._-，且不得为 . / ..）")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	if len(files) == 0 {
		return defaultIndexFiles(), nil
	}
	return files, nil
}
