package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/monitor"
)

// Exercises the production router and auth wrapper, not direct handler calls.
func TestOperationsHTTPAuthMaintenanceAndReports(t *testing.T) {
	s, ts := newTestServer(t)
	topo, _ := s.Topo.Snapshot()
	s.Alerts.SetTopology(topo)
	roomA := filepath.Join(t.TempDir(), "room-a")
	roomB := filepath.Join(t.TempDir(), "room-b")
	if err := s.Alerts.SwitchDataDir(roomA); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{"GET", "/api/maintenance"}, {"POST", "/api/maintenance"}, {"DELETE", "/api/maintenance/x"}, {"POST", "/api/health-report"}, {"GET", "/api/ai/reports"}} {
		req, _ := http.NewRequest(route.method, ts.URL+route.path, strings.NewReader(`{}`))
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s: %d", route.method, route.path, resp.StatusCode)
		}
	}
	start, end := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	body := fmtJSON(map[string]interface{}{"scope": "site", "start": start, "end": end, "reason": "交接维护"})
	var window alert.MaintenanceWindow
	if err := json.Unmarshal(apiRequest(t, ts, "POST", "/api/maintenance", body, 200), &window); err != nil {
		t.Fatal(err)
	}
	if window.ID == "" {
		t.Fatal("missing maintenance id")
	}
	var windows []alert.MaintenanceWindow
	if err := json.Unmarshal(apiRequest(t, ts, "GET", "/api/maintenance", "", 200), &windows); err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].Status != "active" {
		t.Fatalf("windows: %+v", windows)
	}
	apiRequest(t, ts, "POST", "/api/maintenance", `{"scope":"devices","target_ids":["missing"],"start":"`+start.Format(time.RFC3339)+`","end":"`+end.Format(time.RFC3339)+`"}`, 400)
	apiRequest(t, ts, "POST", "/api/maintenance", `{"scope":"site","start":"`+end.Format(time.RFC3339)+`","end":"`+start.Format(time.RFC3339)+`"}`, 400)
	apiRequest(t, ts, "DELETE", "/api/maintenance/missing", "", 404)
	if _, err := os.Stat(filepath.Join(roomA, "maintenance.json")); err != nil {
		t.Fatal(err)
	}
	// Reload as after restart and inspect through HTTP again.
	reloaded := alert.NewEngine(s.Cfg.Get, s.Mon, filepath.Join(roomA, "alerts.jsonl"))
	if len(reloaded.MaintenanceWindows(time.Now())) != 1 {
		t.Fatal("restart did not load maintenance")
	}
	if err := s.Alerts.SwitchDataDir(roomA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(apiRequest(t, ts, "GET", "/api/maintenance", "", 200), &windows); err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].ID != window.ID {
		t.Fatal("maintenance lost on restart")
	}
	apiRequest(t, ts, "DELETE", "/api/maintenance/"+window.ID, "", 200)
	apiRequest(t, ts, "DELETE", "/api/maintenance/"+window.ID, "", 200)
	if err := json.Unmarshal(apiRequest(t, ts, "GET", "/api/maintenance", "", 200), &windows); err != nil {
		t.Fatal(err)
	}
	if windows[0].Status != "cancelled" {
		t.Fatal("cancel not reflected")
	}
	apiRequest(t, ts, "POST", "/api/health-report", `{"range":"bogus"}`, 400)
	var report Report
	raw := apiRequest(t, ts, "POST", "/api/health-report", `{"range":"7d"}`, 200)
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.ID == "" || report.Model != "deterministic" || !strings.Contains(string(raw), `"availability_pct":null`) {
		t.Fatalf("bad report %s", raw)
	}
	if _, err := os.Stat(filepath.Join(roomA, "reports", report.ID+".json")); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, ts, "GET", "/api/ai/reports/"+report.ID, "", 200)
	req, _ := http.NewRequest("GET", ts.URL+"/api/ai/reports/"+report.ID+".md", nil)
	req.SetBasicAuth("", testPassword)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	md, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(string(md), "netPing 程序统计") || strings.Contains(string(md), "由 netPing AI") {
		t.Fatalf("download: %d %s", resp.StatusCode, md)
	}
	// Switching only the data owner here isolates artifacts; the core profile tests
	// exercise the larger monitor/config activation transaction.
	if err := s.Alerts.SwitchDataDir(roomB); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(apiRequest(t, ts, "GET", "/api/maintenance", "", 200))); got != "[]" {
		t.Fatal("maintenance leaked", got)
	}
	if got := strings.TrimSpace(string(apiRequest(t, ts, "GET", "/api/ai/reports", "", 200))); got != "[]" {
		t.Fatal("report leaked", got)
	}
	apiRequest(t, ts, "GET", "/api/ai/reports/"+report.ID, "", 404)
	apiRequest(t, ts, "DELETE", "/api/maintenance/"+window.ID, "", 404)
	if err := s.Alerts.SwitchDataDir(roomA); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, ts, "GET", "/api/ai/reports/"+report.ID, "", 200)
	apiRequest(t, ts, "DELETE", "/api/ai/reports/"+report.ID, "", 200)
	apiRequest(t, ts, "GET", "/api/ai/reports/"+report.ID, "", 404)
}

// The existing manager lifecycle test checks empty B lists, but not B writes,
// alert history, or restoration of all three artifact types after A/B/A/B.
func TestOperationsProfileActivationRoundTrip(t *testing.T) {
	s, ts := newProfileTestServer(t)
	topo, _ := s.Topo.Snapshot()
	topo.FindNode("n1").Monitor.Alert = true
	if err := s.Topo.Replace(topo); err != nil {
		t.Fatal(err)
	}
	s.Alerts.SetTopology(topo)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(apiReqProfile(t, ts, "POST", "/api/profiles", `{"name":"运维B","copy_current":true}`, "default", 200), &created); err != nil || created.ID == "" {
		t.Fatalf("create B: %+v %v", created, err)
	}
	type artifacts struct {
		window alert.MaintenanceWindow
		report Report
	}
	createArtifacts := func(profile, rg, reason string, failures int) artifacts {
		t.Helper()
		var a artifacts
		now := time.Now()
		body := fmtJSON(map[string]interface{}{"scope": "site", "start": now.Add(-time.Minute), "end": now.Add(time.Hour), "reason": reason})
		if err := json.Unmarshal(apiReqProfile(t, ts, "POST", "/api/maintenance", body, profile, 200), &a.window); err != nil {
			t.Fatal(err)
		}
		s.Alerts.Evaluate(now, []monitor.Status{{ID: "n1", Kind: "node", IP: "10.10.0.10", ConsecFail: failures}})
		raw := apiReqProfile(t, ts, "POST", "/api/health-report", fmtJSON(map[string]string{"range": rg}), profile, 200)
		if err := json.Unmarshal(raw, &a.report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"availability_pct":null`) || !strings.Contains(string(raw), `"loss_pct":null`) || !strings.Contains(string(raw), `"avg_ms":null`) {
			t.Fatalf("no-sample report gave numeric health: %s", raw)
		}
		return a
	}
	checkArtifacts := func(profile string, a artifacts) {
		t.Helper()
		var windows []alert.MaintenanceWindow
		if err := json.Unmarshal(apiReqProfile(t, ts, "GET", "/api/maintenance", "", profile, 200), &windows); err != nil {
			t.Fatal(err)
		}
		if len(windows) != 1 || windows[0].ID != a.window.ID || windows[0].Status != "active" {
			t.Fatalf("%s maintenance not isolated: %+v", profile, windows)
		}
		var events []alert.Event
		if err := json.Unmarshal(apiReqProfile(t, ts, "GET", "/api/alerts", "", profile, 200), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].MaintenanceID != a.window.ID || events[0].Suppressed != "maintenance" || events[0].Sent {
			t.Fatalf("%s alert history not isolated: %+v", profile, events)
		}
		var reports []Report
		if err := json.Unmarshal(apiReqProfile(t, ts, "GET", "/api/ai/reports", "", profile, 200), &reports); err != nil {
			t.Fatal(err)
		}
		if len(reports) != 1 || reports[0].ID != a.report.ID || reports[0].Range != a.report.Range {
			t.Fatalf("%s reports not isolated: %+v", profile, reports)
		}
		md := apiReqProfile(t, ts, "GET", "/api/ai/reports/"+a.report.ID+".md", "", profile, 200)
		if !strings.Contains(string(md), a.report.Title) || !strings.Contains(string(md), "netPing 程序统计") {
			t.Fatalf("%s report download changed: %s", profile, md)
		}
	}
	a := createArtifacts("default", "24h", "A维护", 5)
	checkArtifacts("default", a)
	apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID, `{"action":"activate"}`, "default", 200)
	for _, path := range []string{"/api/maintenance", "/api/alerts", "/api/ai/reports"} {
		if raw := apiReqProfile(t, ts, "GET", path, "", created.ID, 200); strings.TrimSpace(string(raw)) != "[]" {
			t.Fatalf("A leaked into B %s: %s", path, raw)
		}
	}
	apiReqProfile(t, ts, "GET", "/api/ai/reports/"+a.report.ID+".md", "", created.ID, 404)
	apiReqProfile(t, ts, "DELETE", "/api/maintenance/"+a.window.ID, "", created.ID, 404)
	b := createArtifacts(created.ID, "7d", "B维护", 9)
	checkArtifacts(created.ID, b)
	apiReqProfile(t, ts, "POST", "/api/profiles/default", `{"action":"activate"}`, created.ID, 200)
	checkArtifacts("default", a)
	// A cancellation must not touch B's active maintenance, even with identical target IDs.
	apiReqProfile(t, ts, "DELETE", "/api/maintenance/"+a.window.ID, "", "default", 200)
	apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID, `{"action":"activate"}`, "default", 200)
	checkArtifacts(created.ID, b)
}
