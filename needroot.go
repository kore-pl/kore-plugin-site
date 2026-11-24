package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是「需要宿主 root」这一失败模式的唯一出口：把「权限不足」翻译成
// `E_NEED_ROOT` + 可操作 hint，而不是一句 permission denied 让用户自己猜。
//
// 为什么必须有这一层：插件降权到 kore-site 后，写 /etc/nginx/conf.d 和执行
// nginx -t / -s reload 都会失败；失败信息是内核/OS 的原文，用户看到的是
// "permission denied" 却不知道下一步该做什么（产品易用性四原则 ④「小白能看懂」）。

const (
	// hintNginxCommand 面向「nginx 命令因缺 root 失败」：可执行动作是让 kore 以 root 启动。
	hintNginxCommand = "nginx 命令需要 root：请以 root 启动 Kore（sudo systemctl restart kore），" +
		"插件会经内核提权通道执行已声明的 root 动作（nginx-test / nginx-reload / nginx-dump），" +
		"插件进程自身不需要 root。若 kore 已是 root 仍报此错，检查插件是否已获 system:admin 授权。"
	// hintConfDirWrite 面向「降级/旧版本才可能出现的目录不可写」：ADR-0052 已否决
	// 「给插件用户 conf.d 目录写权限」（access_log 注入 = root RCE），写 vhost 的唯一
	// 合法通道是内核受控写 file_write。因此这里**不再提 chgrp/chmod/setfacl**。
	hintConfDirWrite = "写 vhost 走内核受控写通道（ADR-0052），不需要给插件用户 conf.d 目录写权限：" +
		"请以 root 启动 Kore（sudo systemctl restart kore）后重试；插件进程自身不持有 root，" +
		"也不应把 conf.d 目录的写权限交给插件用户（那条路已被 ADR-0052 否决）。"
	// hintVHostDeleteChannel 面向「受控删除不可用 / 内核非 root / 声明缺失」。
	hintVHostDeleteChannel = "删除 vhost 走内核受控删除通道（ADR-0053）：请以 root 启动 Kore" +
		"（sudo systemctl restart kore）后重试；插件自身不需要 root，也不需要给 conf.d 目录授权。" +
		"若内核已是 root 仍失败，请确认插件版本包含 file_delete 声明（旧内核收到该动作会 E_INVALID）。"
	// hintVHostChannel 面向「通道不可用 / 内核非 root / 声明缺失」：同一个写路径的降级口径。
	hintVHostChannel = "写 vhost 走内核受控写通道：请以 root 启动 Kore（sudo systemctl restart kore）后重试；" +
		"插件自身不需要 root，也不需要给 conf.d 目录授权。若内核已是 root 仍失败，" +
		"请确认插件版本包含 file_write 声明（旧内核收到 content 会 E_INVALID）。"
)

// needRootErr 构造带可操作提示的 E_NEED_ROOT（detail.hint 由 UI 直接展示）。
func needRootErr(message, hint string) *pluginapi.Error {
	protocolErr := &pluginapi.Error{Code: pluginapi.E_NEED_ROOT, Message: message}
	raw, err := json.Marshal(map[string]string{"hint": hint})
	if err != nil {
		// 序列化 hint 失败不该把错误码也搞丢：返回不带 detail 的同码错误。
		return protocolErr
	}
	protocolErr.Detail = raw
	return protocolErr
}

// permissionDeniedMarkers 是「权限被拒」的判据集合。
//
// 覆盖三类真实输出：OS 层（permission denied / operation not permitted）、
// nginx 自己给 master 发信号失败（`kill(…, 1) failed (13: Permission denied)`）、
// 以及 docker exec 在容器外被拒的 `permission denied while trying to connect`。
var permissionDeniedMarkers = []string{
	"permission denied",
	"operation not permitted",
	"failed (13",
}

// looksLikePermissionDenied 报告命令输出/错误里是否出现权限被拒的判据。
func looksLikePermissionDenied(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range permissionDeniedMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// needRootCommandErr 把「命令因缺 root 权限失败」升级成 E_NEED_ROOT + hint。
//
// 返回 nil 表示这条命令的失败原因与 root 无关（语法错误、端口冲突…），
// 由调用点保持原有的 E_INVALID / E_UPSTREAM 语义——**不把所有失败都归因到权限**，
// 那会掩盖真正的配置错误。
func needRootCommandErr(out commandOutcome, what string) *pluginapi.Error {
	if !out.needRoot() {
		return nil
	}
	detail := describeCommand(out)
	if detail == "" {
		detail = "权限被拒，无更多输出"
	}
	return needRootErr(what+"失败："+detail, hintNginxCommand)
}

// vhostWriteErr 把 vhost 写入失败翻译成协议错误：
//   - 内核通道回 E_NEED_ROOT → 同码 + 受控写口径 hint；
//   - 通道回其它协议错误 → 保留 code/message/hint（不吞、不改写成成功）；
//   - 本地回落写盘遇到「root 属主 + 非 root」→ E_NEED_ROOT（旧路径的边界用例）。
func vhostWriteErr(err error) *pluginapi.Error {
	var failure *rootActionFailure
	if errors.As(err, &failure) {
		if failure.Code == pluginapi.E_NEED_ROOT {
			return needRootErr(failure.Message, hintVHostChannel)
		}
		detail, mErr := json.Marshal(map[string]string{"hint": failure.Hint})
		protocolErr := &pluginapi.Error{Code: failure.Code, Message: failure.Message}
		if mErr == nil {
			protocolErr.Detail = detail
		}
		return protocolErr
	}
	if perr := needRootForConfDir(vhostDir(), err); perr != nil {
		return perr
	}
	return internalErr("写入 vhost 失败: " + err.Error())
}

// vhostDeleteErr 把受控删除失败翻译成协议错误：
//   - 内核通道回 E_NEED_ROOT → 同码 + 受控删除口径 hint；
//   - 通道回其它协议错误 → 保留 code/message/hint（不吞、不改写成成功）；
//   - 本地回落 os.Remove 遇「root 属主 + 非 root」→ E_NEED_ROOT（旧路径的边界用例）。
func vhostDeleteErr(err error) *pluginapi.Error {
	var failure *rootActionFailure
	if errors.As(err, &failure) {
		if failure.Code == pluginapi.E_NEED_ROOT {
			return needRootErr(failure.Message, hintVHostDeleteChannel)
		}
		detail, mErr := json.Marshal(map[string]string{"hint": failure.Hint})
		protocolErr := &pluginapi.Error{Code: failure.Code, Message: failure.Message}
		if mErr == nil {
			protocolErr.Detail = detail
		}
		return protocolErr
	}
	if perr := needRootForConfDir(vhostDir(), err); perr != nil {
		return perr
	}
	return internalErr("删除 vhost 失败: " + err.Error())
}

// needRootForConfDir 在「conf.d 不可写 **且** 属主是 root **且** 当前进程非 root」时
// 返回 E_NEED_ROOT；其余情形返回 nil，让调用方保持原有的 E_INTERNAL 语义。
//
// 为什么要卡「属主是 root」而不是一律 E_NEED_ROOT：目录属主不是 root 时，root 也
// 帮不上忙（那是 DAC 位/挂载只读的问题），报「需要 root」会把人带偏。
// 边界用例（自己建的临时目录 chmod 0000）因此仍走 E_INTERNAL，不被本规则改写。
func needRootForConfDir(dir string, cause error) *pluginapi.Error {
	if cause == nil || !errors.Is(cause, fs.ErrPermission) {
		return nil
	}
	return needRootForConfDirDecision(dir, cause, os.Geteuid())
}

// needRootForConfDirDecision 是 needRootForConfDir 的纯判定内核（euid 与属主可注入，
// 便于非 root 环境下测试「属主是 root」这条分支——本机无法 chown 到 root）。
func needRootForConfDirDecision(dir string, cause error, euid int) *pluginapi.Error {
	if cause == nil || !errors.Is(cause, fs.ErrPermission) {
		return nil
	}
	// root 自己不会被 DAC 拦：此时 EACCES 另有原因（只读挂载、SELinux），不归因权限。
	if euid == 0 {
		return nil
	}
	if isRoot, known := dirOwnerIsRoot(dir); !known || !isRoot {
		return nil
	}
	return needRootErr(
		"conf.d 目录 "+dir+" 属于 root 且当前用户无权写入（"+cause.Error()+"）",
		hintConfDirWrite,
	)
}
