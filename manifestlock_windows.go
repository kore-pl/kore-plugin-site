//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Go 标准库 syscall 包在 Windows 上不导出 LockFileEx（它在 internal/syscall/windows），
// 因此直接按名称绑定 kernel32 的两个过程——仍是标准库、零新增依赖。
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// lockfileExclusiveLock 是 LOCKFILE_EXCLUSIVE_LOCK（winnt.h）。
// 刻意不带 LOCKFILE_FAIL_IMMEDIATELY：未拿到锁时阻塞等待，与 unix 的 LOCK_EX 语义一致。
const lockfileExclusiveLock = 0x00000002

// lockExclusive 用 LockFileEx 加跨进程排他锁（阻塞等待）。
// 锁随句柄/进程退出由系统释放，不存在崩溃后遗留悬挂锁的问题。
func lockExclusive(f *os.File) error {
	var overlapped syscall.Overlapped
	ok, _, err := procLockFileEx.Call(
		f.Fd(), lockfileExclusiveLock, 0,
		1, 0, // 锁 1 字节（Low=1, High=0），锁范围只需要"存在且唯一"
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ok == 0 {
		return err
	}
	return nil
}

// unlockFile 释放 lockExclusive 加的锁。
func unlockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	ok, _, err := procUnlockFileEx.Call(
		f.Fd(), 0,
		1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ok == 0 {
		return err
	}
	return nil
}
