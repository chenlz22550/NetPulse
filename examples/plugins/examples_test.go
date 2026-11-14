// 示例插件的端到端测试：用真实的插件管理器加载、启用并运行 examples/plugins 下的示例，
// 保证示例与文档里的协议始终一致。需要的外部程序（go、python）不存在时跳过对应用例。
package plugins_test

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/plugin"
)

func copyFile(t *testing.T, src, dst string, edit func(string) string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		data = []byte(edit(string(data)))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExampleManifests(t *testing.T) {
	for _, id := range []string{"email-notify", "event-log"} {
		mf, err := plugin.LoadManifest(id)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if mf.Description == "" || mf.Version == "" || mf.APIVersion != plugin.APIVersion {
			t.Errorf("%s: incomplete manifest %+v", id, mf)
		}
	}
}

func TestEventLogEndToEnd(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	base := t.TempDir()
	dir, data := filepath.Join(base, "plugins"), filepath.Join(base, "data")
	exe := filepath.Join(dir, "event-log", "event-log")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	copyFile(t, filepath.Join("event-log", "plugin.yaml"), filepath.Join(dir, "event-log", "plugin.yaml"), nil)
	if out, err := exec.Command(goBin, "build", "-o", exe, "./event-log").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	m := plugin.New(dir, data)
	defer m.Close()
	if err := m.SetEnabled("event-log", true); err != nil {
		t.Fatal(err)
	}
	// 「测试运行」：示例数据，不落盘。
	tr, err := m.Test("event-log", plugin.HookAlert, "", nil)
	if err != nil || !tr.OK || !strings.Contains(tr.Message, "未写入") {
		t.Fatalf("test run: %+v %v", tr, err)
	}
	for _, h := range []string{plugin.HookDiscovery, plugin.HookLocate} {
		if tr, err := m.Test("event-log", h, "", nil); err != nil || !tr.OK {
			t.Fatalf("test run %s: %+v %v", h, tr, err)
		}
	}
	// 真实事件：与 main.go 中 alerts.OnEvents 发出的 payload 相同。
	evs := []alert.Event{
		{Time: time.Now(), Type: alert.EvDown, TargetID: "n1", Kind: "node", Name: "3楼打印机", IP: "192.168.50.29", Location: "接入交换机 Port 12", Detail: "连续 5 次无响应", Sent: true},
		{Time: time.Now(), Type: alert.EvLoss, TargetID: "n2", Kind: "node", Name: "前台电脑", IP: "192.168.50.50", Detail: "1 分钟丢包 35%"},
	}
	before := len(m.Runs(0))
	m.Emit(plugin.HookAlert, map[string]interface{}{"events": evs, "notified": true})
	for deadline := time.Now().Add(20 * time.Second); len(m.Runs(0)) == before && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if last := m.Runs(1); len(last) != 1 || !last[0].OK || last[0].Message != "已记录 2 条" {
		t.Fatalf("alert run: %+v", last)
	}
	// AI 工具。
	tools := m.Tools()
	if len(tools) != 1 || tools[0].FullName != "plugin_event-log_recent_events" || !tools[0].AI || tools[0].Write {
		t.Fatalf("tools: %+v", tools)
	}
	res, err := m.CallTool("plugin_event-log_recent_events", map[string]json.RawMessage{"query": json.RawMessage(`"192.168.50.29"`)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	got := res.(map[string]interface{})
	if got["total_saved"] != float64(2) || got["returned"] != float64(1) || got["events"].([]interface{})[0].(map[string]interface{})["name"] != "3楼打印机" {
		t.Fatalf("recent_events: %v", got)
	}
	if _, err := os.Stat(filepath.Join(data, "plugins", "event-log", "events.jsonl")); err != nil {
		t.Fatalf("data file: %v", err)
	}
}

// fakeSMTP 接收一封邮件，返回监听端口和取回邮件内容的函数。
func fakeSMTP(t *testing.T) (int, func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var mail []string
	go func() {
		conn, err := ln.Accept()
		ln.Close() // 只接收一封，之后的连接立即被拒绝
		if err != nil {
			return
		}
		defer conn.Close()
		rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		send := func(s string) { _, _ = rw.WriteString(s + "\r\n"); _ = rw.Flush() }
		send("220 fake")
		inData := false
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			up := strings.ToUpper(line)
			switch {
			case inData && line == ".":
				inData = false
				send("250 ok")
			case inData:
				mu.Lock()
				mail = append(mail, line)
				mu.Unlock()
			case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
				send("250 fake")
			case strings.HasPrefix(up, "DATA"):
				inData = true
				send("354 go ahead")
			case strings.HasPrefix(up, "QUIT"):
				send("221 bye")
				return
			default:
				send("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(mail, "\n")
	}
}

func TestEmailNotifyEndToEnd(t *testing.T) {
	python := ""
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if out, err := exec.Command(p, "-c", "import sys; print(sys.version_info[0])").Output(); err == nil && strings.TrimSpace(string(out)) == "3" {
				python = name
				break
			}
		}
	}
	if python == "" {
		t.Skip("python 3 not in PATH")
	}
	base := t.TempDir()
	dir, data := filepath.Join(base, "plugins"), filepath.Join(base, "data")
	copyFile(t, filepath.Join("email-notify", "main.py"), filepath.Join(dir, "email-notify", "main.py"), nil)
	copyFile(t, filepath.Join("email-notify", "plugin.yaml"), filepath.Join(dir, "email-notify", "plugin.yaml"), func(s string) string {
		return strings.Replace(s, `command: ["python", "main.py"]`, `command: ["`+python+`", "main.py"]`, 1)
	})
	m := plugin.New(dir, data)
	defer m.Close()
	// 必填项未填不能启用。
	if err := m.SetEnabled("email-notify", true); err == nil || !strings.Contains(err.Error(), "SMTP 服务器") {
		t.Fatalf("enabled without settings: %v", err)
	}
	port, mail := fakeSMTP(t)
	if err := m.UpdateSettings("email-notify", map[string]interface{}{
		"smtp_host": "127.0.0.1", "smtp_port": float64(port), "security": "none",
		"from_addr": "netping@example.com", "to_addrs": "a@example.com\nb@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled("email-notify", true); err != nil {
		t.Fatal(err)
	}
	if !m.HasNotifiers() {
		t.Fatal("email-notify is not a notifier")
	}
	content := "### 机房网络监控 · 四号机房\n<font color=\"warning\">● 离线</font> **192.168.50.29** 3楼打印机\n>位置：接入交换机 Port 12\n>连续 5 次无响应"
	n, err := m.Notify("alert", content)
	if n != 1 || err != nil {
		t.Fatalf("notify: %d %v (%+v)", n, err, m.Runs(1))
	}
	if run := m.Runs(1)[0]; run.Message != "已发送给 2 个收件人" {
		t.Fatalf("run: %+v", run)
	}
	got := mail()
	for _, want := range []string{"To: a@example.com, b@example.com", "Subject: =?utf-8?", "text/plain", "text/html"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mail misses %q:\n%s", want, got)
		}
	}
	// SMTP 不通时返回失败原因（假服务器只接收一封，此时已关闭）。
	if n, err := m.Notify("alert", content); n != 1 || err == nil || !strings.Contains(err.Error(), "邮件通知") {
		t.Fatalf("notify after server closed: %d %v", n, err)
	}
}
