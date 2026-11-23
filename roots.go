package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ADR-0023 阶段 C：内核启动插件时注入 KORE_PLUGIN_DATA=<install>/data/plugin-data/<id>
// 与 KORE_INSTALL_DIR。站点默认根优先落到 $KORE_PLUGIN_DATA/sites/<domain>；
// 未注入时回退历史默认 /var/www/<domain>，并打一条 WARN（不静默降级）。
// 显式配置（site.create 的 root、clone 的 root）永远优先，本文件只决定"未指定时的默认值"。
// 既有站点不做任何迁移：只读标记 outside_install + hint，搬迁走另外的方法。

const pluginDataEnv = "KORE_PLUGIN_DATA"
const installDirEnv = "KORE_INSTALL_DIR"

var legacyRootWarn sync.Once

// defaultSiteRoot 返回未显式指定 root 时的默认站点根。
// 第二个返回值表示是否落在插件数据根内（true=零侵入模式）。
func defaultSiteRoot(domain string) (string, bool) {
	if data := os.Getenv(pluginDataEnv); data != "" {
		return filepath.Join(data, "sites", domain), true
	}
	legacyRootWarn.Do(func() {
		fmt.Fprintf(os.Stderr, "[WARN] 未注入 %s：站点根回退到历史默认 %s（非零侵入模式，ADR-0023 阶段 C）；"+
			"安装器注入该变量后新建站点才会落到安装目录内，既有站点不迁移。\n",
			pluginDataEnv, fmt.Sprintf(defaultRoot, "<domain>"))
	})
	return fmt.Sprintf(defaultRoot, domain), false
}

// mustDefaultSiteRoot 是 parseSiteState 用的单返回值包装（解析期不能返回错误）。
func mustDefaultSiteRoot(domain string) string {
	root, _ := defaultSiteRoot(domain)
	return root
}

// siteRootOutsideInstall 判断站点根是否在安装目录之外。
// 未注入 KORE_INSTALL_DIR 时无法判定，返回 false（不误报），由调用方在 hint 里保持沉默。
func siteRootOutsideInstall(root string) bool {
	install := os.Getenv(installDirEnv)
	if install == "" || root == "" {
		return false
	}
	return !withinDir(install, root)
}

// outsideInstallHint 是只读提示：只说明"可迁移"，不承诺自动搬迁。
func outsideInstallHint() string {
	return "站点根在安装目录之外（零侵入边界外）：可用 site.move 迁入安装目录；当前版本不自动搬迁。"
}
