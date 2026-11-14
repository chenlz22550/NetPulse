package server

// AI 自主发现（扫描发现页）：由大模型自己决定探测哪些网段、识别哪些设备、查它们接在哪个端口，
// 最后给出“新增设备 / 新增扫描任务 / 其他发现”的清单。模型只能调用只读工具，
// 清单经服务器校验（地址必须是本次或最近实际探测到、且尚未登记的）后，由用户勾选应用，可撤销。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/llm"
	"netping/internal/topology"
)

// 自主发现时模型可以调用的工具（全部只读）。
var discoverToolNames = []string{
	"get_network_context", "probe_subnet", "fingerprint_host",
	"get_discovery", "list_devices", "get_device", "get_locate_status", "get_switch_mac_table", "locate_device",
	"tcp_check", "arp_lookup", "dns_lookup", "trace_route", "ping_now",
}

const (
	discMaxRounds   = 14
	discMaxCalls    = 36
	discMaxTokens   = 8192
	discMaxDevices  = 120
	discMaxJobs     = 8
	discMaxNotes    = 12
	discFlag        = "ai_discovered"
	discDefaultBudg = 8 // 默认最多探测的 /24 数量
)

func discSystemPrompt(site string, budget int) string {
	return `你是 netPing 的设备发现助手。netPing 持续 ping 机房里的交换机和终端，并维护交换机-端口-设备的拓扑。
机房：` + site + `；当前时间：` + time.Now().Format("2006-01-02 15:04:05 (Mon)") + `。
用户在「扫描发现」页点了「AI 自主发现」：请你自己规划并完成一次设备发现，找出网络里还没有登记的设备，判断它们是什么、接在哪里，给出添加建议。
你只能调用只读工具，所有修改都由用户勾选确认后才执行。

工作步骤：
1. 下面已给出 get_network_context 的结果（本机网卡、ARP 分组、拓扑分组、扫描任务、候选网段）。结合用户的要求，决定要探测哪些网段：
   优先 candidate_subnets、用户点名的网段、ARP 中未登记地址较多的网段；跨网段时可从网关、已有设备所在网段推断相邻网段（如 .1.0/24、.2.0/24），但要有依据，不要盲目遍历。
   本次最多探测约 ` + fmt.Sprint(budget) + ` 个 /24（probe_subnet 每次最多 4 个 /24）。已被扫描任务覆盖的网段也可探测，用于找出未登记设备。
2. 用 probe_subnet 探测，只关注 status=new 的地址（moved 是已登记设备换了 IP，正常扫描会自动处理，最多在 notes 里提一句）。
3. 用 fingerprint_host（每次最多 8 个）识别值得识别的新设备：优先厂商未知、主机名为空、可能是网络设备/服务器/摄像头/打印机的地址；手机、随机 MAC 的设备不必逐个识别。
   新设备总数很多时，挑代表性的识别，其余凭厂商/主机名判断并降低 confidence。
4. 有 MAC 的新设备可用 locate_device（参数 mac）查它接在哪台交换机的哪个端口；交换机没配置 SNMP 时跳过这一步。
5. 根据 SNMP 描述（sys_descr 含 Switch/交换机/Comware/VRP/RGOS 等）、端口（22/23/80/161 组合）判断是网管型交换机的，kind 写 switch。
工具调用总共不超过 ` + fmt.Sprint(discMaxCalls-4) + ` 次，探测够用就停止。

最终输出：先用 Markdown 写不超过 200 字的总结（探测了哪些网段、发现多少新设备、主要类型、值得注意的地方），然后输出且只输出一个 ` + "```json" + ` 代码块：
{"devices":[{"ip":"192.168.1.23","kind":"node","name":"财务部 HP 打印机","category":"打印机","alert":false,"switch":"sw-2","port":"12","confidence":"high","evidence":"9100 开放；网页标题 HP LaserJet M404；locate_device 定位到 sw-2 口 12"}],
 "scan_jobs":[{"name":"二楼办公网段","subnets":["192.168.2.0/24"],"interval_min":30,"auto_add":true,"reason":"该网段有 23 台未登记设备且没有扫描任务覆盖"}],
 "notes":["192.168.1.50 是已登记设备 n-xxx 换了 IP，下次扫描会自动更新"]}
规则：
- devices 只能包含本次 probe_subnet / fingerprint_host 实际看到、status 为 new 的地址；最多 ` + fmt.Sprint(discMaxDevices) + ` 台；不要编造 IP、MAC。
- kind：node（终端）或 switch（带端口的网络设备）。kind=switch 时 type 写设备类型：switch 交换机、router 路由器、firewall 防火墙、gateway 上网行为管理、modem 光猫、ap 无线AP、ac 无线控制器、lb 负载均衡、other；只在证据明确（SNMP 描述、网页标题、型号）时把防火墙/路由器/光猫/AP 写成 switch，否则按 node 并给 category。category（仅 node）只能是：` + strings.Join(deviceCategories, "、") + `，证据不足留空。
- name：简短中文名，必须有依据（如“3 号摄像头 海康”“财务部打印机”），没有依据就用“厂商 + 类别”，不要留空。
- alert：服务器、存储、录像机、网关、防火墙、交换机、门禁等关键设备为 true；手机平板、笔记本、台式机为 false。
- switch/port：只有 locate_device 实际查到时才填（switch 为拓扑中的交换机 id）。
- confidence：high（有端口/网页/SNMP 等直接证据）、medium（凭厂商或主机名）、low（几乎没有信息）。evidence 写具体证据，一句话。
- scan_jobs：给有设备但没有扫描任务覆盖的网段建议定时扫描任务（每个网段最大 /20）；已有任务覆盖的不要重复。
- notes：其他值得告诉用户的发现（IP 冲突、疑似私接路由器/无线 AP、不可达的网段、需要配置 SNMP 才能定位等），每条一句话。`
}

// ---------------------------------------------------------------------------
// 结果校验

type discDevice struct {
	IP         string `json:"ip"`
	Kind       string `json:"kind"`
	Type       string `json:"type,omitempty"` // kind=switch 时的设备类型
	Name       string `json:"name"`
	Category   string `json:"category,omitempty"`
	Alert      bool   `json:"alert"`
	Switch     string `json:"switch,omitempty"`
	Port       string `json:"port,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	// 以下由服务器按实际观察填写
	MAC      string   `json:"mac,omitempty"`
	Vendor   string   `json:"vendor,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	Ping     bool     `json:"ping"`
	Model    string   `json:"model,omitempty"`
	Hints    []string `json:"hints,omitempty"`
	Attach   string   `json:"attach,omitempty"` // 接线的显示文字
}

type discJob struct {
	Name        string   `json:"name"`
	Subnets     []string `json:"subnets"`
	IntervalMin int      `json:"interval_min"`
	AutoAdd     bool     `json:"auto_add"`
	Reason      string   `json:"reason,omitempty"`
}

type discRaw struct {
	Summary  string          `json:"summary"`
	Devices  []discDevice    `json:"devices"`
	ScanJobs []discJob       `json:"scan_jobs"`
	Notes    json.RawMessage `json:"notes"`
}

// notesOf 兼容字符串数组与 [{"text":..}] 两种写法。
func notesOf(raw json.RawMessage) []string {
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return ss
	}
	// Unmarshal 失败时可能已分配并部分填充 ss，切换格式前清除残留。
	ss = nil
	var objs []map[string]interface{}
	if json.Unmarshal(raw, &objs) == nil {
		for _, o := range objs {
			for _, k := range []string{"text", "note", "reason"} {
				if v, ok := o[k].(string); ok {
					ss = append(ss, v)
					break
				}
			}
		}
	}
	return ss
}

// checkDiscDevice 校验并补全一条新增设备建议；返回空字符串表示有效，否则返回跳过原因。
func (s *Server) checkDiscDevice(t *topology.Topology, d *discDevice) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(d.IP))
	if err != nil || !ip.Is4() {
		return "IP 无效"
	}
	d.IP = ip.String()
	h, ok := s.observed(d.IP)
	if !ok {
		return "最近没有实际探测到该地址"
	}
	d.MAC, d.Vendor, d.Hostname, d.Ping = h.MAC, h.Vendor, h.Hostname, h.Ping || !h.ARP
	if len(h.SharedMAC) > 0 {
		d.MAC = ""
	}
	d.Model, d.Hints = h.Model, h.Hints
	if id := registeredBy(t, d.IP, d.MAC); id != "" {
		return "已在拓扑中（" + id + "）"
	}
	if d.Kind != "switch" {
		d.Kind = "node"
	}
	d.Type = strings.ToLower(strings.TrimSpace(d.Type))
	if d.Kind != "switch" || !inList(topology.DeviceTypes, d.Type) {
		d.Type = ""
	}
	if d.Type == "switch" {
		d.Type = ""
	}
	d.Category = strings.TrimSpace(d.Category)
	if d.Kind == "switch" || !isCategory(d.Category) {
		d.Category = ""
	}
	d.Name = strings.TrimSpace(d.Name)
	if utf8.RuneCountInString(d.Name) > 40 {
		d.Name = string([]rune(d.Name)[:40])
	}
	if d.Name == "" {
		d.Name = strings.TrimSpace(orDefault(d.Hostname, d.Vendor+" "+d.Category))
	}
	switch d.Confidence {
	case "high", "medium", "low":
	default:
		d.Confidence = "medium"
	}
	d.Evidence = truncRunes(strings.TrimSpace(d.Evidence), 300)
	d.Switch, d.Port = strings.TrimSpace(d.Switch), strings.TrimSpace(d.Port)
	d.Attach = ""
	if d.Kind == "switch" || d.Switch == "" || d.Port == "" {
		d.Switch, d.Port = "", ""
	} else if sw := t.FindSwitch(d.Switch); sw == nil || (len(sw.Ports) > 0 && !inList(sw.Ports, d.Port)) {
		d.Switch, d.Port = "", "" // 端口不存在：按未定位添加
	} else {
		d.Attach = t.AttachLabel(topology.Endpoint{Switch: d.Switch, Port: d.Port})
	}
	return ""
}

// checkDiscJob 校验扫描任务建议：网段格式、探测范围、是否已被已有任务覆盖。
func (s *Server) checkDiscJob(cfg config.Config, j *discJob) string {
	var covered []netip.Prefix
	for _, ex := range cfg.Discovery.Jobs {
		for _, x := range s.Disc.JobSubnets(ex) {
			if p, err := netip.ParsePrefix(x); err == nil {
				covered = append(covered, p.Masked())
			}
		}
	}
	var subs []string
	for _, x := range j.Subnets {
		c, err := s.checkProbeSubnet(x)
		if err != nil {
			continue
		}
		p := netip.MustParsePrefix(c)
		if prefixWithin(p, covered) || inList(subs, c) {
			continue
		}
		subs = append(subs, c)
	}
	if len(subs) == 0 {
		return "网段无效或已被现有任务覆盖"
	}
	if _, err := config.ExpandSubnets(subs, nil); err != nil {
		return err.Error()
	}
	j.Subnets = subs
	j.Name = truncRunes(strings.TrimSpace(j.Name), 40)
	if j.Name == "" {
		j.Name = strings.Join(subs, "、")
	}
	if j.IntervalMin < 5 || j.IntervalMin > 1440 {
		j.IntervalMin = 30
	}
	j.Reason = truncRunes(strings.TrimSpace(j.Reason), 200)
	return ""
}

func (s *Server) buildDiscResult(raw *discRaw) map[string]interface{} {
	t, _ := s.Topo.Snapshot()
	cfg := s.Cfg.Get()
	devs := []discDevice{}
	var dropped []map[string]string
	seen := map[string]bool{}
	for _, d := range raw.Devices {
		if len(devs) >= discMaxDevices {
			break
		}
		if seen[strings.TrimSpace(d.IP)] {
			continue
		}
		seen[strings.TrimSpace(d.IP)] = true
		if why := s.checkDiscDevice(t, &d); why != "" {
			dropped = append(dropped, map[string]string{"ip": d.IP, "reason": why})
			continue
		}
		devs = append(devs, d)
	}
	jobs := []discJob{}
	for _, j := range raw.ScanJobs {
		if len(jobs) >= discMaxJobs {
			break
		}
		if s.checkDiscJob(cfg, &j) == "" {
			jobs = append(jobs, j)
		}
	}
	notes := []string{}
	for _, n := range notesOf(raw.Notes) {
		if n = truncRunes(strings.TrimSpace(n), 300); n != "" && len(notes) < discMaxNotes {
			notes = append(notes, n)
		}
	}
	return map[string]interface{}{"devices": devs, "scan_jobs": jobs, "notes": notes, "dropped": dropped, "categories": deviceCategories}
}

// ---------------------------------------------------------------------------
// HTTP：运行

func (s *Server) handleAIDiscover(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Hint    string   `json:"hint"`    // 用户的要求（如“重点找二楼的摄像头”）
		Subnets []string `json:"subnets"` // 指定要探测的网段（可空，由模型决定）
		Budget  int      `json:"budget"`  // 最多探测多少个 /24
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	var subs []string
	for _, x := range trimSubnets(req.Subnets) {
		c, err := s.checkProbeSubnet(x)
		if err != nil {
			return badRequest("%v", err)
		}
		subs = append(subs, c)
	}
	budget := req.Budget
	if budget <= 0 {
		budget = discDefaultBudg
	}
	budget = clampInt(budget, 1, 32)
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
	if timeout < 6*time.Minute {
		timeout = 6 * time.Minute // 探测网段和识别设备本身需要时间
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	remote := "AI发现/" + clientAddr(r)

	emit(map[string]interface{}{"t": "status", "text": "正在汇总网络线索…"})
	nc, _ := toolNetworkContext(s, nil, remote)
	t, _ := s.Topo.Snapshot()
	site := t.Site.Name
	if t.Site.Subnet != "" {
		site += "（" + t.Site.Subnet + "，网关 " + t.Site.Gateway + "）"
	}
	if m, ok := nc.(map[string]interface{}); ok {
		if cs, ok := m["candidate_subnets"].([]map[string]interface{}); ok {
			emit(map[string]interface{}{"t": "stats", "stats": map[string]interface{}{"candidates": len(cs), "nodes": len(t.Nodes), "switches": len(t.Switches)}})
		}
	}
	user := "【get_network_context】\n" + aiJSON(nc, 24000) + "\n\n"
	if len(subs) > 0 {
		user += "【用户指定要探测的网段】" + strings.Join(subs, "、") + "（先探测这些；除非用户要求，不必再探测别的网段）\n"
	}
	if h := strings.TrimSpace(req.Hint); h != "" {
		user += "【用户的要求】" + truncRunes(h, 500) + "\n"
	}
	user += "请开始自主发现，最后按要求输出总结和 JSON 清单。"
	msgs := []llm.Message{
		{Role: "system", Content: discSystemPrompt(site, budget)},
		{Role: "user", Content: user},
	}
	res, err := s.aiLoopWith(ctx, c, msgs, discoverToolNames, discMaxRounds, discMaxCalls, discMaxTokens, remote, emit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		log.Printf("AI 自主发现失败（%s）：%v", clientAddr(r), err)
		emit(map[string]interface{}{"t": "error", "error": err.Error()})
		return nil
	}
	js, summary := extractJSON(res.Final)
	var raw discRaw
	if js == "" || json.Unmarshal([]byte(js), &raw) != nil {
		log.Printf("AI 自主发现：模型返回格式不正确（%d 字）", utf8.RuneCountInString(res.Final))
		emit(map[string]interface{}{"t": "error", "error": "模型返回的结果格式不正确（可能输出被截断），请重试；也可以指定网段缩小范围。", "summary": summary})
		return nil
	}
	if summary == "" {
		summary = raw.Summary
	}
	out := s.buildDiscResult(&raw)
	out["summary"] = summary
	out["time"] = time.Now().Format("2006-01-02 15:04:05")
	ms := time.Since(start).Milliseconds()
	log.Printf("AI 自主发现（%s，%s）：新设备 %d 台、扫描任务 %d 个、其他发现 %d 条、丢弃 %d 条 —— 工具 %d 次，%d tokens，%.1f 秒",
		clientAddr(r), c.AI.Model, len(out["devices"].([]discDevice)), len(out["scan_jobs"].([]discJob)), len(out["notes"].([]string)),
		len(out["dropped"].([]map[string]string)), res.Calls, res.Usage.TotalTokens, float64(ms)/1000)
	emit(map[string]interface{}{"t": "result", "result": out})
	emit(map[string]interface{}{"t": "done", "usage": res.Usage, "tools": res.Calls, "ms": ms, "model": c.AI.Model})
	return nil
}

func trimSubnets(in []string) []string {
	var out []string
	for _, x := range in {
		for _, y := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '、' }) {
			if y = strings.TrimSpace(y); y != "" {
				out = append(out, y)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP：应用与撤销

type discUndo struct {
	Nodes    []string `json:"nodes"`
	Switches []string `json:"switches"`
	Jobs     []string `json:"jobs"`
}

func (s *Server) handleAIDiscoverApply(w http.ResponseWriter, r *http.Request) error {
	defer s.Topo.ProfileWork()()
	var req struct {
		Devices  []discDevice `json:"devices"`
		ScanJobs []discJob    `json:"scan_jobs"`
		Undo     *discUndo    `json:"undo"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Undo != nil {
		return s.discUndo(w, r, *req.Undo)
	}
	if len(req.Devices)+len(req.ScanJobs) == 0 {
		return badRequest("没有选择任何建议")
	}
	now := time.Now()
	var undo discUndo
	var skipped []map[string]string
	located := 0
	unlocated := false
	if len(req.Devices) > 0 {
		err := s.Topo.Update(func(t *topology.Topology) error {
			undo.Nodes, undo.Switches, skipped, located, unlocated = nil, nil, nil, 0, false
			for _, d := range req.Devices {
				if why := s.checkDiscDevice(t, &d); why != "" {
					skipped = append(skipped, map[string]string{"ip": d.IP, "reason": why})
					continue
				}
				note := "AI 自主发现于 " + now.Format("2006-01-02 15:04")
				if d.Evidence != "" {
					note += "：" + d.Evidence
				}
				if d.Kind == "switch" {
					sw := &topology.Switch{Name: d.Name, Type: d.Type, Role: "access", IP: d.IP, MAC: d.MAC, Vendor: d.Vendor,
						Model: truncPlain(d.Model, 60), Note: note}
					sw.Monitor.Alert = d.Alert
					sw.ID = t.NewSwitchID(sw)
					t.Switches = append(t.Switches, sw)
					undo.Switches = append(undo.Switches, sw.ID)
					continue
				}
				n := discovery.NewNode(t, discovery.Host{IP: d.IP, MAC: d.MAC, Vendor: d.Vendor, Hostname: d.Hostname, Ping: d.Ping, ARP: d.MAC != ""}, now)
				n.Name = d.Name
				n.Note = strings.Replace(n.Note, "网段扫描自动发现于 "+now.Format("2006-01-02 15:04"), note, 1)
				var flags []string // 用 ai_discovered 代替扫描添加的 discovered 标记
				for _, f := range n.Flags {
					if f != "discovered" {
						flags = append(flags, f)
					}
				}
				n.Flags = append(flags, discFlag)
				if d.Category != "" {
					n.Tags = append(n.Tags, d.Category)
				}
				if d.Alert && n.Monitor.IsEnabled() {
					n.Monitor.Alert = true
				}
				if d.Switch != "" {
					n.Attach = topology.Endpoint{Switch: d.Switch, Port: d.Port}
					var fl []string
					for _, f := range n.Flags {
						if f != "unlocated" {
							fl = append(fl, f)
						}
					}
					n.Flags = fl
					located++
				} else {
					unlocated = true
				}
				t.Nodes = append(t.Nodes, n)
				undo.Nodes = append(undo.Nodes, n.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(req.ScanJobs) > 0 {
		before := map[string]bool{}
		for _, j := range s.Cfg.Get().Discovery.Jobs {
			before[j.ID] = true
		}
		_, err := s.updateConfig(func(c *config.Config) error {
			for _, j := range req.ScanJobs {
				if why := s.checkDiscJob(*c, &j); why != "" {
					skipped = append(skipped, map[string]string{"job": strings.Join(j.Subnets, "、"), "reason": why})
					continue
				}
				nj := config.DefaultJob()
				nj.ID, nj.Name, nj.Subnets, nj.IntervalMin, nj.AutoAdd = "", j.Name, j.Subnets, j.IntervalMin, j.AutoAdd
				c.Discovery.Jobs = append(c.Discovery.Jobs, nj)
			}
			return nil
		})
		if err != nil {
			return badRequest("%v", err)
		}
		for _, j := range s.Cfg.Get().Discovery.Jobs {
			if !before[j.ID] {
				undo.Jobs = append(undo.Jobs, j.ID)
			}
		}
	}
	if unlocated && s.Disc.OnAdded != nil {
		s.Disc.OnAdded() // 未定位的新设备：触发一次端口定位
	}
	log.Printf("应用 AI 自主发现（来源 %s）：新增节点 %d、交换机 %d、扫描任务 %d，跳过 %d 条", clientAddr(r),
		len(undo.Nodes), len(undo.Switches), len(undo.Jobs), len(skipped))
	s.okRev(w, map[string]interface{}{"nodes": len(undo.Nodes), "switches": len(undo.Switches), "jobs": len(undo.Jobs),
		"located": located, "skipped": skipped, "undo": undo, "discovery_enabled": s.Cfg.Get().Discovery.Enabled})
	return nil
}

// discUndo 撤销：只删除由 AI 自主发现添加的节点/交换机，以及本次新建的扫描任务。
func (s *Server) discUndo(w http.ResponseWriter, r *http.Request, u discUndo) error {
	removed := 0
	if len(u.Nodes)+len(u.Switches) > 0 {
		err := s.Topo.Update(func(t *topology.Topology) error {
			removed = 0
			for _, id := range u.Nodes {
				if n := t.FindNode(id); n != nil && n.HasFlag(discFlag) && !n.Locked && t.DeleteNode(id) {
					removed++
				}
			}
			for _, id := range u.Switches {
				if sw := t.FindSwitch(id); sw != nil && strings.HasPrefix(sw.Note, "AI 自主发现于") {
					if _, ok := t.DeleteSwitch(id); ok {
						removed++
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	jobs := 0
	if len(u.Jobs) > 0 {
		_, err := s.updateConfig(func(c *config.Config) error {
			keep := c.Discovery.Jobs[:0]
			for _, j := range c.Discovery.Jobs {
				if inList(u.Jobs, j.ID) {
					jobs++
					continue
				}
				keep = append(keep, j)
			}
			c.Discovery.Jobs = keep
			return nil
		})
		if err != nil {
			return badRequest("%v", err)
		}
	}
	log.Printf("撤销 AI 自主发现（来源 %s）：删除设备 %d、扫描任务 %d", clientAddr(r), removed, jobs)
	s.okRev(w, map[string]interface{}{"removed": removed, "jobs": jobs})
	return nil
}

func truncPlain(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
