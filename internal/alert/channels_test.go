package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netping/internal/config"
)

func TestNotificationChannels(t *testing.T) {
	for _, kind := range []string{"wecom", "dingtalk", "feishu", "slack", "webhook"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				switch kind {
				case "wecom":
					if body["msgtype"] != "markdown" {
						t.Error(body)
					}
				case "dingtalk":
					ts := r.URL.Query().Get("timestamp")
					if ts == "" || r.URL.Query().Get("sign") != channelSignature("secret", ts+"\nsecret") {
						t.Error("invalid dingtalk signature")
					}
					if body["markdown"].(map[string]interface{})["text"] != "hello" {
						t.Error(body)
					}
				case "feishu":
					ts, _ := body["timestamp"].(string)
					if ts == "" || body["sign"] != channelSignature(ts+"\nsecret", "") {
						t.Error("invalid feishu signature")
					}
					if body["content"].(map[string]interface{})["text"] != "hello" {
						t.Error(body)
					}
				default:
					if body["text"] != "hello" {
						t.Error(body)
					}
				}
				switch kind {
				case "slack":
					w.Write([]byte("ok"))
				case "feishu":
					w.Write([]byte(`{"code":0}`))
				case "webhook":
					w.WriteHeader(204)
				default:
					w.Write([]byte(`{"errcode":0}`))
				}
			}))
			defer srv.Close()
			if err := sendChannel(config.AlertConfig{SiteTitle: "test"}, kind, srv.URL, "secret", `<font color="info">hello</font>`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNotificationFanoutAndErrors(t *testing.T) {
	received := make(chan bool, 1)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received <- true; w.Write([]byte("ok")) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":19024,"msg":"secret"}`)) }))
	defer bad.Close()
	err := SendNotifications(config.AlertConfig{SlackURL: good.URL, FeishuURL: bad.URL}, "test")
	if err == nil || !strings.Contains(err.Error(), "飞书") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-received:
	default:
		t.Fatal("failure prevented other channel")
	}
	if SendNotifications(config.AlertConfig{}, "test") == nil {
		t.Fatal("empty channels accepted")
	}
	for _, body := range []string{`{}`, `not json`, `{"code":1}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if sendChannel(config.AlertConfig{}, "feishu", srv.URL, "", "test") == nil {
			t.Errorf("accepted %s", body)
		}
		srv.Close()
	}
}
