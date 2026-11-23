package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 站点流量统计：只读解析 nginx access_log（combined 格式），不联网、不起定时器、不写任何文件。
// 数据源优先级：显式 log_path > vhost 里的 access_log 指令 > 约定路径 /var/log/nginx/<domain>.access.log。
// 插件通常以低权用户运行，读不到 /var/log/nginx 时返回空态 + hint（不伪造数据）。

const (
	trafficDefaultLines = 500
	trafficMaxLines     = 5000
	trafficMaxTailBytes = 4 << 20
)

type trafficParams struct {
	Domain  string `json:"domain"`
	LogPath string `json:"log_path,omitempty"`
	Lines   int    `json:"lines,omitempty"`
}

type trafficPath struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

type trafficIP struct {
	IP    string `json:"ip"`
	Count int    `json:"count"`
}

type trafficStatus struct {
	Class string `json:"class"`
	Count int    `json:"count"`
}

type siteTrafficResult struct {
	nginxStatus
	Domain      string          `json:"domain"`
	Available   bool            `json:"available"`
	Reason      string          `json:"reason,omitempty"`
	LogPath     string          `json:"log_path,omitempty"`
	Tried       []string        `json:"tried,omitempty"`
	Lines       int             `json:"lines"`
	Parsed      int             `json:"parsed"`
	Skipped     int             `json:"skipped"`
	Total       int             `json:"total_requests"`
	BytesSent   int64           `json:"bytes_sent"`
	UniqueIPs   int             `json:"unique_ips"`
	WindowStart string          `json:"window_start,omitempty"`
	WindowEnd   string          `json:"window_end,omitempty"`
	Status      []trafficStatus `json:"status"`
	TopPaths    []trafficPath   `json:"top_paths"`
	TopIPs      []trafficIP     `json:"top_ips"`
	Hint        string          `json:"hint,omitempty"`
}

// combinedLogPattern 匹配 nginx 默认 combined 格式的核心字段（IP、时间、请求行、状态、字节）。
var combinedLogPattern = regexp.MustCompile(`^(\S+) \S+ \S+ \[([^\]]+)\] "(\S+) ([^"]*?) [^"]*" (\d{3}) (\d+|-)`)

// accessLogFromVHost 读取 vhost 的 access_log 指令（第一个非 off 的路径）。
func accessLogFromVHost(content string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "access_log" {
			path := strings.Trim(fields[1], `"';`)
			if path != "" && path != "off" {
				return path
			}
		}
	}
	return ""
}

// trafficLogCandidates 返回待尝试的日志路径：显式路径优先，其次 vhost 指令，最后约定路径。
func trafficLogCandidates(domain, explicit string) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, old := range out {
			if old == p {
				return
			}
		}
		out = append(out, p)
	}
	add(strings.TrimSpace(explicit))
	if content, err := os.ReadFile(vhostPath(domain)); err == nil {
		add(accessLogFromVHost(string(content)))
	}
	add(filepath.Join("/var/log/nginx", domain+".access.log"))
	add("/var/log/nginx/access.log")
	return out
}

// trafficPathAllowed 只允许读日志类路径（/var/log 与插件 state 目录），避免变成任意文件读取器。
func trafficPathAllowed(path string) bool {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || strings.Contains(clean, "..") {
		return false
	}
	prefixes := []string{"/var/log/"}
	if stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR")); stateDir != "" {
		prefixes = append(prefixes, filepath.Clean(stateDir)+string(os.PathSeparator))
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}

// readLogTail 读文件末尾最多 4 MiB，避免把大日志整个读进内存（G-19 无轮询/低开销）。
func readLogTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if i := indexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:] // 丢掉被截断的首行
		}
	}
	return buf, nil
}

func indexByte(data []byte, b byte) int {
	for i := range data {
		if data[i] == b {
			return i
		}
	}
	return -1
}

func (s *siteService) traffic(p trafficParams) (siteTrafficResult, *pluginapi.Error) {
	domain := strings.TrimSpace(p.Domain)
	if perr := validateDomain(domain); perr != nil {
		return siteTrafficResult{}, perr
	}
	lines := p.Lines
	if lines <= 0 {
		lines = trafficDefaultLines
	}
	if lines > trafficMaxLines {
		lines = trafficMaxLines
	}
	explicit := strings.TrimSpace(p.LogPath)
	if explicit != "" && !trafficPathAllowed(explicit) {
		return siteTrafficResult{}, invalidParam("log_path 只允许 /var/log 或插件数据目录内的日志文件")
	}
	if _, _, perr := readSiteState(domain); perr != nil {
		return siteTrafficResult{}, perr
	}

	tried := trafficLogCandidates(domain, explicit)
	result := siteTrafficResult{
		nginxStatus: s.runner.status(), Domain: domain, Lines: lines,
		Status: []trafficStatus{}, TopPaths: []trafficPath{}, TopIPs: []trafficIP{},
	}
	var data []byte
	var reason string
	for _, path := range tried {
		buf, err := readLogTail(path, trafficMaxTailBytes)
		if err == nil {
			result.LogPath = path
			data = buf
			break
		}
		if errors.Is(err, os.ErrPermission) {
			reason = "permission_denied"
			result.LogPath = path
		} else if reason == "" {
			reason = "no_log_file"
		}
	}
	result.Tried = tried
	if data == nil {
		result.Available = false
		result.Reason = reason
		result.Hint = "没有读到站点访问日志：优先在 vhost 里加 access_log /var/log/nginx/" + domain +
			".access.log; 若日志已存在但插件读不到（日志常为 root:adm 0640），请给插件用户读权限或把日志放到插件数据目录，再用 log_path 指定。"
		return result, nil
	}

	rows := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(rows) > lines {
		rows = rows[len(rows)-lines:]
	}
	pathCount := map[string]int{}
	ipCount := map[string]int{}
	statusCount := map[string]int{}
	var bytesSent int64
	first, last := "", ""
	for _, row := range rows {
		m := combinedLogPattern.FindStringSubmatch(row)
		if m == nil {
			result.Skipped++
			continue
		}
		result.Parsed++
		ip, ts, requestPath, status, size := m[1], m[2], m[4], m[5], m[6]
		ipCount[ip]++
		if i := strings.IndexByte(requestPath, '?'); i >= 0 {
			requestPath = requestPath[:i]
		}
		if requestPath != "" {
			pathCount[requestPath]++
		}
		statusCount[status[:1]+"xx"]++
		if size != "-" {
			if n, err := strconv.ParseInt(size, 10, 64); err == nil {
				bytesSent += n
			}
		}
		if first == "" {
			first = ts
		}
		last = ts
	}
	if result.Parsed == 0 {
		result.Available = false
		result.Reason = "no_entries"
		result.Hint = "日志文件存在，但没有可解析的 nginx combined 格式行；确认 access_log 使用默认 log_format。"
		return result, nil
	}
	result.Available = true
	result.Total = result.Parsed
	result.BytesSent = bytesSent
	result.UniqueIPs = len(ipCount)
	result.WindowStart, result.WindowEnd = first, last
	result.Status = topStatus(statusCount)
	result.TopPaths = topPaths(pathCount, 10)
	result.TopIPs = topIPs(ipCount, 10)
	if result.Skipped > 0 {
		result.Hint = "部分行不是 nginx combined 格式，已跳过（skipped 字段如实计数）。"
	}
	return result, nil
}

func topStatus(counts map[string]int) []trafficStatus {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]trafficStatus, 0, len(keys))
	for _, k := range keys {
		out = append(out, trafficStatus{Class: k, Count: counts[k]})
	}
	return out
}

func topPaths(counts map[string]int, limit int) []trafficPath {
	type pair struct {
		key   string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, pair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].key < pairs[j].key
	})
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}
	out := make([]trafficPath, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, trafficPath{Path: p.key, Count: p.count})
	}
	return out
}

func topIPs(counts map[string]int, limit int) []trafficIP {
	type pair struct {
		key   string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, pair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].key < pairs[j].key
	})
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}
	out := make([]trafficIP, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, trafficIP{IP: p.key, Count: p.count})
	}
	return out
}

func trafficHandler(svc *siteService, raw json.RawMessage) (any, *pluginapi.Error) {
	var p trafficParams
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return nil, invalidParam("params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalidParam("params 解析失败: " + err.Error())
		}
	}
	return svc.traffic(p)
}
