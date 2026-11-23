package main

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// verbHandler 是单个业务方法的实现签名：自己校验入参、自己返回结果载荷。
type verbHandler func(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error)

// verbRegistry 是业务方法的唯一注册表：分发、未知方法判定都以此为准。
// 新增能力只需在此登记一行，不再改分发 switch。
var verbRegistry = map[string]verbHandler{
	"list":          listHandler,
	"create":        createHandler,
	"delete":        deleteHandler,
	"status":        statusHandler,
	"get":           getHandler,
	"bindSSL":       bindSSLHandler,
	"unbindSSL":     unbindSSLHandler,
	"setRewrite":    setRewriteHandler,
	"getRewrite":    getRewriteHandler,
	"setProxy":      setProxyHandler,
	"unsetProxy":    unsetProxyHandler,
	"setRedirect":   setRedirectHandler,
	"unsetRedirect": unsetRedirectHandler,
	"disable":       disableHandler,
	"enable":        enableHandler,
	"setIndex":      setIndexHandler,
	"clone":         cloneHandler,
	"setPassword":   setPasswordHandler,
	"clearPassword": clearPasswordHandler,
}

// handleSiteVerb 按注册表执行方法；未注册的方法回 E_NOTFOUND。
func handleSiteVerb(verb string, params json.RawMessage, response *pluginapi.Response) *pluginapi.Error {
	handler, ok := verbRegistry[verb]
	if !ok {
		return notFound(verb)
	}
	svc, perr := newSiteService()
	if perr != nil {
		return perr
	}
	result, perr := handler(svc, params)
	if perr != nil {
		return perr
	}
	response.Result = encode(result)
	return nil
}

// listHandler 返回清单内站点的真实生效状态。
func listHandler(svc *siteService, _ json.RawMessage) (any, *pluginapi.Error) {
	return svc.list()
}

// createHandler 建站：vhost + 清单 → nginx -t → reload，失败回滚。
func createHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := parseSiteParams(raw)
	if perr != nil {
		return nil, perr
	}
	return svc.create(p)
}

// deleteHandler 删站：备份并删除 → nginx -t → reload，任一失败恢复。
func deleteHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := parseSiteParams(raw)
	if perr != nil {
		return nil, perr
	}
	return svc.delete(p.Domain)
}

// statusHandler 返回单站点四个独立状态信号（只读，不 reload）。
func statusHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := parseSiteParams(raw)
	if perr != nil {
		return nil, perr
	}
	return svc.status(p.Domain)
}
