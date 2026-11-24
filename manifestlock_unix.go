//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive 对 f 加**跨进程**排他锁；已被占用时阻塞等待。
//
// 用 flock 而不是锁目录 / 锁文件存在性判断，是因为 flock 的锁**随进程消亡由内核释放**：
// 插件被 SIGKILL（故障注入用例会真杀进程）也不会留下悬挂锁把后续写操作永久堵死。
// 临界区只有「读清单 → 改内存 → 原子写回」，不含 nginx -t / reload，阻塞时间以毫秒计。
func lockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return nil
		}
		// EINTR：信号打断等待，重试而不是把可恢复的中断当失败。
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}

// unlockFile 释放 lockExclusive 加的锁。
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
