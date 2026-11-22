package main

import "strings"

// L2 配置以 vhost 文件本身为唯一事实来源：受管片段用注释标记块
// （"# >>> kore-site:<key> >>>" … "# <<< kore-site:<key> <<<"）括起来，
// 每次修改都走「解析 → 改状态 → 整体重渲染」，不做脆弱的行内字符串拼接。
// 这样 getRewrite 之类的读取方只解析注释块，不依赖第二份状态文件。
const (
	blockBeginPrefix = "# >>> kore-site:"
	blockEndPrefix   = "# <<< kore-site:"
	// rewriteNone 表示未启用任何伪静态模板（默认 location / try_files =404）。
	rewriteNone = "none"
)

// 受管注释块的固定 key（redirect 以 from_path 为 key，形如 redirect:/old）。
const (
	blockKeySSL            = "ssl"
	blockKeyRewrite        = "rewrite"
	blockKeyProxy          = "proxy"
	blockKeyHTTPRedirect   = "http-redirect"
	blockKeyRedirectPrefix = "redirect:"
)

// sslState 是已绑定的证书配置；force_https 由是否存在 http-redirect 服务块表示。
type sslState struct {
	CertPath   string
	KeyPath    string
	ForceHTTPS bool
}

// proxyState 是反向代理配置。
type proxyState struct {
	Upstream string
	WS       bool
}

// redirectRule 是一条精确匹配重定向（location = <from> → return <code> <to>）。
type redirectRule struct {
	FromPath string
	ToURL    string
	Code     int
}

// siteState 是单站点的完整可编辑状态：parseSiteState 与 renderVHost 互为逆运算。
type siteState struct {
	Domain    string
	Root      string
	Port      int
	Index     []string
	SSL       *sslState
	Rewrite   string
	Proxy     *proxyState
	Redirects []redirectRule
}

// disabledSuffix 是停用站点的 vhost 后缀：conf.d/*.conf 不会匹配它，
// 因此改名即等于"从 nginx 配置里摘掉"，而配置本身完整保留。
const disabledSuffix = ".disabled"

// siteDomainParams 是只带 domain 的入参（unbindSSL / get / getRewrite 等共用）。
type siteDomainParams struct {
	Domain string `json:"domain"`
}

// blockEntry 是一个受管注释块：key + 块内原始行（不含标记行）。
type blockEntry struct {
	key  string
	body []string
}

// scanBlocks 顺序扫描内容中的全部受管注释块；未闭合的块不进入结果。
func scanBlocks(content string) []blockEntry {
	var (
		blocks []blockEntry
		cur    = -1
	)
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, blockBeginPrefix) && strings.HasSuffix(trimmed, ">>>") {
			key := strings.TrimSuffix(strings.TrimPrefix(trimmed, blockBeginPrefix), ">>>")
			blocks = append(blocks, blockEntry{key: strings.TrimSpace(key)})
			cur = len(blocks) - 1
			continue
		}
		if strings.HasPrefix(trimmed, blockEndPrefix) && strings.HasSuffix(trimmed, "<<<") {
			cur = -1
			continue
		}
		if cur >= 0 {
			blocks[cur].body = append(blocks[cur].body, line)
		}
	}
	return blocks
}

// findBlock 按 key 取块体；不存在返回 false。
func findBlock(blocks []blockEntry, key string) ([]string, bool) {
	for _, b := range blocks {
		if b.key == key {
			return b.body, true
		}
	}
	return nil, false
}

// hasBlock 判断某个受管块是否存在（http-redirect 用它表达 force_https）。
func hasBlock(blocks []blockEntry, key string) bool {
	_, ok := findBlock(blocks, key)
	return ok
}

// directiveValue 从块内取 "name value;" 指令值，去分号与可选引号。
func directiveValue(body []string, name string) string {
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, name+" ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, name+" "))
		value = strings.TrimSuffix(value, ";")
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

// disabledVHostPath 返回停用态 vhost 路径（vhost 原路径 + .disabled）。
func disabledVHostPath(domain string) string {
	return vhostPath(domain) + disabledSuffix
}
