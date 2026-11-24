package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件守着四件事（缺一条就是「声明与代码漂移」）：
//
//  1. info.json 的 root_actions 与代码真正调用的 id 集合**双向相等**且顺序一致；
//  2. exec 动作能过 ADR-0022 的协议校验；file_write / file_delete 动作能过
//     ADR-0052 / ADR-0053 的协议校验——pluginapi v0.3.0 起 SDK 自带 FileWrite /
//     FileDelete 字段，这里直接调 pluginapi.ValidateRootAction，**不再有 skip 分支**；
//  3. nginx 形状没有 Tail ⇒ 运行时追加任何 token 都必须被拒；
//  4. vhost 写/删是 file_write / file_delete（不 exec），声明去掉即 FAIL（反例自证的常驻靶子）。

// declaredManifest 只取本仓关心的字段；root_actions 直接用协议类型，
// 避免自己再抄一份 schema（抄了就会和 pkg/pluginapi 漂移）。
type declaredManifest struct {
	RootActions []pluginapi.RootAction `json:"root_actions"`
}

// nginxConfDDir 是协议类别 nginx.conf.d 的目录。**单一真源在 SDK**
// （pluginapi.FileWritePrefixClasses["nginx.conf.d"].Dir）；这里保留常量只是为了让
// 断言失败信息可读，TestRootActionsDeclared 会逐字比对 SDK 的值，漂移即 FAIL。
const nginxConfDDir = "/etc/nginx/conf.d/"

// loadDeclaredManifest 读仓库根的 info.json 并按协议类型解析。
func loadDeclaredManifest(t *testing.T) declaredManifest {
	t.Helper()
	raw, err := os.ReadFile("info.json")
	if err != nil {
		t.Fatalf("读取 info.json 失败: %v", err)
	}
	var m declaredManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 info.json 失败: %v", err)
	}
	return m
}

// TestRootActionsDeclared 断言每条声明通过对应形状校验，且与代码调用面双向一致。
//
// 这条用例就是「反例自证」的靶子：把 info.json 里的 root_actions 删掉或删一条，
// 它会立刻 FAIL（数量/集合/校验三处任一命中），不需要人工检查。
func TestRootActionsDeclared(t *testing.T) {
	m := loadDeclaredManifest(t)

	// 类别目录的单一真源在 SDK：先确认它和本仓钉的常量一致，再往下断言声明。
	class, ok := pluginapi.FileWritePrefixClasses["nginx.conf.d"]
	if !ok {
		t.Fatalf("SDK 未定义类别 nginx.conf.d：本仓 file_write / file_delete 声明没有合法类别可用")
	}
	if class.Dir != nginxConfDDir {
		t.Fatalf("类别目录漂移：SDK=%q、本仓常量=%q（文件会被写到两个目录之一）", class.Dir, nginxConfDDir)
	}

	if len(m.RootActions) != len(codeRootActionIDs) {
		t.Fatalf("声明 %d 条、代码调用 %d 条，必须相等（声明=%v 代码=%v）",
			len(m.RootActions), len(codeRootActionIDs), actionIDsOf(m.RootActions), codeRootActionIDs)
	}

	seen := make(map[string]bool, len(m.RootActions))
	for index, action := range m.RootActions {
		if seen[action.ID] {
			t.Fatalf("root_actions id=%q 重复声明", action.ID)
		}
		seen[action.ID] = true
		if index >= len(codeRootActionIDs) || action.ID != codeRootActionIDs[index] {
			t.Fatalf("root_actions[%d] id=%q 与 codeRootActionIDs[%d]=%q 不一致（声明顺序即代码顺序）",
				index, action.ID, index, codeRootActionIDs[index])
		}
		if action.FileDelete != nil {
			// file_delete：先钉死声明取值，再过 SDK 的 ADR-0053 协议校验；
			// 两条都必须真跑——SDK 有该字段时不允许再 skip。
			if len(action.Argv) != 0 {
				t.Fatalf("file_delete 动作 %q 不得声明 argv（删除不执行外部命令）", action.ID)
			}
			if !action.WritesHost {
				t.Fatalf("file_delete 动作 %q 必须 writes_host=true（删宿主文件要进变更账本）", action.ID)
			}
			if action.Target != nginxConfDDir {
				t.Fatalf("file_delete 动作 %q 的 target=%q 必须逐字等于类别目录 %q", action.ID, action.Target, nginxConfDDir)
			}
			if action.PurgeOp != nil {
				t.Fatalf("file_delete 动作 %q 不得声明静态 purge_op（回滚目标由内核按备份路径运行时生成）", action.ID)
			}
			if action.FileDelete.PrefixClass != "nginx.conf.d" {
				t.Fatalf("file_delete.prefix_class=%q 期望 nginx.conf.d", action.FileDelete.PrefixClass)
			}
			if err := pluginapi.ValidateRootAction(action); err != nil {
				t.Fatalf("root_actions[%d] id=%q 未过 ADR-0053 协议校验: %v", index, action.ID, err)
			}
			continue
		}
		if action.FileWrite == nil {
			if err := pluginapi.ValidateRootAction(action); err != nil {
				t.Fatalf("root_actions[%d] id=%q 未过 ADR-0022/协议校验: %v", index, action.ID, err)
			}
			continue
		}
		// file_write：先钉死声明取值，再过 SDK 的 ADR-0052 协议校验。
		if len(action.Argv) != 0 {
			t.Fatalf("file_write 动作 %q 不得声明 argv（写文件不执行外部命令）", action.ID)
		}
		if !action.WritesHost {
			t.Fatalf("file_write 动作 %q 必须 writes_host=true（要进 changes.jsonl 变更账本）", action.ID)
		}
		if action.Target != nginxConfDDir {
			t.Fatalf("file_write 动作 %q 的 target=%q 必须逐字等于类别目录 %q", action.ID, action.Target, nginxConfDDir)
		}
		if action.FileWrite.PrefixClass != "nginx.conf.d" {
			t.Fatalf("prefix_class=%q 期望 nginx.conf.d", action.FileWrite.PrefixClass)
		}
		if action.FileWrite.Mode != "0644" {
			t.Fatalf("mode=%q 期望 0644（权限位是安全边界）", action.FileWrite.Mode)
		}
		if action.FileWrite.ContentPolicy != "nginx.vhost.v1" {
			t.Fatalf("content_policy=%q 期望 nginx.vhost.v1", action.FileWrite.ContentPolicy)
		}
		if action.FileWrite.MaxBytes != maxVHostContentBytes {
			t.Fatalf("max_bytes=%d 期望 %d（协议硬顶）", action.FileWrite.MaxBytes, maxVHostContentBytes)
		}
		if err := pluginapi.ValidateRootAction(action); err != nil {
			t.Fatalf("root_actions[%d] id=%q 未过 ADR-0052 协议校验: %v", index, action.ID, err)
		}
	}

	for _, id := range codeRootActionIDs {
		if !seen[id] {
			t.Fatalf("代码会调用 root action %q，但 info.json 没声明它（运行期内核会拒绝）", id)
		}
	}
}

// TestRootActionsShape 断言两类动作各自的安全形状：
// exec 动作只读/幂等、writes_host=false；file_write 动作是唯一的写宿主动作。
func TestRootActionsShape(t *testing.T) {
	m := loadDeclaredManifest(t)
	for _, action := range m.RootActions {
		if action.Irreversible {
			t.Fatalf("root action %q 声明了 irreversible：本仓动作均幂等/可回滚，不该逼用户每轮核销令牌", action.ID)
		}
		if action.FileWrite != nil {
			if !action.WritesHost {
				t.Fatalf("file_write 动作 %q 必须 writes_host=true", action.ID)
			}
			if len(action.Argv) != 0 {
				t.Fatalf("file_write 动作 %q 不得有 argv", action.ID)
			}
			continue
		}
		if action.FileDelete != nil {
			if !action.WritesHost {
				t.Fatalf("file_delete 动作 %q 必须 writes_host=true", action.ID)
			}
			if len(action.Argv) != 0 {
				t.Fatalf("file_delete 动作 %q 不得有 argv（删除不执行外部命令）", action.ID)
			}
			continue
		}
		if action.WritesHost {
			t.Fatalf("exec 动作 %q 声明了 writes_host：nginx 三条命令不直接写宿主文件", action.ID)
		}
		if len(action.Argv) < 2 {
			t.Fatalf("root action %q 的 argv 过短（%v）：至少要有命令名 + 操作旗标", action.ID, action.Argv)
		}
		joined := strings.Join(action.Argv, " ")
		for _, forbidden := range []string{"stop", "quit", "-s stop", "-s quit"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("root action %q 的 argv 含停服语句 %q：停服等于 Denial of Service", action.ID, forbidden)
			}
		}
	}
}

// TestRootActionsRejectAppendedArgs 是三级校验的**运行时**证据：exec 动作
// 声明 argv 合法，但追加任何 token 都必须被拒。
func TestRootActionsRejectAppendedArgs(t *testing.T) {
	m := loadDeclaredManifest(t)
	tails := [][]string{
		{"-s", "stop"},
		{"-s", "quit"},
		{"-c", "/tmp/evil.conf"},
		{"-e", "/tmp/evil.log"},
		{"stop"},
	}
	for _, action := range m.RootActions {
		if len(action.Argv) == 0 {
			continue // 文件语义动作不执行命令，没有 Tail 面
		}
		if err := pluginapi.ValidateRootAppendedArgv(action.Argv, nil); err != nil {
			t.Fatalf("root action %q 声明 argv 本身未过校验: %v", action.ID, err)
		}
		for _, tail := range tails {
			if err := pluginapi.ValidateRootAppendedArgv(action.Argv, tail); err == nil {
				t.Fatalf("root action %q 允许追加参数 %v：ADR-0022 的 nginx 形状没有 Tail，必须拒绝", action.ID, tail)
			}
		}
	}
}

// TestVHostWriteDeclarationPresent 是**反例自证**的常驻形式：删掉 info.json 里的
// vhost-write / file_write 声明，本用例立刻 FAIL。
func TestVHostWriteDeclarationPresent(t *testing.T) {
	m := loadDeclaredManifest(t)
	for _, action := range m.RootActions {
		if action.ID != rootActionVHostWrite {
			continue
		}
		if action.FileWrite == nil {
			t.Fatalf("动作 %q 缺少 file_write 声明：写 vhost 会退回执行面或被内核拒绝", action.ID)
		}
		if action.FileWrite.PrefixClass != "nginx.conf.d" {
			t.Fatalf("file_write.prefix_class=%q 期望 nginx.conf.d", action.FileWrite.PrefixClass)
		}
		return
	}
	t.Fatalf("info.json 未声明 %q：site.create/编辑类方法将没有受控写通道", rootActionVHostWrite)
}

// TestVHostDeleteDeclarationPresent 是删站迁移的反例自证常驻靶子：删掉 info.json 的
// vhost-delete / file_delete 声明，本用例立刻 FAIL——删站会退回 os.Remove，装 root 后
// kore-site 对 conf.d 没有目录写权限，必然 DAC 失败。
func TestVHostDeleteDeclarationPresent(t *testing.T) {
	m := loadDeclaredManifest(t)
	for _, action := range m.RootActions {
		if action.ID != rootActionVHostDelete {
			continue
		}
		if action.FileDelete == nil {
			t.Fatalf("动作 %q 缺少 file_delete 声明：删站会退回 os.Remove 并被 DAC 拒绝", action.ID)
		}
		if action.FileDelete.PrefixClass != "nginx.conf.d" {
			t.Fatalf("file_delete.prefix_class=%q 期望 nginx.conf.d", action.FileDelete.PrefixClass)
		}
		if !action.WritesHost {
			t.Fatalf("动作 %q 必须 writes_host=true（删除要进 changes.jsonl 账本）", action.ID)
		}
		if action.Target != nginxConfDDir {
			t.Fatalf("动作 %q 的 target=%q 期望 %q", action.ID, action.Target, nginxConfDDir)
		}
		if action.PurgeOp != nil {
			t.Fatalf("动作 %q 不得声明静态 purge_op", action.ID)
		}
		if len(action.Argv) != 0 {
			t.Fatalf("动作 %q 不得声明 argv", action.ID)
		}
		return
	}
	t.Fatalf("info.json 未声明 %q：删站/停用没有受控删除通道", rootActionVHostDelete)
}

// actionIDsOf 提取声明里的 id 列表（失败信息用）。
func actionIDsOf(actions []pluginapi.RootAction) []string {
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		ids = append(ids, action.ID)
	}
	return ids
}
