package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, dir, hook, settings, payload string, test bool) response {
	t.Helper()
	d, _ := json.Marshal(dir)
	req := fmt.Sprintf(`{"api_version":1,"hook":%q,"test":%v,"plugin":{"data_dir":%s},"settings":%s,"payload":%s}`, hook, test, d, settings, payload)
	return run(strings.NewReader(req))
}

func TestEventLog(t *testing.T) {
	dir := t.TempDir()
	set := `{"max_rows":3,"record_recovery":false}`
	alert := `{"events":[{"time":"2026-11-14T10:00:00+08:00","type":"down","target_id":"n1","name":"打印机","ip":"10.0.0.5","detail":"连续 5 次无响应"},
		{"time":"2026-11-14T10:05:00+08:00","type":"recover","target_id":"n1","name":"打印机","ip":"10.0.0.5","detail":"离线时长 5分0秒"}]}`
	// 测试模式不落盘。
	if r := call(t, dir, "alert", set, alert, true); !r.OK || !strings.Contains(r.Message, "未写入") {
		t.Fatalf("test mode: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatal("test mode wrote the file")
	}
	// 恢复事件按设置跳过。
	if r := call(t, dir, "alert", set, alert, false); !r.OK || r.Message != "已记录 1 条" {
		t.Fatalf("alert: %+v", r)
	}
	disc := `{"job":"机房网段","changes":[{"time":"2026-11-14T10:10:00+08:00","type":"added","node_id":"n2","ip":"10.0.0.9","mac":"AA:BB:CC:00:00:01","detail":"新设备"}]}`
	loc := `{"changes":[{"time":"2026-11-14T10:20:00+08:00","type":"located","node_id":"n2","name":"摄像头","mac":"AA:BB:CC:00:00:01","from":"未定位","to":"接入交换机 Port 3"}]}`
	for _, hp := range [][2]string{{"discovery", disc}, {"locate", loc}} {
		if r := call(t, dir, hp[0], set, hp[1], false); !r.OK {
			t.Fatalf("%s: %+v", hp[0], r)
		}
	}
	// 超过 max_rows 只保留最新的 3 条。
	if r := call(t, dir, "alert", set, strings.Replace(alert, "10.0.0.5", "10.0.0.6", -1), false); !r.OK {
		t.Fatalf("alert 2: %+v", r)
	}
	if r := call(t, dir, "alert", set, strings.Replace(alert, "10.0.0.5", "10.0.0.7", -1), false); !r.OK {
		t.Fatalf("alert 3: %+v", r)
	}
	recs, err := readRecords(filepath.Join(dir, "events.jsonl"))
	if err != nil || len(recs) != 3 || recs[2].IP != "10.0.0.7" {
		t.Fatalf("records: %+v %v", recs, err)
	}
	// 查询工具。
	q := func(args string) map[string]interface{} {
		t.Helper()
		r := call(t, dir, "tool", set, `{"tool":"recent_events","arguments":`+args+`}`, false)
		if !r.OK {
			t.Fatalf("query %s: %+v", args, r)
		}
		return r.Result.(map[string]interface{})
	}
	if res := q(`{}`); res["returned"] != 3 || res["events"].([]record)[0].IP != "10.0.0.7" {
		t.Fatalf("query all: %v", res)
	}
	if res := q(`{"query":"10.0.0.6","limit":5}`); res["returned"] != 1 {
		t.Fatalf("query by ip: %v", res)
	}
	if res := q(`{"source":"locate"}`); res["returned"] != 1 || res["events"].([]record)[0].Detail != "未定位 → 接入交换机 Port 3" {
		t.Fatalf("query by source: %v", res)
	}
	// 错误情况。
	if r := call(t, dir, "tool", set, `{"tool":"nope","arguments":{}}`, false); r.OK {
		t.Fatal("unknown tool accepted")
	}
	if r := call(t, dir, "notify", set, `{}`, false); r.OK {
		t.Fatal("unsupported hook accepted")
	}
	if r := run(strings.NewReader("not json")); r.OK {
		t.Fatal("bad request accepted")
	}
}
