package server

import (
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/store"
	"netping/internal/topology"
)

func (s *Server) handleTopology(w http.ResponseWriter) {
	t, rev := s.Topo.Snapshot()
	writeJSON(w, map[string]interface{}{
		"topology":        t,
		"rev":             rev,
		"changed_on_disk": s.Topo.ChangedOnDisk(),
	})
}

func (s *Server) okRev(w http.ResponseWriter, extra map[string]interface{}) {
	if extra == nil {
		extra = map[string]interface{}{}
	}
	extra["ok"] = true
	extra["rev"] = s.Topo.Rev()
	writeJSON(w, extra)
}

func (s *Server) handleReload(w http.ResponseWriter) error {
	if err := s.Topo.Reload(); err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

func (s *Server) handleGetYAML(w http.ResponseWriter, r *http.Request) error {
	data, err := s.Topo.YAML()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="topology.yaml"`)
	}
	_, _ = w.Write(data)
	return nil
}

func (s *Server) handlePutYAML(w http.ResponseWriter, r *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	t, err := topology.Parse(data)
	if err != nil {
		return err
	}
	if err := s.Topo.Replace(t); err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"nodes": len(t.Nodes), "switches": len(t.Switches)})
	return nil
}

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) error {
	var site topology.Site
	if err := decode(r, &site); err != nil {
		return err
	}
	if err := s.Topo.Update(func(t *topology.Topology) error { t.Site = site; return nil }); err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

// ---- 节点 ----

func (s *Server) handleNodeSave(w http.ResponseWriter, r *http.Request, origID string) error {
	var req struct {
		topology.Node
		Locked *bool `json:"locked"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	n := req.Node
	if req.Locked != nil {
		n.Locked = *req.Locked
	}
	var savedID string
	err := s.Topo.Update(func(t *topology.Topology) error {
		if origID == "" {
			if n.ID == "" {
				n.ID = t.NewNodeID(&n)
			}
			t.Nodes = append(t.Nodes, &n)
			savedID = n.ID
			return nil
		}
		for i, x := range t.Nodes {
			if x.ID == origID {
				if req.Locked == nil {
					n.Locked = x.Locked // 旧客户端未提交该字段时保留锁定状态
				}
				if n.ID == "" {
					n.ID = origID
				}
				if n.Hostname == "" {
					n.Hostname = x.Hostname // 主机名由网段扫描维护，网页表单不提交时保留
				}
				t.Nodes[i] = &n
				savedID = n.ID
				return nil
			}
		}
		return notFound("节点 " + origID)
	})
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"id": savedID})
	return nil
}

func (s *Server) handleNodeDelete(w http.ResponseWriter, id string) error {
	err := s.Topo.Update(func(t *topology.Topology) error {
		if !t.DeleteNode(id) {
			return notFound("节点 " + id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

// ---- 交换机 ----

func (s *Server) handleSwitchSave(w http.ResponseWriter, r *http.Request, origID string) error {
	var sw topology.Switch
	if err := decode(r, &sw); err != nil {
		return err
	}
	var savedID string
	err := s.Topo.Update(func(t *topology.Topology) error {
		if origID == "" {
			if sw.ID == "" {
				sw.ID = t.NewSwitchID(&sw)
			}
			if sw.Position == nil {
				sw.Position = freeSpot(t)
			}
			t.Switches = append(t.Switches, &sw)
			savedID = sw.ID
			return nil
		}
		for i, x := range t.Switches {
			if x.ID != origID {
				continue
			}
			if sw.ID == "" {
				sw.ID = origID
			}
			t.RenameSwitchRefs(origID, sw.ID) // 改名时同步更新引用
			t.Switches[i] = &sw
			savedID = sw.ID
			return nil
		}
		return notFound("交换机 " + origID)
	})
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"id": savedID})
	return nil
}

// freeSpot 给新交换机找一个不重叠的位置：放在现有交换机的右侧。
func freeSpot(t *topology.Topology) *topology.Position {
	x, y := 80.0, 40.0
	for _, s := range t.Switches {
		if s.Position != nil && s.Position.X+700 > x {
			x = s.Position.X + 700
		}
	}
	return &topology.Position{X: x, Y: y}
}

func (s *Server) handleSwitchDelete(w http.ResponseWriter, id string) error {
	detached := 0
	err := s.Topo.Update(func(t *topology.Topology) error {
		n, ok := t.DeleteSwitch(id)
		if !ok {
			return notFound("交换机 " + id)
		}
		detached = n
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"detached_nodes": detached})
	return nil
}

// ---- 链路 ----

func (s *Server) handleLinkSave(w http.ResponseWriter, r *http.Request, origID string) error {
	var l topology.Link
	if err := decode(r, &l); err != nil {
		return err
	}
	err := s.Topo.Update(func(t *topology.Topology) error {
		if origID == "" {
			t.Links = append(t.Links, &l)
			return nil
		}
		for i, x := range t.Links {
			if x.ID == origID {
				if l.ID == "" {
					l.ID = origID
				}
				t.Links[i] = &l
				return nil
			}
		}
		return notFound("链路 " + origID)
	})
	if err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

func (s *Server) handleLinkDelete(w http.ResponseWriter, id string) error {
	err := s.Topo.Update(func(t *topology.Topology) error {
		if !t.DeleteLink(id) {
			return notFound("链路 " + id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

// ---- 位置 ----

type positionsReq struct {
	Nodes    map[string]*topology.Position `json:"nodes"`
	Switches map[string]*topology.Position `json:"switches"`
}

func roundPos(p *topology.Position) *topology.Position {
	if p == nil {
		return nil
	}
	return &topology.Position{X: math.Round(p.X), Y: math.Round(p.Y)}
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) error {
	var req positionsReq
	if err := decode(r, &req); err != nil {
		return err
	}
	err := s.Topo.Update(func(t *topology.Topology) error {
		for id, p := range req.Nodes {
			if n := t.FindNode(id); n != nil {
				n.Position = roundPos(p)
			}
		}
		for id, p := range req.Switches {
			if sw := t.FindSwitch(id); sw != nil && p != nil {
				sw.Position = roundPos(p)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, nil)
	return nil
}

func (s *Server) handleLayoutReset(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Switch string `json:"switch"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			return err
		}
	}
	count := 0
	err := s.Topo.Update(func(t *topology.Topology) error {
		// 不指定交换机时整理全部节点（包括未定位的）；指定时只处理挂在这台交换机上的
		for _, n := range t.Nodes {
			if n.Position == nil {
				continue
			}
			if req.Switch == "" || n.Attach.Switch == req.Switch {
				n.Position = nil
				count++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"reset": count})
	return nil
}

// ---- 状态 ----

type summary struct {
	Total      int            `json:"total"`
	ByState    map[string]int `json:"by_state"`
	GatewayIP  string         `json:"gateway_ip"`
	GatewayRTT *float64       `json:"gateway_rtt"`
	AlertsOn   bool           `json:"alerts_on"`
}

func (s *Server) statusPayload() map[string]interface{} {
	sts := s.Mon.Statuses(true)
	t, rev := s.Topo.Snapshot()
	sum := summary{ByState: map[string]int{}, GatewayIP: t.Site.Gateway, AlertsOn: s.Cfg.Get().Alert.Enabled}
	byID := make(map[string]monitor.Status, len(sts))
	for _, st := range sts {
		sum.Total++
		sum.ByState[st.State]++
		byID[st.ID] = st
		if st.IP == t.Site.Gateway && st.LastRTT >= 0 {
			v := st.Avg1m
			sum.GatewayRTT = &v
		}
	}
	return map[string]interface{}{
		"rev":             rev,
		"paused":          s.Mon.Paused(),
		"manual_paused":   s.Mon.ManualPaused(),
		"pause_reason":    s.Mon.PauseReason(),
		"schedule":        s.Cfg.Get().Schedule.Describe(),
		"summary":         sum,
		"statuses":        byID,
		"active_down":     s.Alerts.ActiveDown(),
		"changed_on_disk": s.Topo.ChangedOnDisk(),
		"server_time":     time.Now().UnixMilli(),
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	send := func() bool {
		data, err := jsonMarshal(s.statusPayload())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tk.C:
			if !send() {
				return
			}
		}
	}
}

// ---- 历史 ----

// Point 图表上的一个点。
type Point struct {
	T    int64    `json:"t"`
	Avg  *float64 `json:"avg"`
	Min  *float64 `json:"min"`
	Max  *float64 `json:"max"`
	Loss float64  `json:"loss"`
	Sent int      `json:"sent"`
}

var ranges = map[string]time.Duration{
	"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour,
	"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, id string) error {
	rg := r.URL.Query().Get("range")
	if rg == "" {
		rg = "1h"
	}
	h, err := s.history(id, rg, 720)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]interface{}{
		"id": id, "range": rg, "from": h.From.UnixMilli(), "to": h.To.UnixMilli(),
		"bucket_ms": h.Bucket.Milliseconds(), "points": h.Points,
	})
	return nil
}

type historyResult struct {
	From, To time.Time
	Bucket   time.Duration
	Points   []Point
}

// history 按时间范围聚合延迟/丢包数据，最多约 maxPoints 个点。
func (s *Server) history(id, rg string, maxPoints int) (*historyResult, error) {
	if !topology.ValidID(id) {
		return nil, badRequest("id 不合法")
	}
	dur, ok := ranges[rg]
	if !ok {
		return nil, badRequest("range 只能是 15m/1h/6h/24h/7d/30d")
	}
	now := time.Now()
	from := now.Add(-dur)
	h := &historyResult{From: from, To: now}
	if dur <= time.Hour {
		h.Bucket = (dur / time.Duration(maxPoints)).Round(time.Second)
		if h.Bucket < time.Second {
			h.Bucket = time.Second
		}
		h.Points = bucketSamples(s.Mon.Recent(id, from.UnixMilli()), h.Bucket)
		return h, nil
	}
	mins := int64(math.Ceil(float64(dur/time.Minute) / float64(maxPoints)))
	h.Bucket = time.Duration(mins) * time.Minute
	recs, err := s.Store.Query(id, from.Unix(), now.Unix()+60)
	if err != nil {
		return nil, err
	}
	h.Points = bucketMinutes(recs, int64(h.Bucket/time.Second))
	return h, nil
}

func fp(v float64) *float64 { v = math.Round(v*100) / 100; return &v }

func bucketSamples(samples []monitor.Sample, bucket time.Duration) []Point {
	bms := bucket.Milliseconds()
	var out []Point
	var cur *Point
	var sum float64
	var recv int
	finish := func() {
		if cur == nil {
			return
		}
		if recv > 0 {
			cur.Avg = fp(sum / float64(recv))
		}
		cur.Loss = math.Round(float64(cur.Sent-recv)*10000/float64(cur.Sent)) / 100
		out = append(out, *cur)
	}
	for _, s := range samples {
		t := s.T / bms * bms
		if cur == nil || cur.T != t {
			finish()
			cur = &Point{T: t}
			sum, recv = 0, 0
		}
		cur.Sent++
		if s.RTT >= 0 {
			recv++
			sum += s.RTT
			if cur.Min == nil || s.RTT < *cur.Min {
				cur.Min = fp(s.RTT)
			}
			if cur.Max == nil || s.RTT > *cur.Max {
				cur.Max = fp(s.RTT)
			}
		}
	}
	finish()
	return out
}

func bucketMinutes(recs []store.Minute, bucketSec int64) []Point {
	groups := map[int64][]store.Minute{}
	var keys []int64
	for _, m := range recs {
		k := m.T / bucketSec * bucketSec
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], m)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		p := Point{T: k * 1000}
		var sum float64
		recv := 0
		for _, m := range groups[k] {
			p.Sent += m.Sent
			if m.Recv == 0 {
				continue
			}
			recv += m.Recv
			sum += m.Avg * float64(m.Recv)
			if p.Min == nil || m.Min < *p.Min {
				p.Min = fp(m.Min)
			}
			if p.Max == nil || m.Max > *p.Max {
				p.Max = fp(m.Max)
			}
		}
		if recv > 0 {
			p.Avg = fp(sum / float64(recv))
		}
		if p.Sent > 0 {
			p.Loss = math.Round(float64(p.Sent-recv)*10000/float64(p.Sent)) / 100
		}
		out = append(out, p)
	}
	return out
}

// ---- 配置 / 告警 ----

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) error {
	cur := s.Cfg.Get()
	next := cur
	if err := decode(r, &next); err != nil {
		return err
	}
	next.Auth = cur.Auth // 登录密码只能通过 /api/auth/password 修改
	next.AI = cur.AI     // AI 设置（含 API Key）只能通过 /api/ai/settings 修改
	if err := s.Cfg.Update(next); err != nil {
		return badRequest("%v", err)
	}
	restart := next.Listen != cur.Listen || next.TopologyFile != cur.TopologyFile
	writeJSON(w, map[string]interface{}{"ok": true, "restart_required": restart, "config": publicConfig(s.Cfg.Get())})
	return nil
}

func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) error {
	cfg := s.Cfg.Get().Alert
	if r.ContentLength != 0 {
		// 允许用表单里尚未保存的 webhook 测试。
		var a config.AlertConfig
		if err := decode(r, &a); err != nil {
			return err
		}
		c := s.Cfg.Get()
		c.Alert = a
		c.Normalize()
		cfg = c.Alert
	}
	if err := s.Alerts.SendTest(cfg); err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, map[string]interface{}{"ok": true})
	return nil
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	s.Mon.SetPaused(req.Paused)
	log.Printf("网页上%s监控", map[bool]string{true: "手动暂停", false: "恢复"}[req.Paused])
	if s.OnPauseChange != nil {
		s.OnPauseChange(req.Paused)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "paused": req.Paused})
	return nil
}

// handleAlerting 批量开启/关闭设备告警：{"ids": [...], "enabled": true}
func (s *Server) handleAlerting(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IDs     []string `json:"ids"`
		Enabled bool     `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if len(req.IDs) == 0 {
		return badRequest("ids 不能为空")
	}
	changed := 0
	err := s.Topo.Update(func(t *topology.Topology) error {
		changed, _ = t.SetAlert(req.IDs, req.Enabled)
		return nil
	})
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"changed": changed})
	return nil
}

// ---- 网段扫描 ----

func (s *Server) handleDiscoveryScan(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Job     string   `json:"job"`
		Subnets []string `json:"subnets"`
		AutoAdd *bool    `json:"auto_add"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			return err
		}
	}
	if s.Disc.Running() {
		return &apiError{status: http.StatusConflict, msg: discovery.ErrRunning.Error()}
	}
	d := s.Cfg.Get().Discovery
	if len(req.Subnets) > 0 {
		if _, err := config.ExpandSubnets(req.Subnets, d.Exclude); err != nil {
			return badRequest("%v", err)
		}
	} else if req.Job != "" {
		if _, ok := d.FindJob(req.Job); !ok {
			return badRequest("扫描任务 %q 不存在", req.Job)
		}
	} else if len(d.Jobs) == 0 {
		return badRequest("还没有扫描任务，请先添加")
	}
	go func() {
		var err error
		if len(req.Subnets) > 0 {
			autoAdd := req.AutoAdd == nil || *req.AutoAdd
			_, err = s.Disc.ScanAdhoc(req.Subnets, autoAdd, "临时扫描")
		} else {
			var ids []string
			if req.Job != "" {
				ids = []string{req.Job}
			}
			var rs []*discovery.ScanResult
			rs, err = s.Disc.ScanJobs(ids, "手动扫描")
			for _, x := range rs {
				log.Printf("手动扫描「%s」完成：%d 个地址在线，%d 项变更", x.JobName, x.Alive, len(x.Changes))
			}
		}
		if err != nil && err != discovery.ErrRunning {
			log.Printf("手动扫描失败: %v", err)
		}
	}()
	writeJSON(w, map[string]interface{}{"ok": true, "started": true})
	return nil
}

func (s *Server) handleDiscoveryAdd(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IPs []string `json:"ips"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	added, err := s.Disc.AddUnknown(req.IPs)
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"added": len(added)})
	return nil
}

func (s *Server) handleDiscoveryApply(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		NodeID string  `json:"node_id"`
		IP     string  `json:"ip"`
		IPMode *string `json:"ip_mode"` // 不传=保持原分配方式
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	mode := "-"
	if req.IPMode != nil {
		mode = *req.IPMode
	}
	if mode != "-" && mode != "" && mode != "dhcp" && mode != "static" {
		return badRequest("ip_mode 只能是 dhcp、static 或空")
	}
	ch, err := s.Disc.ApplySuggestion(req.NodeID, req.IP, mode)
	if err != nil {
		return badRequest("%v", err)
	}
	s.okRev(w, map[string]interface{}{"change": ch})
	return nil
}

// ---- 端口定位 ----

func (s *Server) locateStatus() map[string]interface{} {
	targets := []map[string]string{}
	for _, tg := range s.Loc.Targets() {
		targets = append(targets, map[string]string{"switch": tg.Switch, "name": tg.Name, "host": tg.Host})
	}
	return map[string]interface{}{"running": s.Loc.Running(), "targets": targets, "last": s.Loc.Last()}
}

func (s *Server) handleLocateRun(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		DryRun     bool `json:"dry_run"`
		ApplyMoves bool `json:"apply_moves"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			return err
		}
	}
	res, err := s.Loc.RunNow("手动定位", req.DryRun, req.ApplyMoves)
	if err != nil {
		if err == locate.ErrRunning {
			return &apiError{status: http.StatusConflict, msg: err.Error()}
		}
		return badRequest("%v", err)
	}
	s.okRev(w, map[string]interface{}{"result": res})
	return nil
}

func (s *Server) handleLocateTest(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Switch    string `json:"switch"`
		Host      string `json:"host"`
		Community string `json:"community"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Switch != "" {
		c := s.Cfg.Get().Locate
		o := c.Switches[req.Switch]
		t, _ := s.Topo.Snapshot()
		if req.Host == "" {
			req.Host = o.Host
			if req.Host == "" {
				if sw := t.FindSwitch(req.Switch); sw != nil {
					req.Host = sw.IP
				}
			}
		}
		if req.Community == "" {
			req.Community = o.Community
			if req.Community == "" {
				req.Community = c.Community
			}
		}
	}
	res, err := s.Loc.Test(req.Host, req.Community)
	if err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, res)
	return nil
}

func (s *Server) handleLocateApply(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	res, err := s.Loc.ApplyMove(req.NodeID)
	if err != nil {
		return badRequest("%v", err)
	}
	s.okRev(w, map[string]interface{}{"result": res})
	return nil
}

func (s *Server) handleLocateTable(w http.ResponseWriter, r *http.Request, id string) error {
	rows, st, err := s.Loc.MACTable(id, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, map[string]interface{}{"ports": rows, "status": st})
	return nil
}
