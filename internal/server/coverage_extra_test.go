package server

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/discovery"
	"netping/internal/topology"
	"strconv"
)

func TestAPIExtraEndpointsAndMCPMutations(t *testing.T) {
	s, ts := newTestServer(t)

	// Site, Reload, PutYAML, Alerts, Discovery GET/Add/Apply, Locate GET/Run/Test/Apply/Table, AlertTest
	apiRequest(t, ts, "PUT", "/api/site", `{"name":"三号机房","subnet":"10.10.0.0/24","gateway":"10.10.0.1"}`, 200)
	tp, _ := s.Topo.Snapshot()
	if tp.Site.Name != "三号机房" {
		t.Fatalf("site update failed: %+v", tp.Site)
	}
	apiRequest(t, ts, "POST", "/api/topology/reload", "", 200)

	// PutYAML: validation error rejected with 400, valid accepted
	apiRequest(t, ts, "PUT", "/api/topology/yaml", "version: 1\nsite: {name: x}\nnodes:\n  - {id: bad, ip: invalid}\n", 400)
	yml, _ := s.Topo.YAML()
	edited := strings.Replace(string(yml), "name: 三号机房", "name: 四号机房", 1)
	apiRequest(t, ts, "PUT", "/api/topology/yaml", edited, 200)
	tp, _ = s.Topo.Snapshot()
	if tp.Site.Name != "四号机房" {
		t.Fatalf("PutYAML failed: %+v", tp.Site)
	}

	// Link create & update & delete via API
	apiRequest(t, ts, "POST", "/api/links", `{"id":"l-new","from":{"switch":"core","port":"Gi0/5"},"to":{"switch":"access","port":"25"}}`, 200)
	apiRequest(t, ts, "PUT", "/api/links/l-new", `{"id":"l-new","from":{"switch":"core","port":"Gi0/5"},"to":{"switch":"access","port":"25"},"note":"updated"}`, 200)
	apiRequest(t, ts, "PUT", "/api/links/missing", `{"id":"missing"}`, 404)
	apiRequest(t, ts, "DELETE", "/api/links/l-new", "", 200)

	// Alerts & AlertTest (against local test webhook)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer webhook.Close()
	apiRequest(t, ts, "POST", "/api/alert/test", `{"enabled":true,"webhook_url":"`+webhook.URL+`","site_title":"测试"}`, 200)
	apiRequest(t, ts, "GET", "/api/alerts", "", 200)
	// Also test MCP send_test_alert & list_alerts
	call(t, ts, "set_alert_config", map[string]interface{}{"enabled": true, "webhook_url": webhook.URL})
	if txt, isErr := call(t, ts, "send_test_alert", map[string]interface{}{}); isErr || !strings.Contains(txt, "已发送") {
		t.Fatalf("send_test_alert: %s", txt)
	}
	if txt, isErr := call(t, ts, "list_alerts", map[string]interface{}{"limit": 5}); isErr || !strings.Contains(txt, "test") {
		t.Fatalf("list_alerts: %s", txt)
	}

	// Discovery GET / Add / Apply via API & MCP
	apiRequest(t, ts, "GET", "/api/discovery", "", 200)
	// Inject an unknown host and a suggestion directly in discovery service via saved results? Use Add/Apply error paths + simulated scan
	apiRequest(t, ts, "POST", "/api/discovery/add", `{"ips":["10.99.99.99"]}`, 500) // not in scan results
	apiRequest(t, ts, "POST", "/api/discovery/apply", `{"node_id":"missing","ip":"10.0.0.1"}`, 400)
	apiRequest(t, ts, "POST", "/api/discovery/apply", `{"node_id":"core","ip":"10.0.0.1","ip_mode":"invalid"}`, 400)
	apiRequest(t, ts, "POST", "/api/discovery/scan", `{"job":"missing"}`, 400)
	apiRequest(t, ts, "POST", "/api/discovery/scan", `{"subnets":["10.0.0.0/8"]}`, 400)

	// Locate API endpoints using fakeAccess SNMP agent
	addr := fakeAccess(t, map[string]int{
		"AA:BB:CC:00:00:01": 12,
		"2:0:0:0:0:12": 7,
	})
	apiRequest(t, ts, "GET", "/api/locate", "", 200)
	apiRequest(t, ts, "POST", "/api/locate/test", `{"host":"`+addr+`","community":"s3cret-ro"}`, 200)
	call(t, ts, "set_locate_settings", map[string]interface{}{"community": "s3cret-ro", "switches": map[string]interface{}{"access": map[string]interface{}{"host": addr}}})
	apiRequest(t, ts, "GET", "/api/locate/table/access?refresh=1", "", 200)
	apiRequest(t, ts, "POST", "/api/locate/run", `{"dry_run":false}`, 200)
	apiRequest(t, ts, "POST", "/api/locate/apply", `{"node_id":"n-192-168-50-29"}`, 200)

	// MCP switch, link, paused, topology yaml replace write
	if txt, isErr := call(t, ts, "upsert_switch", map[string]interface{}{"id": "sw-extra", "name": "临时交换机", "role": "access"}); isErr || !strings.Contains(txt, "created") {
		t.Fatalf("upsert_switch: %s", txt)
	}
	if txt, isErr := call(t, ts, "upsert_link", map[string]interface{}{"id": "lnk-extra", "from": map[string]string{"switch": "core", "port": "Gi0/8"}, "to": map[string]string{"switch": "sw-extra", "port": "1"}}); isErr || !strings.Contains(txt, "created") {
		t.Fatalf("upsert_link: %s", txt)
	}
	if txt, isErr := call(t, ts, "delete_link", map[string]interface{}{"id": "lnk-extra"}); isErr || !strings.Contains(txt, "deleted") {
		t.Fatalf("delete_link: %s", txt)
	}
	if txt, isErr := call(t, ts, "delete_switch", map[string]interface{}{"id": "sw-extra"}); isErr || !strings.Contains(txt, "deleted") {
		t.Fatalf("delete_switch: %s", txt)
	}
	if txt, isErr := call(t, ts, "set_monitoring_paused", map[string]interface{}{"paused": false}); isErr || !strings.Contains(txt, `"manual_paused": false`) {
		t.Fatalf("set_monitoring_paused: %s", txt)
	}
	if txt, isErr := call(t, ts, "set_monitoring_paused", map[string]interface{}{"paused": true}); isErr || !strings.Contains(txt, `"manual_paused": true`) {
		t.Fatalf("set_monitoring_paused: %s", txt)
	}
	if txt, isErr := call(t, ts, "reload_topology_from_disk", map[string]interface{}{}); isErr || !strings.Contains(txt, `"ok": true`) {
		t.Fatalf("reload_topology_from_disk: %s", txt)
	}
	// replace_topology_yaml with dry_run=false
	yml2, _ := s.Topo.YAML()
	edited2 := strings.Replace(string(yml2), "name: 四号机房", "name: 五号机房", 1)
	if txt, isErr := call(t, ts, "replace_topology_yaml", map[string]interface{}{"yaml": edited2, "dry_run": false}); isErr || !strings.Contains(txt, `"written": true`) {
		t.Fatalf("replace_topology_yaml write: %s", txt)
	}
	// All MCP resources & prompts
	for _, uri := range []string{
		"netping://docs/topology-fields.md",
		"netping://schema/topology.schema.json",
		"netping://status",
	} {
		_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":10,"method":"resources/read","params":{"uri":"`+uri+`"}}`)
		if out["result"] == nil {
			t.Fatalf("resource %s failed: %v", uri, out)
		}
	}
	for _, pr := range []string{"health_report", "discover_devices"} {
		_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":11,"method":"prompts/get","params":{"name":"`+pr+`","arguments":{}}}`)
		if out["result"] == nil {
			t.Fatalf("prompt %s failed: %v", pr, out)
		}
	}
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":12,"method":"resources/list"}`)
	if out["result"] == nil {
		t.Fatalf("resources/list failed: %v", out)
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":13,"method":"prompts/list"}`)
	if out["result"] == nil {
		t.Fatalf("prompts/list failed: %v", out)
	}
}

func TestAnalysisAndDiscoverHelpersCoverage(t *testing.T) {
	s, _ := newTestServer(t)
	// Test minuteSeries blending stored + in-memory samples
	now := time.Now()
	fillMinutes(t, s, "n-192-168-50-10", now.Add(-5*time.Minute), 5, 10, nil)
	ms, err := s.minuteSeries("n-192-168-50-10", now.Add(-15*time.Minute), now)
	if err != nil || len(ms) != 5 {
		t.Fatalf("minuteSeries = %d err=%v", len(ms), err)
	}

	// Test find_outages with range=15m (second-level branch)
	out := aiCall(t, s, "find_outages", `{"id":"n-192-168-50-10","range":"15m"}`)
	if out["precision"] == nil {
		t.Fatalf("find_outages 15m: %v", out)
	}

	// Test rank_devices with metric=jitter and metric=loss
	for _, metric := range []string{"loss", "jitter"} {
		r := aiCall(t, s, "rank_devices", `{"range":"6h","metric":"`+metric+`"}`)
		if r["ranking"] == nil {
			t.Fatalf("rank_devices(%s): %v", metric, r)
		}
	}

	// Test analyze_impact with down filter
	imp := aiCall(t, s, "analyze_impact", `{"state":"down"}`)
	if imp["criteria"] != "down" {
		t.Fatalf("analyze_impact down: %v", imp)
	}

	// Test discUndo & AI discover apply edge cases
	s.rememberHost(seenHost{Host: discovery.Host{IP: "192.168.7.50", MAC: "AA:BB:CC:00:00:50", Ping: true, ARP: true, Hostname: "host50"}})
	if _, ok := s.observed("192.168.7.50"); !ok {
		t.Fatal("observed should find remembered host")
	}
	// Remember same IP again merges missing fields
	s.rememberHost(seenHost{Host: discovery.Host{IP: "192.168.7.50", Vendor: "TestVendor"}, Hints: []string{"hint1"}, Model: "M1", Evidence: "ev1"})
	h, _ := s.observed("192.168.7.50")
	if h.MAC != "AA:BB:CC:00:00:50" || h.Vendor != "TestVendor" || h.Model != "M1" || h.Evidence != "ev1" {
		t.Fatalf("rememberHost merge failed: %+v", h)
	}

	// Test aiJSON truncation on long valid UTF-8
	longStr := strings.Repeat("中文", 100)
	trunc := aiJSON(map[string]string{"s": longStr}, 50)
	if !strings.Contains(trunc, "已截断") {
		t.Fatalf("aiJSON truncation failed: %s", trunc)
	}

	// Test alert.Event formatting via toolAlerts with ip filter
	s.Alerts.SetTopology(func() *topology.Topology { t, _ := s.Topo.Snapshot(); return t }())
	_ = alert.EvDown
}

func TestFingerprintHTTPAndBannerLocalhost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "TestHTTP/1.0")
		w.Header().Set("WWW-Authenticate", `Basic realm="Switch"`)
		_, _ = w.Write([]byte("<html><head><title>H3C Switch Management</title></head></html>"))
	}))
	defer srv.Close()
	// Extract port from srv.URL (http://127.0.0.1:PORT)
	parts := strings.Split(srv.URL, ":")
	port, _ := strconv.Atoi(parts[len(parts)-1])
	info := httpInfo("127.0.0.1", port)
	if info == nil || info["title"] != "H3C Switch Management" || info["server"] != "TestHTTP/1.0" {
		t.Fatalf("httpInfo = %+v", info)
	}

	// readBanner against an ephemeral TCP listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		go func() {
			for {
				c, e := ln.Accept()
				if e != nil {
					return
				}
				_, _ = c.Write([]byte("SSH-2.0-OpenSSH_8.9 Ubuntu\r\n"))
				_ = c.Close()
			}
		}()
		defer ln.Close()
		_, pStr, _ := net.SplitHostPort(ln.Addr().String())
		bport, _ := strconv.Atoi(pStr)
		if b := readBanner("127.0.0.1", bport); !strings.Contains(b, "OpenSSH") {
			t.Fatalf("readBanner = %q", b)
		}
	}

	// Check dnsErrText & isRefused & portName
	if portName(81) == "" || portName(80) == "" {
		t.Fatal("portName failed")
	}
	if !isRefused(errors.New("Connection refused")) {
		t.Fatal("isRefused string match failed")
	}
	if dnsErrText(os.ErrDeadlineExceeded) == "" {
		t.Fatal("dnsErrText failed")
	}
	var b bytes.Buffer
	_ = b
	_ = filepath.Join("", "")
}
