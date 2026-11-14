package server

// 插件：网页「插件」页的接口，以及把插件工具接入 MCP 与 AI 诊断。
// 插件只能通过把文件放进数据目录的 plugins/ 安装；网页只负责启停、设置和测试，MCP 不能启停或配置插件。

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"netping/internal/plugin"
)

func (s *Server) pluginsPayload() map[string]interface{} {
	c := s.Cfg.Get()
	return map[string]interface{}{
		"available":     s.Plugins != nil,
		"dir":           s.Plugins.Dir(),
		"api_version":   plugin.APIVersion,
		"hooks":         plugin.Hooks,
		"plugins":       s.Plugins.List(),
		"runs":          s.Plugins.Runs(50),
		"alert_enabled": c.Alert.Enabled,
		"mcp_read_only": c.MCP.ReadOnly,
	}
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request, arg string) error {
	m := r.Method
	if arg == "guide" && m == http.MethodGet {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.PluginDocMD)
		return nil
	}
	if arg == "" && m == http.MethodGet {
		writeJSON(w, s.pluginsPayload())
		return nil
	}
	if s.Plugins == nil {
		return badRequest("插件系统未启用")
	}
	switch {
	case arg == "reload" && m == http.MethodPost:
		n := s.Plugins.Reload()
		log.Printf("插件：重新扫描 %s，有效插件 %d 个（%s）", s.Plugins.Dir(), n, clientAddr(r))
		writeJSON(w, s.pluginsPayload())
		return nil
	case strings.HasSuffix(arg, "/test") && m == http.MethodPost:
		var req struct {
			Hook      string                     `json:"hook"`
			Tool      string                     `json:"tool"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if r.ContentLength != 0 {
			if err := decode(r, &req); err != nil {
				return err
			}
		}
		res, err := s.Plugins.Test(strings.TrimSuffix(arg, "/test"), req.Hook, req.Tool, req.Arguments)
		if err != nil {
			return badRequest("%v", err)
		}
		writeJSON(w, res)
		return nil
	case arg != "" && !strings.Contains(arg, "/") && m == http.MethodPut:
		var req struct {
			Enabled  *bool                  `json:"enabled"`
			Settings map[string]interface{} `json:"settings"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.Settings != nil {
			if err := s.Plugins.UpdateSettings(arg, req.Settings); err != nil {
				return badRequest("%v", err)
			}
		}
		if req.Enabled != nil {
			if err := s.Plugins.SetEnabled(arg, *req.Enabled); err != nil {
				return badRequest("%v", err)
			}
		}
		writeJSON(w, s.pluginsPayload())
		return nil
	}
	return &apiError{status: http.StatusNotFound, msg: "接口不存在: " + m + " " + r.URL.Path}
}

// pluginTools 把已启用插件的工具包装成 MCP 工具。
func (s *Server) pluginTools() []*mcpTool {
	var out []*mcpTool
	for _, t := range s.Plugins.Tools() {
		t := t
		out = append(out, &mcpTool{
			Name:        t.FullName,
			Title:       t.PluginName + " · " + t.Title,
			Description: "[插件 " + t.PluginName + "] " + t.Description,
			Schema:      t.InputSchema,
			Write:       t.Write,
			OpenWorld:   true, // 插件可以访问 netPing 以外的系统
			Run: func(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
				if t.Write {
					log.Printf("MCP[%s] 调用插件工具 %s", remote, t.FullName)
				}
				return s.Plugins.CallTool(t.FullName, a, remote)
			},
		})
	}
	return out
}

// lookupTool 按名字查找内置工具或已启用插件的工具。
func (s *Server) lookupTool(name string) *mcpTool {
	if t := findTool(name); t != nil {
		return t
	}
	if strings.HasPrefix(name, "plugin_") {
		for _, t := range s.pluginTools() {
			if t.Name == name {
				return t
			}
		}
	}
	return nil
}

// aiDiagnoseNames 是 AI 诊断可调用的工具：内置只读工具 + 插件声明了 ai: true 的只读工具。
func (s *Server) aiDiagnoseNames() []string {
	names := aiToolNames
	for _, t := range s.Plugins.Tools() {
		if t.AI && !t.Write {
			names = append(names[:len(names):len(names)], t.FullName)
		}
	}
	return names
}
