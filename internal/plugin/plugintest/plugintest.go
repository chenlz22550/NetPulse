// Package plugintest 提供测试用的假插件：把测试程序自身当作插件可执行文件，
// 不依赖 Python、shell 等外部运行环境，在 Windows / macOS / Linux 上行为一致。
//
// 用法：在测试包的 TestMain 开头调用 RunIfHelper()，再用 Install 写出插件目录。
// 假插件的行为由设置项 mode 决定（见 helper）。
package plugintest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RunIfHelper 在当前进程是被 netPing 当作插件启动时（带 NETPING_PLUGIN_ID 环境变量）执行假插件逻辑并退出。
func RunIfHelper() {
	if os.Getenv("NETPING_PLUGIN_ID") == "" {
		return
	}
	os.Exit(helper())
}

func helper() int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		return 2
	}
	var req struct {
		Hook   string `json:"hook"`
		Plugin struct {
			DataDir string `json:"data_dir"`
		} `json:"plugin"`
		Settings map[string]interface{} `json:"settings"`
		Payload  json.RawMessage        `json:"payload"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(os.Stderr, "bad request:", err)
		return 2
	}
	// 记录收到的请求，供测试断言。
	if f, err := os.OpenFile(filepath.Join(req.Plugin.DataDir, "calls.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_, _ = f.Write(append([]byte(strings.TrimSpace(string(data))), '\n'))
		_ = f.Close()
	}
	mode, _ := req.Settings["mode"].(string)
	switch mode {
	case "fail":
		fmt.Println(`{"ok":false,"error":"boom"}`)
	case "exit":
		fmt.Fprintln(os.Stderr, "line one")
		fmt.Fprintln(os.Stderr, "something broke")
		return 3
	case "raw":
		fmt.Println("plain text output")
	case "silent":
	case "sleep":
		time.Sleep(30 * time.Second)
	case "big":
		chunk := strings.Repeat("x", 64<<10)
		for i := 0; i < 20; i++ {
			fmt.Print(chunk)
		}
	default:
		out, _ := json.Marshal(map[string]interface{}{
			"ok": true, "message": "hook=" + req.Hook,
			"result": map[string]interface{}{"hook": req.Hook, "settings": req.Settings, "payload": req.Payload},
		})
		fmt.Println(string(out))
	}
	return 0
}

// Install 在 pluginsDir 下写出插件 id：command 指向当前测试程序，body 为 plugin.yaml 中除 id / command 外的内容。
func Install(t testing.TB, pluginsDir, id, body string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(pluginsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, _ := json.Marshal([]string{exe, "-test.run=^$"}) // JSON 数组也是合法的 YAML
	manifest := "id: " + id + "\ncommand: " + string(cmd) + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Calls 读取假插件记录的请求（每次调用一条，已解析为 map）。
func Calls(t testing.TB, dataDir, id string) []map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "plugins", id, "calls.jsonl"))
	if err != nil {
		return nil
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("calls.jsonl: %v", err)
		}
		out = append(out, m)
	}
	return out
}
