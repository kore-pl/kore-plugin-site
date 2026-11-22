package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseSiteState 从已落盘 vhost 解析出可编辑状态。容错优先：v1 生成的
// 纯文本 vhost（无任何标记块）解析为「无 SSL/伪静态/反代/重定向」。
func parseSiteState(domain, content string) siteState {
	st := siteState{
		Domain:  domain,
		Root:    fmt.Sprintf(defaultRoot, domain),
		Port:    defaultPort,
		Index:   defaultIndexFiles(),
		Rewrite: rewriteNone,
	}
	if root := parseRootDirective(content); root != "" {
		st.Root = root
	}
	if idx := parseIndexDirective(content); len(idx) > 0 {
		st.Index = idx
	}
	if port := parseListenPort(content); port > 0 {
		st.Port = port
	}
	blocks := scanBlocks(content)
	st.SSL = parseSSLState(blocks)
	st.Rewrite = parseRewriteTemplate(blocks)
	st.Proxy = parseProxyState(blocks)
	st.Redirects = parseRedirectRules(blocks)
	return st
}

// parseSSLState 从 ssl 块还原证书路径；force_https 看是否存在 80 跳转服务块。
func parseSSLState(blocks []blockEntry) *sslState {
	body, ok := findBlock(blocks, blockKeySSL)
	if !ok {
		return nil
	}
	cert := directiveValue(body, "ssl_certificate")
	key := directiveValue(body, "ssl_certificate_key")
	if cert == "" || key == "" {
		return nil
	}
	return &sslState{CertPath: cert, KeyPath: key, ForceHTTPS: hasBlock(blocks, blockKeyHTTPRedirect)}
}

// parseRewriteTemplate 从 rewrite 块的 "# template: <name>" 注释还原模板名。
func parseRewriteTemplate(blocks []blockEntry) string {
	body, ok := findBlock(blocks, blockKeyRewrite)
	if !ok {
		return rewriteNone
	}
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# template:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(trimmed, "# template:"))
		if isRewriteTemplate(name) {
			return name
		}
	}
	return rewriteNone
}

// parseProxyState 从 proxy 块还原上游地址与 WebSocket 开关。
func parseProxyState(blocks []blockEntry) *proxyState {
	body, ok := findBlock(blocks, blockKeyProxy)
	if !ok {
		return nil
	}
	upstream := directiveValue(body, "proxy_pass")
	if upstream == "" {
		return nil
	}
	ws := false
	for _, line := range body {
		if strings.TrimSpace(line) == "# ws: 1" {
			ws = true
			break
		}
	}
	return &proxyState{Upstream: upstream, WS: ws}
}

// parseRedirectRules 按文件顺序还原所有 redirect:<from_path> 块。
func parseRedirectRules(blocks []blockEntry) []redirectRule {
	var rules []redirectRule
	for _, b := range blocks {
		if !strings.HasPrefix(b.key, blockKeyRedirectPrefix) {
			continue
		}
		fromPath := strings.TrimPrefix(b.key, blockKeyRedirectPrefix)
		value := directiveValue(b.body, "return")
		fields := strings.Fields(value)
		if len(fields) != 2 {
			continue
		}
		code, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		rules = append(rules, redirectRule{FromPath: fromPath, ToURL: fields[1], Code: code})
	}
	return rules
}

// parseListenPort 取第一条"非 ssl"的 listen 指令端口（TLS 监听固定在 443）。
func parseListenPort(content string) int {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "listen ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 || strings.Contains(fields[1], "ssl") {
			continue
		}
		value := strings.TrimSuffix(fields[1], ";")
		port, err := strconv.Atoi(value)
		if err == nil && port > 0 && port <= 65535 {
			return port
		}
	}
	return 0
}

// parseIndexDirective 取 server 级 index 指令的文件名列表。
func parseIndexDirective(content string) []string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "index ") {
			continue
		}
		var files []string
		for _, field := range strings.Fields(trimmed) {
			name := strings.TrimSuffix(field, ";")
			if name != "" && name != "index" {
				files = append(files, name)
			}
		}
		if len(files) > 0 {
			return files
		}
	}
	return nil
}

// defaultIndexFiles 是未设置 index 时的标准默认文档列表。
func defaultIndexFiles() []string {
	return []string{"index.html", "index.htm", "index.php"}
}
