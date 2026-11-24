package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// PHP 站点必须落到 PHP-FPM：nginx 自身不解析 .php，只配 try_files 的站点拿到
// /index.php 会当静态文件下载或 404。因此本插件把「socket 路径」作为站点状态
// 的一部分显式写进受管块 kore-site:php，并在启用时校验 socket 真实存在——
// 探测不到就回 E_INVALID，绝不静默生成一个"挂得上域名、跑不起来"的站点。

const (
	// envPHPSocketDirs 覆盖 FPM socket 扫描目录（冒号分隔）；测试与非常规布局用。
	envPHPSocketDirs = "KORE_SITE_PHP_SOCKET_DIRS"
	// defaultPHPSocketDirs 覆盖 Debian/Ubuntu（/run/php）与 RHEL/Alpine（/run/php-fpm）。
	defaultPHPSocketDirs = "/run/php:/var/run/php:/run/php-fpm:/var/run/php-fpm"
	// phpAutoVersion 表示"用探测到的最高版本"。
	phpAutoVersion = "auto"
)

var (
	// phpFpmSocketRE 从 php<X.Y>-fpm.sock 文件名解析版本（Debian 布局）。
	phpFpmSocketRE = regexp.MustCompile(`^php-?([0-9]+\.[0-9]+)-fpm\.sock$`)
	// phpVersionRE 约束版本号形状：会写进 nginx 配置注释并被解析回来，只允许数字与点。
	phpVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	// phpSocketPathRE 约束 socket 路径字符集：它会原样进入 nginx 配置，
	// 与 rootPathRE 同级别的防配置注入硬闸（禁止空白、分号、花括号、换行、引号）。
	phpSocketPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]*$`)
	// phpGenericSockets 是不带版本号的通用 socket 名（RHEL www.sock / 通用 php-fpm.sock）。
	phpGenericSockets = []string{"php-fpm.sock", "www.sock"}
)

// fastcgiParams 是内联的最小 FastCGI 参数集（等价 Debian 的 fastcgi_params + REDIRECT_STATUS）。
//
// 刻意内联而不写 `include fastcgi_params;`：vhost 片段保持自包含，不依赖宿主是否
// 存在 /etc/nginx/fastcgi_params。缺失时 nginx -t 会报 file not found，那条报错与
// 「PHP 没装」毫无关系，会把排障引到错误方向。
var fastcgiParams = []string{
	"SCRIPT_FILENAME $document_root$fastcgi_script_name;",
	"QUERY_STRING $query_string;",
	"REQUEST_METHOD $request_method;",
	"CONTENT_TYPE $content_type;",
	"CONTENT_LENGTH $content_length;",
	"SCRIPT_NAME $fastcgi_script_name;",
	"REQUEST_URI $request_uri;",
	"DOCUMENT_URI $document_uri;",
	"DOCUMENT_ROOT $document_root;",
	"SERVER_PROTOCOL $server_protocol;",
	"REQUEST_SCHEME $scheme;",
	"HTTPS $https if_not_empty;",
	"GATEWAY_INTERFACE CGI/1.1;",
	"SERVER_SOFTWARE nginx/$nginx_version;",
	"REMOTE_ADDR $remote_addr;",
	"REMOTE_PORT $remote_port;",
	"SERVER_ADDR $server_addr;",
	"SERVER_PORT $server_port;",
	"SERVER_NAME $server_name;",
	"REDIRECT_STATUS 200;",
}

// phpRewriteTemplates 是依赖 PHP-FPM 的伪静态模板集合。
// 选这些模板却给不出 PHP ⇒ 必须拒绝（否则就是本缺陷的原形：配置写着 wordpress、
// 实际由 nginx 当静态处理，页面永远 404）。
var phpRewriteTemplates = map[string]bool{
	"wordpress": true,
	"laravel":   true,
	"thinkphp":  true,
	"discuz":    true,
}

// isPHPRewriteTemplate 判断模板是否必须配 PHP-FPM。
func isPHPRewriteTemplate(name string) bool {
	return phpRewriteTemplates[name]
}

// versionOrEmpty 是 nil 安全的版本读取：未启用 PHP 时返回空串，避免调用方到处判空。
func (p *phpState) versionOrEmpty() string {
	if p == nil {
		return ""
	}
	return p.Version
}

// socketOrEmpty 是 nil 安全的 socket 读取。
func (p *phpState) socketOrEmpty() string {
	if p == nil {
		return ""
	}
	return p.Socket
}

// fpmCandidate 是一个探测到的 FPM socket 候选；Version 解析不出时为空串。
type fpmCandidate struct {
	Version string `json:"version,omitempty"`
	Socket  string `json:"socket"`
}

// phpResolution 是一次 PHP 解析结果：State 可直接落盘，Verified 表示 socket
// 在本机真实可见（容器模式下显式声明的 socket 不可见属正常，如实披露而非假装验证过）。
type phpResolution struct {
	State    phpState
	Verified bool
	Source   string
}

// phpSocketDirs 返回本次生效的 FPM socket 扫描目录（环境变量 > 内置默认）。
func phpSocketDirs() []string {
	raw := strings.TrimSpace(os.Getenv(envPHPSocketDirs))
	if raw == "" {
		raw = defaultPHPSocketDirs
	}
	dirs := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, part := range strings.Split(raw, ":") {
		dir := strings.TrimSpace(part)
		// 只接受绝对、已 Clean 的目录：扫描面本身也不允许被 "../" 带偏。
		if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// matchFPMSocketName 判断文件名是否是 FPM socket 布局，并尽量解析版本。
func matchFPMSocketName(name string) (string, bool) {
	if m := phpFpmSocketRE.FindStringSubmatch(name); m != nil {
		return m[1], true
	}
	for _, generic := range phpGenericSockets {
		if name == generic {
			return "", true
		}
	}
	return "", false
}

// isUnixSocket 判断路径确实是一个 unix socket。
// 只认 socket：普通文件/目录/软链到普通文件一律不算，避免把残留占位文件当"已装 PHP"。
func isUnixSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// detectFPMCandidates 扫描已知目录，返回真实可用的 FPM socket，高版本在前。
func detectFPMCandidates() []fpmCandidate {
	candidates := make([]fpmCandidate, 0, 4)
	seen := make(map[string]bool, 4)
	for _, dir := range phpSocketDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// 目录不存在/无权限是常态（没装 PHP），不是错误：继续看下一个目录。
			continue
		}
		for _, entry := range entries {
			version, ok := matchFPMSocketName(entry.Name())
			if !ok {
				continue
			}
			socket := filepath.Join(dir, entry.Name())
			if seen[socket] || !isUnixSocket(socket) {
				continue
			}
			seen[socket] = true
			candidates = append(candidates, fpmCandidate{Version: version, Socket: socket})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Version != candidates[j].Version {
			return versionGreater(candidates[i].Version, candidates[j].Version)
		}
		return candidates[i].Socket < candidates[j].Socket
	})
	return candidates
}

// versionGreater 按 X.Y 数值比较版本；无法解析的版本排在有版本的之后（最小）。
func versionGreater(a, b string) bool {
	am, aok := splitVersion(a)
	bm, bok := splitVersion(b)
	switch {
	case aok && bok:
		if am[0] != bm[0] {
			return am[0] > bm[0]
		}
		return am[1] > bm[1]
	case aok:
		return true
	case bok:
		return false
	default:
		return a > b
	}
}

// splitVersion 把 "8.3" 拆成 (8,3)；形状不对返回 ok=false。
func splitVersion(raw string) ([2]int, bool) {
	if !phpVersionRE.MatchString(raw) {
		return [2]int{}, false
	}
	parts := strings.SplitN(raw, ".", 2)
	var out [2]int
	for i, part := range parts {
		value := 0
		for _, r := range part {
			value = value*10 + int(r-'0')
			if value > 99 {
				return [2]int{}, false
			}
		}
		out[i] = value
	}
	return out, true
}

// phpVersionFromSocket 从 socket 路径末段解析版本（解析不出返回空串）。
func phpVersionFromSocket(socket string) string {
	if m := phpFpmSocketRE.FindStringSubmatch(filepath.Base(socket)); m != nil {
		return m[1]
	}
	return ""
}

// validatePHPSocketPath 校验 socket 路径：绝对、无冗余分量、字符集安全。
// 与 validateRootPath 同一套逻辑：路径会原样进 nginx 配置，这是注入防线。
func validatePHPSocketPath(socket string) error {
	if socket == "" {
		return errors.New("不能为空")
	}
	if !filepath.IsAbs(socket) {
		return errors.New("必须是绝对路径")
	}
	if socket != filepath.Clean(socket) {
		return errors.New("不能含冗余路径分量")
	}
	if !phpSocketPathRE.MatchString(socket) {
		return errors.New("仅允许字母数字与 ._/@+-，禁止空白、分号、花括号与换行")
	}
	return nil
}

// phpNoFPMMessage 是探测不到 FPM 时的统一报错文案：必须给出「怎么办」，不能只说"失败"。
func phpNoFPMMessage() string {
	return "未检测到可用的 PHP-FPM socket（已扫描 " + strings.Join(phpSocketDirs(), " ") + "）。" +
		"请先安装并启动 PHP-FPM（Debian/Ubuntu: apt install php-fpm；RHEL: dnf install php-fpm），" +
		"再重试；若 nginx 运行在容器内，请在插件配置 php_fpm_socket 里填容器内 socket 路径。" +
		"本插件不会生成一个跑不起来的静态站来假装成功"
}

// describeCandidates 把候选列表压成一行可读文本（版本 + 路径）。
func describeCandidates(candidates []fpmCandidate) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		label := c.Version
		if label == "" {
			label = "无版本号"
		}
		parts = append(parts, label+" → "+c.Socket)
	}
	if len(parts) == 0 {
		return "（无）"
	}
	return strings.Join(parts, "；")
}

// resolvePHPState 把「请求的版本 / 显式 socket」解析成可落盘的 phpState。
//
// 优先级：入参 socket > 配置 php_fpm_socket > 自动探测。
//   - 自动探测必须命中真实 unix socket，否则 E_INVALID（绝不静默降级成静态站）；
//   - 显式声明的 socket 允许宿主看不见（nginx 在容器里属正常），但 Verified=false 如实披露；
//   - 请求了具体版本而探测结果里没有该版本时 E_INVALID，并列出已探测到的版本。
func resolvePHPState(settings pluginSettings, version, socketOverride string) (phpResolution, *pluginapi.Error) {
	version = strings.TrimSpace(version)
	socketOverride = strings.TrimSpace(socketOverride)
	if version != "" && version != phpAutoVersion && !phpVersionRE.MatchString(version) {
		return phpResolution{}, invalidParam("php_version 非法：只允许 X.Y（如 8.3）或 auto，当前 " + version)
	}

	explicit, source := socketOverride, "param"
	if explicit == "" {
		explicit, source = strings.TrimSpace(settings.PHPFpmSocket), "config"
	}
	if explicit != "" {
		if err := validatePHPSocketPath(explicit); err != nil {
			return phpResolution{}, invalidParam("PHP-FPM socket 非法: " + err.Error())
		}
		parsed := phpVersionFromSocket(explicit)
		if version != "" && version != phpAutoVersion && parsed != "" && parsed != version {
			return phpResolution{}, invalidParam(
				"请求 php_version=" + version + "，但 socket " + explicit + " 对应的是 PHP " + parsed +
					"；请改用一致的版本，或修改 php_fpm_socket")
		}
		label := parsed
		if version != "" && version != phpAutoVersion {
			label = version
		}
		return phpResolution{
			State:    phpState{Version: label, Socket: explicit},
			Verified: isUnixSocket(explicit),
			Source:   source,
		}, nil
	}

	candidates := detectFPMCandidates()
	if len(candidates) == 0 {
		return phpResolution{}, invalidParam(phpNoFPMMessage())
	}
	if version == "" || version == phpAutoVersion {
		return phpResolution{
			State:    phpState{Version: candidates[0].Version, Socket: candidates[0].Socket},
			Verified: true,
			Source:   "detected",
		}, nil
	}
	for _, c := range candidates {
		if c.Version == version {
			return phpResolution{
				State:    phpState{Version: c.Version, Socket: c.Socket},
				Verified: true,
				Source:   "detected",
			}, nil
		}
	}
	return phpResolution{}, invalidParam(
		"未找到 PHP " + version + " 的 FPM socket；已检测到: " + describeCandidates(candidates) +
			"。请先安装 php" + version + "-fpm，或把 php_version 改成 auto")
}

// phpParams 是 site.setPHP 入参；php_version 缺省或 auto 时取探测到的最高版本。
type phpParams struct {
	Domain     string `json:"domain"`
	PHPVersion string `json:"php_version,omitempty"`
	Socket     string `json:"socket,omitempty"`
}

// phpSetResult 是 site.setPHP / site.unsetPHP 的结果。
type phpSetResult struct {
	applyResult
	PHPEnabled  bool   `json:"php_enabled"`
	PHPVersion  string `json:"php_version,omitempty"`
	PHPSocket   string `json:"php_socket,omitempty"`
	PHPVerified bool   `json:"php_socket_verified"`
	PHPSource   string `json:"php_socket_source,omitempty"`
	PHPHint     string `json:"php_hint,omitempty"`
}

// phpGetResult 是 site.getPHP 的结果（只读：不跑 nginx、不改文件）。
type phpGetResult struct {
	Domain     string         `json:"domain"`
	VHostPath  string         `json:"vhost_path"`
	Enabled    bool           `json:"enabled"`
	PHPEnabled bool           `json:"php_enabled"`
	PHPVersion string         `json:"php_version,omitempty"`
	PHPSocket  string         `json:"php_socket,omitempty"`
	Available  []fpmCandidate `json:"available"`
	Hint       string         `json:"hint,omitempty"`
}

// phpUnverifiedHint 是显式 socket 在本机不可见时给用户的下一步（不是错误，是如实披露）。
const phpUnverifiedHint = "宿主上当前看不到该 socket（nginx 在容器内时属正常）；" +
	"若站点 502，请核对 nginx 侧该路径是否存在、PHP-FPM 是否已启动"

// setPHPHandler 为站点启用/切换 PHP-FPM；解析不到 FPM 直接 E_INVALID，不落盘。
func setPHPHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[phpParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	resolution, perr := resolvePHPState(svc.settings, p.PHPVersion, p.Socket)
	if perr != nil {
		return nil, perr
	}
	target := resolution.State
	res, st, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		st.PHP = &target
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return buildPHPSetResult(res, st, resolution), nil
}

// unsetPHPHandler 关闭站点 PHP；PHP 伪静态模板仍启用时拒绝（否则站点立刻全站 404）。
func unsetPHPHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	p, perr := decodeParams[siteDomainParams](raw)
	if perr != nil {
		return nil, perr
	}
	if perr := validateDomain(p.Domain); perr != nil {
		return nil, perr
	}
	res, st, perr := svc.applyEdit(p.Domain, func(st *siteState) *pluginapi.Error {
		if st.PHP == nil {
			return nil // 幂等：本来就没开 PHP
		}
		if isPHPRewriteTemplate(st.Rewrite) {
			return &pluginapi.Error{
				Code: pluginapi.E_INVALID,
				Message: "当前伪静态模板 " + st.Rewrite + " 依赖 PHP-FPM，关闭 PHP 会让整站 404；" +
					"请先 site.setRewrite template=none 再关闭 PHP",
			}
		}
		st.PHP = nil
		return nil
	})
	if perr != nil {
		return nil, perr
	}
	return buildPHPSetResult(res, st, phpResolution{}), nil
}

// buildPHPSetResult 把编辑结果与最终站点状态拼成对外结果（php_enabled 取自真实现状）。
func buildPHPSetResult(res applyResult, st siteState, resolution phpResolution) phpSetResult {
	out := phpSetResult{applyResult: res, PHPEnabled: st.PHP != nil}
	if st.PHP == nil {
		return out
	}
	out.PHPVersion = st.PHP.Version
	out.PHPSocket = st.PHP.Socket
	out.PHPVerified = resolution.Verified
	out.PHPSource = resolution.Source
	if !resolution.Verified {
		out.PHPHint = phpUnverifiedHint
	}
	return out
}

// getPHPHandler 返回站点 PHP 现状 + 本机可用候选（供前端做版本下拉）。
func getPHPHandler(_ *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
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
	result := phpGetResult{
		Domain:    p.Domain,
		VHostPath: vhostPath(p.Domain),
		Enabled:   enabled,
		Available: detectFPMCandidates(),
	}
	if st.PHP != nil {
		result.PHPEnabled = true
		result.PHPVersion = st.PHP.Version
		result.PHPSocket = st.PHP.Socket
	}
	if len(result.Available) == 0 && !result.PHPEnabled {
		result.Hint = phpNoFPMMessage()
	}
	return result, nil
}
