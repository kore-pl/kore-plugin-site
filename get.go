package main

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// sslSummary / rewriteSummary / proxySummary / redirectSummary 是 site.get 的
// 只读子视图：字段全部显式定形，不用 map[string]any 逃避结构设计。
type sslSummary struct {
	Bound      bool   `json:"bound"`
	CertPath   string `json:"cert_path,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	ForceHTTPS bool   `json:"force_https"`
	Protocols  string `json:"protocols,omitempty"`
}

type rewriteSummary struct {
	Template string `json:"template"`
}

type proxySummary struct {
	Enabled  bool   `json:"enabled"`
	Upstream string `json:"upstream,omitempty"`
	WS       bool   `json:"ws"`
}

type redirectSummary struct {
	FromPath string `json:"from_path"`
	ToURL    string `json:"to_url"`
	Code     int    `json:"code"`
}

// siteGetResult 是 site.get 的完整配置摘要（只读，不跑 nginx、不改文件）。
type siteGetResult struct {
	Domain    string   `json:"domain"`
	VHostPath string   `json:"vhost_path"`
	Managed   bool     `json:"managed"`
	Enabled   bool     `json:"enabled"`
	Disabled  bool     `json:"disabled"`
	Root      string   `json:"root"`
	Port      int      `json:"port"`
	Index     []string `json:"index"`
	// SiteDirExists 是兼容字段：true 当且仅当 SiteDirState == "present"。
	SiteDirExists bool `json:"site_dir_exists"`
	// SiteDirState 是三态 present/absent/unknown（§3.5 #6，与 site.status 同口径）。
	SiteDirState string `json:"site_dir_state"`
	// SiteDirReason 只在 unknown 时给出原因与处置。
	SiteDirReason string            `json:"site_dir_reason,omitempty"`
	SSL           sslSummary        `json:"ssl"`
	Rewrite       rewriteSummary    `json:"rewrite"`
	Proxy         proxySummary      `json:"proxy"`
	Redirects     []redirectSummary `json:"redirects"`
}

// getHandler 返回站点完整配置摘要；禁用站点读 .disabled 副本但仍然可见。
func getHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
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
	manifest, perr := svc.store.loadManifest()
	if perr != nil {
		return nil, perr
	}
	path := vhostPath(p.Domain)
	result := siteGetResult{
		Domain:    p.Domain,
		VHostPath: path,
		Managed:   manifest.Sites[p.Domain] == path,
		Enabled:   enabled,
		Disabled:  !enabled,
		Root:      st.Root,
		Port:      st.Port,
		Index:     st.Index,
		Rewrite:   rewriteSummary{Template: st.Rewrite},
		Redirects: make([]redirectSummary, 0, len(st.Redirects)),
	}
	result.SiteDirState, result.SiteDirReason = probeSiteDir(svc.runner, st.Root)
	result.SiteDirExists = result.SiteDirState == siteDirPresent
	if st.SSL != nil {
		result.SSL = sslSummary{
			Bound:      true,
			CertPath:   st.SSL.CertPath,
			KeyPath:    st.SSL.KeyPath,
			ForceHTTPS: st.SSL.ForceHTTPS,
			Protocols:  sslProtocols,
		}
	}
	if st.Proxy != nil {
		result.Proxy = proxySummary{Enabled: true, Upstream: st.Proxy.Upstream, WS: st.Proxy.WS}
	}
	for _, rule := range st.Redirects {
		result.Redirects = append(result.Redirects, redirectSummary{
			FromPath: rule.FromPath, ToURL: rule.ToURL, Code: rule.Code,
		})
	}
	return result, nil
}
