package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// upstreamRE 严格约束反代上游：只允许 http://host:port，host 为域名或 IP。
// 这条正则同时关掉了 userinfo（user:pass@）、路径、查询串与任何空白/分号注入面。
var upstreamRE = regexp.MustCompile(`^http://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?):([0-9]{1,5})$`)

// proxyParams 是 site.setProxy 入参。
type proxyParams struct {
	Domain   string `json:"domain"`
	Upstream string `json:"upstream"`
	WS       *bool  `json:"ws,omitempty"`
}

// proxySetResult 是 site.setProxy 结果。
type proxySetResult struct {
	applyResult
	Proxy          bool   `json:"proxy"`
	Upstream       string `json:"upstream"`
	WS             bool   `json:"ws"`
	RewriteCleared bool   `json:"rewrite_cleared"`
}

// proxyUnsetResult 是 site.unsetProxy 结果。
type proxyUnsetResult struct {
	applyResult
	Proxy bool `json:"proxy"`
}

// setProxyHandler 生成 location / 反代块；ws=true 时补 WebSocket 升级头。
func setProxyHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[proxyParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	upstream, perr := parseUpstream(p.Upstream)
	if perr != nil {
		return nil, perr
	}
	ws := p.WS != nil && *p.WS
	rewriteCleared := false
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		if st.Rewrite != rewriteNone {
			st.Rewrite = rewriteNone
			rewriteCleared = true
		}
		st.Proxy = &proxyState{Upstream: upstream, WS: ws}
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return proxySetResult{
		applyResult:    res,
		Proxy:          true,
		Upstream:       upstream,
		WS:             ws,
		RewriteCleared: rewriteCleared,
	}, nil
}

// unsetProxyHandler 移除反代块，站点回到默认静态处理。
func unsetProxyHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		st.Proxy = nil
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return proxyUnsetResult{applyResult: res, Proxy: false}, nil
}

// parseUpstream 校验并归一上游地址。
func parseUpstream(raw string) (string, *pluginapi.Error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", invalidParam("缺少必填字段 upstream")
	}
	m := upstreamRE.FindStringSubmatch(value)
	if m == nil {
		if strings.HasPrefix(value, "https://") {
			return "", invalidParam("upstream 只支持 http://，TLS 终止由站点 SSL 承担")
		}
		return "", invalidParam("upstream 必须是 http://host:port（host 只允许域名或 IP，禁止路径与凭据）")
	}
	port, err := strconv.Atoi(m[3])
	if err != nil || port < 1 || port > 65535 {
		return "", invalidParam("upstream 端口必须在 1..65535")
	}
	return value, nil
}
