package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"netping/internal/telnet"
)

func terminalOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}

// handleTelnet is only exposed through the authenticated web API, never MCP/AI.
func (s *Server) handleTelnet(w http.ResponseWriter, r *http.Request, id string) error {
	if !terminalOrigin(r) {
		return &apiError{status: http.StatusForbidden, msg: "终端仅允许从当前网站连接"}
	}
	if !websocket.IsWebSocketUpgrade(r) {
		return badRequest("请从端口定位页面打开 Telnet 终端")
	}
	topo, _ := s.Topo.Snapshot()
	host := ""
	found := false
	for _, sw := range topo.Switches {
		if sw.ID == id {
			host, found = sw.IP, true
			break
		}
	}
	if !found {
		return notFound("交换机")
	}
	if override := s.Cfg.Get().Locate.Switches[id].Host; strings.TrimSpace(override) != "" {
		host = override
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return badRequest("请先保存该交换机的读取地址或管理 IP")
	}
	// SNMP read addresses may include a UDP port; Telnet has its own port.
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	port := 23
	if p := r.URL.Query().Get("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return badRequest("Telnet 端口必须为 1–65535")
		}
		port = n
	}
	upgrader := websocket.Upgrader{CheckOrigin: terminalOrigin, HandshakeTimeout: 10 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil
	}
	defer ws.Close()
	status := func(kind, message string) {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = ws.WriteJSON(map[string]string{"type": kind, "message": message})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port)))
	if err != nil {
		status("error", fmt.Sprintf("Telnet 连接失败：%v；请检查地址、端口及网络后重新连接", err))
		return nil
	}
	defer conn.Close()
	s.telnetMu.Lock()
	if s.telnetClosed {
		s.telnetMu.Unlock()
		status("error", "服务已关闭，请启动 netPing 后重新连接")
		return nil
	}
	if len(s.telnetSessions) >= 8 {
		s.telnetMu.Unlock()
		status("error", "同时最多打开 8 个 Telnet 终端，请先关闭不用的连接")
		return nil
	}
	if s.telnetSessions == nil {
		s.telnetSessions = make(map[net.Conn]struct{})
	}
	s.telnetSessions[conn] = struct{}{}
	s.telnetMu.Unlock()
	defer func() { s.telnetMu.Lock(); delete(s.telnetSessions, conn); s.telnetMu.Unlock() }()
	status("connected", "已连接 "+net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port)))
	ws.SetReadLimit(64 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(75 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	client := telnet.New(conn)
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = ws.Close()
					return
				}
			}
		}
	}()
	// The single data writer forwards bytes, keeping split UTF-8 sequences intact.
	go func() {
		defer ws.Close()
		b := make([]byte, 16<<10)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Minute))
			n, e := client.Read(b)
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if n > 0 && ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
				return
			}
			if e != nil {
				_ = ws.WriteJSON(map[string]string{"type": "closed", "message": "交换机连接已断开（或 15 分钟无输出）"})
				return
			}
		}
	}()
	for {
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if ws.ReadJSON(&msg) != nil {
			break
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		switch msg.Type {
		case "input":
			err = client.Send([]byte(msg.Data))
		case "resize":
			err = client.Resize(msg.Cols, msg.Rows)
		default:
			err = fmt.Errorf("未知终端消息")
		}
		if err != nil {
			break
		}
	}
	return nil
}
