package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件守着四件事（缺一条就是「声明与代码漂移」）：
//
//  1. info.json 的 root_actions 与代码真正调用的 id 集合**双向相等**且顺序一致；
//  2. exec 动作能过 ADR-0022 的协议校验；file_write 动作能过 ADR-0052 的声明形状
//     （与 argv 互斥、writes_host、target 等于类别目录）——SDK 尚未含 file_write
//     时按协议文档**显式 skip**，不偷偷放宽成本地校验；
//  3. nginx 形状没有 Tail ⇒ 运行时追加任何 token 都必须被拒；
//  4. vhost 写是 file_write（不 exec），声明去掉即 FAIL（反例自证的常驻靶子）。

// declaredManifest 只取本仓关心的字段；root_actions 直接用协议类型，
// 避免自己再抄一份 schema（抄了就会和 pkg/pluginapi 漂移）。
type declaredManifest struct {
	RootActions []pluginapi.RootAction `json:"root_actions"`
}

// declaredFileWrite 是 file_write 声明的本地定形。
//
// 为什么需要第二份：本仓钉的 pluginapi v0.1.3 的 RootAction / FileWrite 还没有
// file_write 字段（K2 晚于该版本发布）。要在**不引入 any** 的前提下断言声明取值，
// 只能按 info.json 的实际形状本地定形；SDK 升版后由 sdkHasFileWrite 自动接管。
type declaredFileWrite struct {
	PrefixClass   string `json:"prefix_class"`
	Mode          string `json:"mode"`
	ContentPolicy string `json:"content_policy"`
	MaxBytes      int    `json:"max_bytes"`
}

// declaredRootAction 是「协议之外的声明字段」的本地定形。
type declaredFileDelete struct {
	PrefixClass string `json:"prefix_class"`
}

type declaredRootAction struct {
	ID           string              `json:"id"`
	Argv         []string            `json:"argv"`
	WritesHost   bool                `json:"writes_host"`
	Target       string              `json:"target"`
	Irreversible bool                `json:"irreversible"`
	FileWrite    *declaredFileWrite  `json:"file_write"`
	FileDelete   *declaredFileDelete `json:"file_delete"`
	// PurgeOp 只用来断言「声明里不得出现静态 purge_op」：删除的回滚目标只能由内核
	// 按本次备份路径运行时生成，静态写死会让账本回滚错对象。
	PurgeOp json.RawMessage `json:"purge_op"`
}

// nginxConfDDir 是协议类别 nginx.conf.d 的目录（pluginapi.FileWritePrefixClasses 的
// 唯一真源在 SDK；本仓 SDK v0.1.3 未含该类，故按协议文档钉值并在此注明来源）。
const nginxConfDDir = "/etc/nginx/conf.d/"

// loadDeclaredManifest 读仓库根的 info.json，同时返回协议视图与本地定形视图。
func loadDeclaredManifest(t *testing.T) (declaredManifest, []declaredRootAction) {
	t.Helper()
	raw, err := os.ReadFile("info.json")
	if err != nil {
		t.Fatalf("读取 info.json 失败: %v", err)
	}
	var m declaredManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 info.json 失败: %v", err)
	}
	var local struct {
		RootActions []declaredRootAction `json:"root_actions"`
	}
	if err := json.Unmarshal(raw, &local); err != nil {
		t.Fatalf("按本地定形解析 info.json 失败: %v", err)
	}
	return m, local.RootActions
}

// sdkHasFileWrite 报告当前 SDK 的 RootAction 是否含 FileWrite 字段。
//
// 协议 §3.1.3.2 要求插件走 pluginapi.ValidateRootAction；SDK 尚未包含该字段时
// 显式 skip 并写明所需版本，**不得**改写成一份本地宽松校验冒充通过。
func sdkHasFileWrite() bool {
	_, ok := reflect.TypeOf(pluginapi.RootAction{}).FieldByName("FileWrite")
	return ok
}

// sdkHasFileDelete 报告当前 SDK 的 RootAction 是否含 FileDelete 字段。
//
// 与 sdkHasFileWrite 同口径：SDK 尚无该字段时显式 skip 并写明所需版本（pluginapi
// v0.3.0），**不得**把协议校验改写成一份本地宽松校验冒充通过。
func sdkHasFileDelete() bool {
	_, ok := reflect.TypeOf(pluginapi.RootAction{}).FieldByName("FileDelete")
	return ok
}

// TestRootActionsDeclared 断言每条声明通过对应形状校验，且与代码调用面双向一致。
//
// 这条用例就是「反例自证」的靶子：把 info.json 里的 root_actions 删掉或删一条，
// 它会立刻 FAIL（数量/集合/校验三处任一命中），不需要人工检查。
func TestRootActionsDeclared(t *testing.T) {
	m, local := loadDeclaredManifest(t)
	if len(m.RootActions) != len(codeRootActionIDs) {
		t.Fatalf("声明 %d 条、代码调用 %d 条，必须相等（声明=%v 代码=%v）",
			len(m.RootActions), len(codeRootActionIDs), actionIDsOf(m.RootActions), codeRootActionIDs)
	}
	if len(local) != len(m.RootActions) {
		t.Fatalf("两份解析的条目数不一致: %d vs %d", len(local), len(m.RootActions))
	}

	seen := make(map[string]bool, len(m.RootActions))
	for index, action := range m.RootActions {
		declared := local[index]
		if seen[action.ID] {
			t.Fatalf("root_actions id=%q 重复声明", action.ID)
		}
		seen[action.ID] = true
		if index >= len(codeRootActionIDs) || action.ID != codeRootActionIDs[index] {
			t.Fatalf("root_actions[%d] id=%q 与 codeRootActionIDs[%d]=%q 不一致（声明顺序即代码顺序）",
				index, action.ID, index, codeRootActionIDs[index])
		}
		if declared.FileDelete != nil {
			// file_delete 的本地形状断言（SDK 未含该字段时的唯一可执行校验）。
			if len(declared.Argv) != 0 {
				t.Fatalf("file_delete 动作 %q 不得声明 argv（删除不执行外部命令）", action.ID)
			}
			if !declared.WritesHost {
				t.Fatalf("file_delete 动作 %q 必须 writes_host=true（删宿主文件要进变更账本）", action.ID)
			}
			if declared.Target != nginxConfDDir {
				t.Fatalf("file_delete 动作 %q 的 target=%q 必须逐字等于类别目录 %q", action.ID, declared.Target, nginxConfDDir)
			}
			if len(declared.PurgeOp) != 0 {
				t.Fatalf("file_delete 动作 %q 不得声明静态 purge_op（回滚目标由内核按备份路径运行时生成）", action.ID)
			}
			if declared.FileDelete.PrefixClass != "nginx.conf.d" {
				t.Fatalf("file_delete.prefix_class=%q 期望 nginx.conf.d", declared.FileDelete.PrefixClass)
			}
			if !sdkHasFileDelete() {
				t.Logf("SKIP ValidateRootAction(%q)：需要 pluginapi ≥ 含 file_delete 的版本（当前 SDK 无 RootAction.FileDelete）", action.ID)
				continue
			}
			if err := pluginapi.ValidateRootAction(action); err != nil {
				t.Fatalf("root_actions[%d] id=%q 未过 ADR-0053 协议校验: %v", index, action.ID, err)
			}
			continue
		}
		if declared.FileWrite == nil {
			if err := pluginapi.ValidateRootAction(action); err != nil {
				t.Fatalf("root_actions[%d] id=%q 未过 ADR-0022/协议校验: %v", index, action.ID, err)
			}
			continue
		}
		// file_write 的本地形状断言（SDK 未含该字段时的唯一可执行校验）。
		if len(declared.Argv) != 0 {
			t.Fatalf("file_write 动作 %q 不得声明 argv（写文件不执行外部命令）", action.ID)
		}
		if !declared.WritesHost {
			t.Fatalf("file_write 动作 %q 必须 writes_host=true（要进 changes.jsonl 变更账本）", action.ID)
		}
		if declared.Target != nginxConfDDir {
			t.Fatalf("file_write 动作 %q 的 target=%q 必须逐字等于类别目录 %q", action.ID, declared.Target, nginxConfDDir)
		}
		if declared.FileWrite.PrefixClass != "nginx.conf.d" {
			t.Fatalf("prefix_class=%q 期望 nginx.conf.d", declared.FileWrite.PrefixClass)
		}
		if declared.FileWrite.Mode != "0644" {
			t.Fatalf("mode=%q 期望 0644（权限位是安全边界）", declared.FileWrite.Mode)
		}
		if declared.FileWrite.ContentPolicy != "nginx.vhost.v1" {
			t.Fatalf("content_policy=%q 期望 nginx.vhost.v1", declared.FileWrite.ContentPolicy)
		}
		if declared.FileWrite.MaxBytes != maxVHostContentBytes {
			t.Fatalf("max_bytes=%d 期望 %d（协议硬顶）", declared.FileWrite.MaxBytes, maxVHostContentBytes)
		}
		if !sdkHasFileWrite() {
			t.Logf("SKIP ValidateRootAction(%q)：需要 pluginapi ≥ 含 file_write 的版本（当前 SDK 无 RootAction.FileWrite）", action.ID)
			continue
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
	m, local := loadDeclaredManifest(t)
	for index, action := range m.RootActions {
		declared := local[index]
		if declared.Irreversible {
			t.Fatalf("root action %q 声明了 irreversible：本仓动作均幂等/可回滚，不该逼用户每轮核销令牌", action.ID)
		}
		if declared.FileWrite != nil {
			if !declared.WritesHost {
				t.Fatalf("file_write 动作 %q 必须 writes_host=true", action.ID)
			}
			if len(action.Argv) != 0 {
				t.Fatalf("file_write 动作 %q 不得有 argv", action.ID)
			}
			continue
		}
		if declared.FileDelete != nil {
			if !declared.WritesHost {
				t.Fatalf("file_delete 动作 %q 必须 writes_host=true", action.ID)
			}
			if len(action.Argv) != 0 {
				t.Fatalf("file_delete 动作 %q 不得有 argv（删除不执行外部命令）", action.ID)
			}
			continue
		}
		if declared.WritesHost {
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
	m, _ := loadDeclaredManifest(t)
	tails := [][]string{
		{"-s", "stop"},
		{"-s", "quit"},
		{"-c", "/tmp/evil.conf"},
		{"-e", "/tmp/evil.log"},
		{"stop"},
	}
	for _, action := range m.RootActions {
		if len(action.Argv) == 0 {
			continue // file_write 不执行命令，没有 Tail 面
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
	_, local := loadDeclaredManifest(t)
	for _, action := range local {
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
	_, local := loadDeclaredManifest(t)
	for _, action := range local {
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
		if len(action.PurgeOp) != 0 {
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
