package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"netping/internal/config"
)

// startSSHDevice runs an actual SSH shell and SFTP subsystem, without host SSH dependencies.
func startSSHDevice(t *testing.T, noSFTP ...bool) (string, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == "admin" && string(p) == "device-password" {
			return nil, nil
		}
		return nil, fmt.Errorf("bad credentials")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
			go func() {
				defer c.Close()
				defer func() { mu.Lock(); delete(conns, c); mu.Unlock() }()
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(reqs)
				for incoming := range chans {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					go func() {
						defer ch.Close()
						for req := range requests {
							switch req.Type {
							case "pty-req", "window-change":
								req.Reply(true, nil)
							case "shell":
								req.Reply(true, nil)
								go func() { io.WriteString(ch, "Switch#"); io.Copy(ch, ch) }()
							case "subsystem":
								var sub struct{ Name string }
								ssh.Unmarshal(req.Payload, &sub)
								if sub.Name != "sftp" || (len(noSFTP) > 0 && noSFTP[0]) {
									req.Reply(false, nil)
									continue
								}
								req.Reply(true, nil)
								fs, err := sftp.NewServer(ch)
								if err != nil {
									return
								}
								defer fs.Close()
								fs.Serve()
								return
							default:
								req.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func sshHeaders(ts *httptest.Server) http.Header {
	return http.Header{"Origin": []string{ts.URL}, "Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(":"+testPassword))}}
}

func openTestSSH(t *testing.T, s *Server, ts *httptest.Server, address string) *websocket.Conn {
	t.Helper()
	host, port, _ := net.SplitHostPort(address)
	cfg := s.Cfg.Get()
	cfg.Locate.Switches = map[string]config.LocateSwitch{"access": {Host: host}}
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/locate/ssh/access?port="+port, sshHeaders(ts))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	return ws
}

func sshStatus(t *testing.T, ws *websocket.Conn, kind string) map[string]string {
	t.Helper()
	for {
		typ, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.TextMessage {
			continue
		}
		var msg map[string]string
		if err := json.Unmarshal(b, &msg); err != nil {
			t.Fatal(err)
		}
		if msg["type"] == "error" && kind != "error" {
			t.Fatal(msg)
		}
		if msg["type"] == kind {
			return msg
		}
	}
}

func TestSSHAndSFTPUpload(t *testing.T) {
	s, ts := newTestServer(t)
	address, fingerprint := startSSHDevice(t)
	ws := openTestSSH(t, s, ts, address)
	if err := ws.WriteJSON(sshMessage{Type: "auth", Username: "admin", Password: "device-password", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	challenge := sshStatus(t, ws, "hostkey")
	if challenge["fingerprint"] != fingerprint {
		t.Fatal(challenge)
	}
	if err := ws.WriteJSON(sshMessage{Type: "trust", Fingerprint: fingerprint}); err != nil {
		t.Fatal(err)
	}
	connected := sshStatus(t, ws, "connected")
	token := connected["session"]
	if len(token) != 48 {
		t.Fatal(connected)
	}
	if err := ws.WriteJSON(sshMessage{Type: "input", Data: "show version\r"}); err != nil {
		t.Fatal(err)
	}
	output := ""
	for !strings.Contains(output, "show version") {
		typ, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.BinaryMessage {
			output += string(b)
		}
	}
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0, 255, 3, 7}, 32768)
	upload := func(token, name, overwrite, origin string, body []byte) (int, map[string]interface{}) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/locate/ssh-upload/"+token, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		q := req.URL.Query()
		q.Set("dir", dir)
		q.Set("name", name)
		q.Set("overwrite", overwrite)
		req.URL.RawQuery = q.Encode()
		req.Header = sshHeaders(ts)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	if code, out := upload(token, "firmware.bin", "0", ts.URL, payload); code != 200 || out["bytes"] != float64(len(payload)) {
		t.Fatalf("upload %d %v", code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "firmware.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("file mismatch %v", err)
	}
	if code, _ := upload(token, "firmware.bin", "0", ts.URL, []byte("replace")); code != 400 {
		t.Fatalf("no overwrite: %d", code)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "firmware.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("existing file changed")
	}
	if code, _ := upload(token, "firmware.bin", "1", ts.URL, []byte("replace")); code != 200 {
		t.Fatalf("overwrite: %d", code)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "firmware.bin"))
	if string(got) != "replace" {
		t.Fatalf("overwrite data %q", got)
	}
	if code, _ := upload(token, "../escape", "0", ts.URL, nil); code != 400 {
		t.Fatal("invalid filename accepted")
	}
	if code, _ := upload(token, "other", "0", "https://foreign.example", nil); code != 403 {
		t.Fatal("foreign origin accepted")
	}
	if code, _ := upload("invalid", "other", "0", ts.URL, nil); code != 400 {
		t.Fatal("invalid session accepted")
	}
	// Cancel a streaming browser request while the file is open. The transfer
	// channel must close without taking down the interactive SSH connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/locate/ssh-upload/"+token, pr)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Set("dir", dir)
	q.Set("name", "cancelled.bin")
	req.URL.RawQuery = q.Encode()
	req.Header = sshHeaders(ts)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	if _, err := pw.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	s.sshMu.Lock()
	terminal := s.sshSessions[token]
	s.sshMu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "cancelled.bin")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "cancelled.bin")); err != nil {
		t.Fatal("transfer did not reach remote file", err)
	}
	cancel()
	_ = pw.Close()
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled HTTP request leaked")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		terminal.mu.Lock()
		active := terminal.uploading
		terminal.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, out := upload(token, "after-cancel.bin", "0", ts.URL, []byte("ok")); code != 200 {
		t.Fatalf("upload after cancellation: %d %v", code, out)
	}
	known, err := os.ReadFile(filepath.Join(s.BaseDir, "data", "ssh_known_hosts.json"))
	if err != nil || !bytes.Contains(known, []byte(fingerprint)) || bytes.Contains(known, []byte("device-password")) {
		t.Fatalf("host storage %q %v", known, err)
	}
	ws.Close()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.sshMu.Lock()
		n := len(s.sshSessions)
		s.sshMu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := upload(token, "after-close", "0", ts.URL, nil); code != 400 {
		t.Fatal("session usable after terminal close")
	}
	// A remembered host does not ask for trust again, and wrong passwords fail clearly.
	ws2 := openTestSSH(t, s, ts, address)
	_ = ws2.WriteJSON(sshMessage{Type: "auth", Username: "admin", Password: "wrong"})
	if msg := sshStatus(t, ws2, "error"); !strings.Contains(msg["message"], "SSH 登录失败") {
		t.Fatal(msg)
	}
}

func TestSSHWithoutSFTP(t *testing.T) {
	s, ts := newTestServer(t)
	address, fingerprint := startSSHDevice(t, true)
	if _, err := s.checkSSHHost(address, fingerprint, true); err != nil {
		t.Fatal(err)
	}
	ws := openTestSSH(t, s, ts, address)
	_ = ws.WriteJSON(sshMessage{Type: "auth", Username: "admin", Password: "device-password"})
	token := sshStatus(t, ws, "connected")["session"]
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/locate/ssh-upload/"+token+"?name=test.bin", strings.NewReader("test"))
	req.Header = sshHeaders(ts)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "SFTP") {
		t.Fatalf("unsupported SFTP: %d %s", resp.StatusCode, b)
	}
	_ = ws.WriteJSON(sshMessage{Type: "input", Data: "still-connected\r"})
	for {
		typ, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.BinaryMessage && strings.Contains(string(b), "still-connected") {
			break
		}
	}
}

func TestSSHHostKeyChangedAndCorruption(t *testing.T) {
	s, _ := newTestServer(t)
	if known, err := s.checkSSHHost("switch:22", "SHA256:first", false); known || err != nil {
		t.Fatal(known, err)
	}
	if known, err := s.checkSSHHost("switch:22", "SHA256:first", true); !known || err != nil {
		t.Fatal(known, err)
	}
	if _, err := s.checkSSHHost("switch:22", "SHA256:changed", true); err == nil {
		t.Fatal("changed host key accepted")
	}
	if err := os.WriteFile(filepath.Join(s.BaseDir, "data", "ssh_known_hosts.json"), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.checkSSHHost("other:22", "SHA256:key", true); err == nil {
		t.Fatal("corrupt host file accepted")
	}
}
