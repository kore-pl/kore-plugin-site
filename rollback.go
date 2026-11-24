package main

import (
	"errors"
	"os"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件实现 R6-3 回滚竞态修复：apply 层的回滚从「锁外整文件快照恢复」改为
// 「清单锁内定向撤销」——只撤销本次操作对本域名的影响，不覆盖 nginx -t 窗口内
// 并发写者登记的其他站点。撤销元信息由 createLocked / deleteLocked 在持锁时采集。

// rollbackCreate 在清单锁内定向撤销一次 create 的效果：只移除本域名的影响，
// 不触碰并发写者对其他域名的改动。
//
// 为什么不能用 apply 层的锁外整文件快照回滚：快照在 create 之前（锁外）拍下，
// 而 create 在锁内写清单；nginx -t 期间并发写者可能已经登记了新站点。此时若用
// 旧快照整文件 restore，会把并发写者的条目一起抹掉（R6-3 跨锁竞态）。
// 这里改为在锁内重新读取当前清单、只删/只修本域名条目，天然与并发写者串行。
//
// created=true 表示本次是新建（原清单无此域名、磁盘无此 vhost）；
// created=false 表示覆盖已有站点，backup 是本次写入前的原内容备份（可能为空）。
func (s *vhostStore) rollbackCreate(domain, path, backup string, created bool) error {
	perr := s.withManifestLock(func() *pluginapi.Error {
		var problems []string
		// 1) 恢复 vhost 文件本体。
		switch {
		case created:
			// 新建：删掉本次写出的片段。
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, "删除新建 vhost: "+err.Error())
			}
		case backup != "":
			// 覆盖：把备份原子写回原路径（同目录 rename）。
			if err := os.Rename(backup, path); err != nil {
				problems = append(problems, "恢复 vhost 备份: "+err.Error())
			}
		default:
			// 覆盖但无备份（原 vhost 不在磁盘）：删掉本次写出的片段。
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, "删除 vhost: "+err.Error())
			}
		}

		// 2) 定向更新清单：只处理本域名条目，其他域名原样保留。
		m, mErr := s.loadManifest()
		if mErr != nil {
			problems = append(problems, mErr.Message)
			return internalErr(strings.Join(problems, "; "))
		}
		if created {
			delete(m.Sites, domain) // 原本不存在，回滚后也应不存在
		} else {
			m.Sites[domain] = path // 原本存在，恢复到本次写入前的路径
		}
		if err := atomicWrite(s.manifestPath(), encode(m)); err != nil {
			problems = append(problems, "更新清单: "+err.Error())
		}
		if len(problems) > 0 {
			return internalErr(strings.Join(problems, "; "))
		}
		return nil
	})
	return perrToError(perr)
}

// rollbackDelete 在清单锁内定向撤销一次 delete 的效果：恢复本域名的 vhost 备份，
// 并把本域名重新登记进清单；并发写者新增的其他站点条目原样保留。
//
// backup 是删除前原片段的同目录备份（可能为空——原文件不在磁盘时）；path 是
// deleteWithBackup 返回的 vhost 路径。调用方只在本次确实 deleted=true 时调用。
func (s *vhostStore) rollbackDelete(domain, path, backup string) error {
	perr := s.withManifestLock(func() *pluginapi.Error {
		var problems []string
		// 1) 有备份则原子写回；无备份时删除前的磁盘状态本就没有该片段，无需动作。
		if backup != "" {
			if err := os.Rename(backup, path); err != nil {
				problems = append(problems, "恢复删除备份: "+err.Error())
			}
		}
		// 2) 只把本域名重新登记进当前清单，不动并发写者的条目。
		m, mErr := s.loadManifest()
		if mErr != nil {
			problems = append(problems, mErr.Message)
			return internalErr(strings.Join(problems, "; "))
		}
		m.Sites[domain] = path
		if err := atomicWrite(s.manifestPath(), encode(m)); err != nil {
			problems = append(problems, "更新清单: "+err.Error())
		}
		if len(problems) > 0 {
			return internalErr(strings.Join(problems, "; "))
		}
		return nil
	})
	return perrToError(perr)
}

// perrToError 把临界区错误转成普通 error，供 apply 层合并到回滚错误文案。
func perrToError(perr *pluginapi.Error) error {
	if perr == nil {
		return nil
	}
	return errors.New(perr.Message)
}
