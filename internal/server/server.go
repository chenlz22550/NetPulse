// Package server 提供管理网页和 REST/SSE 接口。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/profiles"
	"netping/internal/store"
	"netping/internal/topology"
	"netping/web"
)

// Version 程序版本，由 main 设置。
var Version = "dev"

// Server 管理网页服务。
type Server struct {
	Cfg      *config.Manager
	Topo     *topology.Store
	Mon      *monitor.Monitor
	Alerts   *alert.Engine
	Store    *store.Store
	Disc     *discovery.Service
	Loc      *locate.Service
	Profiles *profiles.Manager
	BaseDir  string
	// DocsMD / SchemaJSON 为字段说明与 JSON Schema，作为 MCP 资源提供。
	DocsMD     []byte
	SchemaJSON []byte
	// OnPauseChange 在网页上暂停/恢复监控后调用（用于同步托盘菜单）。
	OnPauseChange func(bool)

	started        time.Time
	http           *http.Server
	auth           authState
	telnetMu       sync.Mutex
	telnetSessions map[net.Conn]struct{}
	telnetClosed   bool
	sshMu          sync.Mutex
	sshSessions    map[string]*sshTerminal
	sshHostMu      sync.Mutex
	seenMu         sync.Mutex
	seen           map[string]seenHost // AI 自主发现 / MCP 探测中实际看到的地址（用于校验 AI 建议）
}

// Start 开始监听，返回实际监听地址。
func (s *Server) Start() (string, error) {
	s.started = time.Now()
	cfg := s.Cfg.Get()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.routeAPI)
	mux.HandleFunc("/mcp", s.handleMCP)
	static := http.FileServer(http.FS(web.Static()))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	s.http = &http.Server{Handler: s.withAuth(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP 服务异常退出: %v", err)
		}
	}()
	return ln.Addr().String(), nil
}

// Close 关闭服务。
func (s *Server) Close() {
	s.telnetMu.Lock()
	s.telnetClosed = true
	for conn := range s.telnetSessions {
		_ = conn.Close()
	}
	s.telnetMu.Unlock()
	if s.http != nil {
		_ = s.http.Close()
	}
}

func (s *Server) withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Cfg.Get()
		path := r.URL.Path
		// 设置了 MCP 令牌时，/mcp 改用令牌认证（在 handleMCP 中校验）。
		if path == "/mcp" && cfg.MCP.Token != "" {
			h.ServeHTTP(w, r)
			return
		}
		if publicPaths[path] || s.authorized(r, cfg.Auth) {
			h.ServeHTTP(w, r)
			return
		}
		switch {
		case path == "/mcp":
			w.Header().Set("WWW-Authenticate", `Basic realm="netPing", charset="UTF-8"`)
			http.Error(w, "需要登录：请在网页「MCP」页生成访问令牌，或用 HTTP Basic 认证提供网页登录密码", http.StatusUnauthorized)
		case strings.HasPrefix(path, "/api/"):
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "需要登录", "login_required": true})
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/login.html", http.StatusFound)
		default:
			http.Error(w, "需要登录", http.StatusUnauthorized)
		}
	})
}

// ---------------------------------------------------------------------------

type apiError struct {
	status        int
	msg           string
	code          string
	activeProfile string
	problems      []string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, a ...interface{}) error {
	return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

func notFound(what string) error {
	return &apiError{status: http.StatusNotFound, msg: what + " 不存在"}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]interface{}{"error": err.Error()}
	var ae *apiError
	var ve *topology.ValidationError
	switch {
	case errors.As(err, &ae):
		status = ae.status
		if ae.code != "" {
			body["code"] = ae.code
		}
		if ae.activeProfile != "" {
			body["active_profile"] = ae.activeProfile
		}
	case errors.As(err, &ve):
		status = http.StatusBadRequest
		body["error"] = "拓扑校验失败"
		body["problems"] = ve.Problems
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func decode(r *http.Request, v interface{}) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		return badRequest("请求内容不是有效的 JSON: %v", err)
	}
	return nil
}

// routeAPI 手工路由（Go 1.20 的 ServeMux 不支持按方法/路径参数匹配）。
func (s *Server) routeAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	head := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.Join(parts[1:], "/")
	}
	m := r.Method
	var err error
	if !skipProfileHeaderCheck(head, arg, m) {
		if err := s.checkProfileHeader(r); err != nil {
			writeErr(w, err)
			return
		}
	}
	switch {
	case head == "meta" && m == http.MethodGet:
		s.handleMeta(w)
	case head == "profiles":
		err = s.handleProfiles(w, r, arg)
	case head == "maintenance":
		if m != http.MethodGet {
			defer s.Topo.ProfileWork()()
		}
		err = s.handleMaintenance(w, r, arg)
	case head == "health-report" && arg == "" && m == http.MethodPost:
		err = s.handleHealthReport(w, r)
	case head == "reports" && arg == "health" && m == http.MethodPost:
		err = s.handleHealthReport(w, r)
	case head == "topology" && arg == "" && m == http.MethodGet:
		s.handleTopology(w)
	case head == "topology" && arg == "reload" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleReload(w)
	case head == "topology" && arg == "yaml" && m == http.MethodGet:
		err = s.handleGetYAML(w, r)
	case head == "topology" && arg == "yaml" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handlePutYAML(w, r)
	case head == "site" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleSite(w, r)
	case head == "nodes" && arg == "cleanup" && m == http.MethodGet:
		s.handleNodeCleanupPreview(w)
	case head == "nodes" && arg == "cleanup" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleNodeCleanup(w, r)
	case head == "nodes" && strings.HasSuffix(arg, "/lock") && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleNodeLock(w, r, strings.TrimSuffix(arg, "/lock"))
	case head == "nodes" && arg == "" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleNodeSave(w, r, "")
	case head == "nodes" && arg != "" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleNodeSave(w, r, arg)
	case head == "nodes" && arg != "" && m == http.MethodDelete:
		defer s.Topo.ProfileWork()()
		err = s.handleNodeDelete(w, arg)
	case head == "switches" && arg == "" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleSwitchSave(w, r, "")
	case head == "switches" && arg != "" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleSwitchSave(w, r, arg)
	case head == "switches" && arg != "" && m == http.MethodDelete:
		defer s.Topo.ProfileWork()()
		err = s.handleSwitchDelete(w, arg)
	case head == "links" && arg == "" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleLinkSave(w, r, "")
	case head == "links" && arg != "" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleLinkSave(w, r, arg)
	case head == "links" && arg != "" && m == http.MethodDelete:
		defer s.Topo.ProfileWork()()
		err = s.handleLinkDelete(w, arg)
	case head == "positions" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handlePositions(w, r)
	case head == "layout" && arg == "reset" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleLayoutReset(w, r)
	case head == "ports" && arg == "scan" && m == http.MethodPost:
		err = s.handlePortScan(w, r)
	case head == "ports" && arg == "monitoring" && (m == http.MethodPut || m == http.MethodGet):
		if m == http.MethodPut {
			defer s.Topo.ProfileWork()()
		}
		err = s.handleTCPMonitoring(w, r)
	case head == "status" && m == http.MethodGet:
		writeJSON(w, s.statusPayload())
	case head == "stream" && m == http.MethodGet:
		s.handleStream(w, r)
	case head == "history" && arg != "" && m == http.MethodGet:
		err = s.handleHistory(w, r, arg)
	case head == "auth":
		err = s.handleAuth(w, r, arg)
	case head == "ai":
		err = s.handleAI(w, r, arg)
	case head == "config" && m == http.MethodGet:
		writeJSON(w, publicConfig(s.Cfg.Get()))
	case head == "config" && m == http.MethodPut:
		defer s.Topo.ProfileWork()()
		err = s.handleConfig(w, r)
	case head == "alert" && arg == "test" && m == http.MethodPost:
		err = s.handleAlertTest(w, r)
	case head == "alerts" && m == http.MethodGet:
		writeJSON(w, s.Alerts.History(300))
	case head == "locate" && arg == "" && m == http.MethodGet:
		writeJSON(w, s.locateStatus())
	case head == "locate" && strings.HasPrefix(arg, "telnet/") && m == http.MethodGet:
		err = s.handleTelnet(w, r, strings.TrimPrefix(arg, "telnet/"))
	case head == "ssh" && strings.HasPrefix(arg, "nodes/") && m == http.MethodGet:
		err = s.handleNodeSSH(w, r, strings.TrimPrefix(arg, "nodes/"))
	case head == "locate" && strings.HasPrefix(arg, "ssh/") && m == http.MethodGet:
		err = s.handleSSH(w, r, strings.TrimPrefix(arg, "ssh/"))
	case head == "locate" && strings.HasPrefix(arg, "ssh-upload/") && m == http.MethodPost:
		err = s.handleSSHUpload(w, r, strings.TrimPrefix(arg, "ssh-upload/"))
	case head == "locate" && arg == "run" && m == http.MethodPost:
		err = s.handleLocateRun(w, r)
	case head == "locate" && arg == "test" && m == http.MethodPost:
		err = s.handleLocateTest(w, r)
	case head == "locate" && arg == "apply" && m == http.MethodPost:
		err = s.handleLocateApply(w, r)
	case head == "locate" && strings.HasPrefix(arg, "table/") && m == http.MethodGet:
		err = s.handleLocateTable(w, r, strings.TrimPrefix(arg, "table/"))
	case head == "discovery" && arg == "" && m == http.MethodGet:
		writeJSON(w, map[string]interface{}{"status": s.Disc.Status(), "last": s.Disc.Last(), "results": s.Disc.Results(),
			"suggestions": s.Disc.Suggestions(), "unknown": s.Disc.Unknown(), "events": s.Disc.Events(200)})
	case head == "discovery" && arg == "scan" && m == http.MethodPost:
		err = s.handleDiscoveryScan(w, r)
	case head == "discovery" && arg == "add" && m == http.MethodPost:
		err = s.handleDiscoveryAdd(w, r)
	case head == "discovery" && arg == "apply" && m == http.MethodPost:
		err = s.handleDiscoveryApply(w, r)
	case head == "mcp" && m == http.MethodGet:
		s.handleMCPInfo(w)
	case head == "alerting" && m == http.MethodPost:
		defer s.Topo.ProfileWork()()
		err = s.handleAlerting(w, r)
	case head == "monitor" && arg == "pause" && m == http.MethodPost:
		err = s.handlePause(w, r)
	default:
		err = &apiError{status: http.StatusNotFound, msg: "接口不存在: " + m + " " + r.URL.Path}
	}
	if err != nil {
		writeErr(w, err)
	}
}

func skipProfileHeaderCheck(head, arg, method string) bool {
	if head == "auth" || head == "stream" {
		return true
	}
	if method == http.MethodGet && (head == "meta" || (head == "topology" && arg == "") || (head == "profiles" && arg == "")) {
		return true
	}
	return false
}

func (s *Server) handleMeta(w http.ResponseWriter) {
	writeJSON(w, map[string]interface{}{
		"version":        Version,
		"started":        s.started.UnixMilli(),
		"base_dir":       s.BaseDir,
		"topo_file":      s.Topo.Path(),
		"active_profile": s.activeProfileID(),
		"paused":         s.Mon.Paused(),
		"schedule":       s.Cfg.Get().Schedule.Describe(),
		"roles":          topology.Roles,
		"device_types":   topology.DeviceTypes,
		"type_names":     topology.DeviceTypeNames,
		"flags":          topology.KnownFlags,
		"node_sides":     topology.NodeSides,
		"server_time":    time.Now().UnixMilli(),
	})
}

func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }
