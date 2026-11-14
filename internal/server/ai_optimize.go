package server

// AI 一键优化（拓扑页）：把全部设备的资料与近 7 天统计交给模型，得到
// 1) 设备分类（写入标签）；2) 配置建议（告警开关、名称、阈值等，可勾选应用）；3) 需要检查的设备。
// 模型只给建议，修改由用户在网页上勾选后通过 /api/ai/optimize/apply 执行，并可撤销。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"netping/internal/llm"
	"netping/internal/topology"
)

// deviceCategories 设备类别；分类结果作为同名标签写入节点。
var deviceCategories = []string{
	"服务器", "虚拟机", "台式机", "笔记本", "手机平板", "打印机", "摄像头", "录像机", "无线AP",
	"路由器防火墙", "光猫", "网络存储", "IP电话", "门禁考勤", "智能设备", "工控设备", "其他",
}

func isCategory(c string) bool {
	for _, v := range deviceCategories {
		if v == c {
			return true
		}
	}
	return false
}

// nodeCategory 节点当前的类别标签。
func nodeCategory(n *topology.Node) string {
	for _, t := range n.Tags {
		if isCategory(t) {
			return t
		}
	}
	return ""
}

// 配置建议可执行的动作。
var optActions = map[string]string{
	"alert_on":      "开启告警",
	"alert_off":     "关闭告警",
	"flag_critical": "标记为关键设备",
	"rename":        "设置名称",
	"thresholds":    "调整告警阈值",
	"interval":      "调整 ping 间隔",
	"monitor_off":   "停止监控",
	"advice":        "人工处理",
}

const (
	optMaxDevices    = 400
	optMaxContext    = 90000
	optMaxRounds     = 4
	optMaxTokens     = 8192
	optMaxSuggestion = 60
	optMaxCheck      = 20
)

func optSystemPrompt(site string) string {
	return `你是 netPing 的拓扑优化助手。netPing 持续 ping 机房里的交换机和终端，并维护交换机-端口-设备的拓扑。
机房：` + site + `；当前时间：` + time.Now().Format("2006-01-02 15:04:05 (Mon)") + `。
用户在拓扑页点了「AI 一键优化」。下面给出全部设备的资料和近 7 天统计。你的建议会显示给用户逐条勾选后才生效，你不能直接修改配置。
如确有必要，可以调用只读工具补充数据（如 find_outages、correlate_events、get_uplink_path），但大部分结论应直接基于下面的表格，工具调用不超过 6 次。

请完成三件事：
1. 设备分类：只给终端节点（不含交换机）分类，类别只能从以下选择：` + strings.Join(deviceCategories, "、") + `。
   依据厂商（MAC 前缀判断）、主机名、名称、备注、标签、接线端口、IP 段规律。证据不足的不要分类；当前类别已正确的不要重复输出。
   常见线索：海康/大华/宇视→摄像头或录像机；惠普/佳能/兄弟/理光/爱普生且主机名含 PRN/NPI→打印机；Apple/小米/华为终端/OPPO/vivo 且随机 MAC→手机平板；
   VMware/QEMU/Hyper-V 前缀→虚拟机；群晖/威联通→网络存储；Ubiquiti/锐捷 AP/TP-Link AP→无线AP；主机名含 DESKTOP-/PC→台式机，含 LAPTOP-/MacBook→笔记本。
2. 配置建议：action 只能是：
   alert_on（开启告警：网关、核心/汇聚交换机、服务器、存储、录像机、门禁等关键设备）、alert_off（关闭告警：手机平板、笔记本等会正常离开的设备却开了告警）、
   flag_critical（标记关键设备）、rename（name：给没有名称的设备起简短中文名，必须有依据，如“财务部 HP 打印机”“3 号摄像头”，不确定不要起）、
   thresholds（latency_warn_ms 1~5000、loss_warn_pct 1~100：如无线/远端设备延迟天然较高、经常误报时放宽阈值）、
   interval（interval_ms 1000~60000：不重要但数量多的设备可放慢）、monitor_off（停止监控：已确认下线/无用的地址，慎用）、
   advice（不能自动执行的建议，如“未定位设备建议做端口定位”“交换机未配置 IP 无法监控”“同一端口下挂多台建议核实是否有小交换机”，id 可为空表示全局建议）。
   最多 ` + fmt.Sprint(optMaxSuggestion) + ` 条，按重要性排序；不要建议与现状相同的配置；每条 reason 一句话、引用依据。
3. 需要检查的设备：根据历史统计（丢包率、中断分钟数、中断次数、延迟与全网中位数对比、当前状态）挑出最值得现场检查的设备，最多 ` + fmt.Sprint(optMaxCheck) + ` 台，
   priority 为 high/medium/low，reason 写清数据（如“7 天丢包 3.2%，中断 46 分钟，共 12 次”），steps 写机房人员能动手的具体检查步骤。
   标记为 arp_only、停用监控、无 IP 的设备没有 ping 数据，不算故障。手机平板、笔记本离线通常是正常的，优先级应降低。

输出格式：先用 Markdown 写不超过 150 字的总体结论，然后输出且只输出一个 ` + "```json" + ` 代码块，结构如下（字段名必须一致，id 必须来自表格）：
{"classify":[{"category":"打印机","ids":["n-1","n-2"],"reason":"惠普厂商且主机名含 NPI"}],
 "config":[{"id":"n-3","action":"alert_on","reason":"..."},{"id":"n-4","action":"rename","name":"财务部打印机","reason":"..."},
           {"id":"n-5","action":"thresholds","latency_warn_ms":200,"loss_warn_pct":30,"reason":"..."},{"id":"","action":"advice","reason":"..."}],
 "check":[{"id":"n-6","priority":"high","reason":"...","steps":"..."}]}`
}

// optContext 汇总设备资料与近 7 天统计。
func (s *Server) optContext() (string, map[string]interface{}) {
	t, _ := s.Topo.Snapshot()
	sts := s.statusMap()
	cfg := s.Cfg.Get()
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "【默认 ping 参数】间隔 %dms，延迟阈值 %dms，丢包阈值 %.0f%%；企业微信告警通道：%v；监控时段：%s\n",
		cfg.Ping.IntervalMs, cfg.Ping.LatencyWarnMs, cfg.Ping.LossWarnPct, cfg.Alert.Enabled && cfg.Alert.HasChannels(), cfg.Schedule.Describe())
	fmt.Fprintf(&b, "网关：%s\n\n", t.Site.Gateway)

	b.WriteString("【网络设备（交换机/路由器/防火墙/光猫/AP 等）】id | 名称 | 类型 | 角色 | IP | 厂商/型号 | 位置 | 告警 | 状态 | 下挂节点数\n")
	attached := map[string]int{}
	for _, n := range t.Nodes {
		attached[n.Attach.Switch]++
	}
	for _, sw := range t.Switches {
		st := "unmonitored"
		if x, ok := sts[sw.ID]; ok {
			st = x.State
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s %s | %s | %v | %s | %d\n", sw.ID, sw.Name, sw.TypeName(), sw.Role, orDefault(sw.IP, "无IP"), sw.Vendor, sw.Model, sw.Location, sw.Monitor.Alert, st, attached[sw.ID])
	}

	b.WriteString("\n【终端节点】id | IP | MAC | 厂商 | 主机名 | 名称 | 位置 | 接线 | 同端口台数 | 标记 | 标签 | 备注 | 告警 | 阈值(延迟ms/丢包%) | 当前状态 | 7天丢包% | 7天平均ms | 7天峰值ms | 7天中断分钟 | 7天异常次数 | 24h丢包%\n")
	portCnt := map[string]int{}
	for _, n := range t.Nodes {
		if n.Attach.Switch != "" && n.Attach.Port != "" {
			portCnt[n.Attach.Switch+"|"+n.Attach.Port]++
		}
	}
	var losses, avgs []float64
	stats, categorized := 0, 0
	for i, n := range t.Nodes {
		if i >= optMaxDevices {
			fmt.Fprintf(&b, "……另有 %d 个节点未列出\n", len(t.Nodes)-i)
			break
		}
		if nodeCategory(n) != "" {
			categorized++
		}
		st := "unmonitored"
		if x, ok := sts[n.ID]; ok {
			st = x.State
		}
		th := "默认"
		if n.Monitor.LatencyWarnMs > 0 || n.Monitor.LossWarnPct > 0 {
			th = fmt.Sprintf("%d/%.0f", n.Monitor.LatencyWarnMs, n.Monitor.LossWarnPct)
		}
		stat := "- | - | - | - | - | -"
		if n.IP != "" && n.Monitor.IsEnabled() && stats < analysisMaxDevices {
			stats++
			if ms, err := s.minuteSeries(n.ID, now.Add(-7*24*time.Hour), now); err == nil && len(ms) > 0 {
				w := statMinutes(ms)
				day := statMinutes(ms[sort.Search(len(ms), func(k int) bool { return ms[k].T >= now.Add(-24*time.Hour).Unix() }):])
				// 异常次数：连续的丢包分钟记为一次
				episodes := 0
				var prev int64 = -1
				for _, m := range ms {
					if m.Sent > 0 && m.Recv < m.Sent {
						if m.T-prev > 60 {
							episodes++
						}
						prev = m.T
					}
				}
				stat = fmt.Sprintf("%.2f | %.1f | %.0f | %d | %d | %.2f", w.lossPct(), w.avg(), w.max, w.downMin, episodes, day.lossPct())
				losses = append(losses, w.lossPct())
				if w.recv > 0 {
					avgs = append(avgs, w.avg())
				}
			}
		}
		note := truncRunes(strings.ReplaceAll(n.Note, "\n", " "), 40)
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s | %s | %s | %d | %s | %s | %s | %v | %s | %s | %s\n",
			n.ID, orDefault(n.IP, "无IP"), n.MAC, n.Vendor, n.Hostname, n.Name, n.Location, t.AttachLabel(n.Attach),
			portCnt[n.Attach.Switch+"|"+n.Attach.Port], strings.Join(n.Flags, ","), strings.Join(n.Tags, ","), note,
			n.Monitor.Alert, th, st, stat)
	}
	med := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		sort.Float64s(v)
		return v[len(v)/2]
	}
	fmt.Fprintf(&b, "\n【全网参照】7 天丢包率中位数 %.2f%%，平均延迟中位数 %.1fms（“-”表示没有 ping 数据）\n", med(losses), med(avgs))
	text := b.String()
	if len(text) > optMaxContext {
		cut := text[:optMaxContext]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		text = cut + "\n……（设备太多，后面的已省略）"
	}
	return text, map[string]interface{}{"switches": len(t.Switches), "nodes": len(t.Nodes), "categorized": categorized, "with_stats": stats}
}

// ---------------------------------------------------------------------------
// 模型结果解析与校验

type optRaw struct {
	Summary  string `json:"summary"`
	Classify []struct {
		Category string   `json:"category"`
		IDs      []string `json:"ids"`
		Reason   string   `json:"reason"`
	} `json:"classify"`
	Config []optItem `json:"config"`
	Check  []struct {
		ID       string `json:"id"`
		Priority string `json:"priority"`
		Reason   string `json:"reason"`
		Steps    string `json:"steps"`
	} `json:"check"`
}

// optItem 一条配置建议（也是 apply 接口的请求项）。
type optItem struct {
	ID            string  `json:"id"`
	Action        string  `json:"action"`
	Name          string  `json:"name,omitempty"`
	LatencyWarnMs int     `json:"latency_warn_ms,omitempty"`
	LossWarnPct   float64 `json:"loss_warn_pct,omitempty"`
	IntervalMs    int     `json:"interval_ms,omitempty"`
	Reason        string  `json:"reason,omitempty"`
	// 以下由服务器填写
	Kind       string `json:"kind,omitempty"`
	Label      string `json:"label,omitempty"`
	Title      string `json:"title,omitempty"`
	Change     string `json:"change,omitempty"`
	Applicable bool   `json:"applicable"`
}

// extractJSON 取出回复中的 JSON（```json 代码块，或第一个 { 到最后一个 }），返回 JSON 和之前的说明文字。
func extractJSON(text string) (string, string) {
	if i := strings.LastIndex(text, "```json"); i >= 0 {
		rest := text[i+7:]
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest), strings.TrimSpace(text[:i])
	}
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i >= 0 && j > i {
		return text[i : j+1], strings.TrimSpace(strings.TrimRight(text[:i], "`"))
	}
	return "", strings.TrimSpace(text)
}

// devMon 返回设备的监控参数（节点或交换机）。
func devMon(t *topology.Topology, id string) (kind string, mon *topology.MonitorSpec, name string) {
	if n := t.FindNode(id); n != nil {
		return "node", &n.Monitor, n.Name
	}
	if sw := t.FindSwitch(id); sw != nil {
		return "switch", &sw.Monitor, sw.Name
	}
	return "", nil, ""
}

// checkItem 校验并补全一条配置建议；返回 false 表示应丢弃（无效或与现状相同）。
func checkItem(t *topology.Topology, it *optItem) bool {
	it.Name = strings.TrimSpace(it.Name)
	it.Reason = truncRunes(strings.TrimSpace(it.Reason), 200)
	title, ok := optActions[it.Action]
	if !ok {
		return false
	}
	it.Title = title
	if it.Action == "advice" {
		if it.ID != "" {
			if k, _, _ := devMon(t, it.ID); k == "" {
				it.ID = ""
			}
		}
		if it.ID != "" {
			name, ip, _ := deviceLabel(t, it.ID)
			it.Label = devText(name, ip)
			it.Kind, _, _ = devMon(t, it.ID)
		}
		it.Applicable = false
		return it.Reason != ""
	}
	kind, mon, curName := devMon(t, it.ID)
	if kind == "" {
		return false
	}
	it.Kind = kind
	name, ip, _ := deviceLabel(t, it.ID)
	it.Label = devText(name, ip)
	it.Applicable = true
	switch it.Action {
	case "alert_on", "alert_off":
		want := it.Action == "alert_on"
		if mon.Alert == want || !mon.IsEnabled() && want {
			return false
		}
		it.Change = map[bool]string{true: "告警：关 → 开", false: "告警：开 → 关"}[want]
	case "flag_critical":
		n := t.FindNode(it.ID)
		if n == nil || n.HasFlag("critical") {
			return false
		}
		it.Change = "增加标记 critical"
	case "rename":
		if it.Name == "" || utf8.RuneCountInString(it.Name) > 40 || it.Name == curName {
			return false
		}
		it.Change = fmt.Sprintf("名称：%s → %s", orDefault(curName, "（空）"), it.Name)
	case "thresholds":
		if it.LatencyWarnMs < 0 || it.LatencyWarnMs > 5000 || it.LossWarnPct < 0 || it.LossWarnPct > 100 || it.LatencyWarnMs == 0 && it.LossWarnPct == 0 {
			return false
		}
		var parts []string
		if it.LatencyWarnMs > 0 && it.LatencyWarnMs != mon.LatencyWarnMs {
			parts = append(parts, fmt.Sprintf("延迟阈值 %s → %dms", orDefault(intStr(mon.LatencyWarnMs), "默认"), it.LatencyWarnMs))
		}
		if it.LossWarnPct > 0 && it.LossWarnPct != mon.LossWarnPct {
			parts = append(parts, fmt.Sprintf("丢包阈值 %s → %.0f%%", orDefault(floatStr(mon.LossWarnPct), "默认"), it.LossWarnPct))
		}
		if len(parts) == 0 {
			return false
		}
		it.Change = strings.Join(parts, "，")
	case "interval":
		if it.IntervalMs < 1000 || it.IntervalMs > 60000 || it.IntervalMs == mon.IntervalMs {
			return false
		}
		it.Change = fmt.Sprintf("ping 间隔 %s → %dms", orDefault(intStr(mon.IntervalMs), "默认"), it.IntervalMs)
	case "monitor_off":
		if !mon.IsEnabled() {
			return false
		}
		it.Change = "停止 ping 监控"
	}
	return true
}

// devText 设备的展示文字：名称与 IP（名称就是 IP 时不重复）。
func devText(name, ip string) string {
	if ip == "" || name == ip {
		return name
	}
	return strings.TrimSpace(name + " " + ip)
}

func intStr(v int) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%dms", v)
}

func floatStr(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f%%", v)
}

// buildOptResult 校验模型输出，只保留引用了真实设备、且会带来变化的建议。
func buildOptResult(t *topology.Topology, raw *optRaw) map[string]interface{} {
	type dev struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Current string `json:"current,omitempty"`
	}
	var groups []map[string]interface{}
	assigned := map[string]bool{}
	for _, g := range raw.Classify {
		cat := strings.TrimSpace(g.Category)
		if !isCategory(cat) {
			continue
		}
		var devs []dev
		for _, id := range g.IDs {
			n := t.FindNode(id)
			if n == nil || assigned[id] || nodeCategory(n) == cat {
				continue
			}
			assigned[id] = true
			devs = append(devs, dev{ID: id, Label: devText(n.DisplayName(), n.IP), Current: nodeCategory(n)})
		}
		if len(devs) > 0 {
			groups = append(groups, map[string]interface{}{"category": cat, "reason": truncRunes(g.Reason, 200), "devices": devs})
		}
	}
	var cfg []optItem
	seen := map[string]bool{}
	for _, it := range raw.Config {
		key := it.ID + "|" + it.Action
		if it.Action == "advice" {
			key += "|" + it.Reason
		}
		if seen[key] || len(cfg) >= optMaxSuggestion {
			continue
		}
		if checkItem(t, &it) {
			seen[key] = true
			cfg = append(cfg, it)
		}
	}
	var checks []map[string]interface{}
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	for _, c := range raw.Check {
		if len(checks) >= optMaxCheck {
			break
		}
		if t.FindNode(c.ID) == nil && t.FindSwitch(c.ID) == nil {
			continue
		}
		p := c.Priority
		if _, ok := rank[p]; !ok {
			p = "medium"
		}
		name, ip, attach := deviceLabel(t, c.ID)
		checks = append(checks, map[string]interface{}{"id": c.ID, "label": devText(name, ip), "attach": attach,
			"priority": p, "reason": truncRunes(c.Reason, 300), "steps": truncRunes(c.Steps, 400)})
	}
	sort.SliceStable(checks, func(i, j int) bool {
		return rank[checks[i]["priority"].(string)] < rank[checks[j]["priority"].(string)]
	})
	return map[string]interface{}{"classify": groups, "config": cfg, "check": checks, "categories": deviceCategories}
}

// ---------------------------------------------------------------------------
// HTTP

func (s *Server) handleAIOptimize(w http.ResponseWriter, r *http.Request) error {
	c := s.Cfg.Get()
	if !c.AI.Enabled {
		return badRequest("AI 诊断未启用（在「AI 诊断」页开启）")
	}
	if c.AI.APIKey == "" {
		return badRequest("尚未设置 API Key，请先在「AI 诊断」页右侧填写")
	}
	select {
	case aiSem <- struct{}{}:
		defer func() { <-aiSem }()
	default:
		return &apiError{status: http.StatusTooManyRequests, msg: "同时进行的 AI 分析太多，请稍后再试"}
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
	start := time.Now()
	timeout := time.Duration(c.AI.TimeoutSec) * time.Second
	if timeout < 3*time.Minute {
		timeout = 3 * time.Minute // 设备多时输出较长
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	emit(map[string]interface{}{"t": "status", "text": "正在汇总设备资料和近 7 天历史数据…"})
	ctxText, stats := s.optContext()
	emit(map[string]interface{}{"t": "stats", "stats": stats})
	t, _ := s.Topo.Snapshot()
	site := t.Site.Name
	if t.Site.Subnet != "" {
		site += "（" + t.Site.Subnet + "）"
	}
	msgs := []llm.Message{
		{Role: "system", Content: optSystemPrompt(site)},
		{Role: "user", Content: ctxText + "\n请按要求给出设备分类、配置建议和需要检查的设备。"},
	}
	res, err := s.aiLoop(ctx, c, msgs, optMaxRounds, optMaxTokens, "AI优化/"+clientAddr(r), emit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		log.Printf("AI 一键优化失败（%s）：%v", clientAddr(r), err)
		emit(map[string]interface{}{"t": "error", "error": err.Error()})
		return nil
	}
	js, summary := extractJSON(res.Final)
	var raw optRaw
	if js == "" || json.Unmarshal([]byte(js), &raw) != nil {
		log.Printf("AI 一键优化：模型返回格式不正确（%d 字）", utf8.RuneCountInString(res.Final))
		emit(map[string]interface{}{"t": "error", "error": "模型返回的结果格式不正确（可能输出被截断），请重试；设备很多时可在「AI 诊断」页关闭深度思考或换用输出更长的模型。"})
		return nil
	}
	if summary == "" {
		summary = raw.Summary
	}
	t, rev := s.Topo.Snapshot()
	out := buildOptResult(t, &raw)
	out["summary"] = summary
	out["rev"] = rev
	out["time"] = time.Now().Format("2006-01-02 15:04:05")
	ms := time.Since(start).Milliseconds()
	log.Printf("AI 一键优化（%s，%s）：分类 %d 组、建议 %d 条、待检查 %d 台 —— 工具 %d 次，%d tokens，%.1f 秒",
		clientAddr(r), c.AI.Model, len(out["classify"].([]map[string]interface{})), len(out["config"].([]optItem)),
		len(out["check"].([]map[string]interface{})), res.Calls, res.Usage.TotalTokens, float64(ms)/1000)
	emit(map[string]interface{}{"t": "result", "result": out})
	emit(map[string]interface{}{"t": "done", "usage": res.Usage, "tools": res.Calls, "ms": ms, "model": c.AI.Model})
	return nil
}

// devSnap 设备中可能被优化修改的字段，用于撤销。
type devSnap struct {
	ID      string               `json:"id"`
	Kind    string               `json:"kind"`
	Name    string               `json:"name"`
	Tags    []string             `json:"tags"`
	Flags   []string             `json:"flags"`
	Monitor topology.MonitorSpec `json:"monitor"`
}

func snapOf(t *topology.Topology, id string) (devSnap, bool) {
	if n := t.FindNode(id); n != nil {
		return devSnap{ID: id, Kind: "node", Name: n.Name, Tags: append([]string(nil), n.Tags...), Flags: append([]string(nil), n.Flags...), Monitor: n.Monitor}, true
	}
	if sw := t.FindSwitch(id); sw != nil {
		return devSnap{ID: id, Kind: "switch", Name: sw.Name, Monitor: sw.Monitor}, true
	}
	return devSnap{}, false
}

func (s *Server) handleAIOptimizeApply(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Classify []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"classify"`
		Config  []optItem `json:"config"`
		Restore []devSnap `json:"restore"` // 撤销：恢复为之前的快照
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if len(req.Classify)+len(req.Config)+len(req.Restore) == 0 {
		return badRequest("没有选择任何建议")
	}
	var undo []devSnap
	changed := map[string]bool{}
	var skipped []string
	err := s.Topo.Update(func(t *topology.Topology) error {
		snapped := map[string]bool{}
		snap := func(id string) {
			if !snapped[id] {
				if sn, ok := snapOf(t, id); ok {
					undo = append(undo, sn)
					snapped[id] = true
				}
			}
		}
		for _, sn := range req.Restore {
			snap(sn.ID)
			if n := t.FindNode(sn.ID); n != nil {
				n.Name, n.Tags, n.Flags, n.Monitor = sn.Name, sn.Tags, sn.Flags, sn.Monitor
				changed[sn.ID] = true
			} else if sw := t.FindSwitch(sn.ID); sw != nil {
				sw.Name, sw.Monitor = sn.Name, sn.Monitor
				changed[sn.ID] = true
			}
		}
		for _, c := range req.Classify {
			n := t.FindNode(c.ID)
			if n == nil || !isCategory(c.Category) {
				skipped = append(skipped, c.ID)
				continue
			}
			if nodeCategory(n) == c.Category {
				continue
			}
			snap(c.ID)
			tags := []string{}
			for _, tg := range n.Tags {
				if !isCategory(tg) {
					tags = append(tags, tg)
				}
			}
			n.Tags = append(tags, c.Category)
			changed[c.ID] = true
		}
		for _, it := range req.Config {
			if !checkItem(t, &it) || !it.Applicable {
				skipped = append(skipped, it.ID)
				continue
			}
			snap(it.ID)
			_, mon, _ := devMon(t, it.ID)
			switch it.Action {
			case "alert_on", "alert_off":
				mon.Alert = it.Action == "alert_on"
			case "flag_critical":
				n := t.FindNode(it.ID)
				n.Flags = append(n.Flags, "critical")
			case "rename":
				if n := t.FindNode(it.ID); n != nil {
					n.Name = it.Name
				} else {
					t.FindSwitch(it.ID).Name = it.Name
				}
			case "thresholds":
				if it.LatencyWarnMs > 0 {
					mon.LatencyWarnMs = it.LatencyWarnMs
				}
				if it.LossWarnPct > 0 {
					mon.LossWarnPct = it.LossWarnPct
				}
			case "interval":
				mon.IntervalMs = it.IntervalMs
			case "monitor_off":
				f := false
				mon.Enabled = &f
			}
			changed[it.ID] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	verb := "应用 AI 优化建议"
	if len(req.Restore) > 0 {
		verb = "撤销 AI 优化"
	}
	log.Printf("%s（来源 %s）：修改 %d 台设备，跳过 %d 条", verb, clientAddr(r), len(changed), len(skipped))
	s.okRev(w, map[string]interface{}{"changed": len(changed), "skipped": len(skipped), "undo": undo})
	return nil
}
