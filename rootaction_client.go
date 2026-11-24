package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 插件→内核 root-action 通道的客户端（ADR-0018 / ADR-0022 / ADR-0049）。
//
// 存在理由：插件降权后以 kore-site 运行，`nginx -s reload` 要给 master 发信号、
// `nginx -t` / `nginx -T` 要读 root 才能读的配置与私钥——三条命令都需要 root。
// 插件**永不**自己 exec sudo / setuid，只把"要执行哪个已声明动作"告诉内核，
// 由内核按 info.json 的固定 argv 执行并审计。
//
// 安全边界（与 ADR-0022 一致）：
//  1. 动作由 id 选择，argv 来自清单（固定），插件不传命令、不传参数；
//  2. nginx 的五条形状都没有 Tail ⇒ 本通道的调用面**没有 args 字段**，
//     少一个入参就少一条注入面；
//  3. 内核只允许插件调用它自己声明过的动作，并要求 system:admin。

// rootActionTimeout 是 HTTP 层等待上限；单条动作本身的执行上限由 info.json
// 的 timeout_s（20s）兜底，这里多留 5s 余量，只防 HTTP 永久挂住。
const rootActionTimeout = 25 * time.Second

// maxVHostContentBytes 等于协议硬顶 pluginapi.MaxFileWriteContentBytes（256 KiB）。
// 本仓当前钉的 SDK v0.1.3 尚未导出该常量；SDK 升到含 file_write 的版本后改为直接引用，
// 数值不得放大（ADR-0052：上限是安全边界不是建议值）。
const maxVHostContentBytes = 256 << 10

// maxRootActionBody 是响应体读取上限（内核 stdout/stderr 各截断 1 MiB，留余量）。
const maxRootActionBody = 4 << 20

// rootActionOutput 是 root 动作的执行结果（内核 privilege.Result 的子集，显式定形）。
type rootActionOutput struct {
	// Stdout / Stderr 是内核采集的命令输出。
	Stdout string
	Stderr string
	// ExitCode 是命令的真实退出码；-1 表示没跑起来或超时被 kill。
	ExitCode int
}

// rootActionFailure 是内核通道返回的协议错误，保留 code 与 hint。
//
// 保留 Code 的原因：`E_NEED_ROOT` 必须能被上层**结构化**识别并升级成
// 同一错误码 + 可操作提示；把它拍平成字符串就只能靠文案匹配，那是脆的。
type rootActionFailure struct {
	Code    string
	Message string
	Hint    string
}

// Error 实现 error：code + message（附 hint），与内核错误体一致。
func (e *rootActionFailure) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s（%s）", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// rootActionFunc 是通道调用面；nil 表示通道不可用（内核非 root 启动时不建通道），
// 调用方据此走降级路径而不是去连管理面。
//
// 签名里**没有 args**：本插件的三条 nginx 动作在 ADR-0022 的形状表里都没有 Tail，
// 追加参数会被内核拒绝，所以这里根本不提供这个入口。
type rootActionFunc func(actionID string) (rootActionOutput, error)

// rootActionClient 是通道客户端：内核 UDS 上的 HTTP + 插件 bearer token。
type rootActionClient struct {
	pluginID string
	token    string
	client   *http.Client
}

// newRootActionClient 在三个环境变量齐备时构造客户端；缺一返回 nil（通道不可用）。
//
// 三个变量由 supervisor 注入：非 root 的 kore 不建 socket，因此这里拿到空值，
// 插件必须显式降级（E_NEED_ROOT + hint），不能假装成功、也不去连管理面。
func newRootActionClient() *rootActionClient {
	socket := os.Getenv("KORE_KERNEL_SOCK")
	token := os.Getenv("KORE_PLUGIN_TOKEN")
	pluginID := os.Getenv("KORE_PLUGIN_ID")
	if socket == "" || token == "" || pluginID == "" {
		return nil
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &rootActionClient{
		pluginID: pluginID,
		token:    token,
		client:   &http.Client{Transport: transport, Timeout: rootActionTimeout},
	}
}

// rootActionFuncFromEnv 在通道三要素齐备时返回调用面；缺一返回 nil。
func rootActionFuncFromEnv() rootActionFunc {
	client := newRootActionClient()
	if client == nil {
		return nil
	}
	return client.Call
}

// rootActionRequest / rootActionResponse / rootActionError 是通道的 JSON 契约。
type rootActionRequest struct {
	ActionID string `json:"action_id"`
}

type rootActionResponse struct {
	Executed bool   `json:"executed"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Unit     string `json:"unit"`
}

type rootActionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// Call 经内核执行一条声明过的 root 动作，返回输出与真实退出码。
//
// 非 2xx 一律解析成 *rootActionFailure（保留 code/hint）；解析不出协议错误时
// 返回带状态码的普通错误——两条路径都不吞错、不改写成成功。
func (c *rootActionClient) Call(actionID string) (rootActionOutput, error) {
	return c.post(rootActionRequest{ActionID: actionID})
}

// vhostWriteRequest 是 file_write 动作的请求体（ADR-0052）：单层文件名 + 正文。
//
// 目录不在请求里——它只来自协议类别 nginx.conf.d（pluginapi.FileWritePrefixClasses），
// 插件自报目录的字段根本不存在，所以这里没有第二条注入面。
type vhostWriteRequest struct {
	ActionID string   `json:"action_id"`
	Args     []string `json:"args"`
	Content  string   `json:"content"`
}

// WriteVHost 经内核受控写通道把一个 vhost 片段写到 conf.d。
//
// 不 exec 任何命令：file_write 声明在 info.json 里，内核按声明分派到受控写
// （备份 / 原子替换 / 属主复核 / 审计 / changes.jsonl 全在内核侧）。
func (c *rootActionClient) WriteVHost(domain string, content []byte) error {
	if len(content) > maxVHostContentBytes {
		return fmt.Errorf("vhost 正文 %d 字节超过协议硬顶 %d", len(content), maxVHostContentBytes)
	}
	if _, err := c.post(vhostWriteRequest{
		ActionID: rootActionVHostWrite,
		Args:     []string{vhostFileName(domain)},
		Content:  string(content),
	}); err != nil {
		return err
	}
	return nil
}

// vhostWriteFuncFromEnv 在通道三要素齐备时返回受控写调用面；缺一返回 nil。
func vhostWriteFuncFromEnv() vhostWriteFunc {
	client := newRootActionClient()
	if client == nil {
		return nil
	}
	return client.WriteVHost
}

// vhostDeleteRequest 是 file_delete 动作的请求体（ADR-0053）：只有 action_id + 单层文件名。
//
// 与 file_write 的差别只有「没有 content」——内核明确拒绝带正文的删除请求
// （两条语义不许混在一条请求里），所以这里**不提供** content 字段。
type vhostDeleteRequest struct {
	ActionID string   `json:"action_id"`
	Args     []string `json:"args"`
}

// DeleteVHost 经内核受控删除通道删掉 conf.d 下本插件的 vhost 片段。
//
// 不 exec 任何命令、也不由插件进程 os.Remove：装 root 后 kore-site 对 conf.d 无目录
// 写权限，unlink 同样是 DAC 失败。内核按 info.json 的 file_delete 声明做
// 归属校验（kore-<plugin>-*.conf）→ 删前属主复核 → 强制备份 → 审计 → unlink。
func (c *rootActionClient) DeleteVHost(domain string) error {
	_, err := c.post(vhostDeleteRequest{
		ActionID: rootActionVHostDelete,
		Args:     []string{vhostFileName(domain)},
	})
	return err
}

// vhostDeleteFuncFromEnv 在通道三要素齐备时返回受控删除调用面；缺一返回 nil。
func vhostDeleteFuncFromEnv() vhostDeleteFunc {
	client := newRootActionClient()
	if client == nil {
		return nil
	}
	return client.DeleteVHost
}

// post 是通道请求的公共 HTTP 层：序列化 → UDS → 解析协议错误 / 结果。
func (c *rootActionClient) post(payload any) (rootActionOutput, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return rootActionOutput{}, fmt.Errorf("序列化 root-action 请求失败：%w", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		"http://kore/api/v1/plugins/"+c.pluginID+"/root-action", bytes.NewReader(body))
	if err != nil {
		return rootActionOutput{}, fmt.Errorf("构造 root-action 请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return rootActionOutput{}, fmt.Errorf("调用内核 root-action 失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRootActionBody))
	if err != nil {
		return rootActionOutput{}, fmt.Errorf("读取 root-action 响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var protocolErr rootActionError
		if json.Unmarshal(raw, &protocolErr) == nil && protocolErr.Code != "" {
			return rootActionOutput{}, &rootActionFailure{
				Code: protocolErr.Code, Message: protocolErr.Message, Hint: protocolErr.Hint,
			}
		}
		return rootActionOutput{}, fmt.Errorf("内核 root-action 返回 HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result rootActionResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return rootActionOutput{}, fmt.Errorf("解析 root-action 响应失败：%w", err)
	}
	return rootActionOutput{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}, nil
}

// isNeedRootFailure 报告错误是否为内核的 E_NEED_ROOT（结构化判断，不匹配文案）。
func isNeedRootFailure(err error) bool {
	var failure *rootActionFailure
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Code == pluginapi.E_NEED_ROOT
}
