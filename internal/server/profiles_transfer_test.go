package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPProfileExportImport(t *testing.T) {
	s, ts := newProfileTestServer(t)

	// 导出（默认脱敏）：带下载文件名，不含凭据与全局配置。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/profiles/default/export", nil)
	req.SetBasicAuth("", testPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var masked map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&masked)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", resp.StatusCode, resp.Header)
	}
	rawMasked, _ := json.Marshal(masked)
	for _, secret := range []string{"default-secret", "comm-default", "sk-global-test", "mcp-global-test", testPassword} {
		if strings.Contains(string(rawMasked), secret) {
			t.Fatalf("masked export leaked %q", secret)
		}
	}
	if masked["format"] != "netpulse-room-config" || masked["secrets"] != "masked" {
		t.Fatalf("export header: %v", masked["format"])
	}
	full := apiReqProfile(t, ts, "GET", "/api/profiles/default/export?secrets=include", "", "", 200)
	if !strings.Contains(string(full), "default-secret") {
		t.Fatalf("include export should contain room secret")
	}

	// 预览导入为新机房：不创建任何东西。
	before := len(s.Profiles.List())
	prev := apiReqProfile(t, ts, "POST", "/api/profiles/import?dry_run=1", string(full), "default", 200)
	if !strings.Contains(string(prev), `"new_profile":true`) || len(s.Profiles.List()) != before {
		t.Fatalf("dry run created profile: %s", prev)
	}
	// 导入为新机房。
	out := apiReqProfile(t, ts, "POST", "/api/profiles/import?name="+"导入副本", string(full), "default", 200)
	var created struct {
		ID      string `json:"id"`
		Preview struct {
			TargetName string `json:"target_name"`
		} `json:"preview"`
		ActiveProfile string `json:"active_profile"`
	}
	_ = json.Unmarshal(out, &created)
	if created.ID == "" || created.ActiveProfile != "default" || len(s.Profiles.List()) != before+1 {
		t.Fatalf("import as new: %s", out)
	}

	// 非法文件：400 + 问题列表，且目标机房文件不变。
	settingsPath := filepath.Join(s.Profiles.RoomDir(created.ID), "settings.yaml")
	topoPath := filepath.Join(s.Profiles.RoomDir(created.ID), "topology.yaml")
	sb, _ := os.ReadFile(settingsPath)
	tb, _ := os.ReadFile(topoPath)
	bad := strings.Replace(string(full), `"settings"`, `"auth":{"password":"x"},"settings"`, 1)
	body := apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/import", bad, "default", 400)
	if !strings.Contains(string(body), `"code":"invalid_import"`) || !strings.Contains(string(body), "auth") {
		t.Fatalf("invalid import response: %s", body)
	}
	apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/import", `{"format":"netpulse-room-config","version":2}`, "default", 400)
	sa, _ := os.ReadFile(settingsPath)
	ta, _ := os.ReadFile(topoPath)
	if string(sa) != string(sb) || string(ta) != string(tb) {
		t.Fatalf("invalid import modified files")
	}

	// 覆盖导入（脱敏文件）到新机房：预览说明凭据保留，确认后返回备份版本。
	pv := apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/import?dry_run=1", string(rawMasked), "default", 200)
	if !strings.Contains(string(pv), `"kept"`) || !strings.Contains(string(pv), `"mode":"replace"`) {
		t.Fatalf("overwrite preview: %s", pv)
	}
	res := apiReqProfile(t, ts, "POST", "/api/profiles/"+created.ID+"/import", string(rawMasked), "default", 200)
	if !strings.Contains(string(res), `"backup_version":"v-`) {
		t.Fatalf("import result: %s", res)
	}
	rs, _, _, _ := s.Profiles.GetSettings(created.ID)
	if !strings.Contains(rs.Alert.WebhookURL, "default-secret") {
		t.Fatalf("masked import should keep target secrets, got %q", rs.Alert.WebhookURL)
	}
	// 全局配置不受影响。
	if c := s.Cfg.Get(); c.AI.APIKey != "sk-global-test" || c.MCP.Token != "mcp-global-test" || c.Auth.Password != testPassword {
		t.Fatalf("global config changed")
	}
	apiReqProfile(t, ts, "GET", "/api/profiles/room-missing/export", "", "", 404)
	apiReqProfile(t, ts, "POST", "/api/profiles/room-missing/import", string(full), "default", 404)
}
