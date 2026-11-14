package discovery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/pinger"
	"netping/internal/topology"
)

// 同一个 MAC 在 ARP 中对应超过这么多 IP，视为路由器代理 ARP，其 ARP 信息不可信。
const proxyARPThreshold = 8

// ScanResult 是一次扫描的完整结果。
type ScanResult struct {
	Job         string       `json:"job"`
	JobName     string       `json:"job_name"`
	Started     time.Time    `json:"started"`
	Finished    time.Time    `json:"finished"`
	DurationMs  int64        `json:"duration_ms"`
	Reason      string       `json:"reason"`
	Subnets     []string     `json:"subnets"`
	Scanned     int          `json:"scanned"`
	Alive       int          `json:"alive"`
	PingAlive   int          `json:"ping_alive"`
	ARPEntries  int          `json:"arp_entries"`
	ProxyMACs   []string     `json:"proxy_macs,omitempty"`
	Hosts       []Host       `json:"hosts"`
	Changes     []Change     `json:"changes"`
	Suggestions []Suggestion `json:"suggestions"`
	Unknown     []Host       `json:"unknown"`
	Error       string       `json:"error,omitempty"`
	ARPError    string       `json:"arp_error,omitempty"`
}

// AdhocJobID 是临时扫描（不属于任何任务）的结果键。
const AdhocJobID = "adhoc"

// Notifier 发送扫描变更通知（企业微信）。
type Notifier func(title string, lines []string)

// Service 网段扫描服务。
type Service struct {
	Cfg    *config.Manager
	Topo   *topology.Store
	Mon    *monitor.Monitor
	Notify Notifier
	// OnAdded 在扫描添加了新设备后调用（用于触发端口定位）。
	OnAdded func()

	dataDir string

	mu       sync.Mutex
	running  bool
	current  string // 正在运行的任务名
	done     int
	total    int
	phase    string
	last     *ScanResult            // 最近一次扫描（任意任务）
	results  map[string]*ScanResult // 每个任务最近一次扫描
	events   []Change
	pending  map[string]time.Time // 正在重新解析 IP 的节点（暂缓离线告警）
	lastTry  map[string]time.Time
	upAt     map[string]string // 节点最近一次能 ping 通时的 IP
	lastFast time.Time
}

// New 创建扫描服务，dataDir 用于保存各任务最近一次结果和变更记录。
func New(cfg *config.Manager, topo *topology.Store, mon *monitor.Monitor, dataDir string) *Service {
	s := &Service{Cfg: cfg, Topo: topo, Mon: mon, dataDir: dataDir, results: map[string]*ScanResult{},
		pending: map[string]time.Time{}, lastTry: map[string]time.Time{}, upAt: map[string]string{}}
	s.load()
	return s
}

func (s *Service) resultsPath() string { return filepath.Join(s.dataDir, "discovery_results.json") }
func (s *Service) eventsPath() string  { return filepath.Join(s.dataDir, "discovery_events.jsonl") }

func (s *Service) load() {
	if data, err := os.ReadFile(s.resultsPath()); err == nil {
		_ = json.Unmarshal(data, &s.results)
	} else if data, err := os.ReadFile(filepath.Join(s.dataDir, "discovery_last.json")); err == nil {
		var r ScanResult // 1.2.0 只保存了一次结果
		if json.Unmarshal(data, &r) == nil {
			r.Job, r.JobName = "default", "机房网段"
			s.results["default"] = &r
		}
	}
	if s.results == nil {
		s.results = map[string]*ScanResult{}
	}
	for _, r := range s.results {
		if s.last == nil || r.Finished.After(s.last.Finished) {
			s.last = r
		}
	}
	f, err := os.Open(s.eventsPath())
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var c Change
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			s.events = append(s.events, c)
		}
	}
	if len(s.events) > 500 {
		s.events = s.events[len(s.events)-500:]
	}
}

// JobSubnets 返回任务实际扫描的网段：未配置时使用拓扑中的 site.subnet。
func (s *Service) JobSubnets(j config.ScanJob) []string {
	if len(j.Subnets) > 0 {
		return j.Subnets
	}
	t, _ := s.Topo.Snapshot()
	if t.Site.Subnet != "" {
		return []string{t.Site.Subnet}
	}
	return nil
}

// Subnets 返回所有启用任务覆盖的网段（去重）。
func (s *Service) Subnets() []string {
	var out []string
	seen := map[string]bool{}
	for _, j := range s.Cfg.Get().Discovery.Jobs {
		if !j.Enabled {
			continue
		}
		for _, n := range s.JobSubnets(j) {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// JobStatus 是扫描任务的配置与最近运行情况。
type JobStatus struct {
	config.ScanJob
	Effective []string `json:"effective_subnets"`
	LastRun   string   `json:"last_run,omitempty"`
	NextRun   string   `json:"next_run,omitempty"`
	Alive     int      `json:"alive"`
	Scanned   int      `json:"scanned"`
	Changes   int      `json:"changes"`
	Pending   int      `json:"pending"` // 待确认 + 未登记
	Error     string   `json:"error,omitempty"`
}

// Status 返回扫描状态（供网页轮询）。
func (s *Service) Status() map[string]interface{} {
	d := s.Cfg.Get().Discovery
	jobs := make([]JobStatus, 0, len(d.Jobs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range d.Jobs {
		js := JobStatus{ScanJob: j, Effective: s.JobSubnets(j)}
		if js.Effective == nil {
			js.Effective = []string{}
		}
		if r := s.results[j.ID]; r != nil {
			js.LastRun = r.Finished.Format("2006-01-02 15:04:05")
			js.Alive, js.Scanned, js.Changes, js.Error = r.Alive, r.Scanned, len(r.Changes), r.Error
			js.Pending = len(r.Suggestions) + len(r.Unknown)
			if d.Enabled && j.Enabled {
				js.NextRun = r.Finished.Add(time.Duration(j.IntervalMin) * time.Minute).Format("2006-01-02 15:04:05")
			}
		}
		jobs = append(jobs, js)
	}
	return map[string]interface{}{
		"running": s.running, "job": s.current, "done": s.done, "total": s.total, "phase": s.phase,
		"enabled": d.Enabled, "jobs": jobs,
	}
}

// Last 返回最近一次扫描结果（任意任务）。
func (s *Service) Last() *ScanResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Results 返回各任务最近一次扫描结果（含临时扫描 adhoc）。
func (s *Service) Results() map[string]*ScanResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*ScanResult, len(s.results))
	for k, v := range s.results {
		out[k] = v
	}
	return out
}

// Events 返回最近的变更记录（新的在前）。
func (s *Service) Events(limit int) []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.events)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Change, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, s.events[i])
	}
	return out
}

// HoldDown 供告警引擎调用：节点正在重新解析 IP 时暂缓离线告警（最长 90 秒）。
func (s *Service) HoldDown(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.pending[id]
	return ok && time.Now().Before(until)
}

// ErrRunning 表示已有扫描在进行。
var ErrRunning = errors.New("已有扫描正在进行，请稍后")

// Running 是否有扫描在进行。
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Run 按各任务的间隔自动扫描，并处理“离线即重新解析”，直到 stop 关闭。
func (s *Service) Run(stop <-chan struct{}) {
	start := time.Now()
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tk.C:
			d := s.Cfg.Get().Discovery
			if s.Mon.Paused() || s.Running() {
				continue
			}
			var due []string
			if d.Enabled && now.Sub(start) > 30*time.Second {
				s.mu.Lock()
				for _, j := range d.Jobs {
					r := s.results[j.ID]
					if j.Enabled && (r == nil || now.Sub(r.Finished) >= time.Duration(j.IntervalMin)*time.Minute) {
						due = append(due, j.ID)
					}
				}
				s.mu.Unlock()
			}
			if len(due) > 0 {
				go s.scanLogged(due, "定时扫描")
				continue
			}
			if d.FastResolve && d.IPTracking != "off" {
				s.checkFastResolve(now, d)
			}
		}
	}
}

func (s *Service) scanLogged(ids []string, reason string) {
	rs, err := s.ScanJobs(ids, reason)
	if err != nil && !errors.Is(err, ErrRunning) {
		log.Printf("网段扫描失败: %v", err)
	}
	for _, r := range rs {
		if r.Error != "" {
			log.Printf("扫描任务「%s」（%s）失败：%s", r.JobName, reason, r.Error)
			continue
		}
		log.Printf("扫描任务「%s」（%s）完成：%d 个地址在线，%d 项变更，用时 %dms", r.JobName, reason, r.Alive, len(r.Changes), r.DurationMs)
	}
}

// checkFastResolve：可跟踪的设备连续 ping 失败时，立即扫描其所在网段的任务，以便在离线告警前更新 IP。
func (s *Service) checkFastResolve(now time.Time, d config.Discovery) {
	t, _ := s.Topo.Snapshot()
	var all []string
	for _, j := range d.Jobs {
		all = append(all, s.JobSubnets(j)...)
	}
	cands := map[string]string{} // id → ip
	for _, st := range s.Mon.Statuses(false) {
		if st.Kind != "node" {
			continue
		}
		s.mu.Lock()
		if st.ConsecFail == 0 && st.ConsecOK > 0 {
			s.upAt[st.ID] = st.IP // 记录设备在该 IP 上能 ping 通
		}
		wasUp := st.IP != "" && s.upAt[st.ID] == st.IP
		recent := now.Sub(s.lastTry[st.ID]) < 10*time.Minute
		s.mu.Unlock()
		// 只处理“原来能 ping 通、现在连续失败”的设备；从未响应过 ping 的设备不触发
		if st.ConsecFail < 2 || !wasUp || recent {
			continue
		}
		n := t.FindNode(st.ID)
		if n == nil || n.IP != st.IP || !Trackable(n, d.IPTracking) || !config.InSubnets(n.IP, all) {
			continue
		}
		cands[n.ID] = n.IP
	}
	if len(cands) == 0 {
		return
	}
	var jobs []string
	for _, j := range d.Jobs {
		subs := s.JobSubnets(j)
		for _, ip := range cands {
			if config.InSubnets(ip, subs) {
				jobs = append(jobs, j.ID)
				break
			}
		}
	}
	s.mu.Lock()
	if len(jobs) == 0 || now.Sub(s.lastFast) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastFast = now
	for id := range cands {
		s.lastTry[id] = now
		s.pending[id] = now.Add(90 * time.Second)
	}
	s.mu.Unlock()
	go func() {
		s.scanLogged(jobs, fmt.Sprintf("%d 台可跟踪设备 ping 失败，重新解析 IP", len(cands)))
		s.mu.Lock()
		for id := range cands {
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}()
}

func (s *Service) setPhase(p string, done, total int) {
	s.mu.Lock()
	s.phase, s.done, s.total = p, done, total
	s.mu.Unlock()
}

func (s *Service) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Service) end() {
	s.mu.Lock()
	s.running, s.phase, s.current = false, "", ""
	s.mu.Unlock()
}

// ScanJobs 依次运行指定的扫描任务（ids 为空表示全部任务，含未启用定时的），同步返回各任务结果。
func (s *Service) ScanJobs(ids []string, reason string) ([]*ScanResult, error) {
	d := s.Cfg.Get().Discovery
	var jobs []config.ScanJob
	if len(ids) == 0 {
		jobs = d.Jobs
	} else {
		for _, id := range ids {
			j, ok := d.FindJob(id)
			if !ok {
				return nil, fmt.Errorf("扫描任务 %q 不存在", id)
			}
			jobs = append(jobs, j)
		}
	}
	if len(jobs) == 0 {
		return nil, errors.New("还没有扫描任务，请先添加（指定一个或多个网段）")
	}
	if !s.begin() {
		return nil, ErrRunning
	}
	defer s.end()
	var out []*ScanResult
	for _, j := range jobs {
		out = append(out, s.scanOne(j, d, reason))
	}
	return out, nil
}

// ScanAdhoc 临时扫描指定网段（不保存为任务），autoAdd 决定是否自动添加新设备。
func (s *Service) ScanAdhoc(subnets []string, autoAdd bool, reason string) (*ScanResult, error) {
	d := s.Cfg.Get().Discovery
	subnets = trimList(subnets)
	if len(subnets) == 0 {
		return nil, errors.New("请指定要扫描的网段，如 192.168.1.0/24")
	}
	if _, err := config.ExpandSubnets(subnets, d.Exclude); err != nil {
		return nil, err
	}
	if !s.begin() {
		return nil, ErrRunning
	}
	defer s.end()
	j := config.ScanJob{ID: AdhocJobID, Name: "临时扫描", Subnets: subnets, AutoAdd: autoAdd}
	return s.scanOne(j, d, reason), nil
}

func trimList(in []string) []string {
	var out []string
	for _, x := range in {
		for _, y := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' }) {
			if y = strings.TrimSpace(y); y != "" {
				out = append(out, y)
			}
		}
	}
	return out
}

// jobOptions 返回任务对应的核对选项。
func jobOptions(j config.ScanJob, d config.Discovery) Options {
	src := j.Name
	if j.ID == AdhocJobID {
		src = "临时扫描"
	}
	return Options{Tracking: d.IPTracking, AutoAdd: j.AutoAdd, Now: time.Now(), OUI: d.OUI, Tags: j.Tags, Alert: j.Alert, Source: src}
}

// scanOne 执行一个任务的扫描并把结果应用到拓扑（调用方已持有 running）。
func (s *Service) scanOne(j config.ScanJob, d config.Discovery, reason string) *ScanResult {
	s.mu.Lock()
	s.current = j.Name
	s.mu.Unlock()
	res := &ScanResult{Job: j.ID, JobName: j.Name, Started: time.Now(), Reason: reason, Subnets: s.JobSubnets(j)}
	finish := func(err error) *ScanResult {
		if err != nil {
			res.Error = err.Error()
		}
		if res.Subnets == nil {
			res.Subnets = []string{}
		}
		if res.Hosts == nil {
			res.Hosts = []Host{}
		}
		if res.Changes == nil {
			res.Changes = []Change{}
		}
		if res.Suggestions == nil {
			res.Suggestions = []Suggestion{}
		}
		if res.Unknown == nil {
			res.Unknown = []Host{}
		}
		res.Finished = time.Now()
		res.DurationMs = res.Finished.Sub(res.Started).Milliseconds()
		s.save(res)
		return res
	}
	if len(res.Subnets) == 0 {
		return finish(errors.New("未配置扫描网段（任务的 subnets 或 topology.yaml 的 site.subnet）"))
	}
	ips, err := config.ExpandSubnets(res.Subnets, d.Exclude)
	if err != nil {
		return finish(err)
	}
	res.Scanned = len(ips)

	// 1. ping 扫描
	alive := s.sweep(ips, d)
	// 2. ARP
	s.setPhase("读取 ARP 表", len(ips), len(ips))
	arp, aerr := ReadARP()
	if aerr != nil {
		res.ARPError = aerr.Error()
	}
	res.ARPEntries = len(arp)
	if len(arp) == 0 && aerr == nil && len(alive) > 0 && arpEmptyHint != "" {
		res.ARPError = arpEmptyHint
	}
	hosts := mergeHosts(ips, alive, arp, d.Exclude, res)
	markLocal(hosts)
	// 3. 主机名（NetBIOS / mDNS / 反向 DNS）
	t, _ := s.Topo.Snapshot()
	s.setPhase("解析主机名", len(ips), len(ips))
	ResolveNames(hosts)
	// 4. 核对并应用
	s.setPhase("更新拓扑", len(ips), len(ips))
	opt := jobOptions(j, d)
	preview := Reconcile(t.Clone(), hosts, opt)
	applied := preview
	hasMutation := false
	for _, c := range preview.Changes {
		if c.Type != ChConflict {
			hasMutation = true
		}
	}
	if hasMutation {
		err = s.Topo.Update(func(tt *topology.Topology) error {
			applied = Reconcile(tt, hosts, opt)
			return nil
		})
		if err != nil {
			res.Hosts = hosts
			return finish(fmt.Errorf("应用扫描结果失败: %w", err))
		}
	}
	for i := range applied.Changes {
		applied.Changes[i].Job = j.Name
	}
	res.Changes, res.Suggestions, res.Unknown = applied.Changes, applied.Suggestions, applied.Unknown
	t, _ = s.Topo.Snapshot()
	annotate(hosts, t, applied)
	res.Hosts = hosts
	s.recordEvents(applied.Changes)
	s.notify(res)
	s.added(applied.Changes)
	return finish(nil)
}

func (s *Service) sweep(ips []string, d config.Discovery) map[string]float64 {
	alive := map[string]float64{}
	var mu sync.Mutex
	jobs := make(chan string)
	var wg sync.WaitGroup
	done := 0
	timeout := time.Duration(d.TimeoutMs) * time.Millisecond
	workers := d.Concurrency
	if workers > len(ips) {
		workers = len(ips)
	}
	s.setPhase("ping 扫描", 0, len(ips))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := pinger.New()
			if err != nil {
				for range jobs {
				}
				return
			}
			defer p.Close()
			for ip := range jobs {
				r, _ := p.Ping(ip, timeout, 32)
				mu.Lock()
				if r.OK {
					alive[ip] = math.Round(float64(r.RTT)/float64(time.Millisecond)*100) / 100
				}
				done++
				dn := done
				mu.Unlock()
				if dn%8 == 0 {
					s.setPhase("ping 扫描", dn, len(ips))
				}
			}
		}()
	}
	for _, ip := range ips {
		jobs <- ip
	}
	close(jobs)
	wg.Wait()
	return alive
}

// mergeHosts 合并 ping 结果与 ARP 表，并识别代理 ARP。
func mergeHosts(ips []string, alive map[string]float64, arp []ARPEntry, exclude []string, res *ScanResult) []Host {
	inScope := map[string]bool{}
	for _, ip := range ips {
		inScope[ip] = true
	}
	macCount := map[string]int{}
	ipMAC := map[string]string{}
	for _, e := range arp {
		if inScope[e.IP] {
			ipMAC[e.IP] = e.MAC
			macCount[e.MAC]++
		}
	}
	proxy := map[string]bool{}
	for mac, n := range macCount {
		if n > proxyARPThreshold {
			proxy[mac] = true
			res.ProxyMACs = append(res.ProxyMACs, mac)
		}
	}
	sort.Strings(res.ProxyMACs)
	var hosts []Host
	for _, ip := range ips {
		rtt, ok := alive[ip]
		mac := ipMAC[ip]
		if proxy[mac] {
			mac = ""
		}
		if !ok && mac == "" {
			continue
		}
		if config.Excluded(ip, exclude) {
			continue
		}
		hosts = append(hosts, Host{IP: ip, MAC: mac, Ping: ok, RTT: rtt, ARP: mac != ""})
		if ok {
			res.PingAlive++
		}
	}
	res.Alive = len(hosts)
	return hosts
}

// markLocal 标记本机地址并补上本机网卡 MAC（本机不在自己的 ARP 表里）。
func markLocal(hosts []Host) {
	ifs, err := net.Interfaces()
	if err != nil {
		return
	}
	local := map[string]string{}
	for _, inf := range ifs {
		addrs, _ := inf.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				local[ipn.IP.String()] = macString(inf.HardwareAddr)
			}
		}
	}
	for i := range hosts {
		if mac, ok := local[hosts[i].IP]; ok {
			hosts[i].Local = true
			if hosts[i].MAC == "" {
				hosts[i].MAC = mac
			}
			if hosts[i].Hostname == "" {
				hosts[i].Hostname = localHostname()
			}
		}
	}
}

// annotate 为每个在线地址标注在拓扑中的状态。
func annotate(hosts []Host, t *topology.Topology, r Result) {
	byIP := map[string]*topology.Node{}
	for _, n := range t.Nodes {
		if n.IP != "" {
			byIP[n.IP] = n
		}
	}
	swIP := map[string]bool{}
	for _, s := range t.Switches {
		swIP[s.IP] = true
	}
	changed := map[string]string{}
	for _, c := range r.Changes {
		if c.IP != "" && c.Type != ChHostname {
			changed[c.IP] = c.Type
		}
	}
	unknown := map[string]bool{}
	for _, u := range r.Unknown {
		unknown[u.IP] = true
	}
	vendors := LearnOUI(t, nil)
	for i := range hosts {
		h := &hosts[i]
		if h.Vendor == "" {
			h.Vendor = VendorOf(h.MAC, vendors)
		}
		switch {
		case swIP[h.IP]:
			h.Status = "switch"
		case byIP[h.IP] != nil:
			h.NodeID = byIP[h.IP].ID
			h.Status = "registered"
			if ct, ok := changed[h.IP]; ok && ct != ChConflict {
				h.Status = ct
			}
		case unknown[h.IP]:
			h.Status = "unknown"
		default:
			h.Status = "conflict"
		}
	}
}

func (s *Service) recordEvents(cs []Change) {
	if len(cs) == 0 {
		return
	}
	s.mu.Lock()
	s.events = append(s.events, cs...)
	if len(s.events) > 500 {
		s.events = append([]Change(nil), s.events[len(s.events)-500:]...)
	}
	s.mu.Unlock()
	if fi, err := os.Stat(s.eventsPath()); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(s.eventsPath(), s.eventsPath()+".1")
	}
	f, err := os.OpenFile(s.eventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("写入扫描记录失败: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range cs {
		_ = enc.Encode(c)
		log.Printf("网段扫描：%s %s %s %s", c.Type, c.NodeID, c.IP, c.Detail)
	}
}

func (s *Service) save(r *ScanResult) {
	s.mu.Lock()
	s.last = r
	s.results[r.Job] = r
	// 删除已不存在的任务的结果
	keep := map[string]bool{AdhocJobID: true}
	for _, j := range s.Cfg.Get().Discovery.Jobs {
		keep[j.ID] = true
	}
	for k := range s.results {
		if !keep[k] {
			delete(s.results, k)
		}
	}
	data, _ := json.MarshalIndent(s.results, "", " ")
	s.mu.Unlock()
	_ = os.MkdirAll(s.dataDir, 0o755)
	if err := config.WriteFileAtomic(s.resultsPath(), data); err != nil {
		log.Printf("保存扫描结果失败: %v", err)
	}
}

func (s *Service) persist() {
	s.mu.Lock()
	data, _ := json.MarshalIndent(s.results, "", " ")
	s.mu.Unlock()
	_ = config.WriteFileAtomic(s.resultsPath(), data)
}

func (s *Service) added(cs []Change) {
	for _, c := range cs {
		if c.Type == ChAdded && c.MAC != "" && s.OnAdded != nil {
			s.OnAdded()
			return
		}
	}
}

var changeLabel = map[string]string{
	ChAdded: "新设备", ChIPChanged: "IP 变化", ChIPReleased: "IP 释放", ChMACLearned: "补全 MAC", ChConflict: "冲突", ChHostname: "主机名",
}

func (s *Service) notify(r *ScanResult) {
	if s.Notify == nil || !s.Cfg.Get().Discovery.Notify {
		return
	}
	var lines []string
	for _, c := range r.Changes {
		if c.Type == ChMACLearned || c.Type == ChHostname {
			continue
		}
		ip := c.IP
		if c.Type == ChIPChanged || c.Type == ChIPReleased {
			ip = orDash(c.OldIP) + " → " + orDash(c.IP)
		}
		lines = append(lines, fmt.Sprintf("**%s** %s %s %s", changeLabel[c.Type], ip, c.Name, c.MAC))
	}
	for _, sg := range r.Suggestions {
		lines = append(lines, fmt.Sprintf("**待确认** %s 的 MAC %s 出现在 %s（%s）", sg.CurrentIP, sg.MAC, sg.SeenIP, sg.Name))
	}
	if len(lines) > 0 {
		s.Notify("网段扫描「"+r.JobName+"」发现变化", lines)
	}
}

// ---------------------------------------------------------------------------
// 手动操作

// AddUnknown 把各任务最近一次扫描中未自动添加的设备加入拓扑（按所属任务附加标签/告警）。
func (s *Service) AddUnknown(ips []string) ([]Change, error) {
	want := map[string]bool{}
	for _, ip := range ips {
		want[ip] = true
	}
	type cand struct {
		h   Host
		job config.ScanJob
	}
	d := s.Cfg.Get().Discovery
	var cands []cand
	seen := map[string]bool{}
	for jid, r := range s.Results() {
		j, ok := d.FindJob(jid)
		if !ok {
			j = config.ScanJob{ID: jid, Name: r.JobName}
		}
		for _, h := range r.Unknown {
			if want[h.IP] && !seen[h.IP] {
				seen[h.IP] = true
				cands = append(cands, cand{h, j})
			}
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("扫描结果中没有这些未登记设备，请先扫描")
	}
	var added []Change
	now := time.Now()
	err := s.Topo.Update(func(t *topology.Topology) error {
		added = nil
		used := map[string]bool{}
		for _, n := range t.Nodes {
			used[n.IP] = true
			if n.MAC != "" {
				used[n.MAC] = true
			}
		}
		for _, c := range cands {
			h := c.h
			if used[h.IP] || (h.MAC != "" && len(h.SharedMAC) == 0 && used[h.MAC]) {
				continue
			}
			n := NewNode(t, h, now)
			jobOptions(c.job, d).Decorate(n)
			t.Nodes = append(t.Nodes, n)
			used[h.IP] = true
			if n.MAC != "" {
				used[n.MAC] = true
			}
			added = append(added, Change{Time: now, Type: ChAdded, NodeID: n.ID, Name: nodeLabel(n), IP: h.IP, MAC: h.MAC, Detail: "手动添加扫描发现的设备", Job: c.job.Name})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, r := range s.results {
		keep := r.Unknown[:0]
		for _, h := range r.Unknown {
			if !want[h.IP] {
				keep = append(keep, h)
			}
		}
		r.Unknown = keep
	}
	s.mu.Unlock()
	s.persist()
	s.recordEvents(added)
	s.added(added)
	return added, nil
}

// Suggestions 汇总各任务最近一次扫描中待确认的 IP 变化。
func (s *Service) Suggestions() []Suggestion {
	out := []Suggestion{}
	seen := map[string]bool{}
	for _, r := range s.Results() {
		for _, sg := range r.Suggestions {
			if !seen[sg.NodeID] {
				seen[sg.NodeID] = true
				out = append(out, sg)
			}
		}
	}
	return out
}

// Unknown 汇总各任务最近一次扫描中未登记的设备。
func (s *Service) Unknown() []Host {
	out := []Host{}
	seen := map[string]bool{}
	for _, r := range s.Results() {
		for _, h := range r.Unknown {
			if !seen[h.IP] {
				seen[h.IP] = true
				out = append(out, h)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return ipLess(out[i].IP, out[j].IP) })
	return out
}

func ipLess(a, b string) bool {
	pa, pb := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if pa == nil || pb == nil {
		return a < b
	}
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// ApplySuggestion 手动采纳“IP 变化”建议：把节点 IP 改为 ip，ipMode 为 "-" 时不改分配方式。
func (s *Service) ApplySuggestion(nodeID, ip, ipMode string) (Change, error) {
	var ch Change
	err := s.Topo.Update(func(t *topology.Topology) error {
		n := t.FindNode(nodeID)
		if n == nil {
			return fmt.Errorf("节点 %s 不存在", nodeID)
		}
		if k, other := t.FindByIP(ip); k != "" && other != nodeID {
			return fmt.Errorf("IP %s 已被 %s 使用", ip, other)
		}
		ch = Change{Time: time.Now(), Type: ChIPChanged, NodeID: n.ID, Name: nodeLabel(n), OldIP: n.IP, IP: ip, MAC: n.MAC, Detail: "手动采纳扫描建议"}
		n.IP = ip
		if ipMode != "-" {
			n.IPMode = ipMode
		}
		return nil
	})
	if err != nil {
		return ch, err
	}
	s.mu.Lock()
	for _, r := range s.results {
		keep := r.Suggestions[:0]
		for _, sg := range r.Suggestions {
			if sg.NodeID != nodeID {
				keep = append(keep, sg)
			}
		}
		r.Suggestions = keep
	}
	s.mu.Unlock()
	s.persist()
	s.recordEvents([]Change{ch})
	return ch, nil
}

// ---------------------------------------------------------------------------
// 只读探测（AI 自主发现 / MCP 使用）

// ProbeMaxHosts 一次只读探测最多的地址数。
const ProbeMaxHosts = 1024

// Probe 只读探测网段：ping 扫描 + 读取 ARP + 解析主机名 + 标注是否已登记，不修改拓扑、不保存结果。
// 与定时扫描共用“正在扫描”标记，避免同时大量发包。
func (s *Service) Probe(subnets []string, reason string) ([]Host, error) {
	d := s.Cfg.Get().Discovery
	subnets = trimList(subnets)
	if len(subnets) == 0 {
		return nil, errors.New("请指定要探测的网段，如 192.168.1.0/24")
	}
	ips, err := config.ExpandSubnets(subnets, d.Exclude)
	if err != nil {
		return nil, err
	}
	if len(ips) > ProbeMaxHosts {
		return nil, fmt.Errorf("一次最多探测 %d 个地址（本次 %d 个），请拆小网段", ProbeMaxHosts, len(ips))
	}
	if !s.begin() {
		return nil, ErrRunning
	}
	defer s.end()
	s.mu.Lock()
	s.current = reason
	s.mu.Unlock()
	alive := s.sweep(ips, d)
	s.setPhase("读取 ARP 表", len(ips), len(ips))
	arp, _ := ReadARP()
	var tmp ScanResult
	hosts := mergeHosts(ips, alive, arp, d.Exclude, &tmp)
	markLocal(hosts)
	s.setPhase("解析主机名", len(ips), len(ips))
	ResolveNames(hosts)
	t, _ := s.Topo.Snapshot()
	MarkKnown(hosts, t, d.OUI)
	return hosts, nil
}

// MarkKnown 补全厂商，并标注每个地址在拓扑中的状态：switch / registered（node_id）/ new；本机为 local。
func MarkKnown(hosts []Host, t *topology.Topology, oui map[string]string) {
	byIP, byMAC := map[string]string{}, map[string]string{}
	for _, n := range t.Nodes {
		if n.IP != "" {
			byIP[n.IP] = n.ID
		}
		if n.MAC != "" {
			byMAC[n.MAC] = n.ID
		}
	}
	swIP := map[string]string{}
	for _, sw := range t.Switches {
		if sw.IP != "" {
			swIP[sw.IP] = sw.ID
		}
	}
	vendors := LearnOUI(t, oui)
	for i := range hosts {
		h := &hosts[i]
		if h.Vendor == "" {
			h.Vendor = VendorOf(h.MAC, vendors)
		}
		switch {
		case swIP[h.IP] != "":
			h.NodeID, h.Status = swIP[h.IP], "switch"
		case byIP[h.IP] != "":
			h.NodeID, h.Status = byIP[h.IP], "registered"
		case h.MAC != "" && len(h.SharedMAC) == 0 && byMAC[h.MAC] != "":
			h.NodeID, h.Status = byMAC[h.MAC], "moved" // 已登记设备换了 IP
		case h.Local:
			h.Status = "local"
		default:
			h.Status = "new"
		}
	}
}
