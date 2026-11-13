package server

import (
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"netping/internal/config"
)

func TestTelnetRoundTripAndClose(t *testing.T) {
	s, ts := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := s.Cfg.Get()
	cfg.Locate.Switches = map[string]config.LocateSwitch{"access": {Host: "127.0.0.1:161"}}
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = conn.Write(append([]byte{255, 251, 1}, []byte("Password: ")...))
		if err != nil {
			finished <- err
			return
		}
		b := make([]byte, 3)
		if _, err = io.ReadFull(conn, b); err != nil {
			finished <- err
			return
		}
		if string(b) != string([]byte{255, 253, 1}) {
			finished <- io.ErrUnexpectedEOF
			return
		}
		b = make([]byte, len("secret\r\n"))
		if _, err = io.ReadFull(conn, b); err != nil {
			finished <- err
			return
		}
		if string(b) != "secret\r\n" {
			finished <- io.ErrUnexpectedEOF
			return
		}
		_, _ = conn.Write([]byte("\r\nSwitch#"))
		_, err = conn.Read(make([]byte, 1))
		if err == io.EOF {
			err = nil
		}
		finished <- err
	}()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/locate/telnet/access?port=" + port
	h := http.Header{"Origin": []string{ts.URL}, "Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(":"+testPassword))}}
	ws, _, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	// First frame acknowledges the TCP connection; subsequent frames carry raw output.
	var status map[string]string
	if err := ws.ReadJSON(&status); err != nil || status["type"] != "connected" {
		t.Fatalf("status %v: %v", status, err)
	}
	_, b, err := ws.ReadMessage()
	if err != nil || string(b) != "Password: " {
		t.Fatalf("prompt %q: %v", b, err)
	}
	if err := ws.WriteJSON(map[string]string{"type": "input", "data": "secret\r"}); err != nil {
		t.Fatal(err)
	}
	_, b, err = ws.ReadMessage()
	if err != nil || string(b) != "\r\nSwitch#" {
		t.Fatalf("reply %q: %v", b, err)
	}
	// Server shutdown must also close hijacked WebSocket/TCP sessions.
	s.Close()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Telnet connection leaked")
	}
}

func TestTelnetAccessChecks(t *testing.T) {
	_, ts := newTestServer(t)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/locate/telnet/access"
	for _, tc := range []struct {
		origin string
		auth   bool
		code   int
	}{
		{ts.URL, false, http.StatusUnauthorized},
		{"https://foreign.example", true, http.StatusForbidden},
		{"", true, http.StatusForbidden},
	} {
		h := http.Header{"Origin": []string{tc.origin}}
		if tc.auth {
			h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+testPassword)))
		}
		ws, resp, err := websocket.DefaultDialer.Dial(url, h)
		if ws != nil {
			ws.Close()
		}
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || resp == nil || resp.StatusCode != tc.code {
			t.Fatalf("origin %q auth %v: %v %v", tc.origin, tc.auth, resp, err)
		}
	}
}
