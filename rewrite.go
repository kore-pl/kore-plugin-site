package main

import (
	"encoding/json"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// rewriteTemplateNames 是 v1 允许的全部伪静态模板（不接受自定义原始片段）。
var rewriteTemplateNames = []string{"none", "wordpress", "laravel", "thinkphp", "discuz", "nextjs"}

// rewriteTemplateBodies 是各模板的标准 location / 片段；none 为空（走默认 =404）。
var rewriteTemplateBodies = map[string][]string{
	rewriteNone: nil,
	"wordpress": {
		`    location / {`,
		`        try_files $uri $uri/ /index.php?$query_string;`,
		`    }`,
	},
	"laravel": {
		`    location / {`,
		`        try_files $uri $uri/ /index.php?$query_string;`,
		`    }`,
	},
	"thinkphp": {
		`    location / {`,
		`        if (!-e $request_filename) {`,
		`            rewrite ^(.*)$ /index.php?s=$1 last;`,
		`        }`,
		`    }`,
	},
	"discuz": {
		`    location / {`,
		`        rewrite ^([^\.]*)/topic-(.+)\.html$ $1/portal.php?mod=topic&topic=$2 last;`,
		`        rewrite ^([^\.]*)/article-([0-9]+)-([0-9]+)\.html$ $1/portal.php?mod=view&aid=$2&page=$3 last;`,
		`        rewrite ^([^\.]*)/forum-(\w+)-([0-9]+)\.html$ $1/forum.php?mod=forumdisplay&fid=$2&page=$3 last;`,
		`        rewrite ^([^\.]*)/thread-([0-9]+)-([0-9]+)-([0-9]+)\.html$ $1/forum.php?mod=viewthread&tid=$2&extra=page%3D$4&page=$3 last;`,
		`        rewrite ^([^\.]*)/space-(username|uid)-(.+)\.html$ $1/home.php?mod=space&$2=$3 last;`,
		`        rewrite ^([^\.]*)/(fid|tid)-([0-9]+)\.html$ $1/index.php?action=$2&value=$3 last;`,
		`        rewrite ^([^\.]*)/([a-zA-Z0-9_-]+)-([0-9]+)-([0-9]+)\.html$ $1/$2.php?mod=$3&page=$4 last;`,
		`        rewrite ^([^\.]*)/([a-zA-Z0-9_-]+)-([0-9]+)\.html$ $1/$2.php?id=$3 last;`,
		`    }`,
	},
	"nextjs": {
		`    location / {`,
		`        try_files $uri $uri/ /index.html;`,
		`    }`,
	},
}

// isRewriteTemplate 判断模板名是否在枚举内。
func isRewriteTemplate(name string) bool {
	_, ok := rewriteTemplateBodies[name]
	return ok
}

// rewriteParams 是 site.setRewrite 入参。
type rewriteParams struct {
	Domain   string `json:"domain"`
	Template string `json:"template"`
}

// rewriteSetResult 是 site.setRewrite 结果。
type rewriteSetResult struct {
	applyResult
	Template     string `json:"template"`
	ProxyCleared bool   `json:"proxy_cleared"`
}

// rewriteGetResult 是 site.getRewrite 结果（解析注释块，不跑 nginx）。
type rewriteGetResult struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Template  string `json:"template"`
	Enabled   bool   `json:"enabled"`
}

// setRewriteHandler 整体替换伪静态块；与反代互斥（都占用 location /）。
func setRewriteHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[rewriteParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	if strings.TrimSpace(p.Template) == "" {
		return nil, invalidParam("缺少必填字段 template")
	}
	template := strings.ToLower(strings.TrimSpace(p.Template))
	if !isRewriteTemplate(template) {
		return nil, invalidParam("template 必须是 " + strings.Join(rewriteTemplateNames, "|"))
	}
	proxyCleared := false
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		if st.Proxy != nil {
			st.Proxy = nil
			proxyCleared = true
		}
		st.Rewrite = template
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return rewriteSetResult{applyResult: res, Template: template, ProxyCleared: proxyCleared}, nil
}

// getRewriteHandler 返回当前模板名（从注释块解析，禁用站点同样可读）。
func getRewriteHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	st, enabled, perr := readSiteState(p.Domain)
	if perr != nil {
		return nil, perr
	}
	return rewriteGetResult{
		Domain:    p.Domain,
		VHostPath: vhostPath(p.Domain),
		Template:  st.Rewrite,
		Enabled:   enabled,
	}, nil
}
