// event-log 是 netPing 的示例插件：把 alert / discovery / locate 事件追加到数据目录的 events.jsonl，
// 并提供工具 recent_events 查询。只用标准库，编译成单个可执行文件即可使用：
//
//	go build -o event-log .                                   # macOS / Linux
//	GOOS=windows GOARCH=amd64 go build -o event-log.exe .     # Windows（老系统请用 Go 1.20）
//
// 协议：netPing 把一个 JSON 请求写到标准输入，插件把一个 JSON 响应写到标准输出，然后退出。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// request 是 netPing 发来的请求（只声明用到的字段）。
type request struct {
	Hook   string `json:"hook"`
	Time   string `json:"time"`
	Test   bool   `json:"test"`
	Plugin struct {
		DataDir string `json:"data_dir"`
	} `json:"plugin"`
	Settings struct {
		MaxRows        float64 `json:"max_rows"`
		RecordRecovery bool    `json:"record_recovery"`
	} `json:"settings"`
	Payload json.RawMessage `json:"payload"`
}

// response 是写回 netPing 的响应。
type response struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Error   string      `json:"error,omitempty"`
	Result  interface{} `json:"result,omitempty"`
}

// record 是保存的一条事件。
type record struct {
	Time   string `json:"time"`
	Source string `json:"source"` // alert / discovery / locate
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	IP     string `json:"ip,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func main() {
	resp := run(os.Stdin)
	_ = json.NewEncoder(os.Stdout).Encode(resp)
	if !resp.OK {
		os.Exit(1)
	}
}

func run(in io.Reader) response {
	var req request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return response{Error: "请求不是有效的 JSON: " + err.Error()}
	}
	path := filepath.Join(req.Plugin.DataDir, "events.jsonl")
	if req.Hook == "tool" {
		return query(path, req.Payload)
	}
	recs, err := convert(req)
	if err != nil {
		return response{Error: err.Error()}
	}
	if req.Test { // 网页「测试运行」发来的是示例数据，不写入文件
		return response{OK: true, Message: fmt.Sprintf("测试：收到 %d 条 %s 事件（未写入）", len(recs), req.Hook), Result: recs}
	}
	if err := appendRecords(path, recs, int(req.Settings.MaxRows)); err != nil {
		return response{Error: err.Error()}
	}
	return response{OK: true, Message: fmt.Sprintf("已记录 %d 条", len(recs))}
}

// convert 把三种钩子的 payload 统一成 record。
func convert(req request) ([]record, error) {
	var out []record
	switch req.Hook {
	case "alert":
		var p struct {
			Events []struct {
				Time, Type, Name, IP, Detail string
				TargetID                     string `json:"target_id"`
			} `json:"events"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return nil, err
		}
		for _, e := range p.Events {
			if strings.HasSuffix(e.Type, "recover") && !req.Settings.RecordRecovery {
				continue
			}
			out = append(out, record{Time: e.Time, Source: "alert", Type: e.Type, ID: e.TargetID, Name: e.Name, IP: e.IP, Detail: e.Detail})
		}
	case "discovery":
		var p struct {
			Changes []struct {
				Time, Type, Name, IP, MAC, Detail string
				NodeID                            string `json:"node_id"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return nil, err
		}
		for _, c := range p.Changes {
			out = append(out, record{Time: c.Time, Source: "discovery", Type: c.Type, ID: c.NodeID, Name: c.Name, IP: c.IP, MAC: c.MAC, Detail: c.Detail})
		}
	case "locate":
		var p struct {
			Changes []struct {
				Time, Type, Name, IP, MAC, From, To string
				NodeID                              string `json:"node_id"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return nil, err
		}
		for _, c := range p.Changes {
			out = append(out, record{Time: c.Time, Source: "locate", Type: c.Type, ID: c.NodeID, Name: c.Name, IP: c.IP, MAC: c.MAC, Detail: c.From + " → " + c.To})
		}
	default:
		return nil, fmt.Errorf("不支持的钩子 %q", req.Hook)
	}
	return out, nil
}

func readRecords(path string) ([]record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// appendRecords 追加记录；超过 maxRows 时只保留最新的（先写临时文件再改名，避免写坏）。
func appendRecords(path string, recs []record, maxRows int) error {
	if len(recs) == 0 {
		return nil
	}
	if maxRows <= 0 {
		maxRows = 5000
	}
	all, err := readRecords(path)
	if err != nil {
		return err
	}
	if len(all)+len(recs) <= maxRows {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		return writeRecords(f, recs)
	}
	all = append(all, recs...)
	all = all[len(all)-maxRows:]
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := writeRecords(f, all); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeRecords(w io.Writer, recs []record) error {
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// query 实现工具 recent_events。payload 形如 {"tool":"recent_events","arguments":{...}}。
func query(path string, payload json.RawMessage) response {
	var p struct {
		Tool      string `json:"tool"`
		Arguments struct {
			Query  string `json:"query"`
			Source string `json:"source"`
			Limit  int    `json:"limit"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return response{Error: "参数无效: " + err.Error()}
	}
	if p.Tool != "recent_events" {
		return response{Error: "未知工具 " + p.Tool}
	}
	a := p.Arguments
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 200 {
		a.Limit = 200
	}
	all, err := readRecords(path)
	if err != nil {
		return response{Error: err.Error()}
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	out := []record{}
	for i := len(all) - 1; i >= 0 && len(out) < a.Limit; i-- {
		r := all[i]
		if a.Source != "" && r.Source != a.Source {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.IP+" "+r.Name+" "+r.MAC+" "+r.Detail+" "+r.ID), q) {
			continue
		}
		out = append(out, r)
	}
	return response{OK: true, Result: map[string]interface{}{"total_saved": len(all), "returned": len(out), "events": out}}
}
