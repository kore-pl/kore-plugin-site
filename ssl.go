package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TLS 协议与套件固定写死（Mozilla intermediate 基线）：不接受调用方自定义
// 原始 nginx 片段，这是 v1 的安全边界。
const (
	sslProtocols = "TLSv1.2 TLSv1.3"
	sslCiphers   = "ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:" +
		"ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384"
)

// sslBindParams 是 site.bindSSL 入参；force_https 省略时默认 true（强制跳转）。
type sslBindParams struct {
	Domain     string `json:"domain"`
	CertPath   string `json:"cert_path"`
	KeyPath    string `json:"key_path"`
	ForceHTTPS *bool  `json:"force_https,omitempty"`
}

// sslBindResult 是 site.bindSSL 结果。
type sslBindResult struct {
	applyResult
	SSLBound   bool   `json:"ssl_bound"`
	ForceHTTPS bool   `json:"force_https"`
	CertPath   string `json:"cert_path"`
	KeyPath    string `json:"key_path"`
}

// sslUnbindResult 是 site.unbindSSL 结果。
type sslUnbindResult struct {
	applyResult
	SSLBound bool `json:"ssl_bound"`
}

// bindSSLHandler 绑定证书：写入 443 监听 + 证书 + TLS 套件，默认带 80→443 跳转。
func bindSSLHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[sslBindParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	if perr := validateCertFile("cert_path", p.CertPath); perr != nil {
		return nil, perr
	}
	if perr := validateCertFile("key_path", p.KeyPath); perr != nil {
		return nil, perr
	}
	forceHTTPS := true
	if p.ForceHTTPS != nil {
		forceHTTPS = *p.ForceHTTPS
	}
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		st.SSL = &sslState{CertPath: p.CertPath, KeyPath: p.KeyPath, ForceHTTPS: forceHTTPS}
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return sslBindResult{
		applyResult: res,
		SSLBound:    true,
		ForceHTTPS:  forceHTTPS,
		CertPath:    p.CertPath,
		KeyPath:     p.KeyPath,
	}, nil
}

// unbindSSLHandler 移除整个 ssl 块（含 443 监听与跳转），站点回到纯 HTTP。
func unbindSSLHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	res, _, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		st.SSL = nil
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return sslUnbindResult{applyResult: res, SSLBound: false}, nil
}

// validateCertFile 校验证书/私钥路径：绝对、规范化、字符集安全且真实存在。
// 路径会原样进入 nginx 配置，这是防配置注入的硬闸。
func validateCertFile(field, path string) *pluginapi.Error {
	if path == "" {
		return invalidParam("缺少必填字段 " + field)
	}
	if !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return invalidParam(field + " 必须是规范化的绝对路径")
	}
	if !rootPathRE.MatchString(path) {
		return invalidParam(field + " 仅允许字母数字与 ._/@+-，禁止空白、分号、花括号与换行")
	}
	st, err := os.Stat(path)
	if err != nil {
		return invalidParam(field + " 不可读: " + err.Error())
	}
	if !st.Mode().IsRegular() {
		return invalidParam(field + " 不是普通文件: " + path)
	}
	return nil
}
