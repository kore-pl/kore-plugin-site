//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

// dirOwnerIsRoot 在非 Unix 平台无法判定属主：known=false，调用方据此**不**把
// 权限失败归因成「需要 root」（Windows 上没有 uid 0 这个概念）。
func dirOwnerIsRoot(_ string) (isRoot bool, known bool) {
	return false, false
}
