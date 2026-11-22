package main

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// domainMaxLen 是 RFC 1035 域名长度上限。
const domainMaxLen = 253

// domainRE 严格约束站点域名：小写字母数字与连字符组成的标签，至少两个标签。
// 它同时拒绝路径分隔符与 ".."，防止 conf.d vhost 路径穿越（红队 R6 / 提案 §2）。
var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// notAppliedNote 明确本批只做校验与结构化返回，不触碰宿主 nginx。
const notAppliedNote = "M2-A 第一批：仅参数校验与结构化返回，未改动 nginx；独立 conf.d vhost + include 留待 M2-A 第二批"

// statusResult 是 kore.ping 的结果（PLUGIN-PROTOCOL §7.1）。
type statusResult struct {
	Status     string `json:"status"`
	UptimeS    int64  `json:"uptime_s"`
	Goroutines int    `json:"goroutines"`
	RSSKB      int64  `json:"rss_kb"`
}

// dispatch 把请求分发为响应帧；第二个返回值表示收到 kore.shutdown。
func dispatch(request *pluginapi.Request, cfg config, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，优雅退出")
		return response, true
	case "kore.ping":
		response.Result = pingResult(cfg)
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
		return response, false
	}
	verb, ok := businessVerb(request.Method, cfg.id)
	if !ok {
		response.Error = notFound(request.Method)
		return response, false
	}
	if perr := handleSiteVerb(verb, request.Params, response); perr != nil {
		response.Error = perr
		response.Result = nil
	}
	return response, false
}

// handleSiteVerb 执行站点方法：当前只校验参数并返回结构化"提案"，不落盘。
func handleSiteVerb(verb string, params json.RawMessage, response *pluginapi.Response) *pluginapi.Error {
	switch verb {
	case "list":
		response.Result = encode(siteListResult{Sites: []siteInfo{}, Count: 0, Applied: false})
		return nil
	case "create", "delete":
		p, perr := parseSiteParams(params)
		if perr != nil {
			return perr
		}
		path := vhostPath(p.Domain)
		if verb == "create" {
			response.Result = encode(siteCreateResult{
				Domain: p.Domain, VHostPath: path, DefaultServer: false,
				Applied: false, Note: notAppliedNote,
			})
		} else {
			response.Result = encode(siteDeleteResult{
				Domain: p.Domain, VHostPath: path,
				Applied: false, Note: notAppliedNote,
			})
		}
		return nil
	default:
		return notFound(verb)
	}
}

// businessVerb 把 <id>.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method, id string) (string, bool) {
	verb := strings.TrimPrefix(method, id+".")
	switch verb {
	case "list", "create", "delete":
		return verb, true
	default:
		return "", false
	}
}

// parseSiteParams 校验站点方法入参：对象形状、domain 合法性、禁止 default_server。
func parseSiteParams(raw json.RawMessage) (siteParams, *pluginapi.Error) {
	var p siteParams
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return p, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, invalidParam("params 解析失败: " + err.Error())
		}
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return p, perr
	}
	if p.DefaultServer != nil && *p.DefaultServer {
		return p, invalidParam("禁止占用 default_server（红队 R6）")
	}
	return p, nil
}

// validateDomain 校验域名，拒绝空值、超长、大写与非法字符（含路径穿越）。
func validateDomain(domain string) *pluginapi.Error {
	if domain == "" {
		return invalidParam("缺少必填字段 domain")
	}
	if len(domain) > domainMaxLen {
		return invalidParam("domain 超过 253 字符")
	}
	if domain != strings.ToLower(domain) {
		return invalidParam("domain 必须为小写")
	}
	if !domainRE.MatchString(domain) {
		return invalidParam("domain 非法：仅允许小写字母数字、连字符与点，且至少两个标签")
	}
	return nil
}

// invalidParam 构造 E_INVALID 错误体（§9）。
func invalidParam(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

// notFound 构造 E_NOTFOUND 错误体（§9）。
func notFound(method string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "未知方法：" + method}
}

// pingResult 组装 kore.ping 结果。
func pingResult(cfg config) json.RawMessage {
	return encode(statusResult{
		Status:     "ready",
		UptimeS:    int64(time.Since(cfg.startedAt).Seconds()),
		Goroutines: runtime.NumGoroutine(),
		RSSKB:      rssKB(),
	})
}
