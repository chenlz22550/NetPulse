package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const maxSSHUpload = 1 << 30

type sshTerminal struct {
	client    *ssh.Client
	ctx       context.Context
	mu        sync.Mutex
	uploading bool
}

type sshMessage struct {
	Type        string `json:"type"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Data        string `json:"data"`
	Fingerprint string `json:"fingerprint"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

type sshOutput struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (o *sshOutput) json(v interface{}) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return o.ws.WriteJSON(v)
}
func (o *sshOutput) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := o.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// switchTerminalHost uses the same saved target for terminal and SNMP management.
func (s *Server) switchTerminalHost(id string) (string, error) {
	topo, _ := s.Topo.Snapshot()
	for _, sw := range topo.Switches {
		if sw.ID != id {
			continue
		}
		host := sw.IP
		if override := strings.TrimSpace(s.Cfg.Get().Locate.Switches[id].Host); override != "" {
			host = override
		}
		host = strings.TrimSpace(host)
		if host == "" {
			return "", badRequest("请先保存该交换机的读取地址或管理 IP")
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return strings.Trim(host, "[]"), nil
	}
	return "", notFound("交换机")
}

// checkSSHHost remembers only public-key fingerprints, never login credentials.
func (s *Server) checkSSHHost(address, fingerprint string, accept bool) (bool, error) {
	s.sshHostMu.Lock()
	defer s.sshHostMu.Unlock()
	file := filepath.Join(s.BaseDir, "data", "ssh_known_hosts.json")
	hosts := map[string]string{}
	b, err := os.ReadFile(file)
	if err == nil {
		if err := json.Unmarshal(b, &hosts); err != nil {
			return false, fmt.Errorf("读取 SSH 主机记录失败：%w", err)
		}
		if hosts == nil {
			return false, fmt.Errorf("SSH 主机记录无效，请检查 data/ssh_known_hosts.json")
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if old, ok := hosts[address]; ok {
		if old != fingerprint {
			return false, fmt.Errorf("SSH 主机密钥已变化，已拒绝连接。请核对设备身份及 data/ssh_known_hosts.json 中的记录")
		}
		return true, nil
	}
	if !accept {
		return false, nil
	}
	hosts[address] = fingerprint
	b, err = json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		return false, err
	}
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".ssh-hosts-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return false, err
	}
	if err = tmp.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp.Name(), file); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) handleSSH(w http.ResponseWriter, r *http.Request, id string) error {
	host, err := s.switchTerminalHost(id)
	if err != nil {
		return err
	}
	return s.handleSSHHost(w, r, host)
}

func (s *Server) handleNodeSSH(w http.ResponseWriter, r *http.Request, id string) error {
	topo, _ := s.Topo.Snapshot()
	for _, n := range topo.Nodes {
		if n.ID == id {
			host := strings.TrimSpace(n.IP)
			if host == "" {
				return badRequest("节点未设置 IP")
			}
			return s.handleSSHHost(w, r, host)
		}
	}
	return notFound("节点")
}

func (s *Server) handleSSHHost(w http.ResponseWriter, r *http.Request, host string) error {
	if !terminalOrigin(r) {
		return &apiError{status: 403, msg: "终端仅允许从当前网站连接"}
	}
	if !websocket.IsWebSocketUpgrade(r) {
		return badRequest("请从设备的 SSH 按钮打开终端")
	}
	var err error
	port := 22
	if p := r.URL.Query().Get("port"); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return badRequest("SSH 端口必须为 1–65535")
		}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	upgrader := websocket.Upgrader{CheckOrigin: terminalOrigin, HandshakeTimeout: 10 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil
	}
	defer ws.Close()
	out := &sshOutput{ws: ws}
	fail := func(err error) { _ = out.json(map[string]string{"type": "error", "message": err.Error()}) }
	ws.SetReadLimit(64 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var auth sshMessage
	if err := ws.ReadJSON(&auth); err != nil {
		return nil
	}
	if auth.Type != "auth" || strings.TrimSpace(auth.Username) == "" || len(auth.Username) > 256 || len(auth.Password) > 4096 {
		fail(fmt.Errorf("请填写有效的 SSH 用户名和密码后重新连接"))
		return nil
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	messages := make(chan sshMessage, 16)
	_ = ws.SetReadDeadline(time.Now().Add(75 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	go func() {
		defer cancel()
		for {
			var msg sshMessage
			if ws.ReadJSON(&msg) != nil {
				return
			}
			select {
			case messages <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					cancel()
					return
				}
			}
		}
	}()
	dialCtx, dialCancel := context.WithTimeout(ctx, 8*time.Second)
	conn, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(dialCtx, "tcp", address)
	dialCancel()
	if err != nil {
		fail(fmt.Errorf("SSH 连接失败：%v；请检查地址、端口及网络后重试", err))
		return nil
	}
	defer conn.Close()
	s.telnetMu.Lock()
	if s.telnetClosed || len(s.telnetSessions) >= 8 {
		s.telnetMu.Unlock()
		fail(fmt.Errorf("服务已关闭或远程终端达到 8 个，请关闭不用的连接后重试"))
		return nil
	}
	if s.telnetSessions == nil {
		s.telnetSessions = make(map[net.Conn]struct{})
	}
	s.telnetSessions[conn] = struct{}{}
	s.telnetMu.Unlock()
	defer func() { s.telnetMu.Lock(); delete(s.telnetSessions, conn); s.telnetMu.Unlock() }()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	cfg := &ssh.ClientConfig{User: auth.Username, Timeout: 15 * time.Second}
	password := auth.Password
	cfg.Auth = []ssh.AuthMethod{ssh.Password(password), ssh.KeyboardInteractive(func(_, _ string, questions []string, echo []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i, q := range questions {
			if echo[i] || (!strings.Contains(strings.ToLower(q), "password") && !strings.Contains(q, "密码")) {
				return nil, fmt.Errorf("设备要求额外认证，当前仅支持密码登录")
			}
			answers[i] = password
		}
		return answers, nil
	})}
	cfg.HostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		known, err := s.checkSSHHost(address, fingerprint, false)
		if err != nil || known {
			return err
		}
		if err := out.json(map[string]string{"type": "hostkey", "address": address, "fingerprint": fingerprint}); err != nil {
			return err
		}
		// Allow time to compare the first-use fingerprint before sending credentials.
		_ = conn.SetDeadline(time.Now().Add(65 * time.Second))
		timer := time.NewTimer(60 * time.Second)
		defer timer.Stop()
		for {
			select {
			case msg := <-messages:
				if msg.Type != "trust" {
					continue
				}
				if msg.Fingerprint != fingerprint {
					return fmt.Errorf("主机指纹确认不匹配，请重新连接")
				}
				_, err := s.checkSSHHost(address, fingerprint, true)
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				return err
			case <-timer.C:
				return fmt.Errorf("等待确认主机指纹超时，请重新连接")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	cc, chans, reqs, err := ssh.NewClientConn(conn, address, cfg)
	auth.Password = ""
	password = ""
	cfg.Auth = nil
	if err != nil {
		fail(fmt.Errorf("SSH 登录失败：%v；请核对账号、密码或设备 SSH 设置后重试", err))
		return nil
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		fail(fmt.Errorf("创建 SSH 终端失败：%v", err))
		return nil
	}
	defer session.Close()
	cols, rows := auth.Cols, auth.Rows
	if cols < 1 || cols > 1000 {
		cols = 100
	}
	if rows < 1 || rows > 500 {
		rows = 30
	}
	if err := session.RequestPty("xterm", rows, cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}); err != nil {
		fail(fmt.Errorf("设备不支持交互终端：%v", err))
		return nil
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		fail(err)
		return nil
	}
	session.Stdout = out
	session.Stderr = out
	if err := session.Shell(); err != nil {
		fail(fmt.Errorf("启动 SSH 命令行失败：%v", err))
		return nil
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		fail(err)
		return nil
	}
	token := hex.EncodeToString(tokenBytes)
	terminal := &sshTerminal{client: client, ctx: ctx}
	s.sshMu.Lock()
	if s.sshSessions == nil {
		s.sshSessions = make(map[string]*sshTerminal)
	}
	s.sshSessions[token] = terminal
	s.sshMu.Unlock()
	defer func() { s.sshMu.Lock(); delete(s.sshSessions, token); s.sshMu.Unlock() }()
	_ = out.json(map[string]string{"type": "connected", "message": "已连接 " + address, "session": token})
	go func() {
		err := session.Wait()
		msg := "SSH 连接已断开"
		if err != nil {
			msg += "：" + err.Error()
		}
		_ = out.json(map[string]string{"type": "closed", "message": msg})
		cancel()
		_ = ws.Close()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-messages:
			switch msg.Type {
			case "input":
				_, err = io.WriteString(stdin, msg.Data)
			case "resize":
				if msg.Cols > 0 && msg.Cols <= 1000 && msg.Rows > 0 && msg.Rows <= 500 {
					err = session.WindowChange(msg.Rows, msg.Cols)
				}
			default:
				continue
			}
			if err != nil {
				fail(fmt.Errorf("SSH 终端通信失败：%v", err))
				return nil
			}
		}
	}
}

func (s *Server) handleSSHUpload(w http.ResponseWriter, r *http.Request, token string) error {
	if !terminalOrigin(r) {
		return &apiError{status: 403, msg: "上传仅允许从当前网站发起"}
	}
	s.sshMu.Lock()
	terminal := s.sshSessions[token]
	s.sshMu.Unlock()
	if terminal == nil {
		return badRequest("SSH 连接已失效，请重新登录后上传")
	}
	select {
	case <-terminal.ctx.Done():
		return badRequest("SSH 连接已断开，请重新登录后上传")
	default:
	}
	terminal.mu.Lock()
	if terminal.uploading {
		terminal.mu.Unlock()
		return badRequest("已有文件正在上传，请等待完成或取消")
	}
	terminal.uploading = true
	terminal.mu.Unlock()
	defer func() { terminal.mu.Lock(); terminal.uploading = false; terminal.mu.Unlock() }()
	name, dir := r.URL.Query().Get("name"), r.URL.Query().Get("dir")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len(name) > 255 || len(dir) > 4096 || strings.ContainsRune(dir, 0) {
		return badRequest("文件名或远程目录无效")
	}
	if r.ContentLength > maxSSHUpload {
		return badRequest("单个文件不能超过 1 GiB")
	}
	remote := path.Join(dir, name)
	// A separate SFTP channel on the existing SSH connection keeps the shell usable.
	// Closing this channel cancels a transfer without ending the SSH terminal.
	done := make(chan struct{})
	defer close(done)
	type result struct {
		c   *sftp.Client
		err error
	}
	ready := make(chan result)
	opened := make(chan ssh.Channel)
	go func() {
		ch, requests, err := terminal.client.OpenChannel("session", nil)
		if err != nil {
			select {
			case ready <- result{err: err}:
			case <-done:
			}
			return
		}
		go ssh.DiscardRequests(requests)
		select {
		case opened <- ch:
		case <-done:
			_ = ch.Close()
			return
		}
		ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
		if err == nil && !ok {
			err = fmt.Errorf("设备拒绝 SFTP 子系统请求")
		}
		var c *sftp.Client
		if err == nil {
			c, err = sftp.NewClientPipe(ch, ch)
		}
		select {
		case ready <- result{c, err}:
		case <-done:
			_ = ch.Close()
			if c != nil {
				_ = c.Close()
			}
		}
	}()
	var files *sftp.Client
	var channel ssh.Channel
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	// If the peer stalls before accepting a channel, closing SSH is the only
	// way to cancel OpenChannel. Once opened, cancel only the upload channel.
	closePending := func() {
		if channel != nil {
			_ = channel.Close()
		} else {
			_ = terminal.client.Close()
		}
	}
	for files == nil {
		select {
		case ch := <-opened:
			channel = ch
		case res := <-ready:
			if res.err != nil {
				if channel != nil {
					_ = channel.Close()
				}
				return badRequest("无法启动 SFTP：%v；请确认交换机已启用 SFTP 文件服务", res.err)
			}
			files = res.c
		case <-timer.C:
			closePending()
			return badRequest("SFTP 启动超时，请确认设备支持并启用了 SFTP 后重新连接重试")
		case <-r.Context().Done():
			closePending()
			return r.Context().Err()
		case <-terminal.ctx.Done():
			closePending()
			return badRequest("SSH 连接已断开，请重新登录后上传")
		}
	}
	defer files.Close()
	go func() {
		select {
		case <-r.Context().Done():
			_ = files.Close()
		case <-terminal.ctx.Done():
			_ = files.Close()
		case <-done:
		}
	}()
	overwrite := r.URL.Query().Get("overwrite") == "1"
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	dst, err := files.OpenFile(remote, flags)
	if err != nil {
		return badRequest("无法写入 %s：%v；请检查目录、权限及是否有同名文件", remote, err)
	}
	defer dst.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxSSHUpload)
	n, err := io.Copy(dst, r.Body)
	if err == nil {
		err = dst.Close()
	}
	if err != nil {
		return badRequest("上传未完成：%v；远端可能保留部分文件，请检查后重新上传", err)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": remote, "bytes": n})
	return nil
}
