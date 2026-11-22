package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// siteInfo 是站点列表中的单条记录。
type siteInfo struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Enabled   bool   `json:"enabled"`
}

// siteListResult 是 site.list 的结果。
type siteListResult struct {
	Sites   []siteInfo `json:"sites"`
	Count   int        `json:"count"`
	Applied bool       `json:"applied"`
}

// siteCreateResult 描述 site.create 的实际落盘结果。
type siteCreateResult struct {
	Domain        string `json:"domain"`
	VHostPath     string `json:"vhost_path"`
	DefaultServer bool   `json:"default_server"`
	Applied       bool   `json:"applied"`
	BackupPath    string `json:"backup_path,omitempty"`
	Created       bool   `json:"created"`
}

// siteDeleteResult 描述 site.delete 的实际落盘结果。
type siteDeleteResult struct {
	Domain    string `json:"domain"`
	VHostPath string `json:"vhost_path"`
	Applied   bool   `json:"applied"`
	Deleted   bool   `json:"deleted"`
}

// siteParams 是站点方法的入参。DefaultServer 仅用于识别并拒绝抢占默认站点；
// Root 与 Port 为可选覆盖，缺省为 /var/www/<domain> 与 80。
type siteParams struct {
	Domain        string `json:"domain"`
	DefaultServer *bool  `json:"default_server"`
	Root          string `json:"root,omitempty"`
	Port          int    `json:"port,omitempty"`
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

// vhostDir 返回当前插件使用的 conf.d 目录。
func vhostDir() string {
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
