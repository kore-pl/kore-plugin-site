//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package main

import (
	"errors"
	"os"
)

// errLockUnsupported 让不支持跨进程文件锁的平台**显式失败**，而不是静默退化成
// 进程内互斥——后者在多实例共享 conf.d 时依旧会丢更新，且失败是无声的。
var errLockUnsupported = errors.New("当前平台不支持站点清单跨进程文件锁，拒绝在无互斥保护下写清单")

// lockExclusive 在不支持的平台直接报错（kore 目标平台为 linux / darwin / windows）。
func lockExclusive(*os.File) error { return errLockUnsupported }

// unlockFile 在无锁可释放时是空操作。
func unlockFile(*os.File) error { return nil }
