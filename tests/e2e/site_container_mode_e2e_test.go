//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 容器模式验收固定使用任务约定的容器名 / 端口 / 宿主挂载目录。
const (
	containerModeName = "kore-site-nginx"
	containerModePort = "18082"
	containerModeConf = "/tmp/kore-site-cnf"
	containerModeHost = "ct.kore.test"
	containerModeMark = "kore-site-container-ok"
)

// TestDockerContainerMode 验收「宿主没有 nginx、全部命令经 docker exec 在容器内执行」：
// config.set 配 docker 模式 → create 真生效（curl 200 + 标记）→ 坏 conf 被拦并回滚
// （真实 nginx -t 仍通过）→ delete 后不再命中 → 注入式容器名被拒。
func TestDockerContainerMode(t *testing.T) {
	requireDocker(t)
	assertPortFree(t, containerModePort)

	cleanupContainer(t)
	t.Cleanup(func() { cleanupContainer(t) })
	if err := os.MkdirAll(containerModeConf, 0o755); err != nil {
		t.Fatalf("建宿主 conf.d: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(containerModeConf) })

	if out, err := exec.Command("docker", "run", "-d", "--name", containerModeName,
		"-p", containerModePort+":80",
		"-v", containerModeConf+":/etc/nginx/conf.d:ro",
		"nginx:alpine").CombinedOutput(); err != nil {
		t.Fatalf("起 nginx 容器失败: %v %s", err, out)
	}
	waitContainerNginxReady(t)

	binary := buildPlugin(t)
	stateDir := t.TempDir()
	proc := startPlugin(t, binary, containerModeConf, map[string]string{"KORE_STATE_DIR": stateDir})
	defer proc.stop(t)

	// --- 1. config.set 配置 docker 模式，config.get 复核 source=file ---
	setParams := `{"values":{"nginx_mode":"docker","docker_container":"` + containerModeName +
		`","docker_conf_dir":"/etc/nginx/conf.d","host_conf_dir":` + jsonQuote(containerModeConf) + `}}`
	setResp := proc.call(t, "site.config.set", setParams)
	if setResp.Error != nil {
		t.Fatalf("config.set 失败: %s/%s", setResp.Error.Code, setResp.Error.Message)
	}
	var setResult struct {
		Saved           bool `json:"saved"`
		Applied         bool `json:"applied"`
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(setResp.Result, &setResult); err != nil ||
		!setResult.Saved || !setResult.Applied || setResult.RestartRequired {
		t.Fatalf("config.set 结果不对: err=%v %s", err, setResp.Result)
	}
	t.Logf("步骤1 config.set: %s", setResp.Result)
	getResp := proc.call(t, "site.config.get", "")
	var getResult struct {
		Source string `json:"source"`
		Values struct {
			NginxMode       string `json:"nginx_mode"`
			DockerContainer string `json:"docker_container"`
		} `json:"values"`
	}
	if err := json.Unmarshal(getResp.Result, &getResult); err != nil || getResp.Error != nil {
		t.Fatalf("config.get 失败: err=%v %s", err, getResp.Result)
	}
	if getResult.Source != "file" || getResult.Values.NginxMode != "docker" ||
		getResult.Values.DockerContainer != containerModeName {
		t.Fatalf("config.get 未反映 docker 模式: %s", getResp.Result)
	}
	t.Logf("步骤1 config.get: %s", getResp.Result)

	// --- 2. create：test_passed / reloaded / nginx_mode=docker ---
	created := proc.call(t, "site.create", `{"domain":"`+containerModeHost+`"}`)
	if created.Error != nil {
		t.Fatalf("site.create 失败: %s/%s", created.Error.Code, created.Error.Message)
	}
	var createdResult struct {
		VHostPath  string `json:"vhost_path"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
		Applied    bool   `json:"applied"`
		Created    bool   `json:"created"`
		NginxMode  string `json:"nginx_mode"`
		NginxWhy   string `json:"nginx_reason"`
	}
	if err := json.Unmarshal(created.Result, &createdResult); err != nil {
		t.Fatalf("解析 create: %v", err)
	}
	if !createdResult.TestPassed || !createdResult.Reloaded || !createdResult.Applied ||
		!createdResult.Created || createdResult.NginxMode != "docker" {
		t.Fatalf("容器模式 create 应全绿且 nginx_mode=docker: %+v", createdResult)
	}
	t.Logf("步骤2 create: %s", created.Result)

	// --- 3. curl Host: ct.kore.test 必须真被容器内 nginx 加载 ---
	if out, err := dockerExec(containerModeName, "mkdir", "-p", "/var/www/"+containerModeHost); err != nil {
		t.Fatalf("容器内建站点根目录失败: %v %s", err, out)
	}
	indexFile := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(indexFile, []byte(containerModeMark+"\n"), 0o644); err != nil {
		t.Fatalf("写 index.html: %v", err)
	}
	if out, err := exec.Command("docker", "cp", indexFile,
		containerModeName+":/var/www/"+containerModeHost+"/index.html").CombinedOutput(); err != nil {
		t.Fatalf("docker cp index.html 失败: %v %s", err, out)
	}
	code, body := waitHTTPOn(containerModePort, containerModeHost, func(code int, body string) bool {
		return code == http.StatusOK && strings.Contains(body, containerModeMark)
	}, 10*time.Second)
	t.Logf("步骤3 curl -H 'Host: %s' http://127.0.0.1:%s/ -> HTTP %d, body=%q",
		containerModeHost, containerModePort, code, snippet(body))
	dump, err := dockerExec(containerModeName, "nginx", "-T")
	if err != nil {
		t.Fatalf("容器内 nginx -T 失败: %v %s", err, dump)
	}
	if !strings.Contains(dump, "server_name "+containerModeHost+";") {
		t.Fatalf("nginx -T 未包含已建站点 %s", containerModeHost)
	}
	t.Logf("步骤3 nginx -T 含 server_name %s（确认容器内已加载）", containerModeHost)

	// --- 4. 坏 conf：nginx -t 失败 → status 报 test_passed=false → 插件回滚 ---
	brokenPath := filepath.Join(containerModeConf, "kore-site-broken.test.conf")
	brokenConf := "server {\n    listen 80;\n    server_name broken.test;\n    root /srv/www\n}\n"
	if err := os.WriteFile(brokenPath, []byte(brokenConf), 0o600); err != nil {
		t.Fatalf("写坏 conf: %v", err)
	}
	if out, err := dockerExec(containerModeName, "nginx", "-t"); err == nil {
		t.Fatalf("缺分号的 conf 竟然通过 nginx -t: %s", out)
	}
	statusResp := proc.call(t, "site.status", `{"domain":"`+containerModeHost+`"}`)
	var statusResult struct {
		TestPassed  bool   `json:"test_passed"`
		TestSkipped bool   `json:"test_skipped"`
		TestOutput  string `json:"test_output"`
		NginxMode   string `json:"nginx_mode"`
	}
	if err := json.Unmarshal(statusResp.Result, &statusResult); err != nil || statusResp.Error != nil {
		t.Fatalf("status 失败: err=%v %s", err, statusResp.Result)
	}
	if statusResult.TestPassed || statusResult.TestSkipped || statusResult.TestOutput == "" ||
		statusResult.NginxMode != "docker" {
		t.Fatalf("坏 conf 应 test_passed=false 且非 skipped: %s", statusResp.Result)
	}
	t.Logf("步骤4 status（坏 conf 存在）: %s", statusResp.Result)

	rolled := proc.call(t, "site.create", `{"domain":"ct2.kore.test"}`)
	if rolled.Error == nil || rolled.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("坏 conf 存在时 create 应 E_INVALID，实际 error=%v result=%s", rolled.Error, rolled.Result)
	}
	if _, err := os.Stat(filepath.Join(containerModeConf, "kore-site-ct2.kore.test.conf")); !os.IsNotExist(err) {
		t.Fatalf("校验失败后 vhost 应回滚: %v", err)
	}
	if err := os.Remove(brokenPath); err != nil {
		t.Fatalf("移除坏 conf: %v", err)
	}
	if out, err := dockerExec(containerModeName, "nginx", "-t"); err != nil {
		t.Fatalf("回滚后真实 nginx -t 仍失败: %v %s", err, out)
	}
	t.Logf("步骤4 回滚: E_INVALID=%q；ct2 vhost 已撤销；真实 nginx -t 通过", rolled.Error.Message)

	// --- 5. delete：conf 消失、reload 成功、curl 不再命中 ---
	deleted := proc.call(t, "site.delete", `{"domain":"`+containerModeHost+`"}`)
	if deleted.Error != nil {
		t.Fatalf("site.delete 失败: %s/%s", deleted.Error.Code, deleted.Error.Message)
	}
	var deletedResult struct {
		Deleted    bool   `json:"deleted"`
		TestPassed bool   `json:"test_passed"`
		Reloaded   bool   `json:"reloaded"`
		Applied    bool   `json:"applied"`
		NginxMode  string `json:"nginx_mode"`
	}
	if err := json.Unmarshal(deleted.Result, &deletedResult); err != nil {
		t.Fatalf("解析 delete: %v", err)
	}
	if !deletedResult.Deleted || !deletedResult.TestPassed || !deletedResult.Reloaded ||
		!deletedResult.Applied || deletedResult.NginxMode != "docker" {
		t.Fatalf("容器模式 delete 应全绿: %+v", deletedResult)
	}
	if _, err := os.Stat(filepath.Join(containerModeConf, "kore-site-"+containerModeHost+".conf")); !os.IsNotExist(err) {
		t.Fatalf("删除后 vhost 应消失: %v", err)
	}
	code, body = waitHTTPOn(containerModePort, containerModeHost, func(code int, body string) bool {
		return !(code == http.StatusOK && strings.Contains(body, containerModeMark))
	}, 10*time.Second)
	t.Logf("步骤5 delete: %s", deleted.Result)
	t.Logf("步骤5 curl Host: %s -> HTTP %d, body=%q（不再命中站点）",
		containerModeHost, code, snippet(body))
	dump, err = dockerExec(containerModeName, "nginx", "-T")
	if err != nil {
		t.Fatalf("删除后容器内 nginx -T 失败: %v %s", err, dump)
	}
	if strings.Contains(dump, containerModeHost) {
		t.Fatalf("delete 后 nginx -T 仍含 %s", containerModeHost)
	}
	t.Logf("步骤5 nginx -T 已不含 %s（容器内已卸载）", containerModeHost)

	// --- 6. 容器名校验：注入式配置在 config.set 就被拒 ---
	badSet := proc.call(t, "site.config.set",
		`{"values":{"nginx_mode":"docker","docker_container":"../../etc"}}`)
	if badSet.Error == nil || badSet.Error.Code != pluginapi.E_INVALID ||
		!strings.Contains(badSet.Error.Message, "docker_container 非法") {
		t.Fatalf("注入式容器名应被拒: error=%v result=%s", badSet.Error, badSet.Result)
	}
	t.Logf("步骤6 容器名注入被拒: %s/%s", badSet.Error.Code, badSet.Error.Message)
	after := proc.call(t, "site.config.get", "")
	var afterValues struct {
		Values struct {
			DockerContainer string `json:"docker_container"`
		} `json:"values"`
	}
	if err := json.Unmarshal(after.Result, &afterValues); err != nil ||
		afterValues.Values.DockerContainer != containerModeName {
		t.Fatalf("被拒的提交不应改动已生效配置: %s", after.Result)
	}
}

// cleanupContainer 清理验收容器与宿主挂载目录（幂等，失败不报错）。
func cleanupContainer(t *testing.T) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", containerModeName).Run()
	_ = os.RemoveAll(containerModeConf)
}

// waitContainerNginxReady 等容器内 nginx 可执行 `nginx -t`（最多 20s）。
func waitContainerNginxReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if out, err := dockerExec(containerModeName, "nginx", "-t"); err == nil {
			t.Logf("容器就绪: %s", strings.TrimSpace(out))
			return
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", containerModeName).CombinedOutput()
			t.Fatalf("容器 20s 内未就绪，logs:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitHTTPOn 轮询固定端口直到条件满足；返回最后一次状态码与正文。
func waitHTTPOn(port, host string, ok func(code int, body string) bool, timeout time.Duration) (int, string) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var lastCode int
	var lastBody string
	for {
		request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/", nil)
		if err != nil {
			return 0, err.Error()
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			lastCode, lastBody = response.StatusCode, string(body)
		} else {
			lastCode, lastBody = 0, err.Error()
		}
		if ok(lastCode, lastBody) {
			return lastCode, lastBody
		}
		if time.Now().After(deadline) {
			return lastCode, fmt.Sprintf("等待超时：code=%d body=%q", lastCode, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
