package server

// 分析类只读工具：在原始查询工具之上做聚合与关联分析，供 AI 诊断和 MCP 客户端使用。
// 包括：实时 ping、上联路径、故障影响面、中断时段、多设备同时异常、历史排行、与基线对比。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"time"

	"netping/internal/pinger"
	"netping/internal/store"
	"netping/internal/topology"
)

const analysisMaxDevices = 300 // 批量分析最多涉及的设备数

func analysisTools() []*mcpTool {
	return []*mcpTool{
		{
			Name: "ping_now", Title: "立即 ping 测试",
			Description: "从 netPing 所在主机立即对设备或任意 IPv4 地址 ping 若干次，返回每次延迟与丢包统计，并附上该设备的监控数据对比。用于确认“现在到底通不通”、验证修复效果，或测试未纳入监控的地址（如外网 223.5.5.5 判断出口）。",
			Schema: obj(withIDIP(map[string]interface{}{
				"count":      integer("ping 次数，默认 5，最多 10"),
				"timeout_ms": integer("单次超时毫秒，默认 1000（200~3000）"),
				"size":       integer("数据包字节数，默认 32（8~1472）；用大包可以测试 MTU 或链路质量"),
			})),
			Run: toolPingNow,
		},
		{
			Name: "get_uplink_path", Title: "上联路径",
			Description: "按拓扑链路给出设备到核心交换机的逐跳路径：每一跳交换机的实时状态、下挂设备异常数、上联端口，以及同一端口下的其他设备。用于判断问题出在设备本身、接入端口还是上联链路。",
			Schema:      obj(withIDIP(nil)),
			Run:         toolUplinkPath,
		},
		{
			Name: "analyze_impact", Title: "故障影响面分析",
			Description: "把当前异常设备按交换机（含下游交换机）和端口归类，计算每台交换机下的异常比例，找出最可能的公共故障点（交换机/上联/端口），并把同一时刻开始异常的设备分组。多台设备同时出问题时先调用它。",
			Schema: obj(map[string]interface{}{
				"state": enum("统计口径：abnormal（默认，离线+丢包+延迟高）或 down（只看离线）", "abnormal", "down"),
			}),
			Run: toolAnalyzeImpact,
		},
		{
			Name: "find_outages", Title: "中断与丢包时段",
			Description: "从历史数据中提取设备每一段离线/丢包的开始、结束、持续时长和丢包率，统计可用率、累计中断时长，并按小时统计异常分布（用于发现每天固定时段掉线）。15m/1h 为秒级精度，更长范围为分钟级。",
			Schema: obj(withIDIP(map[string]interface{}{
				"range":        enum("时间范围，默认 24h", ranges6...),
				"min_loss_pct": map[string]interface{}{"type": "number", "description": "分钟丢包率超过多少算异常（仅分钟级数据），默认 0 即有丢包就算"},
				"limit":        integer("最多返回多少段，默认 30，最大 100（新的在前）"),
			})),
			Run: toolFindOutages,
		},
		{
			Name: "correlate_events", Title: "多设备同时异常分析",
			Description: "按分钟对齐多台设备的丢包/离线，找出多台设备同一时间出问题的时段（并按交换机归类），以及每台设备的异常有多少是与其他设备同时发生的。不指定 ids/switch 时分析全部监控设备。用于区分停电、交换机重启、上联中断等公共故障与单台设备问题。",
			Schema: obj(map[string]interface{}{
				"ids":          strArr("要对比的设备 id 列表（可选）"),
				"switch":       str("分析该交换机（id）及其下挂的全部设备（可选）"),
				"range":        enum("时间范围，默认 24h", "1h", "6h", "24h", "7d"),
				"min_devices":  integer("同一分钟至少几台异常才算公共事件，默认 2"),
				"min_loss_pct": map[string]interface{}{"type": "number", "description": "分钟丢包率达到多少算该分钟异常，默认 20"},
				"limit":        integer("最多返回多少个公共事件，默认 20，最大 50"),
			}),
			Run: toolCorrelateEvents,
		},
		{
			Name: "rank_devices", Title: "历史稳定性排行",
			Description: "按指定时间范围统计所有（或某交换机下）监控设备的丢包率、平均/峰值延迟、中断分钟数、抖动并排行，同时给出全网中位数作为参照。用于回答“最近一天/一周哪些设备最不稳定”。",
			Schema: obj(map[string]interface{}{
				"range":  enum("时间范围，默认 24h", "1h", "6h", "24h", "7d"),
				"metric": enum("排序指标：loss（丢包率，默认）、latency（平均延迟）、downtime（中断分钟数）、jitter（抖动）", "loss", "latency", "downtime", "jitter"),
				"switch": str("只统计接在该交换机（id）上的设备"),
				"limit":  integer("返回条数，默认 15，最大 50"),
			}),
			Run: toolRankDevices,
		},
		{
			Name: "compare_baseline", Title: "与历史基线对比",
			Description: "把设备最近一段时间的延迟和丢包与昨天同时段、7 天前同时段、过去 7 天整体水平对比，并给出变化幅度。用于判断“现在的延迟算不算异常、是不是变差了”。",
			Schema: obj(withIDIP(map[string]interface{}{
				"window": enum("对比窗口，默认 1h", "15m", "1h", "6h"),
			})),
			Run: toolCompareBaseline,
		},
	}
}

// ---------------------------------------------------------------------------
// 数据辅助

// minuteSeries 返回 [from, to) 的分钟数据：已落盘部分来自历史库，尚未落盘的最近几分钟由内存样本补齐。
func (s *Server) minuteSeries(id string, from, to time.Time) ([]store.Minute, error) {
	recs, err := s.Store.Query(id, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	since := from.UnixMilli()
	var last int64 = math.MinInt64
	if len(recs) > 0 {
		last = recs[len(recs)-1].T
		if (last+60)*1000 > since {
			since = (last + 60) * 1000
		}
	}
	if since >= to.UnixMilli() {
		return recs, nil
	}
	var cur *store.Minute
	var sum float64
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Recv > 0 {
			cur.Avg = r2(sum / float64(cur.Recv))
		}
		recs = append(recs, *cur)
	}
	for _, sm := range s.Mon.Recent(id, since) {
		if sm.T >= to.UnixMilli() {
			break
		}
		mt := sm.T / 60000 * 60
		if mt <= last {
			continue
		}
		if cur == nil || cur.T != mt {
			flush()
			cur = &store.Minute{T: mt}
			sum = 0
		}
		cur.Sent++
		if sm.RTT >= 0 {
			if cur.Recv == 0 || sm.RTT < cur.Min {
				cur.Min = sm.RTT
			}
			if sm.RTT > cur.Max {
				cur.Max = sm.RTT
			}
			cur.Recv++
			sum += sm.RTT
		}
	}
	flush()
	return recs, nil
}

// mstat 一段分钟数据的统计。
type mstat struct {
	minutes, sent, recv, downMin, lossMin int
	sum, min, max, spread                 float64
	avgs                                  []float64
}

func statMinutes(ms []store.Minute) *mstat {
	st := &mstat{min: math.Inf(1)}
	for _, m := range ms {
		if m.Sent <= 0 {
			continue
		}
		st.minutes++
		st.sent += m.Sent
		st.recv += m.Recv
		if m.Recv == 0 {
			st.downMin++
			continue
		}
		if m.Recv < m.Sent {
			st.lossMin++
		}
		st.sum += m.Avg * float64(m.Recv)
		st.avgs = append(st.avgs, m.Avg)
		st.spread += m.Max - m.Min
		if m.Min < st.min {
			st.min = m.Min
		}
		if m.Max > st.max {
			st.max = m.Max
		}
	}
	return st
}

func (st *mstat) lossPct() float64 {
	if st.sent == 0 {
		return 0
	}
	return r2(float64(st.sent-st.recv) * 100 / float64(st.sent))
}

func (st *mstat) avg() float64 {
	if st.recv == 0 {
		return 0
	}
	return r2(st.sum / float64(st.recv))
}

func (st *mstat) jitter() float64 {
	if len(st.avgs) == 0 {
		return 0
	}
	return r2(st.spread / float64(len(st.avgs)))
}

func (st *mstat) p95() float64 {
	if len(st.avgs) == 0 {
		return 0
	}
	v := append([]float64(nil), st.avgs...)
	sort.Float64s(v)
	return r2(v[int(math.Ceil(float64(len(v))*0.95))-1])
}

func (st *mstat) view() map[string]interface{} {
	out := map[string]interface{}{"minutes_with_data": st.minutes, "samples": st.sent, "lost": st.sent - st.recv}
	if st.sent == 0 {
		out["note"] = "没有数据（未监控、暂停/非监控时段或刚开始监控）"
		return out
	}
	out["loss_pct"] = st.lossPct()
	out["down_minutes"], out["loss_minutes"] = st.downMin, st.lossMin
	if st.recv > 0 {
		out["avg_ms"], out["min_ms"], out["max_ms"] = st.avg(), r2(st.min), r2(st.max)
		out["p95_minute_avg_ms"], out["jitter_ms"] = st.p95(), st.jitter()
	}
	return out
}

func pickRange(a map[string]json.RawMessage, k, def string, allowed ...string) (string, time.Duration, error) {
	rg := argStr(a, k)
	if rg == "" {
		rg = def
	}
	for _, v := range allowed {
		if v == rg {
			return rg, ranges[rg], nil
		}
	}
	return "", 0, fmt.Errorf("%s 只能是 %v", k, allowed)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func argFloat(a map[string]json.RawMessage, k string, def float64) float64 {
	var v float64
	if raw, ok := a[k]; ok && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return def
}

// deviceLabel 设备的展示名与 IP。
func deviceLabel(t *topology.Topology, id string) (name, ip, attach string) {
	if n := t.FindNode(id); n != nil {
		return n.DisplayName(), n.IP, t.AttachLabel(n.Attach)
	}
	if sw := t.FindSwitch(id); sw != nil {
		name = sw.Name
		if name == "" {
			name = sw.ID
		}
		return name, sw.IP, "交换机（" + sw.Role + "）"
	}
	return id, "", ""
}

// monitoredIDs 拓扑中有 IP 且启用监控的设备。
func monitoredIDs(t *topology.Topology, onSwitch string) []string {
	var ids []string
	for _, sw := range t.Switches {
		if sw.IP != "" && sw.Monitor.IsEnabled() && (onSwitch == "" || sw.ID == onSwitch) {
			ids = append(ids, sw.ID)
		}
	}
	for _, n := range t.Nodes {
		if n.IP != "" && n.Monitor.IsEnabled() && (onSwitch == "" || n.Attach.Switch == onSwitch) {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// 交换机图

type swEdge struct{ to, localPort, remotePort, link string }

func switchGraph(t *topology.Topology) map[string][]swEdge {
	g := map[string][]swEdge{}
	for _, l := range t.Links {
		if l.From.Switch == "" || l.To.Switch == "" || l.From.Switch == l.To.Switch {
			continue
		}
		g[l.From.Switch] = append(g[l.From.Switch], swEdge{l.To.Switch, l.From.Port, l.To.Port, l.ID})
		g[l.To.Switch] = append(g[l.To.Switch], swEdge{l.From.Switch, l.To.Port, l.From.Port, l.ID})
	}
	return g
}

// rootSwitches 路径终点：role=core 的交换机；没有时用网关所在交换机。
func rootSwitches(t *topology.Topology) []string {
	var roots []string
	for _, sw := range t.Switches {
		if sw.Role == "core" {
			roots = append(roots, sw.ID)
		}
	}
	if len(roots) == 0 && t.Site.Gateway != "" {
		switch k, id := t.FindByIP(t.Site.Gateway); k {
		case "switch":
			roots = append(roots, id)
		case "node":
			if n := t.FindNode(id); n != nil && n.Attach.Switch != "" {
				roots = append(roots, n.Attach.Switch)
			}
		}
	}
	return roots
}

// switchTree 从核心出发的 BFS 树：parent[子]=父，edge[子]=子到父的链路；order 为 BFS 顺序（未连通的交换机各自为根）。
func switchTree(t *topology.Topology) (parent map[string]string, edge map[string]swEdge, order []string) {
	g := switchGraph(t)
	parent, edge = map[string]string{}, map[string]swEdge{}
	seen := map[string]bool{}
	var queue []string
	push := func(id string) {
		if !seen[id] && t.FindSwitch(id) != nil {
			seen[id] = true
			queue = append(queue, id)
		}
	}
	for _, r := range rootSwitches(t) {
		push(r)
	}
	bfs := func() {
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			order = append(order, cur)
			for _, e := range g[cur] {
				if !seen[e.to] && t.FindSwitch(e.to) != nil {
					parent[e.to] = cur
					edge[e.to] = swEdge{to: cur, localPort: e.remotePort, remotePort: e.localPort, link: e.link}
					push(e.to)
				}
			}
		}
	}
	bfs()
	for _, sw := range t.Switches {
		push(sw.ID)
		bfs()
	}
	return
}

// ---------------------------------------------------------------------------
// ping_now

func toolPingNow(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	id, ip := argStr(a, "id"), argStr(a, "ip")
	if id != "" {
		_, rid, err := resolveDevice(t, id, "")
		if err != nil {
			return nil, err
		}
		id = rid
		_, ip, _ = deviceLabel(t, id)
		if ip == "" {
			return nil, errors.New("该设备没有 IP，无法 ping")
		}
	} else if ip == "" {
		return nil, errors.New("需要提供 id 或 ip")
	} else if _, rid := t.FindByIP(ip); rid != "" {
		id = rid
	}
	p4 := net.ParseIP(ip).To4()
	if p4 == nil {
		return nil, fmt.Errorf("%q 不是 IPv4 地址", ip)
	}
	if p4.IsUnspecified() || p4.IsMulticast() || p4.Equal(net.IPv4bcast) {
		return nil, errors.New("不能 ping 广播、组播或全零地址")
	}
	if _, nw, err := net.ParseCIDR(t.Site.Subnet); err == nil && len(nw.Mask) == 4 {
		bc := make(net.IP, 4)
		for i := range bc {
			bc[i] = nw.IP.To4()[i] | ^nw.Mask[i]
		}
		if p4.Equal(bc) {
			return nil, errors.New("不能 ping 网段广播地址")
		}
	}
	count := clampInt(argInt(a, "count", 5), 1, 10)
	timeout := time.Duration(clampInt(argInt(a, "timeout_ms", 1000), 200, 3000)) * time.Millisecond
	size := clampInt(argInt(a, "size", 32), 8, 1472)

	p, err := pinger.New()
	if err != nil {
		return nil, fmt.Errorf("无法创建 ping：%v", err)
	}
	defer p.Close()
	var rtts []interface{}
	var got []float64
	var lastErr string
	for i := 0; i < count; i++ {
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		r, err := p.Ping(p4.String(), timeout, size)
		if err != nil {
			lastErr = err.Error()
		}
		if r.OK {
			ms := r2(float64(r.RTT) / float64(time.Millisecond))
			rtts = append(rtts, ms)
			got = append(got, ms)
		} else {
			rtts = append(rtts, "超时")
		}
	}
	summary := map[string]interface{}{"sent": count, "received": len(got), "loss_pct": r2(float64(count-len(got)) * 100 / float64(count))}
	if len(got) > 0 {
		mn, mx, sum, jit := got[0], got[0], 0.0, 0.0
		for i, v := range got {
			sum += v
			mn, mx = math.Min(mn, v), math.Max(mx, v)
			if i > 0 {
				jit += math.Abs(v - got[i-1])
			}
		}
		summary["min_ms"], summary["avg_ms"], summary["max_ms"] = mn, r2(sum/float64(len(got))), mx
		if len(got) > 1 {
			summary["jitter_ms"] = r2(jit / float64(len(got)-1))
		}
	}
	res := map[string]interface{}{
		"ip": p4.String(), "size": size, "timeout_ms": timeout.Milliseconds(), "rtt_ms": rtts, "summary": summary,
		"time": time.Now().Format("2006-01-02 15:04:05"), "note": "从 netPing 所在主机发出，反映该主机到目标的连通性",
	}
	if lastErr != "" {
		res["error"] = lastErr
	}
	if id != "" {
		name, _, attach := deviceLabel(t, id)
		res["device"] = map[string]interface{}{"id": id, "name": name, "attach": attach}
		if st, ok := s.Mon.Status(id); ok {
			res["monitor"] = map[string]interface{}{"state": st.State, "avg_1m_ms": st.Avg1m, "loss_1m_pct": st.Loss1m, "loss_1h_pct": st.Loss1h}
		}
	} else {
		res["device"] = "不在拓扑中的地址"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// get_uplink_path

func toolUplinkPath(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	sts := s.statusMap()
	stateOf := func(id string) map[string]interface{} {
		m := map[string]interface{}{}
		if st, ok := sts[id]; ok {
			m["state"], m["avg_1m_ms"], m["loss_1m_pct"], m["loss_1h_pct"] = st.State, st.Avg1m, st.Loss1m, st.Loss1h
			m["state_since"] = fmtLocal(st.StateSince)
		} else {
			m["state"] = "unmonitored"
		}
		return m
	}
	name, ip, attach := deviceLabel(t, id)
	res := map[string]interface{}{"device": map[string]interface{}{"id": id, "kind": kind, "name": name, "ip": ip, "attach": attach, "status": stateOf(id)}}

	start := id
	if kind == "node" {
		n := t.FindNode(id)
		if n.Attach.Switch == "" {
			res["note"] = "设备未定位到交换机端口，无法给出路径（可用 locate_device 查询实际端口）"
			return res, nil
		}
		start = n.Attach.Switch
		if n.Attach.Port != "" {
			var peers []map[string]interface{}
			for _, o := range t.Nodes {
				if o.ID != id && o.Attach.Switch == n.Attach.Switch && o.Attach.Port == n.Attach.Port {
					peers = append(peers, map[string]interface{}{"id": o.ID, "name": o.DisplayName(), "ip": o.IP, "state": stateOf(o.ID)["state"]})
				}
			}
			res["same_port_devices"] = peers
			if len(peers) > 0 {
				res["same_port_note"] = "同一端口下有多台设备，端口下可能接了傻瓜交换机、AP 或 IP 电话"
			}
		}
	}
	parent, edge, _ := switchTree(t)
	var hops []map[string]interface{}
	for cur, i := start, 0; cur != "" && i < 32; i++ {
		sw := t.FindSwitch(cur)
		if sw == nil {
			break
		}
		total, bad := 0, 0
		for _, n := range t.Nodes {
			if n.Attach.Switch == cur && n.IP != "" && n.Monitor.IsEnabled() {
				total++
				if st, ok := sts[n.ID]; ok && isAbnormal(st.State) {
					bad++
				}
			}
		}
		hop := map[string]interface{}{"switch": sw.ID, "name": sw.Name, "role": sw.Role, "ip": sw.IP, "location": sw.Location,
			"status": stateOf(sw.ID), "attached_monitored": total, "attached_abnormal": bad}
		if p, ok := parent[cur]; ok {
			e := edge[cur]
			hop["uplink_port"], hop["next_hop"], hop["next_hop_port"], hop["link"] = e.localPort, p, e.remotePort, e.link
		}
		hops = append(hops, hop)
		cur = parent[cur]
	}
	res["path"] = hops
	if last := hops[len(hops)-1]; last["role"] != "core" {
		roots := rootSwitches(t)
		isRoot := false
		for _, r := range roots {
			isRoot = isRoot || r == last["switch"]
		}
		if !isRoot {
			res["note"] = "没有找到通往核心交换机的链路（拓扑中缺少 links，或没有 role=core 的交换机）"
		}
	}
	if gw := t.Site.Gateway; gw != "" {
		g := map[string]interface{}{"ip": gw}
		if _, gid := t.FindByIP(gw); gid != "" {
			g["id"] = gid
			for k, v := range stateOf(gid) {
				g[k] = v
			}
		}
		res["gateway"] = g
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// analyze_impact

type swAgg struct {
	id, name, role, self string
	monitored, bad       int
	subMonitored, subBad int
	byState              map[string]int
	ports                map[string][]string
	devices              []string
}

func toolAnalyzeImpact(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	sts := s.statusMap()
	downOnly := argStr(a, "state") == "down"
	bad := func(st string) bool {
		if downOnly {
			return st == "down"
		}
		return isAbnormal(st)
	}
	live := func(st string) bool { return st == "ok" || isAbnormal(st) }
	stateOf := func(id string) string {
		if st, ok := sts[id]; ok {
			return st.State
		}
		return "unmonitored"
	}

	parent, _, order := switchTree(t)
	aggs := map[string]*swAgg{}
	for _, sw := range t.Switches {
		aggs[sw.ID] = &swAgg{id: sw.ID, name: sw.Name, role: sw.Role, self: stateOf(sw.ID), byState: map[string]int{}, ports: map[string][]string{}}
	}
	type badDev struct {
		id, name, ip, attach, state string
		since                       int64
	}
	var bads []badDev
	var unlocated []string
	totalLive := 0
	for _, sw := range t.Switches {
		if st := stateOf(sw.ID); live(st) {
			totalLive++
			if bad(st) {
				bads = append(bads, badDev{sw.ID, sw.Name, sw.IP, "交换机", st, sts[sw.ID].StateSince})
			}
		}
	}
	for _, n := range t.Nodes {
		st := stateOf(n.ID)
		if !live(st) {
			continue
		}
		totalLive++
		label := n.DisplayName()
		if bad(st) {
			bads = append(bads, badDev{n.ID, label, n.IP, t.AttachLabel(n.Attach), st, sts[n.ID].StateSince})
		}
		ag := aggs[n.Attach.Switch]
		if ag == nil {
			if bad(st) {
				unlocated = append(unlocated, fmt.Sprintf("%s %s（%s）", label, n.IP, st))
			}
			continue
		}
		ag.monitored++
		if bad(st) {
			ag.bad++
			ag.byState[st]++
			if len(ag.devices) < 10 {
				ag.devices = append(ag.devices, fmt.Sprintf("%s %s %s（%s）", n.Attach.Port, label, n.IP, st))
			}
			if n.Attach.Port != "" {
				ag.ports[n.Attach.Port] = append(ag.ports[n.Attach.Port], label)
			}
		}
	}
	// 自下而上累计子树（下游交换机本身也算作上游的“设备”）
	for _, id := range order {
		ag := aggs[id]
		ag.subMonitored, ag.subBad = ag.monitored, ag.bad
	}
	for i := len(order) - 1; i >= 0; i-- {
		ag := aggs[order[i]]
		p, ok := parent[ag.id]
		if !ok {
			continue
		}
		pa := aggs[p]
		pa.subMonitored += ag.subMonitored
		pa.subBad += ag.subBad
		if live(ag.self) {
			pa.subMonitored++
			if bad(ag.self) {
				pa.subBad++
			}
		}
	}
	isCandidate := func(ag *swAgg) bool {
		return bad(ag.self) && ag.self == "down" || ag.subBad >= 2 && ag.subBad*2 >= ag.subMonitored
	}
	var switches, suspects []map[string]interface{}
	for _, id := range order {
		ag := aggs[id]
		if ag.subMonitored == 0 && !live(ag.self) {
			continue
		}
		row := map[string]interface{}{"switch": ag.id, "name": ag.name, "role": ag.role, "self_state": ag.self,
			"attached_monitored": ag.monitored, "attached_abnormal": ag.bad,
			"subtree_monitored": ag.subMonitored, "subtree_abnormal": ag.subBad}
		if p, ok := parent[ag.id]; ok {
			row["upstream"] = p
		}
		if ag.subMonitored > 0 {
			row["subtree_abnormal_pct"] = r2(float64(ag.subBad) * 100 / float64(ag.subMonitored))
		}
		if len(ag.byState) > 0 {
			row["abnormal_by_state"] = ag.byState
		}
		if len(ag.devices) > 0 {
			row["abnormal_devices"] = ag.devices
		}
		switches = append(switches, row)
		if isCandidate(ag) {
			p, hasParent := parent[ag.id]
			rootCause := !hasParent || !isCandidate(aggs[p])
			reason := fmt.Sprintf("下游 %d 台监控设备中 %d 台异常", ag.subMonitored, ag.subBad)
			if ag.self == "down" {
				reason = "交换机本身离线；" + reason
			}
			suspects = append(suspects, map[string]interface{}{"switch": ag.id, "name": ag.name, "reason": reason, "most_upstream": rootCause,
				"hint": "检查该交换机供电/是否重启、它的上联端口和上联线缆；most_upstream=true 的是最靠近核心的公共故障点"})
		}
	}
	sort.SliceStable(suspects, func(i, j int) bool {
		return suspects[i]["most_upstream"] == true && suspects[j]["most_upstream"] != true
	})

	var portClusters []map[string]interface{}
	for _, sw := range t.Switches {
		ag := aggs[sw.ID]
		for port, devs := range ag.ports {
			if len(devs) >= 2 {
				portClusters = append(portClusters, map[string]interface{}{"switch": sw.ID, "name": sw.Name, "port": port, "abnormal": len(devs), "devices": devs,
					"hint": "同一端口多台异常：优先检查该端口下挂的小交换机/AP/网线"})
			}
		}
	}
	sort.SliceStable(portClusters, func(i, j int) bool { return portClusters[i]["abnormal"].(int) > portClusters[j]["abnormal"].(int) })

	// 同一时刻开始异常的设备（2 分钟内）
	sort.Slice(bads, func(i, j int) bool { return bads[i].since < bads[j].since })
	var waves []map[string]interface{}
	for i := 0; i < len(bads); {
		j := i + 1
		for j < len(bads) && bads[j].since > 0 && bads[j].since-bads[j-1].since <= 120000 {
			j++
		}
		if j-i >= 2 && bads[i].since > 0 {
			var names []string
			for _, b := range bads[i:j] {
				if len(names) < 15 {
					names = append(names, fmt.Sprintf("%s %s @%s", b.name, b.ip, b.attach))
				}
			}
			waves = append(waves, map[string]interface{}{"from": fmtLocal(bads[i].since), "to": fmtLocal(bads[j-1].since), "count": j - i, "devices": names})
		}
		i = j
	}

	res := map[string]interface{}{
		"criteria":            map[bool]string{true: "down", false: "abnormal（down+loss+slow）"}[downOnly],
		"monitored_live":      totalLive,
		"abnormal_total":      len(bads),
		"suspects":            suspects,
		"port_clusters":       portClusters,
		"same_time_waves":     waves,
		"switches":            switches,
		"unlocated_abnormal":  unlocated,
		"monitoring_paused":   s.Mon.Paused(),
		"interpretation_tips": "suspects 为异常集中的交换机；port_clusters 为同端口多台异常；same_time_waves 为 2 分钟内同时开始异常的设备组。异常零散分布且时间不一致时更可能是各设备自身问题。",
	}
	if gw := t.Site.Gateway; gw != "" {
		if _, gid := t.FindByIP(gw); gid != "" {
			res["gateway_state"] = stateOf(gid)
		}
	}
	if len(bads) == 0 {
		res["summary"] = "当前没有符合口径的异常设备"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// find_outages

type episode struct {
	start, end int64 // 毫秒
	sent, lost int
	maxMs      float64
	allDown    bool
}

func (e episode) view() map[string]interface{} {
	typ := "loss"
	if e.allDown {
		typ = "down"
	} else if e.lost*2 >= e.sent {
		typ = "heavy_loss"
	}
	m := map[string]interface{}{"start": fmtLocal(e.start), "end": fmtLocal(e.end), "duration_sec": (e.end - e.start) / 1000,
		"type": typ, "sent": e.sent, "lost": e.lost}
	if e.sent > 0 {
		m["loss_pct"] = r2(float64(e.lost) * 100 / float64(e.sent))
	}
	if e.maxMs > 0 {
		m["max_ms"] = e.maxMs
	}
	return m
}

func toolFindOutages(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	_, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	rg, dur, err := pickRange(a, "range", "24h", ranges6...)
	if err != nil {
		return nil, err
	}
	limit := clampInt(argInt(a, "limit", 30), 1, 100)
	minLoss := argFloat(a, "min_loss_pct", 0)
	now := time.Now()
	from := now.Add(-dur)
	var eps []episode
	var sent, recv int
	var covered time.Duration
	byHour := make([]int, 24)

	if dur <= time.Hour {
		// 秒级：连续失败的 ping 为一段，失败间隔不超过 10 秒的合并（中间夹杂的成功 ping 计入该段）
		samples := s.Mon.Recent(id, from.UnixMilli())
		var cur *episode
		for i, sm := range samples {
			sent++
			if i > 0 {
				if gap := sm.T - samples[i-1].T; gap < 60000 {
					covered += time.Duration(gap) * time.Millisecond
				}
			}
			if sm.RTT >= 0 {
				recv++
				continue
			}
			if cur != nil && sm.T-cur.end > 10000 {
				eps = append(eps, *cur)
				cur = nil
			}
			if cur == nil {
				cur = &episode{start: sm.T}
			}
			cur.end = sm.T
		}
		if cur != nil {
			eps = append(eps, *cur)
		}
		for i := range eps {
			e := &eps[i]
			for _, sm := range samples {
				if sm.T >= e.start && sm.T <= e.end {
					e.sent++
					if sm.RTT < 0 {
						e.lost++
					}
				}
			}
			e.allDown = e.sent > 0 && e.lost == e.sent
			if e.end == e.start {
				e.end += 1000
			}
		}
	} else {
		ms, err := s.minuteSeries(id, from, now)
		if err != nil {
			return nil, err
		}
		var cur *episode
		for _, m := range ms {
			if m.Sent <= 0 {
				continue
			}
			sent += m.Sent
			recv += m.Recv
			covered += time.Minute
			loss := float64(m.Sent-m.Recv) * 100 / float64(m.Sent)
			isBad := m.Recv < m.Sent && loss > minLoss
			if !isBad {
				if cur != nil {
					eps = append(eps, *cur)
					cur = nil
				}
				continue
			}
			byHour[time.Unix(m.T, 0).Hour()]++
			if cur != nil && m.T*1000 > cur.end {
				eps = append(eps, *cur)
				cur = nil
			}
			if cur == nil {
				cur = &episode{start: m.T * 1000, allDown: true}
			}
			cur.end = (m.T + 60) * 1000
			cur.sent += m.Sent
			cur.lost += m.Sent - m.Recv
			cur.allDown = cur.allDown && m.Recv == 0
			if m.Max > cur.maxMs {
				cur.maxMs = r2(m.Max)
			}
		}
		if cur != nil {
			eps = append(eps, *cur)
		}
	}
	var downSec int64
	longest := episode{}
	downCount := 0
	for _, e := range eps {
		if e.allDown {
			downCount++
			downSec += (e.end - e.start) / 1000
		}
		if e.end-e.start > longest.end-longest.start {
			longest = e
		}
	}
	name, ip, attach := deviceLabel(t, id)
	summary := map[string]interface{}{"episodes": len(eps), "down_episodes": downCount, "total_down_sec": downSec,
		"samples": sent, "lost": sent - recv, "covered_minutes": int(covered.Minutes())}
	if sent > 0 {
		summary["loss_pct"] = r2(float64(sent-recv) * 100 / float64(sent))
	}
	if covered > 0 {
		summary["availability_pct"] = r2(100 - float64(downSec)*100/covered.Seconds())
	}
	if len(eps) > 0 {
		summary["longest"] = longest.view()
	}
	out := make([]map[string]interface{}, 0, limit)
	for i := len(eps) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, eps[i].view())
	}
	res := map[string]interface{}{"id": id, "name": name, "ip": ip, "attach": attach, "range": rg,
		"precision": map[bool]string{true: "秒级（每次 ping）", false: "分钟级"}[dur <= time.Hour],
		"summary":   summary, "episodes": out,
		"type_names": "down 完全中断 / heavy_loss 丢包过半 / loss 丢包"}
	if dur > time.Hour && len(eps) > 0 {
		hours := map[string]int{}
		for h, c := range byHour {
			if c > 0 {
				hours[fmt.Sprintf("%02d时", h)] = c
			}
		}
		res["abnormal_minutes_by_hour"] = hours
	}
	if sent == 0 {
		res["note"] = "该时间范围内没有数据（可能未监控、处于暂停/非监控时段，或程序刚启动）"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// correlate_events

func toolCorrelateEvents(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	rg, dur, err := pickRange(a, "range", "24h", "1h", "6h", "24h", "7d")
	if err != nil {
		return nil, err
	}
	ids := argStrings(a, "ids")
	if sw := argStr(a, "switch"); sw != "" {
		if t.FindSwitch(sw) == nil {
			return nil, fmt.Errorf("找不到交换机 %q", sw)
		}
		ids = append(ids, monitoredIDs(t, sw)...)
	}
	scope := "指定设备"
	if len(ids) == 0 {
		ids = monitoredIDs(t, "")
		scope = "全部监控设备"
	}
	seen := map[string]bool{}
	var uniq []string
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if t.FindNode(id) == nil && t.FindSwitch(id) == nil {
			return nil, fmt.Errorf("找不到 id 为 %q 的设备", id)
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	ids = uniq
	if len(ids) > analysisMaxDevices || dur > 24*time.Hour && len(ids) > 60 {
		return nil, fmt.Errorf("设备太多（%d 台），请缩短时间范围或用 switch/ids 缩小范围", len(ids))
	}
	minDev := clampInt(argInt(a, "min_devices", 2), 2, 1000)
	minLoss := argFloat(a, "min_loss_pct", 20)
	limit := clampInt(argInt(a, "limit", 20), 1, 50)
	now := time.Now()
	from := now.Add(-dur)

	badAt := map[int64][]string{} // 分钟 -> 异常设备
	downAt := map[int64]int{}
	devBad := map[string]map[int64]bool{}
	noData := 0
	for _, id := range ids {
		ms, err := s.minuteSeries(id, from, now)
		if err != nil {
			return nil, err
		}
		if len(ms) == 0 {
			noData++
		}
		set := map[int64]bool{}
		for _, m := range ms {
			if m.Sent <= 0 || m.Recv == m.Sent {
				continue
			}
			if float64(m.Sent-m.Recv)*100/float64(m.Sent) >= minLoss {
				set[m.T] = true
				badAt[m.T] = append(badAt[m.T], id)
				if m.Recv == 0 {
					downAt[m.T]++
				}
			}
		}
		devBad[id] = set
	}
	// 公共事件：连续的“多台同时异常”分钟合并
	var mins []int64
	for m, d := range badAt {
		if len(d) >= minDev {
			mins = append(mins, m)
		}
	}
	sort.Slice(mins, func(i, j int) bool { return mins[i] < mins[j] })
	type event struct {
		start, end int64
		peak, down int
		devs       map[string]bool
	}
	var events []*event
	for _, m := range mins {
		if n := len(events); n > 0 && m-events[n-1].end <= 60 {
			e := events[n-1]
			e.end = m
		} else {
			events = append(events, &event{start: m, end: m, devs: map[string]bool{}})
		}
		e := events[len(events)-1]
		if len(badAt[m]) > e.peak {
			e.peak = len(badAt[m])
		}
		if downAt[m] > e.down {
			e.down = downAt[m]
		}
		for _, id := range badAt[m] {
			e.devs[id] = true
		}
	}
	shared := map[int64]bool{}
	for _, m := range mins {
		shared[m] = true
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].peak != events[j].peak {
			return events[i].peak > events[j].peak
		}
		return events[i].start > events[j].start
	})
	var evOut []map[string]interface{}
	for _, e := range events {
		if len(evOut) >= limit {
			break
		}
		bySwitch := map[string]int{}
		var names []string
		var devIDs []string
		for id := range e.devs {
			devIDs = append(devIDs, id)
		}
		sort.Strings(devIDs)
		for _, id := range devIDs {
			name, ip, attach := deviceLabel(t, id)
			if n := t.FindNode(id); n != nil {
				bySwitch[t.AttachLabel(topology.Endpoint{Switch: n.Attach.Switch})]++
			} else {
				bySwitch["（交换机本身）"]++
			}
			if len(names) < 20 {
				names = append(names, fmt.Sprintf("%s %s @%s", name, ip, attach))
			}
		}
		evOut = append(evOut, map[string]interface{}{
			"start": fmtLocal(e.start * 1000), "end": fmtLocal((e.end + 60) * 1000), "duration_min": (e.end-e.start)/60 + 1,
			"peak_devices": e.peak, "peak_down_devices": e.down, "devices_involved": len(e.devs),
			"share_of_scope_pct": r2(float64(len(e.devs)) * 100 / float64(len(ids))),
			"by_switch":          bySwitch, "devices": names,
		})
	}
	// 每台设备：异常分钟数及其中与其他设备同时发生的比例
	type devRow struct {
		id          string
		bad, shared int
	}
	var rows []devRow
	for id, set := range devBad {
		if len(set) == 0 {
			continue
		}
		r := devRow{id: id, bad: len(set)}
		for m := range set {
			if shared[m] {
				r.shared++
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].bad != rows[j].bad {
			return rows[i].bad > rows[j].bad
		}
		return rows[i].id < rows[j].id
	})
	var devOut []map[string]interface{}
	for _, r := range rows {
		if len(devOut) >= 30 {
			break
		}
		name, ip, attach := deviceLabel(t, r.id)
		devOut = append(devOut, map[string]interface{}{"id": r.id, "name": name, "ip": ip, "attach": attach,
			"abnormal_minutes": r.bad, "shared_minutes": r.shared, "shared_pct": r2(float64(r.shared) * 100 / float64(r.bad))})
	}
	res := map[string]interface{}{
		"scope": scope, "devices_analyzed": len(ids), "devices_without_data": noData, "range": rg,
		"criteria":            fmt.Sprintf("某分钟丢包率 ≥ %.0f%% 记为异常；同一分钟 ≥ %d 台异常记为公共事件", minLoss, minDev),
		"common_events":       evOut,
		"common_events_total": len(events),
		"devices":             devOut,
		"interpretation_tips": "shared_pct 高说明该设备的问题多与其他设备同时发生（公共原因：停电、交换机重启、上联/核心问题）；shared_pct 低说明多为设备自身问题。by_switch 集中在同一交换机说明该交换机或其上联有问题。",
	}
	// 少量设备时给出两两重合度
	if len(ids) <= 10 && len(ids) >= 2 {
		var pairs []map[string]interface{}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				x, y := devBad[ids[i]], devBad[ids[j]]
				inter := 0
				for m := range x {
					if y[m] {
						inter++
					}
				}
				union := len(x) + len(y) - inter
				if union == 0 {
					continue
				}
				pairs = append(pairs, map[string]interface{}{"a": ids[i], "b": ids[j], "both_abnormal_minutes": inter, "overlap_pct": r2(float64(inter) * 100 / float64(union))})
			}
		}
		res["pairwise_overlap"] = pairs
	}
	if len(rows) == 0 {
		res["summary"] = "该时间范围内这些设备都没有达到口径的异常"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// rank_devices

func toolRankDevices(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	rg, dur, err := pickRange(a, "range", "24h", "1h", "6h", "24h", "7d")
	if err != nil {
		return nil, err
	}
	sw := argStr(a, "switch")
	if sw != "" && t.FindSwitch(sw) == nil {
		return nil, fmt.Errorf("找不到交换机 %q", sw)
	}
	ids := monitoredIDs(t, sw)
	if len(ids) > analysisMaxDevices {
		ids = ids[:analysisMaxDevices]
	}
	metric := argStr(a, "metric")
	if metric == "" {
		metric = "loss"
	}
	limit := clampInt(argInt(a, "limit", 15), 1, 50)
	now := time.Now()
	sts := s.statusMap()
	type row struct {
		id string
		st *mstat
	}
	var rows []row
	var losses, avgs []float64
	for _, id := range ids {
		ms, err := s.minuteSeries(id, now.Add(-dur), now)
		if err != nil {
			return nil, err
		}
		st := statMinutes(ms)
		if st.sent == 0 {
			continue
		}
		rows = append(rows, row{id, st})
		losses = append(losses, st.lossPct())
		if st.recv > 0 {
			avgs = append(avgs, st.avg())
		}
	}
	key := func(r row) float64 {
		switch metric {
		case "latency":
			return r.st.avg()
		case "downtime":
			return float64(r.st.downMin)
		case "jitter":
			return r.st.jitter()
		}
		return r.st.lossPct()
	}
	sort.SliceStable(rows, func(i, j int) bool { return key(rows[i]) > key(rows[j]) })
	median := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		v = append([]float64(nil), v...)
		sort.Float64s(v)
		return r2(v[len(v)/2])
	}
	var out []map[string]interface{}
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		name, ip, attach := deviceLabel(t, r.id)
		m := r.st.view()
		m["id"], m["name"], m["ip"], m["attach"] = r.id, name, ip, attach
		if st, ok := sts[r.id]; ok {
			m["current_state"] = st.State
		}
		out = append(out, m)
	}
	return map[string]interface{}{
		"range": rg, "metric": metric, "devices_with_data": len(rows), "devices_considered": len(ids),
		"median":      map[string]interface{}{"loss_pct": median(losses), "avg_ms": median(avgs)},
		"ranking":     out,
		"field_notes": "jitter_ms 为每分钟最大-最小延迟的平均；p95_minute_avg_ms 为分钟平均延迟的 95 分位；down_minutes 为整分钟全部丢包的分钟数",
	}, nil
}

// ---------------------------------------------------------------------------
// compare_baseline

func toolCompareBaseline(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	_, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	win, w, err := pickRange(a, "window", "1h", "15m", "1h", "6h")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	period := func(end time.Time) (*mstat, map[string]interface{}, error) {
		ms, err := s.minuteSeries(id, end.Add(-w), end)
		if err != nil {
			return nil, nil, err
		}
		st := statMinutes(ms)
		v := st.view()
		v["from"], v["to"] = end.Add(-w).Format("01-02 15:04"), end.Format("01-02 15:04")
		return st, v, nil
	}
	cur, curV, err := period(now)
	if err != nil {
		return nil, err
	}
	yst, ystV, err := period(now.Add(-24 * time.Hour))
	if err != nil {
		return nil, err
	}
	wk, wkV, err := period(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		return nil, err
	}
	all, err := s.minuteSeries(id, now.Add(-7*24*time.Hour), now.Add(-w))
	if err != nil {
		return nil, err
	}
	base := statMinutes(all)
	baseV := base.view()
	baseV["from"], baseV["to"] = now.Add(-7*24*time.Hour).Format("01-02 15:04"), now.Add(-w).Format("01-02 15:04")

	change := func(ref *mstat) map[string]interface{} {
		if cur.sent == 0 || ref.sent == 0 {
			return map[string]interface{}{"note": "缺少数据，无法对比"}
		}
		m := map[string]interface{}{"loss_pct_diff": r2(cur.lossPct() - ref.lossPct())}
		if cur.recv > 0 && ref.recv > 0 && ref.avg() > 0 {
			m["avg_ms_diff"] = r2(cur.avg() - ref.avg())
			m["avg_change_pct"] = r2((cur.avg() - ref.avg()) * 100 / ref.avg())
			m["p95_ms_diff"] = r2(cur.p95() - ref.p95())
		}
		return m
	}
	name, ip, attach := deviceLabel(t, id)
	return map[string]interface{}{
		"id": id, "name": name, "ip": ip, "attach": attach, "window": win,
		"current":             curV,
		"yesterday_same":      ystV,
		"last_week_same":      wkV,
		"baseline_7d":         baseV,
		"vs_yesterday":        change(yst),
		"vs_last_week":        change(wk),
		"vs_baseline_7d":      change(base),
		"interpretation_tips": "avg_change_pct 为正表示比参照变慢；延迟增加超过 50% 且绝对值增加超过 5ms、或丢包率增加超过 1 个百分点，通常值得关注。",
	}, nil
}
