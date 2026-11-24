package main

// root action id 常量：**代码与 info.json 的唯一对照**。
//
// 为什么用常量而不是散落的字符串：声明在 info.json、调用在代码里，两边一旦漂移
// 就只有运行期才会暴露（代码调一个没声明的 id → 内核拒绝；声明一个没人用的 id
// → 多一条无用攻击面）。单测 `TestRootActionsDeclared` 双向比对这两个集合。
//
// 三条动作都**不带追加参数**：ADR-0022 的 nginx 形状（-t / -s reload / -T / -v / -V）
// 没有 Tail，运行时追加任何 token 都会被内核拒绝——这正是「插件不能把提权通道
// 变成任意 nginx 调用」的硬约束。
const (
	// rootActionNginxTest 是 `nginx -t`：配置语法检查。
	rootActionNginxTest = "nginx-test"
	// rootActionNginxReload 是 `nginx -s reload`：热重载（不给 stop/quit：停服等于 DoS）。
	rootActionNginxReload = "nginx-reload"
	// rootActionNginxDump 是 `nginx -T`：打印已加载配置并校验（只读，用于读现有 vhost）。
	rootActionNginxDump = "nginx-dump"
)

// codeRootActionIDs 是代码**真正会调用**的动作 id，顺序即 info.json 的声明顺序。
//
// 新增一条动作时必须同时改三处：本切片、info.json、调用点；漏了哪处单测都会红。
var codeRootActionIDs = []string{
	rootActionNginxTest,
	rootActionNginxReload,
	rootActionNginxDump,
}
