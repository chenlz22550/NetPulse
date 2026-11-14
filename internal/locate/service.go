package locate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"netping/internal/config"
	"netping/internal/snmp"
	"netping/internal/topology"
)

// Target 是一台要读取 MAC 表的交换机。
type Target struct {
	Switch    string
	Name      string
	Host      string
	Community string
}

// SwitchStatus 是一台交换机最近一次读取的情况。
type SwitchStatus struct {
	Switch   string    `json:"switch"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	SysName  string    `json:"sys_name,omitempty"`
	SysDescr string    `json:"sys_descr,omitempty"`
	Entries  int       `json:"entries"`
	Source   string    `json:"source,omitempty"`
	Uplinks  []string  `json:"uplinks,omitempty"` // 判定为级联口的端口（及原因）
	Warnings []string  `json:"warnings,omitempty"`
	ReadAt   time.Time `json:"read_at"`
}

// RunResult 是一次定位的完整结果。
type RunResult struct {
	Started    time.Time      `json:"started"`
	DurationMs int64          `json:"duration_ms"`
	Reason     string         `json:"reason"`
	DryRun     bool           `json:"dry_run,omitempty"`
	Switches   []SwitchStatus `json:"switches"`
	Result
	Error string `json:"error,omitempty"`
}

// Notifier 发送通知（企业微信）。
type Notifier func(title string, lines []string)

// Service 端口定位服务。
type Service struct {
	Cfg    *config.Manager
	Topo   *topology.Store
	Notify Notifier
	// OnChanges 在一次定位产生了变更后调用（用于插件的 locate 钩子），不应阻塞。
	OnChanges func(r *RunResult)

	dataDir  string
	mu       sync.Mutex
	running  bool
	last     *RunResult
	tables   map[string]*snmp.Table // 最近读到的 MAC 表
	trigger  chan struct{}
	notified map[string]bool // 已推送过的端口变化（node|to）
}

// New 创建服务。
func New(cfg *config.Manager, topo *topology.Store, dataDir string) *Service {
	s := &Service{Cfg: cfg, Topo: topo, dataDir: dataDir, tables: map[string]*snmp.Table{}, trigger: make(chan struct{}, 1), notified: map[string]bool{}}
	if data, err := os.ReadFile(s.path()); err == nil {
		var r RunResult
		if json.Unmarshal(data, &r) == nil {
			s.last = &r
		}
	}
	return s
}

func (s *Service) path() string { return filepath.Join(s.dataDir, "locate_last.json") }

// ErrRunning 表示定位正在进行。
var ErrRunning = errors.New("端口定位正在进行，请稍后")

// ErrNoTargets 表示没有可读取的交换机。
var ErrNoTargets = errors.New("没有可读取 MAC 表的交换机：请设置 SNMP 团体名，并确认交换机填写了管理 IP（或在端口定位设置中填写读取地址）")

// Targets 返回要读取的交换机列表。
func (s *Service) Targets() []Target {
	c := s.Cfg.Get().Locate
	t, _ := s.Topo.Snapshot()
	var out []Target
	for _, sw := range t.Switches {
		o := c.Switches[sw.ID]
		if o.Disabled {
			continue
		}
		tg := Target{Switch: sw.ID, Name: sw.Name, Host: o.Host, Community: o.Community}
		if tg.Host == "" {
			tg.Host = sw.IP
		}
		if tg.Community == "" {
			tg.Community = c.Community
		}
		if tg.Host != "" && tg.Community != "" {
			out = append(out, tg)
		}
	}
	return out
}

// Trigger 请求尽快执行一次定位（例如扫描刚添加了新设备）。
func (s *Service) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run 定时定位，直到 stop 关闭。
func (s *Service) Run(stop <-chan struct{}) {
	start := time.Now()
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	for {
		var reason string
		select {
		case <-stop:
			return
		case <-s.trigger:
			reason = "扫描发现新设备"
		case now := <-tk.C:
			c := s.Cfg.Get().Locate
			last := s.Last()
			due := now.Sub(start) > 20*time.Second && (last == nil || now.Sub(last.Started) >= time.Duration(c.IntervalMin)*time.Minute)
			if !due {
				continue
			}
			reason = "定时定位"
		}
		if !s.Cfg.Get().Locate.Enabled || len(s.Targets()) == 0 {
			continue
		}
		r, err := s.RunNow(reason, false, false)
		if err != nil && !errors.Is(err, ErrRunning) {
			log.Printf("端口定位失败: %v", err)
		} else if r != nil {
			log.Printf("端口定位（%s）完成：自动定位 %d 台，端口变化待确认 %d 台，用时 %dms", reason, len(r.Changes), len(r.Suggestions), r.DurationMs)
		}
	}
}

func (s *Service) client(tg Target) (*snmp.Client, error) {
	c := &snmp.Client{Host: tg.Host, Community: tg.Community, Timeout: time.Duration(s.Cfg.Get().Locate.TimeoutMs) * time.Millisecond, Retries: 1}
	if err := c.Connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// Dial 按交换机 id 建立 SNMP 连接（使用端口定位的读取地址与团体名），调用方负责 Close。
func (s *Service) Dial(switchID string) (*snmp.Client, error) {
	for _, tg := range s.Targets() {
		if tg.Switch == switchID {
			return s.client(tg)
		}
	}
	t, _ := s.Topo.Snapshot()
	sw := t.FindSwitch(switchID)
	if sw == nil {
		return nil, fmt.Errorf("交换机 %q 不存在", switchID)
	}
	name := sw.Name
	if name == "" {
		name = sw.ID
	}
	return nil, fmt.Errorf("交换机 %s 没有可用的 SNMP 设置（管理 IP 或团体名为空，或已停用），无法读取端口状态：请在「端口定位」页设置只读团体名", name)
}

// readAll 并行读取所有交换机的 MAC 表。
func (s *Service) readAll(targets []Target) (map[string]*snmp.Table, []SwitchStatus) {
	tables := map[string]*snmp.Table{}
	sts := make([]SwitchStatus, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		go func(i int, tg Target) {
			defer wg.Done()
			st := SwitchStatus{Switch: tg.Switch, Name: tg.Name, Host: tg.Host, ReadAt: time.Now()}
			tb, err := s.read(tg)
			if err != nil {
				st.Error = err.Error()
			} else {
				st.OK, st.SysName, st.SysDescr, st.Entries, st.Source, st.Warnings = true, tb.SysName, tb.SysDescr, len(tb.Entries), tb.Source, tb.Warnings
				mu.Lock()
				tables[tg.Switch] = tb
				mu.Unlock()
			}
			sts[i] = st
		}(i, tg)
	}
	wg.Wait()
	s.mu.Lock()
	for k, v := range tables {
		s.tables[k] = v
	}
	s.mu.Unlock()
	return tables, sts
}

func (s *Service) read(tg Target) (*snmp.Table, error) {
	c, err := s.client(tg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ReadTable()
}

// RunNow 读取所有交换机并定位。dryRun 只返回结果不写入；applyMoves 同时修改端口变化的设备。
func (s *Service) RunNow(reason string, dryRun, applyMoves bool, only ...string) (*RunResult, error) {
	targets := s.Targets()
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, ErrRunning
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	r := &RunResult{Started: time.Now(), Reason: reason, DryRun: dryRun}
	tables, sts := s.readAll(targets)
	opt := Options{ApplyMoves: applyMoves, OnlyNodes: only, Now: time.Now()}
	t, _ := s.Topo.Snapshot()
	a := Analyze(t, tables)
	for i := range sts {
		var ups []string
		for p, why := range a.Uplinks[sts[i].Switch] {
			ups = append(ups, p+"（"+why+"）")
		}
		sort.Strings(ups)
		sts[i].Uplinks = ups
	}
	r.Switches = sts
	preview := Apply(t.Clone(), a, opt)
	r.Result = preview
	if !dryRun && len(preview.Changes) > 0 {
		err := s.Topo.Update(func(tt *topology.Topology) error {
			r.Result = Apply(tt, Analyze(tt, tables), opt)
			return nil
		})
		if err != nil {
			r.Error = err.Error()
		}
	}
	r.DurationMs = time.Since(r.Started).Milliseconds()
	if !dryRun && len(only) == 0 {
		s.mu.Lock()
		s.last = r
		s.mu.Unlock()
		data, _ := json.MarshalIndent(r, "", " ")
		_ = os.MkdirAll(s.dataDir, 0o755)
		if err := config.WriteFileAtomic(s.path(), data); err != nil {
			log.Printf("保存定位结果失败: %v", err)
		}
		s.notify(r)
	}
	for _, c := range r.Changes {
		if !dryRun {
			log.Printf("端口定位：%s %s %s → %s %s", c.Type, c.NodeID, c.From, c.To, c.Location)
		}
	}
	return r, nil
}

func (s *Service) notify(r *RunResult) {
	if s.OnChanges != nil && len(r.Changes) > 0 {
		s.OnChanges(r)
	}
	if s.Notify == nil || !s.Cfg.Get().Locate.Notify {
		return
	}
	var lines []string
	for _, c := range r.Changes {
		if c.Type == "location" {
			continue
		}
		lines = append(lines, fmt.Sprintf("**已定位** %s %s → %s", c.Name, c.MAC, c.To))
	}
	s.mu.Lock()
	cur := map[string]bool{}
	for _, sg := range r.Suggestions {
		k := sg.NodeID + "|" + sg.To
		cur[k] = true
		if !s.notified[k] {
			lines = append(lines, fmt.Sprintf("**端口变化待确认** %s（%s）%s → %s", sg.Name, sg.MAC, sg.From, sg.To))
		}
	}
	s.notified = cur
	s.mu.Unlock()
	if len(lines) > 0 {
		s.Notify("端口定位", lines)
	}
}

// Last 返回最近一次定位结果。
func (s *Service) Last() *RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Running 是否正在定位。
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// PortRow 是 MAC 表中的一个端口。
type PortRow struct {
	Port   string    `json:"port"`
	IfName string    `json:"if_name"`
	Uplink string    `json:"uplink,omitempty"`
	MACs   []PortMAC `json:"macs"`
}

// PortMAC 是端口上学到的一个 MAC 及其对应的已登记设备。
type PortMAC struct {
	MAC    string `json:"mac"`
	VLAN   int    `json:"vlan,omitempty"`
	NodeID string `json:"node_id,omitempty"`
	Name   string `json:"name,omitempty"`
	IP     string `json:"ip,omitempty"`
}

// MACTable 返回交换机按端口分组的 MAC 表；refresh 为 true 或没有缓存时现场读取。
func (s *Service) MACTable(switchID string, refresh bool) ([]PortRow, *SwitchStatus, error) {
	t, _ := s.Topo.Snapshot()
	sw := t.FindSwitch(switchID)
	if sw == nil {
		return nil, nil, fmt.Errorf("交换机 %q 不存在", switchID)
	}
	s.mu.Lock()
	tb := s.tables[switchID]
	s.mu.Unlock()
	var st *SwitchStatus
	if tb == nil || refresh {
		var tg *Target
		for _, x := range s.Targets() {
			x := x
			if x.Switch == switchID {
				tg = &x
			}
		}
		if tg == nil {
			return nil, nil, fmt.Errorf("交换机 %s 没有可用的 SNMP 设置（管理 IP 或团体名为空，或已停用）", sw.Name)
		}
		_, sts := s.readAll([]Target{*tg})
		st = &sts[0]
		if !st.OK {
			return nil, st, errors.New(st.Error)
		}
		s.mu.Lock()
		tb = s.tables[switchID]
		s.mu.Unlock()
	}
	a := Analyze(t, map[string]*snmp.Table{switchID: tb})
	byMAC := map[string]*topology.Node{}
	for _, n := range t.Nodes {
		if n.MAC != "" {
			byMAC[n.MAC] = n
		}
	}
	used := map[string][]string{}
	for _, n := range t.Nodes {
		if n.Attach.Switch == switchID && n.Attach.Port != "" {
			used[switchID] = append(used[switchID], n.Attach.Port)
		}
	}
	rows := map[string]*PortRow{}
	var order []string
	for _, e := range tb.Entries {
		if !IsPhysical(e.IfName) && !IsAggregate(e.IfName) {
			continue
		}
		p := MatchPort(sw, e.IfName, used[switchID])
		r := rows[p]
		if r == nil {
			r = &PortRow{Port: p, IfName: e.IfName, Uplink: a.Uplinks[switchID][p], MACs: []PortMAC{}}
			rows[p] = r
			order = append(order, p)
		}
		pm := PortMAC{MAC: e.MAC, VLAN: e.VLAN}
		if n := byMAC[e.MAC]; n != nil {
			pm.NodeID, pm.Name, pm.IP = n.ID, n.Name, n.IP
		}
		r.MACs = append(r.MACs, pm)
	}
	out := make([]PortRow, 0, len(order))
	for _, p := range order {
		out = append(out, *rows[p])
	}
	return out, st, nil
}

// Find 返回某个 MAC 在各交换机上的出现位置与判定结果（使用缓存的 MAC 表，refresh 时重新读取）。
func (s *Service) Find(mac string, refresh bool) (*Placement, []Sighting, string, error) {
	if refresh || len(s.cached()) == 0 {
		targets := s.Targets()
		if len(targets) == 0 {
			return nil, nil, "", ErrNoTargets
		}
		s.readAll(targets)
	}
	t, _ := s.Topo.Snapshot()
	a := Analyze(t, s.cached())
	if p, ok := a.Placed[mac]; ok {
		return &p, p.All, "", nil
	}
	if l := a.Ambiguous[mac]; l != nil {
		return nil, l, "出现在多台交换机的普通端口上且 MAC 数相同，无法判断", nil
	}
	if l := a.UplinkOnly[mac]; l != nil {
		return nil, l, "只出现在级联口上，设备可能接在未读取 MAC 表的交换机下", nil
	}
	return nil, nil, "所有已读取的交换机 MAC 表中都没有该 MAC（设备可能离线超过 MAC 老化时间，或不在这些交换机下）", nil
}

func (s *Service) cached() map[string]*snmp.Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*snmp.Table, len(s.tables))
	for k, v := range s.tables {
		out[k] = v
	}
	return out
}

// Test 用给定地址和团体名测试 SNMP 连接，返回系统信息和 MAC 表条数。
func (s *Service) Test(host, community string) (map[string]interface{}, error) {
	if host == "" || community == "" {
		return nil, errors.New("需要读取地址和团体名")
	}
	tb, err := s.read(Target{Host: host, Community: community})
	if err != nil {
		return nil, err
	}
	ports := map[string]bool{}
	for _, e := range tb.Entries {
		ports[e.IfName] = true
	}
	return map[string]interface{}{"ok": true, "sys_name": tb.SysName, "sys_descr": tb.SysDescr, "interfaces": len(tb.IfNames),
		"mac_entries": len(tb.Entries), "ports_with_macs": len(ports), "source": tb.Source, "warnings": tb.Warnings}, nil
}

// ApplyMove 采纳一台设备的端口变化（使用最近读到的 MAC 表）。
func (s *Service) ApplyMove(nodeID string) (*RunResult, error) {
	r, err := s.RunNow("采纳端口变化", false, true, nodeID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.last != nil {
		keep := s.last.Suggestions[:0]
		for _, sg := range s.last.Suggestions {
			if sg.NodeID != nodeID {
				keep = append(keep, sg)
			}
		}
		s.last.Suggestions = keep
	}
	s.mu.Unlock()
	return r, nil
}
