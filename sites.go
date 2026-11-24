package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// siteInfo 是站点列表中的单条记录：exists/test_ok/reloaded 是真实状态，
// 不是"插件写过了"的乐观值。test_ok 来自一次 nginx -t，reloaded 来自
// nginx -T 里是否出现该域名的 server_name。
type siteInfo struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Enabled   bool   `json:"enabled"`
	// Disabled 表示 vhost 已改名为 .conf.disabled（站点停用但配置保留）。
	Disabled bool `json:"disabled"`
	Exists   bool `json:"exists"`
	TestOK   bool `json:"test_ok"`
	Reloaded bool `json:"reloaded"`
	// TestSkipped 表示本机没有可执行的 nginx（未配置/显式关闭），"没测"不等于"测挂"。
	TestSkipped bool `json:"test_skipped"`
	// ReloadSkipped 表示未执行 nginx -T 探测，因此不知道是否已被加载。
	ReloadSkipped bool `json:"reload_skipped"`
	// TestOutput 是 nginx -t 的真实失败摘要（仅测过且失败时非空）。
	TestOutput string `json:"test_output,omitempty"`
	// OutsideInstall 为 true 表示站点根落在安装目录之外（零侵入边界外），
	// 只读提示可用 site.move 迁入；插件不会自动搬迁。
	OutsideInstall     bool   `json:"outside_install,omitempty"`
	OutsideInstallHint string `json:"outside_install_hint,omitempty"`
}

// nginxStatus 是所有业务结果统一披露的命令层模式：
// mode ∈ host/docker/off，reason 只在跳过（off 或显式关闭）时说明原因，
// 让前端能把「未检测」渲染成「为什么没检测」而不是一个灰点。
type nginxStatus struct {
	NginxMode   string `json:"nginx_mode"`
	NginxReason string `json:"nginx_reason,omitempty"`
}

// siteListResult 是 site.list 的结果。
type siteListResult struct {
	nginxStatus
	Sites   []siteInfo `json:"sites"`
	Count   int        `json:"count"`
	Applied bool       `json:"applied"`
}

// siteCreateResult 描述 site.create 的实际落盘与生效结果。
// Applied 表示写入后校验/重载均未失败（任一环节被显式关闭时由对应 skipped 标记说明）。
type siteCreateResult struct {
	nginxStatus
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	DefaultServer bool   `json:"default_server"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	Applied       bool   `json:"applied"`
	BackupPath    string `json:"backup_path,omitempty"`
	Created       bool   `json:"created"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
	// PHPEnabled 表示本次建站带 PHP-FPM 块；PHPVersion/PHPSocket 为实际落盘值。
	PHPEnabled bool   `json:"php_enabled"`
	PHPVersion string `json:"php_version,omitempty"`
	PHPSocket  string `json:"php_socket,omitempty"`
}

// siteDeleteResult 描述 site.delete 的实际删除与生效结果。
type siteDeleteResult struct {
	nginxStatus
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	Applied       bool   `json:"applied"`
	Deleted       bool   `json:"deleted"`
	BackupPath    string `json:"backup_path,omitempty"`
	TestPassed    bool   `json:"test_passed"`
	Reloaded      bool   `json:"reloaded"`
	TestSkipped   bool   `json:"test_skipped,omitempty"`
	ReloadSkipped bool   `json:"reload_skipped,omitempty"`
}

// siteStatusResult 是 site.status 的结果：conf/测试/目录/加载四个独立信号，
// 让调用方一眼看出"是文件没写、配置不合法、目录缺失，还是没被 nginx 加载"。
type siteStatusResult struct {
	nginxStatus
	Domain      string `json:"domain"`
	VHostPath   string `json:"vhost_path"`
	Managed     bool   `json:"managed"`
	ConfExists  bool   `json:"conf_exists"`
	Disabled    bool   `json:"disabled"`
	TestPassed  bool   `json:"test_passed"`
	TestSkipped bool   `json:"test_skipped"`
	TestOutput  string `json:"test_output,omitempty"`
	// SiteDirExists 是兼容字段：true 当且仅当 SiteDirState == "present"。
	// 容器模式下宿主的 os.Stat 会把「容器内存在」误判成 false，需要区分「未知」时
	// 读 SiteDirState（§3.5 #6）。
	SiteDirExists bool `json:"site_dir_exists"`
	// SiteDirState 是三态：present / absent / unknown。unknown 表示查不到
	// （docker 不可用、容器异常、root 无法解析），**不等于**目录不存在。
	SiteDirState string `json:"site_dir_state"`
	// SiteDirReason 只在 unknown 时给出「为什么查不到 + 怎么办」，present/absent 为空。
	SiteDirReason string `json:"site_dir_reason,omitempty"`
	Loaded        bool   `json:"loaded"`
	LoadChecked   bool   `json:"load_checked"`
	// ReloadSkipped 表示未执行 nginx -T 探测（未配置 nginx 或显式关闭）。
	ReloadSkipped bool `json:"reload_skipped"`
	// OutsideInstall / OutsideInstallHint：站点根在安装目录外时的只读提示（不自动搬迁）。
	OutsideInstall     bool   `json:"outside_install,omitempty"`
	OutsideInstallHint string `json:"outside_install_hint,omitempty"`
	// PHPEnabled / PHPVersion 来自 vhost 里的 kore-site:php 块（真的配了 FPM 才是 true）。
	PHPEnabled bool   `json:"php_enabled"`
	PHPVersion string `json:"php_version,omitempty"`
}

// siteParams 是站点方法的入参。DefaultServer 仅用于识别并拒绝抢占默认站点；
// Root 与 Port 为可选覆盖，缺省为 /var/www/<domain> 与 80。
// PHPVersion 省略 = 纯静态站；"auto" = 用探测到的最高版本；"8.3" = 必须命中该版本。
type siteParams struct {
	Domain        string `json:"domain"`
	DefaultServer *bool  `json:"default_server"`
	Root          string `json:"root,omitempty"`
	Port          int    `json:"port,omitempty"`
	PHPVersion    string `json:"php_version,omitempty"`
}

// siteManifest 是本插件生成的 vhost 清单，用于 delete 时只删自己生成的文件。
// 清单文件本身与 vhost 位于同一 conf.d 目录，文件名为 manifestName。
type siteManifest struct {
	Version int               `json:"version"`
	Sites   map[string]string `json:"sites"`
}

// newSiteManifest 构造空清单。
func newSiteManifest() siteManifest {
	return siteManifest{Version: 1, Sites: make(map[string]string)}
}

// encode 把结果结构体序列化为载荷；失败只可能来自结构体定义错误，回退空对象。
func encode[T any](v T) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

const (
	// vhostPrefix 是所有生成片段的统一前缀，delete 路径前缀约束的第二道闸。
	vhostPrefix = "kore-site-"
	// manifestName 是清单文件名，固定前缀使其不会被误认为 vhost。
	manifestName = vhostPrefix + "manifest.json"
	// defaultVHostDir 是未配置时的宿主 nginx conf.d 路径。
	defaultVHostDir = "/etc/nginx/conf.d"
	// vhostDirEnv 是 conf.d 目录的环境变量覆盖项，供测试与自定义布局使用。
	vhostDirEnv = "KORE_SITE_CONF_DIR"
	// defaultRoot 是未指定 root 时的站点文档根模板（%s 替换域名）。
	defaultRoot = "/var/www/%s"
	// defaultPort 是未指定端口时的监听端口。
	defaultPort = 80
)

// vhostDir 返回当前插件使用的 conf.d 目录（config.json > 环境变量 > 默认值）。
// 配置解析失败时回落到历史环境变量：调用方在 newSiteService 已先报出配置错误。
func vhostDir() string {
	if settings, _, err := loadPluginSettings(); err == nil {
		return settings.HostConfDir
	}
	if dir := os.Getenv(vhostDirEnv); dir != "" {
		return dir
	}
	return defaultVHostDir
}

// vhostFileName 由已校验域名生成独立 conf.d vhost 文件名，不触碰宿主主配置。
// 域名校验保证只含小写字母数字、连字符与点，无路径分隔符与 ".."。
func vhostFileName(domain string) string {
	return vhostPrefix + domain + ".conf"
}

// vhostPath 返回域名对应的 vhost 绝对路径。
func vhostPath(domain string) string {
	return filepath.Join(vhostDir(), vhostFileName(domain))
}

// vhostTemplate 生成单域名 server 块。刻意不含 default_server、不写主配置、
// 不插入 include（include 由宿主主配置既有 conf.d/*.conf 提供）。
func vhostTemplate(domain, root string, port int) []byte {
	return []byte("# kore-plugin-site managed vhost for " + domain + "\n" +
		"# 本片段由 site 插件生成；禁止抢占默认站点（红队 R6）。\n" +
		"server {\n" +
		"    listen " + itoa(port) + ";\n" +
		"    server_name " + domain + ";\n" +
		"    root " + root + ";\n" +
		"    index index.html;\n" +
		"    location / {\n" +
		"        try_files $uri $uri/ =404;\n" +
		"    }\n" +
		"}\n")
}

// parseRootDirective 从已落盘 vhost 中读 root 指令，供 site.status 判断站点目录。
// 只认行首 root 指令，注释与 location 内的同名文本不会误命中。
func parseRootDirective(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "root ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "root "))
		return strings.TrimSuffix(value, ";")
	}
	return ""
}

// itoa 把 1..65535 的端口转成十进制字符串（标准库 strconv 的薄封装在此避免
// 在模板处引入额外 import；保留独立函数便于测试）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// backupName 生成覆盖前的带时间戳备份文件名。
func backupName(domain string, now time.Time) string {
	return vhostFileName(domain) + ".bak." + now.UTC().Format("20060102T150405.000000000Z")
}
