package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"netping/internal/config"
	"netping/internal/llm"
)

// AI 诊断：把当前概况交给大模型，并允许它调用只读查询工具（与 MCP 共用实现）补充数据，流式返回诊断结论。

// aiTools 允许模型调用的只读工具。
var aiToolNames = []string{
	"get_overview", "list_devices", "get_device", "get_history", "list_alerts",
	"get_settings", "get_discovery", "get_locate_status", "get_switch_mac_table", "locate_device",
	// 分析类
	"analyze_impact", "get_uplink_path", "find_outages", "correlate_events", "rank_devices", "compare_baseline", "ping_now",
	// 现场核实类
	"get_switch_ports", "tcp_check", "arp_lookup", "trace_route", "dns_lookup",
}

const (
	aiMaxRounds     = 10    // 最多几轮工具调用
	aiMaxToolCalls  = 24    // 一次诊断最多调用工具次数
	aiMaxToolResult = 16000 // 单个工具结果最多字符数
	aiMaxHistory    = 20    // 对话最多保留条数
	aiMaxMsgLen     = 8000
)

// aiSem 同时进行的诊断数量上限。
var aiSem = make(chan struct{}, 3)

func aiSystemPrompt(site string) string {
	return `你是 netPing 的机房网络诊断助手。netPing 持续 ping 机房里的交换机和终端，记录延迟、抖动、丢包，并维护交换机-端口-设备的拓扑。
机房：` + site + `；当前时间：` + time.Now().Format("2006-01-02 15:04:05 (Mon)") + `。

你可以调用只读工具查询实时状态、历史曲线、告警记录、网段扫描和端口定位结果；你不能修改任何配置。
分析工具（优先用它们代替逐台查询）：
- 多台设备异常 → analyze_impact（按交换机/端口归类，找最靠近核心的公共故障点、同一时刻开始异常的设备组）；
- 单台设备 → get_uplink_path（逐跳看上联交换机和同端口设备）、find_outages（历史每段中断的起止时间、可用率、按小时分布）、compare_baseline（与昨天/上周同时段对比，判断是否变差）；
- “是不是一起掉的”、停电/重启判断 → correlate_events（按分钟对齐多台设备的异常）；
- “最近哪些设备最不稳定” → rank_devices（按丢包/延迟/中断时长排行，附全网中位数）；
- 需要确认当前是否真的不通、或测试外网出口 → ping_now（从 netPing 主机实时 ping，最多 10 次）。
现场核实工具（从 netPing 主机实时探测，用来验证判断，而不是只凭历史数据推测）：
- tcp_check：ping 不通时测常用 TCP 端口——有端口开放或拒绝连接，说明主机在线、只是禁 ping；也可确认打印机、摄像头、远程桌面等服务；
- arp_lookup：同网段设备是否仍回应 ARP、MAC 是否与登记一致（发现 IP 冲突、设备被更换）；
- get_switch_ports：通过 SNMP 读交换机端口的 up/down、速率、双工、错误/丢弃计数的增长、最近状态变化、带宽占用和交换机运行时长（核实网线/端口故障、双工不匹配、闪断、拥塞、交换机重启）；
- trace_route：到外网或跨网段地址逐跳追踪，看断在哪一跳；
- dns_lookup：域名解析是否正常，可指定 DNS 服务器对比。
要求：
1. 先看「当前概况」，需要细节时再调用工具（如某台设备的历史曲线、某个交换机下的设备、最近告警）。不要臆测没有查到的数据；工具调用尽量少而准。
2. 用简体中文回答，结构：**结论**（1~3 句）→ **依据**（关键数据：设备名/IP、数值、时间；注明哪些是刚才实测的、哪些是推断）→ **建议排查**（按可能性排序并标注把握：高/中/低，给机房运维人员能动手执行的具体步骤）。一般不超过 400 字，用户要求详细时再展开。
3. 判断思路：同一交换机或同一端口下多台同时异常 → 优先怀疑该交换机、上联链路或该端口；单台异常 → 设备本身、网线、端口；网关异常会影响整个网段；只丢包不离线 → 链路质量、双工不匹配、拥塞、无线信号；延迟高 → 负载、无线、环路或广播风暴；多台设备同一时刻离线又恢复 → 停电、交换机重启或上联中断。
4. 状态含义：ok 正常、slow 延迟高、loss 丢包、down 离线、unknown 刚开始监控、paused 暂停、unmonitored 未监控。标记为「仅 ARP」或停用监控、无 IP 的设备本来就没有 ping 数据，不算故障。监控暂停或不在监控时段时没有新数据。
5. 如果一切正常，直接说明正常，并指出值得留意的小问题（如个别设备偶发丢包）；数据不足以下结论时说明还需要什么信息。
6. 先核实再下结论：说某台设备“现在离线/不通”前用 ping_now 实测；ping 不通时用 tcp_check 或 arp_lookup 区分“真离线”和“禁 ping”；归因到端口、网线、双工、拥塞或交换机重启前，用 get_switch_ports 找证据（未配置 SNMP 时说明无法核实）。能用工具核实的，不要只凭推测。`
}

func (s *Server) aiClient(c config.Config) *llm.Client {
	proxy := c.AI.ProxyURL
	if proxy == "" {
		proxy = c.Alert.ProxyURL
	}
	return &llm.Client{
		BaseURL: c.AI.BaseURL, APIKey: c.AI.APIKey, Model: c.AI.Model, Thinking: c.AI.Thinking,
		ProxyURL: proxy, TLSSkipVerify: c.Alert.TLSSkipVerify, Timeout: time.Duration(c.AI.TimeoutSec) * time.Second,
	}
}

func aiTools() []llm.Tool {
	var out []llm.Tool
	for _, name := range aiToolNames {
		if t := findTool(name); t != nil {
			out = append(out, llm.Tool{Type: "function", Function: llm.ToolFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
		}
	}
	return out
}

func aiAllowed(name string) bool {
	for _, n := range aiToolNames {
		if n == name {
			return true
		}
	}
	return false
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…（已截断）"
}

func aiJSON(v interface{}, limit int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"结果无法序列化"}`
	}
	if len(b) > limit {
		// 按字节截断后保证仍是合法 UTF-8 文本
		cut := string(b[:limit])
		for !utf8.ValidString(cut) && len(cut) > 0 {
			cut = cut[:len(cut)-1]
		}
		return cut + "…（结果过长已截断，可用更精确的参数重新查询）"
	}
	return string(b)
}

// runAITool 执行模型发起的工具调用。
func (s *Server) runAITool(name, args, remote string) (string, string) {
	t := findTool(name)
	if t == nil || !aiAllowed(name) || t.Write {
		return "", `{"error":"不允许调用该工具"}`
	}
	a := map[string]json.RawMessage{}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return t.Title, `{"error":"参数不是有效的 JSON：` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`
		}
	}
	res, err := t.Run(s, a, remote)
	if err != nil {
		return t.Title, aiJSON(map[string]string{"error": err.Error()}, 2000)
	}
	return t.Title, aiJSON(res, aiMaxToolResult)
}

type aiChatReq struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Focus string `json:"focus"` // 重点诊断的设备 id（可空）
}

func (s *Server) handleAI(w http.ResponseWriter, r *http.Request, arg string) error {
	switch {
	case arg == "settings" && r.Method == http.MethodGet:
		writeJSON(w, aiSettingsView(s.Cfg.Get().AI))
		return nil
	case arg == "settings" && r.Method == http.MethodPut:
		return s.handleAISettings(w, r)
	case arg == "test" && r.Method == http.MethodPost:
		return s.handleAITest(w, r)
	case arg == "diagnose" && r.Method == http.MethodPost:
		return s.handleAIDiagnose(w, r)
	case arg == "optimize" && r.Method == http.MethodPost:
		return s.handleAIOptimize(w, r)
	case arg == "optimize/apply" && r.Method == http.MethodPost:
		return s.handleAIOptimizeApply(w, r)
	}
	return notFound("接口")
}

func keyHint(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "已设置"
	}
	return k[:3] + "…" + k[len(k)-4:]
}

func aiSettingsView(a config.AIConfig) map[string]interface{} {
	return map[string]interface{}{
		"enabled": a.Enabled, "base_url": a.BaseURL, "model": a.Model, "thinking": a.Thinking,
		"proxy_url": a.ProxyURL, "timeout_sec": a.TimeoutSec, "self_check": a.SelfCheckOn(),
		"api_key_set": a.APIKey != "", "api_key_hint": keyHint(a.APIKey),
		"ready": a.Enabled && a.APIKey != "",
	}
}

// aiPublicSettings 供 MCP get_settings 返回的 AI 设置（不含 API Key 与代理）。
func aiPublicSettings(a config.AIConfig) map[string]interface{} {
	return map[string]interface{}{
		"enabled": a.Enabled, "model": a.Model, "base_url": a.BaseURL, "thinking": a.Thinking,
		"self_check": a.SelfCheckOn(), "timeout_sec": a.TimeoutSec, "api_key_set": a.APIKey != "",
	}
}

// toolSetAISettings MCP 修改 AI 诊断设置；API Key、接口地址、代理不允许通过 MCP 修改（防止把 Key 发往别的地址）。
func toolSetAISettings(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	for _, k := range []string{"api_key", "base_url", "proxy_url", "clear_key"} {
		if _, ok := a[k]; ok {
			return nil, fmt.Errorf("不能通过 MCP 修改 %s，请在网页「AI 诊断」页修改", k)
		}
	}
	var changed []string
	c, err := s.updateConfig(func(c *config.Config) error {
		ai := &c.AI
		if v, ok := argBool(a, "enabled"); ok {
			ai.Enabled = v
			changed = append(changed, "enabled")
		}
		if v, ok := argBool(a, "thinking"); ok {
			ai.Thinking = v
			changed = append(changed, "thinking")
		}
		if v, ok := argBool(a, "self_check"); ok {
			ai.SelfCheck = &v
			changed = append(changed, "self_check")
		}
		if m := strings.TrimSpace(argStr(a, "model")); m != "" {
			ai.Model = m
			changed = append(changed, "model")
		}
		if _, ok := a["timeout_sec"]; ok {
			v := argInt(a, "timeout_sec", 0)
			if v < 10 || v > 600 {
				return fmt.Errorf("timeout_sec 必须在 10~600 之间")
			}
			ai.TimeoutSec = v
			changed = append(changed, "timeout_sec")
		}
		if len(changed) == 0 {
			return fmt.Errorf("没有要修改的字段（可选 enabled、thinking、self_check、model、timeout_sec）")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改 AI 诊断设置：%s", remote, strings.Join(changed, "、"))
	return map[string]interface{}{"ok": true, "changed": changed, "ai": aiPublicSettings(c.AI)}, nil
}

func (s *Server) handleAISettings(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Enabled    *bool   `json:"enabled"`
		BaseURL    *string `json:"base_url"`
		Model      *string `json:"model"`
		Thinking   *bool   `json:"thinking"`
		ProxyURL   *string `json:"proxy_url"`
		TimeoutSec *int    `json:"timeout_sec"`
		SelfCheck  *bool   `json:"self_check"`
		APIKey     string  `json:"api_key"`   // 非空时替换
		ClearKey   bool    `json:"clear_key"` // 清除已保存的 Key
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	c := s.Cfg.Get()
	a := &c.AI
	if req.Enabled != nil {
		a.Enabled = *req.Enabled
	}
	if req.BaseURL != nil {
		a.BaseURL = *req.BaseURL
	}
	if req.Model != nil {
		a.Model = *req.Model
	}
	if req.Thinking != nil {
		a.Thinking = *req.Thinking
	}
	if req.ProxyURL != nil {
		a.ProxyURL = strings.TrimSpace(*req.ProxyURL)
	}
	if req.TimeoutSec != nil {
		a.TimeoutSec = *req.TimeoutSec
	}
	if req.SelfCheck != nil {
		v := *req.SelfCheck
		a.SelfCheck = &v
	}
	if k := strings.TrimSpace(req.APIKey); k != "" {
		a.APIKey = k
	} else if req.ClearKey {
		a.APIKey = ""
	}
	if err := s.Cfg.Update(c); err != nil {
		return badRequest("%v", err)
	}
	log.Printf("AI 诊断设置已修改（来源 %s，模型 %s）", clientAddr(r), s.Cfg.Get().AI.Model)
	writeJSON(w, aiSettingsView(s.Cfg.Get().AI))
	return nil
}

func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) error {
	c := s.Cfg.Get()
	cl := s.aiClient(c)
	cl.Thinking = false
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	msg, usage, err := cl.Stream(ctx, llm.Request{
		Messages:  []llm.Message{{Role: "user", Content: "请只回复“连接正常”四个字。"}},
		MaxTokens: 20,
	}, nil)
	if err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "reply": strings.TrimSpace(msg.Content), "model": c.AI.Model, "ms": time.Since(start).Milliseconds(), "usage": usage})
	return nil
}

func (s *Server) handleAIDiagnose(w http.ResponseWriter, r *http.Request) error {
	c := s.Cfg.Get()
	if !c.AI.Enabled {
		return badRequest("AI 诊断未启用")
	}
	if c.AI.APIKey == "" {
		return badRequest("尚未设置 API Key，请先在「AI 诊断」页右侧填写")
	}
	var req aiChatReq
	if err := decode(r, &req); err != nil {
		return err
	}
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" {
		return badRequest("缺少问题")
	}
	if len(req.Messages) > aiMaxHistory {
		req.Messages = req.Messages[len(req.Messages)-aiMaxHistory:]
	}
	select {
	case aiSem <- struct{}{}:
		defer func() { <-aiSem }()
	default:
		return &apiError{status: http.StatusTooManyRequests, msg: "同时进行的诊断太多，请稍后再试"}
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	emit := func(v map[string]interface{}) {
		_ = enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}

	remote := "AI诊断/" + clientAddr(r)
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(c.AI.TimeoutSec)*time.Second)
	defer cancel()

	// 背景：当前概况（必要时加上重点设备的详情与近 1 小时统计）
	t, _ := s.Topo.Snapshot()
	site := t.Site.Name
	if t.Site.Subnet != "" {
		site += "（" + t.Site.Subnet + "，网关 " + t.Site.Gateway + "）"
	}
	emit(map[string]interface{}{"t": "status", "text": "正在汇总当前监控数据…"})
	ov, _ := toolOverview(s, nil, remote)
	ctxText := "【当前概况】\n" + aiJSON(ov, 12000)
	if req.Focus != "" {
		idArg, _ := json.Marshal(req.Focus)
		if dv, err := toolGetDevice(s, map[string]json.RawMessage{"id": idArg}, remote); err == nil {
			ctxText += "\n\n【重点设备】\n" + aiJSON(dv, 6000)
		}
		rng, _ := json.Marshal("1h")
		if h, err := toolHistory(s, map[string]json.RawMessage{"id": idArg, "range": rng, "points": json.RawMessage("30")}, remote); err == nil {
			ctxText += "\n\n【重点设备近 1 小时】\n" + aiJSON(h, 6000)
		}
	}

	msgs := []llm.Message{{Role: "system", Content: aiSystemPrompt(site) + "\n\n" + ctxText}}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		msgs = append(msgs, llm.Message{Role: m.Role, Content: truncRunes(m.Content, aiMaxMsgLen)})
	}
	question := truncRunes(req.Messages[len(req.Messages)-1].Content, 60)

	res, err := s.aiLoop(ctx, c, msgs, aiMaxRounds, 0, remote, emit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("AI 诊断已取消（%s）：%s", clientAddr(r), question)
			return nil
		}
		log.Printf("AI 诊断失败（%s）：%v", clientAddr(r), err)
		emit(map[string]interface{}{"t": "error", "error": err.Error()})
		return nil
	}
	calls, total := res.Calls, res.Usage
	review := "off"
	if c.AI.SelfCheckOn() {
		rv := s.aiSelfCheck(ctx, c, res, remote, emit)
		calls += rv.Calls
		total.Add(rv.Usage)
		review = rv.Result
		if errors.Is(ctx.Err(), context.Canceled) {
			log.Printf("AI 诊断已取消（%s）：%s", clientAddr(r), question)
			return nil
		}
	}
	ms := time.Since(start).Milliseconds()
	log.Printf("AI 诊断（%s，%s）：%s —— 工具 %d 次，%d tokens，%.1f 秒，自检 %s", clientAddr(r), c.AI.Model, question, calls, total.TotalTokens, float64(ms)/1000, reviewText[review])
	emit(map[string]interface{}{"t": "done", "usage": total, "tools": calls, "ms": ms, "model": c.AI.Model, "review": review})
	return nil
}

type aiRunResult struct {
	Final string        // 最后一轮（不再调用工具）的回复
	Msgs  []llm.Message // 最后一轮之前的完整对话（含工具调用与结果），供自检继续使用
	Calls int
	Usage llm.Usage
}

// aiLoop 与模型多轮对话：模型可调用只读工具，直到给出不含工具调用的回复或达到轮数上限。
func (s *Server) aiLoop(ctx context.Context, c config.Config, msgs []llm.Message, maxRounds, maxTokens int, remote string, emit func(map[string]interface{})) (aiRunResult, error) {
	cl := s.aiClient(c)
	tools := aiTools()
	var res aiRunResult
	for round := 0; ; round++ {
		if round > 0 {
			emit(map[string]interface{}{"t": "status", "text": "正在分析…"})
		} else {
			emit(map[string]interface{}{"t": "status", "text": "正在请求模型…"})
		}
		useTools := tools
		if round >= maxRounds || res.Calls >= aiMaxToolCalls {
			useTools = nil // 最后一轮不再给工具，强制给出结论
		}
		msg, usage, err := cl.Stream(ctx, llm.Request{Messages: msgs, Tools: useTools, MaxTokens: maxTokens}, func(d string) {
			emit(map[string]interface{}{"t": "delta", "text": d})
		})
		res.Usage.Add(usage)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("AI 分析超时（超过 %d 秒），可以稍后重试或在「AI 诊断」页调大超时", c.AI.TimeoutSec)
			}
			return res, err
		}
		if len(msg.ToolCalls) == 0 || useTools == nil {
			res.Final, res.Msgs = msg.Content, msgs
			return res, nil
		}
		msgs = append(msgs, msg)
		res.Calls += len(msg.ToolCalls)
		msgs = append(msgs, s.aiExecCalls(msg.ToolCalls, remote, "", emit)...)
		emit(map[string]interface{}{"t": "break"})
	}
}

// aiExecCalls 执行一轮工具调用，返回对应的 tool 消息；phase 非空时附加在 tool 事件上（如自检阶段的 "review"）。
func (s *Server) aiExecCalls(calls []llm.ToolCall, remote, phase string, emit func(map[string]interface{})) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, tc := range calls {
		title, result := s.runAITool(tc.Function.Name, tc.Function.Arguments, remote)
		ev := map[string]interface{}{"t": "tool", "name": tc.Function.Name, "title": title, "args": truncRunes(tc.Function.Arguments, 300)}
		if phase != "" {
			ev["phase"] = phase
		}
		emit(ev)
		out = append(out, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
	}
	return out
}
