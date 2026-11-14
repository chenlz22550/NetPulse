package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxStdout = 1 << 20 // 插件标准输出上限
	maxStderr = 8 << 10 // 只保留标准错误的最后一段
)

// Request 是写入插件标准输入的 JSON（一次调用一个）。
type Request struct {
	APIVersion int                    `json:"api_version"`
	Hook       string                 `json:"hook"` // notify / alert / discovery / locate / tool
	ID         string                 `json:"id"`   // 本次调用的唯一编号
	Time       string                 `json:"time"` // RFC 3339，服务器本地时区
	Test       bool                   `json:"test"` // 网页「测试」按钮发出的示例数据
	NetPing    HostInfo               `json:"netping"`
	Plugin     RequestPlugin          `json:"plugin"`
	Settings   map[string]interface{} `json:"settings"`
	Payload    interface{}            `json:"payload"`
}

// RequestPlugin 是请求中关于插件自身的信息。
type RequestPlugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Dir     string `json:"dir"`      // 插件目录（只放程序，升级时会被覆盖）
	DataDir string `json:"data_dir"` // 插件私有数据目录（可写，升级不受影响）
}

// NotifyPayload 是 notify 钩子的 payload。
type NotifyPayload struct {
	Source   string `json:"source"`   // alert 告警 / discovery 扫描发现 / locate 端口定位 / test 测试
	Title    string `json:"title"`    // 消息标题
	Markdown string `json:"markdown"` // 企业微信风格的 markdown 原文（可能含 <font color> 标签）
	Text     string `json:"text"`     // 去掉颜色标签后的纯文本
}

// ToolPayload 是 tool 钩子的 payload。
type ToolPayload struct {
	Tool      string                     `json:"tool"`
	Arguments map[string]json.RawMessage `json:"arguments"`
	Remote    string                     `json:"remote"` // 调用来源（MCP 客户端 IP 或 ai）
}

// response 是插件写到标准输出的 JSON。
type response struct {
	OK      *bool           `json:"ok"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Result  json.RawMessage `json:"result"`
}

// target 是一次调用的对象（已取出清单与生效设置的快照）。
type target struct {
	id       string
	dir      string
	manifest *Manifest
	settings map[string]interface{}
}

// TestResult 是网页「测试」的返回。
type TestResult struct {
	RunRecord
	Result  interface{} `json:"result,omitempty"`
	Request interface{} `json:"request"` // 实际发给插件的请求（secret 设置已隐藏）
}

// capBuffer 最多保留 limit 字节；tail 为 true 时保留末尾，否则保留开头并记录是否溢出。
// Write 始终返回成功，避免插件因管道关闭而卡住或异常。
type capBuffer struct {
	mu       sync.Mutex
	buf      []byte
	limit    int
	tail     bool
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tail {
		b.buf = append(b.buf, p...)
		if len(b.buf) > b.limit {
			b.overflow = true
			b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
		}
		return len(p), nil
	}
	if room := b.limit - len(b.buf); room >= len(p) {
		b.buf = append(b.buf, p...)
	} else {
		b.overflow = true
		if room > 0 {
			b.buf = append(b.buf, p[:room]...)
		}
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(string(b.buf), "?")
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405-") + hex.EncodeToString(b)
}

// resolveCommand 把清单的 command 解析成可执行文件路径和参数。
// command[0] 带路径分隔符或以 . 开头时相对插件目录；否则先找插件目录下的同名文件，再找系统 PATH。
func resolveCommand(dir string, command []string) (string, []string, error) {
	exe, args := command[0], command[1:]
	local := func(p string) (string, bool) {
		cands := []string{p}
		if runtime.GOOS == "windows" && filepath.Ext(p) == "" {
			cands = append(cands, p+".exe", p+".bat", p+".cmd")
		}
		for _, c := range cands {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				return c, true
			}
		}
		return "", false
	}
	if filepath.IsAbs(exe) {
		if p, ok := local(exe); ok {
			return p, args, nil
		}
		return "", nil, fmt.Errorf("找不到程序 %s", exe)
	}
	if strings.ContainsAny(exe, `/\`) || strings.HasPrefix(exe, ".") {
		if p, ok := local(filepath.Join(dir, filepath.FromSlash(exe))); ok {
			return p, args, nil
		}
		return "", nil, fmt.Errorf("插件目录中找不到 %s", exe)
	}
	if p, ok := local(filepath.Join(dir, exe)); ok {
		return p, args, nil
	}
	p, err := exec.LookPath(exe)
	if err != nil {
		return "", nil, fmt.Errorf("找不到程序 %s：既不在插件目录，也不在系统 PATH 中（运行 netPing 的电脑需要先安装它）", exe)
	}
	if p, err = filepath.Abs(p); err != nil {
		return "", nil, err
	}
	return p, args, nil
}

func setEnv(env []string, kv ...string) []string {
	for i := 0; i+1 < len(kv); i += 2 {
		env = append(env, kv[i]+"="+kv[i+1])
	}
	return env
}

func (m *Manager) buildRequest(t target, hook string, payload interface{}, test bool) Request {
	req := Request{
		APIVersion: APIVersion, Hook: hook, ID: newID(), Time: time.Now().Format(time.RFC3339), Test: test,
		Plugin:   RequestPlugin{ID: t.id, Version: t.manifest.Version, Dir: t.dir, DataDir: filepath.Join(m.dataDir, t.id)},
		Settings: t.settings, Payload: payload,
	}
	if m.Host != nil {
		req.NetPing = m.Host()
	}
	return req
}

// invoke 启动插件进程执行一次调用并记录结果。返回插件给出的 result（没有时为 nil）。
func (m *Manager) invoke(t target, hook, tool string, payload interface{}, test bool) (interface{}, RunRecord) {
	req := m.buildRequest(t, hook, payload, test)
	rec := RunRecord{Time: time.Now(), Plugin: t.id, Hook: hook, Tool: tool, Test: test}
	m.sem <- struct{}{}
	start := time.Now()
	result, msg, stderr, err := m.execute(t, req)
	<-m.sem
	rec.Ms = time.Since(start).Milliseconds()
	rec.OK, rec.Message, rec.Stderr = err == nil, clip(msg, 500), clip(stderr, 2000)
	if err != nil {
		rec.Error = clip(err.Error(), 500)
	}
	m.record(rec)
	return result, rec
}

func (m *Manager) execute(t target, req Request) (result interface{}, message, stderr string, err error) {
	exe, args, err := resolveCommand(t.dir, t.manifest.Command)
	if err != nil {
		return nil, "", "", err
	}
	if err := os.MkdirAll(req.Plugin.DataDir, 0o755); err != nil {
		return nil, "", "", fmt.Errorf("创建插件数据目录失败: %v", err)
	}
	input, err := json.Marshal(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("请求无法序列化: %v", err)
	}
	timeout := time.Duration(t.manifest.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = t.dir
	// 设置（含密钥）只经标准输入传递，不放进环境变量或命令行，避免出现在进程列表里。
	cmd.Env = setEnv(os.Environ(),
		"NETPING_PLUGIN_ID", t.id,
		"NETPING_PLUGIN_DIR", t.dir,
		"NETPING_PLUGIN_DATA_DIR", req.Plugin.DataDir,
		"NETPING_HOOK", req.Hook,
		// 让 Python 在中文 Windows 上也按 UTF-8 读写标准输入输出。
		"PYTHONIOENCODING", "utf-8",
		"PYTHONUTF8", "1",
	)
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	out := &capBuffer{limit: maxStdout}
	errBuf := &capBuffer{limit: maxStderr, tail: true}
	cmd.Stdout, cmd.Stderr = out, errBuf
	cmd.WaitDelay = 2 * time.Second // 插件的子进程占着输出管道时也能按时返回
	hideWindow(cmd)
	runErr := cmd.Run()
	stderr = strings.TrimSpace(errBuf.String())
	if ctx.Err() == context.DeadlineExceeded {
		return nil, "", stderr, fmt.Errorf("超过 %d 秒未结束，已终止（可在 plugin.yaml 调大 timeout_sec，最大 %d）", t.manifest.TimeoutSec, maxTimeoutSec)
	}
	if out.overflow {
		return nil, "", stderr, fmt.Errorf("标准输出超过 %d KB", maxStdout>>10)
	}
	text := strings.TrimSpace(strings.TrimPrefix(out.String(), "\xef\xbb\xbf"))
	var resp response
	isProto := false
	if strings.HasPrefix(text, "{") {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &probe) == nil {
			if _, has := probe["ok"]; has && json.Unmarshal([]byte(text), &resp) == nil && resp.OK != nil {
				isProto = true
			}
		}
	}
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return nil, "", stderr, fmt.Errorf("无法启动 %s: %v", filepath.Base(exe), runErr)
		}
		reason := fmt.Sprintf("退出码 %d", ee.ExitCode())
		switch {
		case isProto && resp.Error != "":
			reason = resp.Error
		case stderr != "":
			reason += "：" + lastLine(stderr)
		}
		return nil, "", stderr, errors.New(reason)
	}
	if isProto {
		if !*resp.OK {
			if resp.Error == "" {
				resp.Error = "插件返回 ok=false"
			}
			return nil, "", stderr, errors.New(resp.Error)
		}
		if len(resp.Result) > 0 && string(resp.Result) != "null" {
			var v interface{}
			if err := json.Unmarshal(resp.Result, &v); err == nil {
				result = v
			}
		}
		return result, resp.Message, stderr, nil
	}
	// 没按协议输出：退出码为 0 即视为成功，输出内容整体作为结果。
	if text != "" {
		var v interface{}
		if json.Unmarshal([]byte(text), &v) == nil {
			result = v
		} else {
			result = text
		}
	}
	return result, "", stderr, nil
}

var (
	fontTagRe = regexp.MustCompile(`</?font[^>]*>`)
	headingRe = regexp.MustCompile(`^#+\s*`)
)

func notifyPayload(source, content string) NotifyPayload {
	text := fontTagRe.ReplaceAllString(content, "")
	title := strings.TrimSpace(text)
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = title[:i]
	}
	title = strings.TrimSpace(headingRe.ReplaceAllString(title, ""))
	return NotifyPayload{Source: source, Title: title, Markdown: content, Text: text}
}

// Notify 把一条通知消息同步发给所有已启用的通知类插件，返回参与的插件数和失败原因。
func (m *Manager) Notify(source, content string) (int, error) {
	ts := m.enabledWith(func(mf *Manifest) bool { return mf.HasHook(HookNotify) })
	if len(ts) == 0 {
		return 0, nil
	}
	payload := notifyPayload(source, content)
	errs := make([]error, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			if _, rec := m.invoke(t, HookNotify, "", payload, false); !rec.OK {
				errs[i] = fmt.Errorf("插件 %s: %s", t.manifest.Name, rec.Error)
			}
		}(i, t)
	}
	wg.Wait()
	return len(ts), errors.Join(errs...)
}

// Emit 把一个事件异步分发给声明了该钩子的已启用插件；不阻塞调用方，队列满时丢弃并记日志。
func (m *Manager) Emit(hook string, payload interface{}) {
	if m == nil || len(m.enabledWith(func(mf *Manifest) bool { return mf.HasHook(hook) })) == 0 {
		return
	}
	m.startOnce.Do(func() { go m.dispatchLoop() })
	select {
	case m.queue <- queued{hook: hook, payload: payload}:
	default:
		log.Printf("插件：事件队列已满，丢弃一个 %s 事件（有插件运行过慢）", hook)
	}
}

// dispatchLoop 按顺序分发事件：同一事件并发发给各插件，全部结束后再处理下一个，保证每个插件收到的顺序。
func (m *Manager) dispatchLoop() {
	for {
		select {
		case <-m.done:
			return
		case q := <-m.queue:
			var wg sync.WaitGroup
			for _, t := range m.enabledWith(func(mf *Manifest) bool { return mf.HasHook(q.hook) }) {
				wg.Add(1)
				go func(t target) {
					defer wg.Done()
					m.invoke(t, q.hook, "", q.payload, false)
				}(t)
			}
			wg.Wait()
		}
	}
}

// CallTool 调用已启用插件的工具，name 为 plugin_<id>_<工具名>。
func (m *Manager) CallTool(name string, args map[string]json.RawMessage, remote string) (interface{}, error) {
	for _, t := range m.enabledWith(func(mf *Manifest) bool { return len(mf.Tools) > 0 }) {
		for _, td := range t.manifest.Tools {
			if FullToolName(t.id, td.Name) != name {
				continue
			}
			if args == nil {
				args = map[string]json.RawMessage{}
			}
			res, rec := m.invoke(t, HookTool, td.Name, ToolPayload{Tool: td.Name, Arguments: args, Remote: remote}, false)
			if !rec.OK {
				return nil, fmt.Errorf("插件 %s 执行失败：%s", t.manifest.Name, rec.Error)
			}
			if res == nil {
				if rec.Message != "" {
					return map[string]interface{}{"ok": true, "message": rec.Message}, nil
				}
				return map[string]interface{}{"ok": true}, nil
			}
			return res, nil
		}
	}
	return nil, fmt.Errorf("工具 %s 不存在或所属插件未启用", name)
}

// SamplePayload 返回各钩子的示例 payload（网页「测试」与开发调试用，字段与真实事件一致）。
func SamplePayload(hook string) interface{} {
	now := time.Now().Format(time.RFC3339)
	switch hook {
	case HookNotify:
		return notifyPayload("test", "### 机房网络监控 · 示例机房\n<font color=\"info\">● 测试消息</font>\n>netPing 插件通知测试\n<font color=\"comment\">"+time.Now().Format("2006-01-02 15:04:05")+"</font>")
	case HookAlert:
		return map[string]interface{}{"notified": false, "events": []map[string]interface{}{
			{"time": now, "type": "down", "target_id": "example-node", "kind": "node", "name": "示例设备", "ip": "192.168.1.30",
				"location": "接入交换机 Port 12 / A03 机柜", "detail": "连续 5 次无响应", "sent": false},
		}}
	case HookDiscovery:
		return map[string]interface{}{"job": "机房网段", "changes": []map[string]interface{}{
			{"time": now, "type": "added", "node_id": "n-aabbccddeeff", "name": "", "ip": "192.168.1.88", "mac": "AA:BB:CC:DD:EE:FF", "detail": "新设备（示例）", "job": "机房网段"},
		}, "suggestions": []interface{}{}}
	case HookLocate:
		return map[string]interface{}{"changes": []map[string]interface{}{
			{"time": now, "type": "located", "node_id": "n-aabbccddeeff", "name": "示例设备", "ip": "192.168.1.88", "mac": "AA:BB:CC:DD:EE:FF",
				"from": "未定位", "to": "接入交换机 Port 12", "detail": "自动挂到端口（示例）"},
		}, "suggestions": []interface{}{}}
	}
	return nil
}

// Test 用示例数据（或指定的工具参数）真实运行一次插件，不要求插件已启用。
func (m *Manager) Test(id, hook, tool string, args map[string]json.RawMessage) (*TestResult, error) {
	if m == nil {
		return nil, errors.New("插件系统未启用")
	}
	m.mu.Lock()
	p, err := m.getLocked(id)
	var t target
	if err == nil {
		t = target{id: p.id, dir: p.dir, manifest: p.manifest, settings: m.effectiveLocked(p)}
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var payload interface{}
	if tool != "" || hook == HookTool {
		found := false
		for _, td := range t.manifest.Tools {
			found = found || td.Name == tool
		}
		if !found {
			return nil, fmt.Errorf("插件 %s 没有工具 %s", id, tool)
		}
		if args == nil {
			args = map[string]json.RawMessage{}
		}
		hook, payload = HookTool, ToolPayload{Tool: tool, Arguments: args, Remote: "test"}
	} else {
		if hook == "" && len(t.manifest.Hooks) > 0 {
			hook = t.manifest.Hooks[0]
		}
		if !t.manifest.HasHook(hook) {
			return nil, fmt.Errorf("插件 %s 没有声明钩子 %s", id, hook)
		}
		payload = SamplePayload(hook)
	}
	res, rec := m.invoke(t, hook, tool, payload, true)
	// 返回给网页的请求示例隐藏密钥。
	shown := m.buildRequest(t, hook, payload, true)
	masked := map[string]interface{}{}
	for k, v := range t.settings {
		masked[k] = v
	}
	for _, d := range t.manifest.Settings {
		if d.Type == SettingSecret && !emptySetting(masked[d.Key]) {
			masked[d.Key] = "******"
		}
	}
	shown.Settings = masked
	return &TestResult{RunRecord: rec, Result: res, Request: shown}, nil
}
