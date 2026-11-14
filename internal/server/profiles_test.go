package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/llm"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/profiles"
	"netping/internal/store"
	"netping/internal/topology"
)

func newProfileTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Get()
	c.Auth = config.Auth{Password: testPassword}
	c.AI.APIKey = "sk-global-test"
	c.MCP.Token = "mcp-global-test"
	c.Alert.Enabled = true
	c.Alert.WebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=default-secret"
	c.Locate.Community = "comm-default"
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	topo, err := topology.Open(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := topo.Replace(&topology.Topology{
		Version:  1,
		Site:     topology.Site{Name: "默认测试机房", Subnet: "10.10.0.0/24", Gateway: "10.10.0.1"},
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
			return "unreachable", "超时（>800ms）"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pm.Shutdown)

	s := &Server{
		Cfg: cfg, Topo: topo, Mon: mon, Alerts: al, Store: st, Disc: disc, Loc: loc, Profiles: pm,
		DocsMD: []byte("# docs"), SchemaJSON: []byte("{}"), BaseDir: dir,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/api/", s.routeAPI)
	ts := httptest.NewServer(s.withAuth(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

func apiReqProfile(t *testing.T, ts *httptest.Server, method, path, body, profileHdr string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	if profileHdr != "" {
		req.Header.Set("X-NetPing-Profile", profileHdr)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: want %d, got %d: %s", method, path, want, resp.StatusCode, data)
	}
	return data
}

func TestHTTPProfilesLifecyclePreviewRollbackAndIsolation(t *testing.T) {
	s, ts := newProfileTestServer(t)

	// 1. Meta & Topology carry active_profile
	metaRaw := apiReqProfile(t, ts, "GET", "/api/meta", "", "", 200)
	if !strings.Contains(string(metaRaw), `"active_profile":"default"`) {
		t.Fatalf("meta missing active_profile: %s", metaRaw)
	}
	topoRaw := apiReqProfile(t, ts, "GET", "/api/topology", "", "default", 200)
	if !strings.Contains(string(topoRaw), `"active_profile":"default"`) {
		t.Fatalf("topology missing active_profile: %s", topoRaw)
	}

	// 2. Maintenance & Health Report work in default room and isolate across rooms
	futureStart := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	futureEnd := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	apiReqProfile(t, ts, "POST", "/api/maintenance", `{"scope":"site","start":"`+futureStart+`","end":"`+futureEnd+`","reason":"default maint"}`, "default", 200)
	mListA := apiReqProfile(t, ts, "GET", "/api/maintenance", "", "default", 200)
	if !strings.Contains(string(mListA), "default maint") {
		t.Fatalf("expected maintenance window in default: %s", mListA)
	}
	apiReqProfile(t, ts, "POST", "/api/health-report", `{"range":"24h"}`, "default", 200)
	apiReqProfile(t, ts, "POST", "/api/reports/health", `{"range":"24h"}`, "default", 200)
	repListA := apiReqProfile(t, ts, "GET", "/api/ai/reports", "", "default", 200)
	if !strings.Contains(string(repListA), "机房健康日报") {
		t.Fatalf("expected health report in default: %s", repListA)
	}

	// SSE stream first frame carries consistent active_profile
	sseReq, _ := http.NewRequest("GET", ts.URL+"/api/stream", nil)
	sseReq.SetBasicAuth("", testPassword)
	sseCtx, cancelSSE := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSSE()
	sseReq = sseReq.WithContext(sseCtx)
	sseResp, err := ts.Client().Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(sseResp.Body)
	var sseFrame string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			sseFrame = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	sseResp.Body.Close()
	if !strings.Contains(sseFrame, `"active_profile":"default"`) {
		t.Fatalf("SSE initial frame missing active_profile: %s", sseFrame)
	}

	// 3. Create Room B (copy_current=true), check settings masking, update settings omitting secret preserves secret
	createResp := apiReqProfile(t, ts, "POST", "/api/profiles", `{"name":"分部机房","copy_current":true}`, "default", 200)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createResp, &created); err != nil || created.ID == "" {
		t.Fatalf("bad create response: %s", createResp)
	}

	// Cannot delete active default
	apiReqProfile(t, ts, "DELETE", "/api/profiles/default", "", "default", 400)

	// Settings GET masks secrets
	setGet := apiReqProfile(t, ts, "GET", "/api/profiles/"+created.ID+"/settings", "", "default", 200)
	if strings.Contains(string(setGet), "default-secret") || strings.Contains(string(setGet), "comm-default") {
		t.Fatalf("GET settings leaked raw secret: %s", setGet)
	}

	// Update Room B settings with new webhook & community
	setPut := apiReqProfile(t, ts, "PUT", "/api/profiles/"+created.ID+"/settings",
		`{"ping":{"interval_ms":2500},"alert":{"enabled":true,"webhook_url":"https://oapi.dingtalk.com/robot/send?access_token=b-secret"},"locate":{"community":"comm-b"}}`, "default", 200)
	if strings.Contains(string(setPut), "b-secret") || strings.Contains(string(setPut), "comm-b") {
		t.Fatalf("PUT settings leaked raw secret: %s", setPut)
	}

	// Partial update of ping preserves Room B's alert & locate secrets
	apiReqProfile(t, ts, "PUT", "/api/profiles/"+created.ID+"/settings", `{"ping":{"interval_ms":1800}}`, "default", 200)
	rsB, _, _, err := s.Profiles.GetSettings(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rsB.Locate.Community != "comm-b" || rsB.Alert.WebhookURL != "https://oapi.dingtalk.com/robot/send?access_token=b-secret" || rsB.Ping.IntervalMs != 1800 {
		t.Fatalf("partial settings update lost secrets or failed: %+v", rsB)
	}

	// 4. Preview Room B
	prevRaw := apiReqProfile(t, ts, "GET", "/api/profiles/"+created.ID+"/preview", "", "default", 200)
	var prev profiles.PreviewResult
	if err := json.Unmarshal(prevRaw, &prev); err != nil {
		t.Fatal(err)
	}
	if !prev.CanActivate || len(prev.ScanRanges) == 0 || len(prev.NotificationDestinations) == 0 || len(prev.Connectivity) == 0 {
		t.Fatalf("incomplete preview: %+v", prev)
	}
	if strings.Contains(string(prevRaw), "b-secret") {
		t.Fatalf("preview leaked secret: %s", prevRaw)
	}

	// 5. Activate Room B
	apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID, `{"action":"activate"}`, "default", 200)

	// Stale X-NetPing-Profile: default must get 409 Conflict with profile_mismatch!
	mismatch := apiReqProfile(t, ts, "POST", "/api/nodes", `{"name":"stale","ip":"10.10.0.99"}`, "default", 409)
	if !strings.Contains(string(mismatch), `"code":"profile_mismatch"`) || !strings.Contains(string(mismatch), created.ID) {
		t.Fatalf("expected profile_mismatch 409, got: %s", mismatch)
	}
	apiReqProfile(t, ts, "GET", "/api/alerts", "", "default", 409)
	apiReqProfile(t, ts, "GET", "/api/ai/reports", "", "default", 409)

	// Room B has empty maintenance & empty reports (isolated from default!)
	mListB := apiReqProfile(t, ts, "GET", "/api/maintenance", "", created.ID, 200)
	if strings.Contains(string(mListB), "default maint") {
		t.Fatalf("default maintenance leaked to room B: %s", mListB)
	}
	repListB := apiReqProfile(t, ts, "GET", "/api/ai/reports", "", created.ID, 200)
	if strings.TrimSpace(string(repListB)) != "[]" {
		t.Fatalf("default reports leaked to room B: %s", repListB)
	}

	// 6. Config version snapshot, inspect masking, and rollback on active Room B
	verResp := apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/versions", `{"note":"B基线"}`, created.ID, 200)
	var verCreated struct {
		Version profiles.VersionMeta `json:"version"`
	}
	if err := json.Unmarshal(verResp, &verCreated); err != nil {
		t.Fatal(err)
	}
	verDetail := apiReqProfile(t, ts, "GET", "/api/profiles/"+created.ID+"/versions/"+verCreated.Version.ID, "", created.ID, 200)
	if strings.Contains(string(verDetail), "b-secret") || strings.Contains(string(verDetail), "comm-b") {
		t.Fatalf("version detail leaked secret: %s", verDetail)
	}

	// Mutate active Room B topology & settings, then rollback via HTTP
	apiReqProfile(t, ts, "POST", "/api/nodes", `{"id":"extra","name":"临时节点","ip":"10.10.0.55"}`, created.ID, 200)
	apiReqProfile(t, ts, "PUT", "/api/profiles/"+created.ID+"/settings", `{"ping":{"interval_ms":4000}}`, created.ID, 200)

	rbResp := apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/versions/"+verCreated.Version.ID+"/rollback", `{}`, created.ID, 200)
	if !strings.Contains(string(rbResp), `"restored_version":"`+verCreated.Version.ID+`"`) {
		t.Fatalf("unexpected rollback response: %s", rbResp)
	}
	tpAfter, _ := s.Topo.Snapshot()
	if tpAfter.FindNode("extra") != nil || s.Cfg.Get().Ping.IntervalMs != 1800 {
		t.Fatalf("HTTP rollback did not restore topology & settings: node=%v interval=%d", tpAfter.FindNode("extra"), s.Cfg.Get().Ping.IntervalMs)
	}

	// Topology YAML export never leaks secrets
	yamlExport := apiReqProfile(t, ts, "GET", "/api/topology/yaml", "", created.ID, 200)
	for _, secret := range []string{"b-secret", "comm-b", "sk-global-test", "mcp-global-test", testPassword} {
		if strings.Contains(string(yamlExport), secret) {
			t.Fatalf("topology YAML export leaked secret %q", secret)
		}
	}

	// 7. AI epoch check rejects mid-flight tool call after profile switch
	ctx := s.withProfileEpoch(context.Background())
	if err := s.checkProfileEpoch(ctx); err != nil {
		t.Fatalf("unexpected epoch error before switch: %v", err)
	}
	// Switch back to default while AI session holds old Room B epoch
	apiReqProfile(t, ts, "POST", "/api/profiles/default", `{"action":"activate"}`, created.ID, 200)
	if err := s.checkProfileEpoch(ctx); err == nil {
		t.Fatal("expected checkProfileEpoch to fail after room switch")
	}
	var tc llm.ToolCall
	tc.ID = "c1"
	tc.Type = "function"
	tc.Function.Name = "get_overview"
	tc.Function.Arguments = "{}"
	msgs, err := s.aiExecCallsInCtx(ctx, aiToolNames, []llm.ToolCall{tc}, "test", "", func(map[string]interface{}) {})
	if err == nil || len(msgs) != 0 {
		t.Fatalf("expected aiExecCallsInCtx to reject old-epoch tool call, got msgs=%d err=%v", len(msgs), err)
	}

	// 8. Delete inactive Room B
	apiReqProfile(t, ts, "DELETE", "/api/profiles/"+created.ID, "", "default", 200)
}
