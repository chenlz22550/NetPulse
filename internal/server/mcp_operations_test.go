package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/llm"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/plugin"
	"netping/internal/profiles"
	"netping/internal/store"
	"netping/internal/topology"
)

func callJSON(t *testing.T, ts *httptest.Server, name string, args interface{}) (map[string]interface{}, string, bool) {
	t.Helper()
	txt, isErr := call(t, ts, name, args)
	if isErr {
		return nil, txt, true
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		t.Fatalf("%s: unmarshal tool json: %v\nraw=%s", name, err, txt)
	}
	return out, txt, false
}

func toolsListMap(t *testing.T, ts *httptest.Server) map[string]map[string]interface{} {
	t.Helper()
	code, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 {
		t.Fatalf("tools/list status=%d body=%v", code, out)
	}
	res := out["result"].(map[string]interface{})
	rawList := res["tools"].([]interface{})
	m := make(map[string]map[string]interface{}, len(rawList))
	for _, it := range rawList {
		obj := it.(map[string]interface{})
		m[obj["name"].(string)] = obj
	}
	return m
}

func newProfileTestServerWithProbe(t *testing.T, probeCalls *int32) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Get()
	c.Auth = config.Auth{Password: testPassword}
	c.AI.APIKey = "sk-test-global"
	c.Alert.Enabled = true
	c.Alert.WebhookURL = "https://example.com/mock-hook?key=secret-hook-token"
	c.Locate.Community = "comm-secret-default"
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	topo, err := topology.Open(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := topo.Replace(&topology.Topology{
		Version:  1,
		Site:     topology.Site{Name: "主测试机房", Subnet: "10.10.0.0/24", Gateway: "10.10.0.1"},
		Switches: []*topology.Switch{{ID: "sw1", Name: "核心1", Role: "core", IP: "10.10.0.254", Ports: []string{"1"}}},
		Nodes:    []*topology.Node{{ID: "n1", Name: "节点1", IP: "10.10.0.10", Attach: topology.Endpoint{Switch: "sw1", Port: "1"}}},
	}); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(dir, "data")
	st, err := store.New(filepath.Join(dataDir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(st, func() int { return 5 })
	mon.SetPaused(true)
	t.Cleanup(mon.Close)

	al := alert.NewEngine(cfg.Get, mon, filepath.Join(dataDir, "alerts.jsonl"))
	disc := discovery.New(cfg, topo, mon, dataDir)
	loc := locate.New(cfg, topo, dataDir)

	pm, err := profiles.Open(profiles.Options{
		BaseDir: dir,
		Cfg:     cfg,
		Topo:    topo,
		Mon:     mon,
		Store:   st,
		Alerts:  al,
		Disc:    disc,
		Loc:     loc,
		ProbeFunc: func(ip string, timeout time.Duration) (string, string) {
			if probeCalls != nil {
				atomic.AddInt32(probeCalls, 1)
			}
			return "unreachable", "mock timeout"
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		Cfg:      cfg,
		Topo:     topo,
		Mon:      mon,
		Alerts:   al,
		Store:    st,
		Disc:     disc,
		Loc:      loc,
		Profiles: pm,
		DocsMD:   []byte("# docs"),
		SchemaJSON: []byte("{}"),
		BaseDir:  dir,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/api/", s.routeAPI)
	ts := httptest.NewServer(s.withAuth(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

func TestMCP_OperationsToolsListAndReadOnly(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)
	tools := toolsListMap(t, ts)

	expectedReadOnly := []string{
		"list_profiles", "preview_profile", "get_profile_settings",
		"list_profile_versions", "get_profile_version", "preview_profile_version_rollback",
		"list_maintenance", "list_reports", "get_report",
		"preview_device_cleanup", "get_plugins",
	}
	expectedWrite := []string{
		"create_profile", "rename_profile", "delete_profile", "activate_profile", "set_profile_settings",
		"create_profile_version", "rollback_profile_version",
		"create_maintenance", "cancel_maintenance",
		"generate_health_report", "generate_ai_report", "delete_report",
		"set_device_locked", "cleanup_devices",
		"generate_ai_optimize", "apply_ai_optimize", "undo_ai_optimize",
	}

	for _, name := range expectedReadOnly {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing read-only tool %s", name)
		}
	}
	for _, name := range expectedWrite {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing write tool %s", name)
		}
	}

	assertAnn := func(name string, ro, dest, ow bool) {
		t.Helper()
		ann := tools[name]["annotations"].(map[string]interface{})
		if ann["readOnlyHint"] != ro || ann["destructiveHint"] != dest || ann["openWorldHint"] != ow {
			t.Fatalf("tool %s annotations=%v want ro=%v dest=%v ow=%v", name, ann, ro, dest, ow)
		}
	}
	assertAnn("get_plugins", true, false, false)
	assertAnn("preview_profile", true, false, true)
	assertAnn("preview_profile_version_rollback", true, false, false)
	assertAnn("generate_ai_optimize", false, false, true)
	assertAnn("generate_ai_report", false, false, true)
	assertAnn("delete_profile", false, true, false)
	assertAnn("activate_profile", false, true, false)
	assertAnn("rollback_profile_version", false, true, false)
	assertAnn("apply_ai_optimize", false, true, false)
	assertAnn("undo_ai_optimize", false, true, false)

	// upsert_device must include locked property
	upsert := tools["upsert_device"]
	props := upsert["inputSchema"].(map[string]interface{})["properties"].(map[string]interface{})
	if _, ok := props["locked"]; !ok {
		t.Fatal("upsert_device schema missing locked field")
	}

	// Verify MCP.ReadOnly hides & blocks every write tool while keeping read-only tools visible
	c := s.Cfg.Get()
	c.MCP.ReadOnly = true
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	roTools := toolsListMap(t, ts)
	for _, name := range expectedReadOnly {
		if _, ok := roTools[name]; !ok {
			t.Fatalf("read-only tool %s hidden in ReadOnly mode", name)
		}
	}
	for _, name := range expectedWrite {
		if _, ok := roTools[name]; ok {
			t.Fatalf("write tool %s must be hidden in ReadOnly mode", name)
		}
		msg, isErr := call(t, ts, name, map[string]interface{}{})
		if !isErr || !strings.Contains(msg, "只读模式") {
			t.Fatalf("write tool %s should be rejected in ReadOnly mode, got isErr=%v msg=%s", name, isErr, msg)
		}
	}
}

func TestMCP_ProfilesAndSettingsLifecycle(t *testing.T) {
	// s.Profiles == nil returns clear error instead of panic
	_, bareTS := newTestServer(t)
	if _, msg, isErr := callJSON(t, bareTS, "list_profiles", map[string]interface{}{}); !isErr || !strings.Contains(msg, "未初始化") {
		t.Fatalf("expected uninitialized error, got err=%v msg=%s", isErr, msg)
	}

	var probeCalls int32
	s, ts := newProfileTestServerWithProbe(t, &probeCalls)

	listOut, _, isErr := callJSON(t, ts, "list_profiles", map[string]interface{}{})
	if isErr {
		t.Fatal("list_profiles failed")
	}
	if listOut["active_profile_id"] != "default" {
		t.Fatalf("unexpected active profile: %v", listOut)
	}
	ep0 := int64(listOut["epoch"].(float64))

	// create_profile (clone default)
	created, _, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "备用机房", "copy_current": true})
	if isErr {
		t.Fatal("create_profile failed")
	}
	backupID := created["profile"].(map[string]interface{})["id"].(string)

	// rename_profile
	if _, msg, isErr := callJSON(t, ts, "rename_profile", map[string]interface{}{"id": backupID, "name": "容灾机房"}); isErr {
		t.Fatalf("rename_profile failed: %s", msg)
	}

	// preview_profile default probe=false does not trigger ProbeFunc
	_, _, isErr = callJSON(t, ts, "preview_profile", map[string]interface{}{"id": backupID})
	if isErr || atomic.LoadInt32(&probeCalls) != 0 {
		t.Fatalf("preview default probe should not call ProbeFunc, calls=%d isErr=%v", probeCalls, isErr)
	}
	// preview_profile probe=true triggers ProbeFunc
	_, _, isErr = callJSON(t, ts, "preview_profile", map[string]interface{}{"id": backupID, "probe": true})
	if isErr || atomic.LoadInt32(&probeCalls) == 0 {
		t.Fatalf("preview probe=true should call ProbeFunc, calls=%d isErr=%v", probeCalls, isErr)
	}

	// get_profile_settings masks secrets
	gsOut, gsText, isErr := callJSON(t, ts, "get_profile_settings", map[string]interface{}{"id": backupID})
	if isErr {
		t.Fatalf("get_profile_settings failed: %v", gsText)
	}
	if strings.Contains(gsText, "secret-hook-token") || strings.Contains(gsText, "comm-secret-default") {
		t.Fatalf("secrets leaked in get_profile_settings: %s", gsText)
	}
	_ = gsOut

	// set_profile_settings forbids global fields
	for _, badKey := range []string{"auth", "mcp", "ai", "listen", "retention_days"} {
		_, msg, isErr := callJSON(t, ts, "set_profile_settings", map[string]interface{}{
			"id":       backupID,
			"settings": map[string]interface{}{badKey: map[string]interface{}{"enabled": true}},
		})
		if !isErr || !strings.Contains(msg, "禁止") {
			t.Fatalf("expected forbidden error for %s, got err=%v msg=%s", badKey, isErr, msg)
		}
	}

	// set_profile_settings updates allowed room settings and preserves masked secrets
	_, setText, isErr := callJSON(t, ts, "set_profile_settings", map[string]interface{}{
		"id": backupID,
		"settings": map[string]interface{}{
			"alert":  map[string]interface{}{"repeat_interval_min": 12},
			"locate": map[string]interface{}{"interval_min": 25},
		},
	})
	if isErr {
		t.Fatalf("set_profile_settings failed: %s", setText)
	}
	if strings.Contains(setText, "secret-hook-token") || strings.Contains(setText, "comm-secret-default") {
		t.Fatalf("secrets leaked in set_profile_settings response: %s", setText)
	}
	rawRS, _, _, err := s.Profiles.GetSettings(backupID)
	if err != nil {
		t.Fatal(err)
	}
	if rawRS.Alert.RepeatIntervalMin != 12 || rawRS.Locate.IntervalMin != 25 || rawRS.Alert.WebhookURL != "https://example.com/mock-hook?key=secret-hook-token" {
		t.Fatalf("room settings update or secret preservation failed: %+v", rawRS)
	}

	// activate_profile requires confirm=true
	if _, _, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": backupID}); !isErr {
		t.Fatal("activate_profile without confirm=true should fail")
	}
	actOut, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": backupID, "confirm": true})
	if isErr {
		t.Fatalf("activate_profile failed: %s", msg)
	}
	if actOut["active_profile_id"] != backupID || int64(actOut["epoch"].(float64)) <= ep0 {
		t.Fatalf("unexpected activate output: %v", actOut)
	}

	// delete_profile requires confirm=true and blocks deleting active profile
	if _, _, isErr := callJSON(t, ts, "delete_profile", map[string]interface{}{"id": "default"}); !isErr {
		t.Fatal("delete_profile without confirm=true should fail")
	}
	if _, _, isErr := callJSON(t, ts, "delete_profile", map[string]interface{}{"id": backupID, "confirm": true}); !isErr {
		t.Fatal("deleting active profile must fail")
	}
	if _, msg, isErr := callJSON(t, ts, "delete_profile", map[string]interface{}{"id": "default", "confirm": true}); isErr {
		t.Fatalf("deleting inactive profile failed: %s", msg)
	}
}

func TestMCP_ProfileVersionsAndRollback(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	vOut, msg, isErr := callJSON(t, ts, "create_profile_version", map[string]interface{}{"id": "default", "note": "初始基线"})
	if isErr {
		t.Fatalf("create_profile_version failed: %s", msg)
	}
	verID := vOut["version"].(map[string]interface{})["id"].(string)

	listOut, _, isErr := callJSON(t, ts, "list_profile_versions", map[string]interface{}{"id": "default"})
	if isErr || len(listOut["versions"].([]interface{})) == 0 {
		t.Fatalf("list_profile_versions failed: %v", listOut)
	}

	// get_profile_version masks secrets
	_, vText, isErr := callJSON(t, ts, "get_profile_version", map[string]interface{}{"id": "default", "version_id": verID})
	if isErr {
		t.Fatalf("get_profile_version failed: %s", vText)
	}
	if strings.Contains(vText, "secret-hook-token") || strings.Contains(vText, "comm-secret-default") {
		t.Fatalf("get_profile_version leaked secrets: %s", vText)
	}

	// Modify topology & settings
	if err := s.Topo.Update(func(top *topology.Topology) error {
		top.Nodes = append(top.Nodes, &topology.Node{ID: "n2", Name: "新增节点", IP: "10.10.0.22"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Profiles.UpdateSettings("default", []byte(`{"ping":{"interval_ms":2000}}`)); err != nil {
		t.Fatal(err)
	}

	// preview_profile_version_rollback is read-only & reports diff cleanly
	prevOut, prevText, isErr := callJSON(t, ts, "preview_profile_version_rollback", map[string]interface{}{"id": "default", "version_id": verID})
	if isErr {
		t.Fatalf("preview rollback failed: %s", prevText)
	}
	if prevOut["can_rollback"] != true {
		t.Fatalf("expected can_rollback=true, got %v", prevOut)
	}
	diff := prevOut["diff"].(map[string]interface{})
	if len(diff["nodes_removed"].([]interface{})) != 1 || len(diff["settings_sections_changed"].([]interface{})) == 0 {
		t.Fatalf("unexpected rollback diff: %v", diff)
	}
	// Topology still contains n2 because preview was read-only
	curTopo, _ := s.Topo.Snapshot()
	if curTopo.FindNode("n2") == nil {
		t.Fatal("preview_profile_version_rollback mutated topology")
	}

	// Invalid version ID
	if _, _, isErr := callJSON(t, ts, "rollback_profile_version", map[string]interface{}{"id": "default", "version_id": "../bad", "confirm": true}); !isErr {
		t.Fatal("invalid version_id should fail")
	}
	// Missing confirm=true
	if _, _, isErr := callJSON(t, ts, "rollback_profile_version", map[string]interface{}{"id": "default", "version_id": verID}); !isErr {
		t.Fatal("rollback without confirm=true should fail")
	}
	// Confirmed rollback restores snapshot and creates pre_rollback backup
	boundID, boundEpoch := s.activeProfileState()
	boundCtx := s.withBoundProfileEpoch(context.Background(), boundID, boundEpoch)
	rbOut, msg, isErr := callJSON(t, ts, "rollback_profile_version", map[string]interface{}{"id": "default", "version_id": verID, "confirm": true})
	if isErr {
		t.Fatalf("rollback failed: %s", msg)
	}
	if rbOut["restored_version"] != verID || rbOut["backup_version"] == "" {
		t.Fatalf("unexpected rollback output: %v", rbOut)
	}
	curTopo, _ = s.Topo.Snapshot()
	if curTopo.FindNode("n2") != nil {
		t.Fatal("rollback did not restore topology")
	}
	if err := s.checkProfileEpoch(boundCtx); err == nil {
		t.Fatal("active rollback must invalidate in-flight AI context")
	}
	if _, _, isErr := callJSON(t, ts, "apply_ai_optimize", map[string]interface{}{
		"profile_id": boundID, "epoch": boundEpoch,
		"classify": []map[string]string{{"id": "n1", "category": "服务器"}},
		"dry_run": true,
	}); !isErr {
		t.Fatal("optimization preview from before rollback must be rejected")
	}
}

func TestMCP_MaintenanceWindows(t *testing.T) {
	_, ts := newProfileTestServerWithProbe(t, nil)

	created, _, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "二号机房"})
	if isErr {
		t.Fatal("create_profile failed")
	}
	otherID := created["profile"].(map[string]interface{})["id"].(string)

	startLocal := time.Now().Add(10 * time.Minute).Format("2006-01-02T15:04")
	endLocal := time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04:05")

	// Cross-room profile_id rejected
	if _, msg, isErr := callJSON(t, ts, "list_maintenance", map[string]interface{}{"profile_id": otherID}); !isErr || !strings.Contains(msg, "当前激活机房") {
		t.Fatalf("expected cross-room error, got err=%v msg=%s", isErr, msg)
	}
	if _, _, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"profile_id": otherID, "scope": "site", "start": startLocal, "end": endLocal, "reason": "割接",
	}); !isErr {
		t.Fatal("cross-room create_maintenance should fail")
	}

	// Reason validation (>500 runes or empty)
	longReason := strings.Repeat("维", 501)
	if _, _, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"scope": "site", "start": startLocal, "end": endLocal, "reason": longReason,
	}); !isErr {
		t.Fatal("reason > 500 runes should fail")
	}
	// Devices empty target_ids
	if _, _, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"scope": "devices", "start": startLocal, "end": endLocal, "reason": "更换模块",
	}); !isErr {
		t.Fatal("devices scope without target_ids should fail")
	}
	// End before start
	if _, _, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"scope": "site", "start": endLocal, "end": startLocal, "reason": "顺序错",
	}); !isErr {
		t.Fatal("end <= start should fail")
	}

	// Valid site window with local time formats
	w1, msg, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"scope": "site", "start": startLocal, "end": endLocal, "reason": "核心电源检修",
	})
	if isErr {
		t.Fatalf("site create_maintenance failed: %s", msg)
	}
	w1ID := w1["window"].(map[string]interface{})["id"].(string)

	// Valid devices window with RFC3339
	w2, msg, isErr := callJSON(t, ts, "create_maintenance", map[string]interface{}{
		"scope":      "devices",
		"target_ids": []string{"n1"},
		"start":      time.Now().Add(5 * time.Minute).Format(time.RFC3339),
		"end":        time.Now().Add(90 * time.Minute).Format(time.RFC3339),
		"reason":     "节点固件升级",
	})
	if isErr {
		t.Fatalf("devices create_maintenance failed: %s", msg)
	}
	_ = w2

	listOut, _, isErr := callJSON(t, ts, "list_maintenance", map[string]interface{}{})
	if isErr || len(listOut["windows"].([]interface{})) != 2 {
		t.Fatalf("expected 2 maintenance windows, got %v", listOut)
	}

	// Cancel w1
	if _, msg, isErr := callJSON(t, ts, "cancel_maintenance", map[string]interface{}{"id": w1ID}); isErr {
		t.Fatalf("cancel_maintenance failed: %s", msg)
	}

	// Activate second room: cancelling w1 from active second room must fail (isolated per room)
	if _, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": otherID, "confirm": true}); isErr {
		t.Fatalf("activate other room failed: %s", msg)
	}
	if _, _, isErr := callJSON(t, ts, "cancel_maintenance", map[string]interface{}{"id": w1ID}); !isErr {
		t.Fatal("cancelling first room maintenance window from second room must fail")
	}
}

func TestMCP_OperationsReports(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	// Deterministic report with no samples preserves null KPIs & no_data notice
	hrOut, msg, isErr := callJSON(t, ts, "generate_health_report", map[string]interface{}{"range": "24h"})
	if isErr {
		t.Fatalf("generate_health_report failed: %s", msg)
	}
	notices := hrOut["notices"].(map[string]interface{})
	if notices["no_data"] != true {
		t.Fatalf("expected no_data=true, got %v", notices)
	}
	rep := hrOut["report"].(map[string]interface{})
	repID := rep["id"].(string)
	summary := rep["summary"].(map[string]interface{})
	if summary["availability_pct"] != nil || summary["loss_pct"] != nil || summary["avg_ms"] != nil {
		t.Fatalf("empty report KPIs must be null, got %v", summary)
	}

	// list_reports
	listOut, _, isErr := callJSON(t, ts, "list_reports", map[string]interface{}{})
	if isErr || len(listOut["reports"].([]interface{})) != 1 {
		t.Fatalf("unexpected list_reports: %v", listOut)
	}

	// get_report in json / markdown / html without leaking local file paths
	for _, fmtName := range []string{"json", "markdown", "html"} {
		_, txt, isErr := callJSON(t, ts, "get_report", map[string]interface{}{"id": repID, "format": fmtName})
		if isErr {
			t.Fatalf("get_report(%s) failed: %s", fmtName, txt)
		}
		if strings.Contains(txt, s.BaseDir) {
			t.Fatalf("get_report(%s) leaked local path %s", fmtName, s.BaseDir)
		}
	}
	// Path traversal rejected
	if _, _, isErr := callJSON(t, ts, "get_report", map[string]interface{}{"id": "../config", "format": "json"}); !isErr {
		t.Fatal("path traversal in get_report must fail")
	}

	// Explicit AI report using fake LLM
	llmSrv, reqs := fakeOptLLM(t, "# 主测试机房 网络运维日报\n\n## 一、总体结论\n健康评级：良。")
	cfg := s.Cfg.Get()
	cfg.AI.Enabled = true
	cfg.AI.BaseURL = llmSrv.URL
	cfg.AI.APIKey = "sk-test-123456"
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}
	aiOut, msg, isErr := callJSON(t, ts, "generate_ai_report", map[string]interface{}{"range": "24h", "note": "检查网关"})
	if isErr {
		t.Fatalf("generate_ai_report failed: %s", msg)
	}
	if len(*reqs) == 0 {
		t.Fatal("generate_ai_report should invoke LLM")
	}
	aiID := aiOut["report"].(map[string]interface{})["id"].(string)

	// delete_report
	if _, msg, isErr := callJSON(t, ts, "delete_report", map[string]interface{}{"id": aiID}); isErr {
		t.Fatalf("delete_report failed: %s", msg)
	}
	if _, _, isErr := callJSON(t, ts, "get_report", map[string]interface{}{"id": aiID}); !isErr {
		t.Fatal("deleted report should not exist")
	}
}

func TestMCP_DeviceLockAndCleanup(t *testing.T) {
	s, ts := newTestServer(t)
	disabled := false
	if err := s.Topo.Replace(&topology.Topology{
		Version:  1,
		Site:     topology.Site{Name: "测试机房"},
		Switches: []*topology.Switch{{ID: "sw1", Name: "核心1", Role: "core", IP: "10.0.0.254", Ports: []string{"1"}}},
		Nodes: []*topology.Node{
			{ID: "n-locked", Name: "锁定无IP设备", IP: "", Locked: true},
			{ID: "n-noip", Name: "无IP设备", IP: ""},
			{ID: "n-disabled", Name: "停监控设备", IP: "10.0.0.50", Monitor: topology.MonitorSpec{Enabled: &disabled}},
			{ID: "n-healthy", Name: "正常设备", IP: "10.0.0.51"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// upsert_device supports locked field
	if _, msg, isErr := callJSON(t, ts, "upsert_device", map[string]interface{}{"id": "n-upserted", "ip": "10.0.0.52", "locked": true}); isErr {
		t.Fatalf("upsert_device locked failed: %s", msg)
	}
	curTopo, _ := s.Topo.Snapshot()
	if n := curTopo.FindNode("n-upserted"); n == nil || !n.Locked {
		t.Fatalf("upsert_device did not set locked=true: %+v", n)
	}

	// set_device_locked rejects switches
	if _, msg, isErr := callJSON(t, ts, "set_device_locked", map[string]interface{}{"id": "sw1", "locked": true}); !isErr || !strings.Contains(msg, "交换机") {
		t.Fatalf("expected switch rejection, got err=%v msg=%s", isErr, msg)
	}

	// preview_device_cleanup finds n-noip and n-disabled, protects n-locked & n-upserted, and returns binding metadata
	prevOut, _, isErr := callJSON(t, ts, "preview_device_cleanup", map[string]interface{}{})
	if isErr {
		t.Fatal("preview_device_cleanup failed")
	}
	if int(prevOut["count"].(float64)) != 2 || int(prevOut["protected"].(float64)) != 2 {
		t.Fatalf("unexpected cleanup preview: %v", prevOut)
	}
	prevPID := prevOut["profile_id"].(string)
	prevEpoch := int64(prevOut["epoch"].(float64))

	// Simulate race: lock n-disabled between preview and execute
	if _, msg, isErr := callJSON(t, ts, "set_device_locked", map[string]interface{}{"id": "n-disabled", "locked": true}); isErr {
		t.Fatalf("set_device_locked failed: %s", msg)
	}

	// cleanup_devices requires confirm=true and non-empty ids
	if _, _, isErr := callJSON(t, ts, "cleanup_devices", map[string]interface{}{
		"profile_id": prevPID, "epoch": prevEpoch, "ids": []string{"n-noip"}, "confirm": false,
	}); !isErr {
		t.Fatal("cleanup_devices without confirm=true should fail")
	}
	if _, _, isErr := callJSON(t, ts, "cleanup_devices", map[string]interface{}{
		"profile_id": prevPID, "epoch": prevEpoch, "ids": []string{}, "confirm": true,
	}); !isErr {
		t.Fatal("empty ids should fail")
	}

	cleanOut, msg, isErr := callJSON(t, ts, "cleanup_devices", map[string]interface{}{
		"profile_id": prevPID,
		"epoch":      prevEpoch,
		"ids":        []string{"n-noip", "n-disabled", "sw1"},
		"confirm":    true,
	})
	if isErr {
		t.Fatalf("cleanup_devices failed: %s", msg)
	}
	deleted := cleanOut["deleted"].([]interface{})
	if len(deleted) != 1 || deleted[0] != "n-noip" {
		t.Fatalf("expected only n-noip deleted, got %v", cleanOut)
	}
	curTopo, _ = s.Topo.Snapshot()
	if curTopo.FindSwitch("sw1") == nil || curTopo.FindNode("n-disabled") == nil || curTopo.FindNode("n-healthy") == nil {
		t.Fatal("switches, locked node, or healthy node were unexpectedly removed")
	}
}

func TestMCP_CleanupProfileMismatchProtection(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	// Add eligible node "n-shared" to Room A (default)
	if _, msg, isErr := callJSON(t, ts, "upsert_device", map[string]interface{}{"id": "n-shared", "ip": ""}); isErr {
		t.Fatalf("upsert n-shared in room A failed: %s", msg)
	}
	prevOut, msg, isErr := callJSON(t, ts, "preview_device_cleanup", map[string]interface{}{})
	if isErr {
		t.Fatalf("preview room A failed: %s", msg)
	}
	roomAPID := prevOut["profile_id"].(string)
	roomAEpoch := int64(prevOut["epoch"].(float64))

	// Create Room B containing a node with the EXACT SAME ID "n-shared" and activate Room B
	created, msg, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "隔离B机房", "copy_current": true})
	if isErr {
		t.Fatalf("create Room B failed: %s", msg)
	}
	roomBID := created["profile"].(map[string]interface{})["id"].(string)
	if _, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": roomBID, "confirm": true}); isErr {
		t.Fatalf("activate Room B failed: %s", msg)
	}

	// Attempt cleanup using Room A's preview profile_id/epoch: MUST reject inside ProfileWork and keep Room B's n-shared!
	if _, errMsg, isErr := callJSON(t, ts, "cleanup_devices", map[string]interface{}{
		"profile_id": roomAPID,
		"epoch":      roomAEpoch,
		"ids":        []string{"n-shared"},
		"confirm":    true,
	}); !isErr || !strings.Contains(errMsg, "当前激活机房") {
		t.Fatalf("expected cross-room cleanup rejection, got isErr=%v msg=%s", isErr, errMsg)
	}
	curTopo, _ := s.Topo.Snapshot()
	if curTopo.FindNode("n-shared") == nil {
		t.Fatal("Room B node n-shared was deleted despite room mismatch")
	}
}

func TestMCP_PluginsReadOnlySanitized(t *testing.T) {
	s, ts := newTestServer(t)

	// Enable ReadOnly mode: get_plugins must still work
	c := s.Cfg.Get()
	c.MCP.ReadOnly = true
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	plugDir := filepath.Join(s.BaseDir, "plugins")
	pOne := filepath.Join(plugDir, "notify-demo")
	if err := os.MkdirAll(pOne, 0o755); err != nil {
		t.Fatal(err)
	}
	// Manifest intentionally embeds secrets, userinfo URLs without query, query URLs, api-key KV, and paths in metadata
	manifest := fmt.Sprintf(`id: notify-demo
name: "Demo k9 https://adminuser:demo-pw-noq@example.com/path"
version: 1.0.0
description: "Uses api-key=mock-hyphen-apikey and %s and https://example.com/hook?tok=qsec99"
author: "ops mock-super-secret"
homepage: "https://u1:demo-home-pw@portal.example.com/home"
api_version: 1
command: ["/bin/sh", "run.sh"]
hooks: ["notify"]
tools:
  - name: query_ext
    title: "Query mock-super-secret"
    description: "Calls https://user:demo-tool-pw@api.example.com/v1"
settings:
  - key: api_secret
    label: "Secret Key k9"
    type: secret
    required: true
  - key: api_key
    label: "API Key"
    type: string
  - key: short_pin
    label: "Short PIN"
    type: secret
  - key: auth_url
    label: "Userinfo URL without query"
    type: string
  - key: webhook_url
    label: "Webhook URL with query"
    type: string
  - key: mode
    label: Mode
    type: string
    default: fast
`, pOne)
	if err := os.WriteFile(filepath.Join(pOne, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// Run outputs bare token, password, and path in both error JSON and stderr
	script := "#!/bin/sh\n" +
		"echo 'stderr bare token=mock-bare-tok https://user:demo-stderr-pw@host/p' >&2\n" +
		"echo '{\"ok\":false,\"message\":\"msg bare mock-super-secret\",\"error\":\"failed https://example.com/hook?secret=mock-super-secret rawtoken=mock-bare-tok from " + pOne + "\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(pOne, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	pm := plugin.New(plugDir, filepath.Join(s.BaseDir, "data"))
	t.Cleanup(pm.Close)
	if err := pm.UpdateSettings("notify-demo", map[string]interface{}{
		"api_secret":  "mock-super-secret",
		"api_key":     "mock-hyphen-apikey",
		"short_pin":   "k9",
		"auth_url":    "https://adminuser:demo-pw-noq@example.com/hook",
		"webhook_url": "https://example.com/webhook?token=mock-url-token",
		"mode":        "safe",
	}); err != nil {
		t.Fatal(err)
	}
	if err := pm.SetEnabled("notify-demo", true); err != nil {
		t.Fatal(err)
	}
	_, _ = pm.Test("notify-demo", "notify", "", nil)
	s.Plugins = pm

	out, text, isErr := callJSON(t, ts, "get_plugins", map[string]interface{}{"runs_limit": 10})
	if isErr {
		t.Fatalf("get_plugins failed in ReadOnly mode: %s", text)
	}

	forbidden := []string{
		"mock-super-secret", "mock-hyphen-apikey", "demo-pw-noq", "mock-url-token",
		"demo-home-pw", "demo-tool-pw", "qsec99", "mock-bare-tok", "demo-stderr-pw", "k9",
		plugDir, s.BaseDir, `"command"`, `"dir"`, `"stderr"`, `"message"`,
	}
	for _, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Fatalf("get_plugins leaked forbidden content %q in:\n%s", needle, text)
		}
	}

	pluginsList := out["plugins"].([]interface{})
	if len(pluginsList) != 1 {
		t.Fatalf("expected 1 plugin, got %v", pluginsList)
	}
	plugObj := pluginsList[0].(map[string]interface{})
	// Structured LastRun summary exists without free-text error/stderr/message
	lastRun := plugObj["last_run"].(map[string]interface{})
	if lastRun["has_error"] != true || lastRun["ok"] != false || lastRun["summary"] == "" {
		t.Fatalf("unexpected structured last_run: %v", lastRun)
	}
	if _, hasErr := lastRun["error"]; hasErr {
		t.Fatalf("last_run must not expose free-text error field: %v", lastRun)
	}

	// Verify sensitive keys (including api_key, short secret, and userinfo URL) are all masked
	settings := plugObj["settings"].([]interface{})
	maskedKeys := map[string]bool{}
	for _, rawS := range settings {
		it := rawS.(map[string]interface{})
		if it["masked"] == true {
			maskedKeys[it["key"].(string)] = true
			if _, hasVal := it["value"]; hasVal {
				t.Fatalf("masked setting %v still has value", it)
			}
		}
	}
	for _, want := range []string{"api_secret", "api_key", "short_pin", "auth_url", "webhook_url"} {
		if !maskedKeys[want] {
			t.Fatalf("expected setting %q to be masked, masked=%v", want, maskedKeys)
		}
	}
	if _, _, isErr := callJSON(t, ts, "get_plugins", map[string]interface{}{"id": "no-such-plugin"}); !isErr {
		t.Fatal("missing plugin id should return error")
	}
}

func TestMCP_AIOptimizeAndUndo(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	// Prepare second room for epoch/profile mismatch verification
	created, _, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "备用机房", "copy_current": true})
	if isErr {
		t.Fatal("create_profile failed")
	}
	otherID := created["profile"].(map[string]interface{})["id"].(string)

	reply := `{
  "summary": "建议将 n1 分类为服务器并改名及开启告警",
  "classify": [{"category": "服务器", "ids": ["n1"], "reason": "测试分类"}],
  "config": [
    {"id": "n1", "action": "rename", "name": "核心Web", "reason": "规范命名"},
    {"id": "n1", "action": "alert_on", "reason": "关键节点开启告警"}
  ],
  "check": []
}`
	llmSrv, _ := fakeOptLLM(t, reply)
	cfg := s.Cfg.Get()
	cfg.AI.Enabled = true
	cfg.AI.BaseURL = llmSrv.URL
	cfg.AI.APIKey = "sk-test-opt"
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}

	genOut, msg, isErr := callJSON(t, ts, "generate_ai_optimize", map[string]interface{}{})
	if isErr {
		t.Fatalf("generate_ai_optimize failed: %s", msg)
	}
	pid := genOut["profile_id"].(string)
	epoch := int64(genOut["epoch"].(float64))

	// Topology must remain unchanged after generate_ai_optimize
	curTopo, _ := s.Topo.Snapshot()
	if curTopo.FindNode("n1").Name != "节点1" {
		t.Fatal("generate_ai_optimize must not mutate topology")
	}

	classifyPayload := []map[string]string{{"id": "n1", "category": "服务器"}}
	cfgPayload := genOut["config"]

	// Default dry_run=true previews without writing
	dryOut, msg, isErr := callJSON(t, ts, "apply_ai_optimize", map[string]interface{}{
		"profile_id": pid,
		"epoch":      epoch,
		"classify":   classifyPayload,
		"config":     cfgPayload,
	})
	if isErr || dryOut["dry_run"] != true || int(dryOut["changed"].(float64)) != 1 {
		t.Fatalf("dry_run apply unexpected: err=%v msg=%s out=%v", isErr, msg, dryOut)
	}
	curTopo, _ = s.Topo.Snapshot()
	if curTopo.FindNode("n1").Name != "节点1" {
		t.Fatal("dry_run=true mutated topology")
	}

	// dry_run=false without confirm=true rejected
	if _, _, isErr := callJSON(t, ts, "apply_ai_optimize", map[string]interface{}{
		"profile_id": pid, "epoch": epoch, "classify": classifyPayload, "dry_run": false,
	}); !isErr {
		t.Fatal("apply without confirm=true must fail")
	}

	// Apply with dry_run=false and confirm=true
	applyOut, msg, isErr := callJSON(t, ts, "apply_ai_optimize", map[string]interface{}{
		"profile_id": pid,
		"epoch":      epoch,
		"classify":   classifyPayload,
		"config":     cfgPayload,
		"dry_run":    false,
		"confirm":    true,
	})
	if isErr || applyOut["dry_run"] != false || int(applyOut["changed"].(float64)) != 1 {
		t.Fatalf("apply failed: err=%v msg=%s out=%v", isErr, msg, applyOut)
	}
	curTopo, _ = s.Topo.Snapshot()
	n1 := curTopo.FindNode("n1")
	if n1.Name != "核心Web" || !n1.Monitor.Alert {
		t.Fatalf("apply did not update n1 as expected: %+v", n1)
	}

	// Switch to second room: applying stale profile_id/epoch MUST fail with conflict
	if _, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": otherID, "confirm": true}); isErr {
		t.Fatalf("activate otherID failed: %s", msg)
	}
	if _, msg, isErr := callJSON(t, ts, "apply_ai_optimize", map[string]interface{}{
		"profile_id": pid, "epoch": epoch, "classify": classifyPayload, "dry_run": false, "confirm": true,
	}); !isErr || !strings.Contains(msg, "已变化") {
		t.Fatalf("stale profile_id/epoch should fail: err=%v msg=%s", isErr, msg)
	}

	// Switch back to default room -> new epoch, so stale epoch still rejected, fresh epoch works for undo
	actBack, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": pid, "confirm": true})
	if isErr {
		t.Fatalf("activate back failed: %s", msg)
	}
	newEpoch := int64(actBack["epoch"].(float64))
	if _, _, isErr := callJSON(t, ts, "undo_ai_optimize", map[string]interface{}{
		"profile_id": pid, "epoch": epoch, "restore": applyOut["undo"], "confirm": true,
	}); !isErr {
		t.Fatal("undo with stale epoch must fail")
	}
	if _, _, isErr := callJSON(t, ts, "undo_ai_optimize", map[string]interface{}{
		"profile_id": pid, "epoch": newEpoch, "restore": applyOut["undo"], "confirm": false,
	}); !isErr {
		t.Fatal("undo without confirm=true must fail")
	}
	if _, msg, isErr := callJSON(t, ts, "undo_ai_optimize", map[string]interface{}{
		"profile_id": pid, "epoch": newEpoch, "restore": applyOut["undo"], "confirm": true,
	}); isErr {
		t.Fatalf("undo_ai_optimize failed: %s", msg)
	}
	curTopo, _ = s.Topo.Snapshot()
	if curTopo.FindNode("n1").Name != "节点1" || curTopo.FindNode("n1").Monitor.Alert {
		t.Fatalf("undo did not restore n1: %+v", curTopo.FindNode("n1"))
	}
}

func TestMCP_RoomBindingMismatchNoSideEffects(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	// Create Room B and keep it active
	created, msg, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "机房B", "copy_current": true})
	if isErr {
		t.Fatalf("create_profile failed: %s", msg)
	}
	roomBID := created["profile"].(map[string]interface{})["id"].(string)
	actOut, msg, isErr := callJSON(t, ts, "activate_profile", map[string]interface{}{"id": roomBID, "confirm": true})
	if isErr {
		t.Fatalf("activate Room B failed: %s", msg)
	}
	roomBEpoch := int64(actOut["epoch"].(float64))
	staleEpoch := roomBEpoch - 1

	startLocal := time.Now().Add(10 * time.Minute).Format("2006-01-02T15:04")
	endLocal := time.Now().Add(2 * time.Hour).Format("2006-01-02T15:04")

	// Mismatched profile_id and stale epoch MUST fail across active-room tools without modifying Room B
	badCalls := []struct {
		name string
		args map[string]interface{}
	}{
		{"list_maintenance", map[string]interface{}{"profile_id": "default"}},
		{"list_maintenance", map[string]interface{}{"profile_id": roomBID, "epoch": staleEpoch}},
		{"create_maintenance", map[string]interface{}{"profile_id": "default", "scope": "site", "start": startLocal, "end": endLocal, "reason": "跨机房测试"}},
		{"create_maintenance", map[string]interface{}{"profile_id": roomBID, "epoch": staleEpoch, "scope": "site", "start": startLocal, "end": endLocal, "reason": "旧epoch测试"}},
		{"cancel_maintenance", map[string]interface{}{"profile_id": "default", "id": "nonexistent"}},
		{"list_reports", map[string]interface{}{"profile_id": "default"}},
		{"get_report", map[string]interface{}{"profile_id": "default", "id": "20261114-100000"}},
		{"delete_report", map[string]interface{}{"profile_id": "default", "id": "20261114-100000"}},
		{"set_device_locked", map[string]interface{}{"profile_id": "default", "id": "n1", "locked": true}},
		{"set_device_locked", map[string]interface{}{"profile_id": roomBID, "epoch": staleEpoch, "id": "n1", "locked": true}},
		{"generate_health_report", map[string]interface{}{"profile_id": "default", "range": "24h"}},
		{"generate_ai_report", map[string]interface{}{"profile_id": "default", "range": "24h"}},
		{"generate_ai_optimize", map[string]interface{}{"profile_id": "default"}},
	}
	for _, tc := range badCalls {
		if _, isErr := call(t, ts, tc.name, tc.args); !isErr {
			t.Fatalf("%s(%v) should fail on profile/epoch mismatch", tc.name, tc.args)
		}
	}

	// Verify Room B remained completely untouched
	curTopo, _ := s.Topo.Snapshot()
	if curTopo.FindNode("n1").Locked {
		t.Fatal("Room B node n1 was unexpectedly locked")
	}
	if wins := s.Alerts.MaintenanceWindows(time.Now()); len(wins) != 0 {
		t.Fatalf("Room B maintenance windows leaked: %+v", wins)
	}
	if reps := s.listReportSummaries(10); len(reps) != 0 {
		t.Fatalf("Room B reports should be empty, got %+v", reps)
	}
}

func TestMCP_AIInFlightRoomSwitchSafety(t *testing.T) {
	s, ts := newProfileTestServerWithProbe(t, nil)

	// Prepare Room B with distinct site name
	created, msg, isErr := callJSON(t, ts, "create_profile", map[string]interface{}{"name": "隔离B机房", "copy_current": false})
	if isErr {
		t.Fatalf("create Room B failed: %s", msg)
	}
	roomBID := created["profile"].(map[string]interface{})["id"].(string)

	// Blocking fake LLM: signals when request arrives and waits until test switches active profile
	var reqStarted chan struct{}
	var releaseLLM chan struct{}
	var llmReply string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqStarted != nil {
			close(reqStarted)
		}
		if releaseLLM != nil {
			<-releaseLLM
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": llmReply}}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(llmSrv.Close)

	cfg := s.Cfg.Get()
	cfg.AI.Enabled = true
	cfg.AI.BaseURL = llmSrv.URL
	cfg.AI.APIKey = "mock-llm-key"
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}

	// 1. Mid-flight room switch during generate_ai_report must NOT save into Room B
	reqStarted = make(chan struct{})
	releaseLLM = make(chan struct{})
	llmReply = "# 主测试机房 网络运维日报\n\n健康评级：良。"

	errCh := make(chan string, 1)
	go func() {
		out, _, _, _, _, _, _, err := s.generateAIReportWithBinding(context.Background(), "default", nil, "24h", "", "MCP/test", nil)
		if err != nil {
			if out != nil {
				errCh <- "unexpected non-nil report on epoch error"
				return
			}
			errCh <- ""
			return
		}
		errCh <- "expected epoch switch error, succeeded instead"
	}()
	<-reqStarted
	if err := s.Profiles.Activate(roomBID); err != nil {
		close(releaseLLM)
		t.Fatal(err)
	}
	close(releaseLLM)
	if failMsg := <-errCh; failMsg != "" {
		t.Fatal(failMsg)
	}
	if reps := s.listReportSummaries(10); len(reps) != 0 {
		t.Fatalf("Room B must NOT receive Room A's in-flight AI report: %+v", reps)
	}

	// Switch back to default for test 2
	if err := s.Profiles.Activate("default"); err != nil {
		t.Fatal(err)
	}

	// 2. Mid-flight room switch during generate_ai_optimize must fail & never mix Room B nodes
	reqStarted = make(chan struct{})
	releaseLLM = make(chan struct{})
	llmReply = `{"summary":"test","classify":[],"config":[],"check":[]}`

	optErrCh := make(chan error, 1)
	go func() {
		_, _, _, _, _, err := s.generateOptimizeSuggestionsWithBinding(context.Background(), "default", nil, "MCP/test", nil)
		optErrCh <- err
	}()
	<-reqStarted
	if err := s.Profiles.Activate(roomBID); err != nil {
		close(releaseLLM)
		t.Fatal(err)
	}
	close(releaseLLM)
	if err := <-optErrCh; err == nil {
		t.Fatal("generateOptimizeSuggestionsWithBinding must fail when room switched mid-call")
	}

	// 3. aiExecCallsInCtx must check epoch inside ProfileWork BEFORE executing read-only AI tool
	if err := s.Profiles.Activate("default"); err != nil {
		t.Fatal(err)
	}
	boundID, boundEp := s.activeProfileState()
	boundCtx := s.withBoundProfileEpoch(context.Background(), boundID, boundEp)
	if err := s.Profiles.Activate(roomBID); err != nil {
		t.Fatal(err)
	}
	var tc llm.ToolCall
	tc.ID = "call_1"
	tc.Type = "function"
	tc.Function.Name = "get_overview"
	tc.Function.Arguments = "{}"
	msgs, err := s.aiExecCallsInCtx(boundCtx, []string{"get_overview"}, []llm.ToolCall{tc}, "test", "", func(map[string]interface{}) {})
	if err == nil || len(msgs) != 0 {
		t.Fatalf("aiExecCallsInCtx must abort before reading new room data, got msgs=%v err=%v", msgs, err)
	}
	_ = ts
}
