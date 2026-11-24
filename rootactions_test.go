package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件守着三件事（缺一条就是「声明与代码漂移」）：
//
//  1. info.json 的 root_actions 每条都能过 ADR-0022 的三级校验（否则内核启用时就会拒）；
//  2. 声明集合与代码真正调用的 id 集合**双向相等**（少声明=运行期失败，多声明=多余攻击面）；
//  3. nginx 形状没有 Tail ⇒ 运行时追加任何 token 都必须被拒（提权通道不能变成任意 nginx 调用）。

// declaredManifest 只取本仓关心的字段；root_actions 直接用协议类型，
// 避免自己再抄一份 schema（抄了就会和 pkg/pluginapi 漂移）。
type declaredManifest struct {
	RootActions []pluginapi.RootAction `json:"root_actions"`
}

// declaredRootAction 是本仓对「协议之外的声明字段」的本地定形。
//
// 为什么需要第二份：本仓依赖的 pluginapi v0.1.3 的 RootAction 还没有
// writes_host / target 字段（协议增补晚于该版本）。要在**不引入 any** 的前提下
// 断言这两个字段的取值，只能按 info.json 的实际形状本地定形。
type declaredRootAction struct {
	ID           string `json:"id"`
	WritesHost   bool   `json:"writes_host"`
	Target       string `json:"target"`
	Irreversible bool   `json:"irreversible"`
}

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

// TestRootActionsDeclared 断言每条声明通过协议校验，且与代码调用面双向一致。
//
// 这条用例就是「反例自证」的靶子：把 info.json 里的 root_actions 删掉或删一条，
// 它会立刻 FAIL（数量/集合/校验三处任一命中），不需要人工检查。
func TestRootActionsDeclared(t *testing.T) {
	m, _ := loadDeclaredManifest(t)
	if len(m.RootActions) == 0 {
		t.Fatalf("info.json 未声明任何 root_actions：插件将无法写 /etc/nginx 或 reload（E_NEED_ROOT 且有通道也调不到）")
	}
	if len(m.RootActions) != len(codeRootActionIDs) {
		t.Fatalf("声明 %d 条、代码调用 %d 条，必须相等（声明=%v 代码=%v）",
			len(m.RootActions), len(codeRootActionIDs), actionIDsOf(m.RootActions), codeRootActionIDs)
	}

	seen := make(map[string]bool, len(m.RootActions))
	for index, action := range m.RootActions {
		if err := pluginapi.ValidateRootAction(action); err != nil {
			t.Fatalf("root_actions[%d] id=%q 未过 ADR-0022/协议校验: %v", index, action.ID, err)
		}
		if seen[action.ID] {
			t.Fatalf("root_actions id=%q 重复声明", action.ID)
		}
		seen[action.ID] = true
		if index >= len(codeRootActionIDs) || action.ID != codeRootActionIDs[index] {
			t.Fatalf("root_actions[%d] id=%q 与 codeRootActionIDs[%d]=%q 不一致（声明顺序即代码顺序）",
				index, action.ID, index, codeRootActionIDs[index])
		}
	}

	for _, id := range codeRootActionIDs {
		if !seen[id] {
			t.Fatalf("代码会调用 root action %q，但 info.json 没声明它（运行期内核会拒绝）", id)
		}
	}
}

// TestRootActionsReadOnlyShape 断言三条动作都是**只读或可逆**、且固定 argv 不含自由参数。
//
// 具体三点：① 不声明 writes_host（写宿主文件走不了 argv 通道，见 CHANGELOG 的边界说明）；
// ② 不声明 irreversible（reload 是幂等动作，不该逼用户每轮核销一次性令牌）；
// ③ argv 长度固定，且不含 `-s stop` / `quit` 这类停服语句。
func TestRootActionsReadOnlyShape(t *testing.T) {
	m, local := loadDeclaredManifest(t)
	if len(local) != len(m.RootActions) {
		t.Fatalf("两份解析的条目数不一致: %d vs %d", len(local), len(m.RootActions))
	}
	for index, action := range m.RootActions {
		declaredLocal := local[index]
		if declaredLocal.WritesHost {
			t.Fatalf("root action %q 声明了 writes_host：nginx 命令不直接写宿主文件，写 vhost 由插件自身落盘", action.ID)
		}
		if declaredLocal.Irreversible {
			t.Fatalf("root action %q 声明了 irreversible：本仓三条动作均幂等，不需一次性 confirm 令牌", action.ID)
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

// TestRootActionsRejectAppendedArgs 是三级校验的**运行时**证据：
// 声明 argv 合法，但追加任何 token 都必须被拒——否则通道就成了任意 nginx 调用口。
func TestRootActionsRejectAppendedArgs(t *testing.T) {
	m, _ := loadDeclaredManifest(t)
	if len(m.RootActions) == 0 {
		t.Fatalf("没有声明可测：先补 root_actions")
	}
	tails := [][]string{
		{"-s", "stop"},
		{"-s", "quit"},
		{"-c", "/tmp/evil.conf"},
		{"-e", "/tmp/evil.log"},
		{"stop"},
	}
	for _, action := range m.RootActions {
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

// actionIDsOf 提取声明里的 id 列表（失败信息用）。
func actionIDsOf(actions []pluginapi.RootAction) []string {
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		ids = append(ids, action.ID)
	}
	return ids
}
