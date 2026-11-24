//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"syscall"
)

// dirOwnerIsRoot 报告目录属主是否为 root（uid 0）；known=false 表示无法判定
// （路径不存在、stat 失败、平台不支持）。无法判定时调用方不得猜「是 root」。
//
// 平台专有 API（syscall.Stat_t）收在这个文件里，与 manifestlock_unix.go 同一
// 分片策略：主代码保持跨平台可编译。
func dirOwnerIsRoot(path string) (isRoot bool, known bool) {
	info, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return false, false
	}
	return stat.Uid == 0, true
}
