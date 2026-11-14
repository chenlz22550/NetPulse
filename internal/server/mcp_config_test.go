package server

import (
	"strings"
	"testing"
)

func TestMCPScanJobsAndSettings(t *testing.T) {
	s, ts := newTestServer(t)

	// 默认有一个“机房网段”任务
	if js := s.Cfg.Get().Discovery.Jobs; len(js) != 1 || js[0].ID != "default" {
		t.Fatalf("default job: %+v", js)
	}
	// 新建任务必须有网段
	if txt, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{"name": "x"}); !isErr || !strings.Contains(txt, "subnets") {
		t.Fatalf("want subnets error: %s", txt)
	}
	// 新建：多个网段、标签、告警
	txt, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{
		"id": "office", "name": "办公区", "subnets": []string{"10.9.1.0/24", "10.9.2.0/25"}, "interval_min": 15, "alert": true, "tags": []string{"办公"},
	})
	if isErr || !strings.Contains(txt, `"created"`) {
		t.Fatalf("create: %s", txt)
	}
	j, ok := s.Cfg.Get().Discovery.FindJob("office")
	if !ok || len(j.Subnets) != 2 || j.IntervalMin != 15 || !j.AutoAdd || !j.Enabled || !j.Alert {
		t.Fatalf("job: %+v", j)
	}
	// 修改只改提供的字段
	if _, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{"id": "office", "enabled": false}); isErr {
		t.Fatal("update failed")
	}
	j, _ = s.Cfg.Get().Discovery.FindJob("office")
	if j.Enabled || len(j.Subnets) != 2 || j.Name != "办公区" {
		t.Fatalf("patch: %+v", j)
	}
	// 非法网段、间隔
	if _, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{"id": "office", "subnets": []string{"10.0.0.0/8"}}); !isErr {
		t.Fatal("/8 should be rejected")
	}
	if _, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{"id": "office", "interval_min": 1}); !isErr {
		t.Fatal("interval 1 should be rejected")
	}
	if _, isErr := call(t, ts, "upsert_scan_job", map[string]interface{}{"id": "office", "bogus": 1}); !isErr {
		t.Fatal("unknown field should be rejected")
	}
	// 未知任务扫描
	if _, isErr := call(t, ts, "scan_network", map[string]interface{}{"job": "nope"}); !isErr {
		t.Fatal("unknown job scan should fail")
	}
	// 全局设置
	if txt, isErr := call(t, ts, "set_discovery_settings", map[string]interface{}{"enabled": false, "ip_tracking": "dhcp", "oui": map[string]string{"60-d7-e3": "深大智能"}}); isErr {
		t.Fatalf("settings: %s", txt)
	}
	d := s.Cfg.Get().Discovery
	if d.Enabled || d.IPTracking != "dhcp" || d.OUI["60-d7-e3"] != "深大智能" || len(d.Jobs) != 2 {
		t.Fatalf("discovery: %+v", d)
	}
	if _, isErr := call(t, ts, "set_discovery_settings", map[string]interface{}{"ip_tracking": "sometimes"}); !isErr {
		t.Fatal("bad ip_tracking should be rejected")
	}
	// 删除任务
	if _, isErr := call(t, ts, "delete_scan_job", map[string]interface{}{"id": "office"}); isErr {
		t.Fatal("delete failed")
	}
	if _, ok := s.Cfg.Get().Discovery.FindJob("office"); ok {
		t.Fatal("job not deleted")
	}

	// 告警设置：启用前需要 webhook
	if _, isErr := call(t, ts, "set_alert_config", map[string]interface{}{"enabled": true}); !isErr {
		t.Fatal("enable without webhook should fail")
	}
	txt, isErr = call(t, ts, "set_alert_config", map[string]interface{}{"enabled": true, "webhook_url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123456", "down_after_failures": 3})
	if isErr || strings.Contains(txt, "abcdef123456") {
		t.Fatalf("alert config (webhook must be masked): %s", txt)
	}
	if a := s.Cfg.Get().Alert; !a.Enabled || a.DownAfterFailures != 3 || a.UpAfterSuccesses != 3 {
		t.Fatalf("alert: %+v", a)
	}
	// ping 默认值
	if _, isErr := call(t, ts, "set_ping_defaults", map[string]interface{}{"interval_ms": 2000, "retention_days": 7}); isErr {
		t.Fatal("ping defaults failed")
	}
	if c := s.Cfg.Get(); c.Ping.IntervalMs != 2000 || c.RetentionDays != 7 || c.Ping.TimeoutMs != 1000 {
		t.Fatalf("ping: %+v %d", c.Ping, c.RetentionDays)
	}
	// 机房信息
	if _, isErr := call(t, ts, "set_site", map[string]interface{}{"name": "二号机房"}); isErr {
		t.Fatal("set_site failed")
	}
	if tp, _ := s.Topo.Snapshot(); tp.Site.Name != "二号机房" || tp.Site.Subnet == "" {
		t.Fatalf("site: %+v", tp.Site)
	}
	// 自动排布
	if txt, isErr := call(t, ts, "reset_layout", map[string]interface{}{}); isErr || !strings.Contains(txt, "reset") {
		t.Fatalf("reset_layout: %s", txt)
	}
	// 设置里能看到扫描任务
	if txt, _ := call(t, ts, "get_settings", map[string]interface{}{}); !strings.Contains(txt, `"jobs"`) || !strings.Contains(txt, "二号机房") {
		t.Fatalf("settings: %s", txt)
	}
	if txt, _ := call(t, ts, "get_discovery", map[string]interface{}{}); !strings.Contains(txt, `"jobs"`) {
		t.Fatalf("get_discovery: %s", txt)
	}
}

func TestMCPNotificationChannels(t *testing.T) {
	s, ts := newTestServer(t)
	txt, bad := call(t, ts, "set_alert_config", map[string]interface{}{
		"enabled": true, "feishu_url": "https://example.com/private-hook", "feishu_secret": "private-secret",
	})
	if bad || strings.Contains(txt, "private-") {
		t.Fatalf("update or masking failed: %s", txt)
	}
	if a := s.Cfg.Get().Alert; a.FeishuSecret != "private-secret" || !a.Enabled {
		t.Fatal("settings not stored")
	}
	txt, bad = call(t, ts, "get_settings", map[string]interface{}{})
	if bad || strings.Contains(txt, "private-") {
		t.Fatalf("settings leaked secret: %s", txt)
	}
	_, bad = call(t, ts, "set_alert_config", map[string]interface{}{"site_title": "test"})
	if bad || s.Cfg.Get().Alert.FeishuSecret != "private-secret" {
		t.Fatal("patch lost secret")
	}
	_, bad = call(t, ts, "set_alert_config", map[string]interface{}{"feishu_url": ""})
	if !bad {
		t.Fatal("enabled without channel accepted")
	}
	_, bad = call(t, ts, "set_alert_config", map[string]interface{}{"enabled": false, "feishu_url": "", "feishu_secret": ""})
	if bad || s.Cfg.Get().Alert.HasChannels() {
		t.Fatal("clear failed")
	}
}
