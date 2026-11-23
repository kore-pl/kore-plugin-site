package main

import "strings"

// renderVHost 把站点状态整体重渲染成 vhost 片段。无任何 L2 特性时输出与
// v1 vhostTemplate 逐字节一致，保证老站点不会被无谓改写（幂等编辑的前提）。
func renderVHost(st siteState) []byte {
	var b strings.Builder
	b.WriteString("# kore-plugin-site managed vhost for " + st.Domain + "\n")
	b.WriteString("# 本片段由 site 插件生成；禁止抢占默认站点（红队 R6）。\n")
	if st.SSL != nil && st.SSL.ForceHTTPS {
		writeHTTPRedirectServer(&b, st)
	}
	b.WriteString("server {\n")
	// force_https 时 80 端口由独立的跳转 server 块接管，主块只监听 443。
	if st.SSL == nil || !st.SSL.ForceHTTPS {
		b.WriteString("    listen " + itoa(st.Port) + ";\n")
	}
	b.WriteString("    server_name " + st.Domain + ";\n")
	if st.SSL != nil {
		writeSSLBlock(&b, st.SSL)
	}
	b.WriteString("    root " + st.Root + ";\n")
	if len(st.Index) > 0 {
		b.WriteString("    index " + strings.Join(st.Index, " ") + ";\n")
	}
	writeAuthBlock(&b, st)
	writeContentBlock(&b, st)
	for _, rule := range st.Redirects {
		writeRedirectBlock(&b, rule)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

// writeSSLBlock 写出 443 监听、证书与 TLS1.2+ 套件；listen 也放在块内，
// 这样 unbindSSL 移除整块即彻底还原为纯 HTTP。
func writeSSLBlock(b *strings.Builder, s *sslState) {
	b.WriteString("    " + beginMarker(blockKeySSL) + "\n")
	b.WriteString("    listen 443 ssl;\n")
	b.WriteString("    ssl_certificate \"" + s.CertPath + "\";\n")
	b.WriteString("    ssl_certificate_key \"" + s.KeyPath + "\";\n")
	b.WriteString("    ssl_protocols " + sslProtocols + ";\n")
	b.WriteString("    ssl_ciphers " + sslCiphers + ";\n")
	b.WriteString("    ssl_prefer_server_ciphers on;\n")
	b.WriteString("    ssl_session_cache shared:SSL:10m;\n")
	b.WriteString("    ssl_session_timeout 1d;\n")
	b.WriteString("    " + endMarker(blockKeySSL) + "\n")
}

// writeContentBlock 写出 location / 主处理：反代与伪静态互斥（两者都占用
// location /，同时存在会让 nginx -t 直接报 duplicate location）。
func writeContentBlock(b *strings.Builder, st siteState) {
	switch {
	case st.Proxy != nil:
		writeProxyBlock(b, st.Proxy)
	case st.Rewrite != rewriteNone:
		writeRewriteBlock(b, st.Rewrite)
	default:
		b.WriteString("    location / {\n")
		b.WriteString("        try_files $uri $uri/ =404;\n")
		b.WriteString("    }\n")
	}
}

// writeProxyBlock 写入反向代理 location，ws=true 时补 WebSocket 升级头。
func writeProxyBlock(b *strings.Builder, p *proxyState) {
	b.WriteString("    " + beginMarker(blockKeyProxy) + "\n")
	if p.WS {
		b.WriteString("    # ws: 1\n")
	}
	b.WriteString("    location / {\n")
	b.WriteString("        proxy_pass " + p.Upstream + ";\n")
	b.WriteString("        proxy_set_header Host $host;\n")
	b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
	if p.WS {
		b.WriteString("        proxy_http_version 1.1;\n")
		b.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("        proxy_set_header Connection \"upgrade\";\n")
	}
	b.WriteString("    }\n")
	b.WriteString("    " + endMarker(blockKeyProxy) + "\n")
}

// writeRewriteBlock 写入伪静态模板：块首注释记录模板名，getRewrite 据此解析。
func writeRewriteBlock(b *strings.Builder, template string) {
	b.WriteString("    " + beginMarker(blockKeyRewrite) + "\n")
	b.WriteString("    # template: " + template + "\n")
	for _, line := range rewriteTemplateBodies[template] {
		b.WriteString(line + "\n")
	}
	b.WriteString("    " + endMarker(blockKeyRewrite) + "\n")
}

// writeRedirectBlock 写出一条精确匹配重定向（location = 优先于前缀匹配）。
func writeRedirectBlock(b *strings.Builder, r redirectRule) {
	key := blockKeyRedirectPrefix + r.FromPath
	b.WriteString("    " + beginMarker(key) + "\n")
	b.WriteString("    location = " + r.FromPath + " {\n")
	b.WriteString("        return " + itoa(r.Code) + " " + r.ToURL + ";\n")
	b.WriteString("    }\n")
	b.WriteString("    " + endMarker(key) + "\n")
}

// writeHTTPRedirectServer 写出 80→443 跳转的独立 server 块（force_https 时）。
func writeHTTPRedirectServer(b *strings.Builder, st siteState) {
	b.WriteString(beginMarker(blockKeyHTTPRedirect) + "\n")
	b.WriteString("server {\n")
	b.WriteString("    listen " + itoa(st.Port) + ";\n")
	b.WriteString("    server_name " + st.Domain + ";\n")
	b.WriteString("    return 301 https://$host$request_uri;\n")
	b.WriteString("}\n")
	b.WriteString(endMarker(blockKeyHTTPRedirect) + "\n")
}

// beginMarker / endMarker 生成受管块的起止注释行。
func beginMarker(key string) string { return blockBeginPrefix + key + " >>>" }
func endMarker(key string) string   { return blockEndPrefix + key + " <<<" }
