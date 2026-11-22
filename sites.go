package main

import "encoding/json"

// siteInfo 是站点列表中的单条记录。
type siteInfo struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Enabled   bool   `json:"enabled"`
}

// siteListResult 是 site.list 的结果；当前无真实站点来源，返回空列表。
type siteListResult struct {
	Sites   []siteInfo `json:"sites"`
	Count   int        `json:"count"`
	Applied bool       `json:"applied"`
}

// siteCreateResult 描述"将要创建"的站点；applied 恒为 false。
type siteCreateResult struct {
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	DefaultServer bool   `json:"default_server"`
	Applied       bool   `json:"applied"`
	Note          string `json:"note"`
}

// siteDeleteResult 描述"将要删除"的站点；applied 恒为 false。
type siteDeleteResult struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Applied   bool   `json:"applied"`
	Note      string `json:"note"`
}

// siteParams 是站点方法的入参。DefaultServer 仅用于识别并拒绝抢占默认站点。
type siteParams struct {
	Domain        string `json:"domain"`
	DefaultServer *bool  `json:"default_server"`
}

// encode 把结果结构体序列化为载荷；失败只可能来自结构体定义错误，回退空对象。
func encode[T any](v T) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// vhostPath 由已校验域名生成独立 conf.d vhost 路径，不触碰宿主主配置。
// 独立 conf.d vhost + include（提案 §2 M2-A），不抢 default_server（红队 R6）。
func vhostPath(domain string) string {
	return "conf.d/" + domain + ".conf"
}
