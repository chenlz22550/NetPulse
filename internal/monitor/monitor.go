// Package monitor 按目标调度 ping，维护最近 1 小时的秒级样本并计算实时指标。
package monitor

import (
	"log"
	"math"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"netping/internal/config"
	"netping/internal/pinger"
	"netping/internal/store"
	"netping/internal/topology"
)

// 节点状态。
const (
	StateUnknown  = "unknown"
	StateOK       = "ok"
	StateSlow     = "slow"
	StateLoss     = "loss"
	StateDown     = "down"
	StatePaused   = "paused"
	StateDisabled = "disabled"
)

// Target 是一个被监控的地址。
type Target struct {
	ParentID      string
	Port          int
	ID            string
	Kind          string // node | switch
	IP            string
	Interval      time.Duration
	Timeout       time.Duration
	Size          int
	LatencyWarnMs float64
	LossWarnPct   float64
}

// Sample 单次 ping 结果，RTT<0 表示丢包。
type Sample struct {
	T   int64   `json:"t"` // Unix 毫秒
	RTT float64 `json:"rtt"`
}

// Status 是一个目标的实时指标。
type Status struct {
	ParentID      string    `json:"parent_id,omitempty"`
	Port          int       `json:"port,omitempty"`
	SSHOpen       bool      `json:"ssh_open"`
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	IP            string    `json:"ip"`
	State         string    `json:"state"`
	StateSince    int64     `json:"state_since"`
	LastRTT       float64   `json:"last_rtt"` // -1 表示最近一次丢包
	LastTime      int64     `json:"last_time"`
	ConsecFail    int       `json:"consec_fail"`
	ConsecOK      int       `json:"consec_ok"`
	Sent1m        int       `json:"sent_1m"`
	Loss1m        float64   `json:"loss_1m"`
	Loss5m        float64   `json:"loss_5m"`
	Loss1h        float64   `json:"loss_1h"`
	Avg1m         float64   `json:"avg_1m"`
	Min1m         float64   `json:"min_1m"`
	Max1m         float64   `json:"max_1m"`
	Jitter1m      float64   `json:"jitter_1m"`
	Avg5m         float64   `json:"avg_5m"`
	Avg1h         float64   `json:"avg_1h"`
	LatencyWarnMs float64   `json:"latency_warn_ms"`
	LossWarnPct   float64   `json:"loss_warn_pct"`
	Spark         []float64 `json:"spark,omitempty"`
}

// BuildTargets 根据拓扑和默认配置生成监控目标列表。
func BuildTargets(t *topology.Topology, d config.PingDefaults) []Target {
	mk := func(id, kind, ip string, m topology.MonitorSpec) Target {
		tg := Target{
			ID: id, Kind: kind, IP: ip,
			Interval:      time.Duration(d.IntervalMs) * time.Millisecond,
			Timeout:       time.Duration(d.TimeoutMs) * time.Millisecond,
			Size:          d.PacketSize,
			LatencyWarnMs: float64(d.LatencyWarnMs),
			LossWarnPct:   d.LossWarnPct,
		}
		if m.IntervalMs >= 200 {
			tg.Interval = time.Duration(m.IntervalMs) * time.Millisecond
		}
		if m.TimeoutMs > 0 {
			tg.Timeout = time.Duration(m.TimeoutMs) * time.Millisecond
		}
		if m.LatencyWarnMs > 0 {
			tg.LatencyWarnMs = float64(m.LatencyWarnMs)
		}
		if m.LossWarnPct > 0 {
			tg.LossWarnPct = m.LossWarnPct
		}
		return tg
	}
	var out []Target
	for _, s := range t.Switches {
		if s.IP != "" && s.Monitor.IsEnabled() {
			out = append(out, mk(s.ID, "switch", s.IP, s.Monitor))
			for _, port := range s.Monitor.TCPPorts {
				tg := mk(TCPID(s.ID, port), "tcp", s.IP, s.Monitor)
				tg.ParentID, tg.Port = s.ID, port
				out = append(out, tg)
			}
		}
	}
	for _, n := range t.Nodes {
		if n.IP != "" && n.Monitor.IsEnabled() {
			out = append(out, mk(n.ID, "node", n.IP, n.Monitor))
			for _, port := range n.Monitor.TCPPorts {
				tg := mk(TCPID(n.ID, port), "tcp", n.IP, n.Monitor)
				tg.ParentID, tg.Port = n.ID, port
				out = append(out, tg)
			}
		}
	}
	return out
}

type flushItem struct {
	id string
	m  store.Minute
}

// Monitor 管理所有监控协程。
type Monitor struct {
	mu        sync.RWMutex
	workers   map[string]*worker
	store     *store.Store
	flushCh   chan flushItem
	paused    atomic.Bool
	downAfter func() int
	active    atomic.Value // func() bool：当前是否处于监控时段
	wg        sync.WaitGroup
	closed    chan struct{}
}

// New 创建 Monitor。downAfter 返回“连续失败几次判定离线”。
func New(st *store.Store, downAfter func() int) *Monitor {
	m := &Monitor{
		workers:   map[string]*worker{},
		store:     st,
		flushCh:   make(chan flushItem, 4096),
		downAfter: downAfter,
		closed:    make(chan struct{}),
	}
	go m.writer()
	return m
}

func (m *Monitor) writer() {
	for it := range m.flushCh {
		if m.store == nil {
			continue
		}
		if err := m.store.Append(it.id, it.m); err != nil {
			log.Printf("写入历史数据失败 %s: %v", it.id, err)
		}
	}
	close(m.closed)
}

// Sync 让正在运行的监控与目标列表一致。
func (m *Monitor) Sync(targets []Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]Target{}
	for _, t := range targets {
		want[t.ID] = t
	}
	for id, w := range m.workers {
		t, ok := want[id]
		if !ok || t.IP != w.getTarget().IP {
			w.stop()
			delete(m.workers, id)
		}
	}
	for id, t := range want {
		if w, ok := m.workers[id]; ok {
			w.setTarget(t)
			continue
		}
		w := newWorker(t, m)
		m.workers[id] = w
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			w.run()
		}()
	}
}

// SetPaused 手动暂停/恢复所有 ping。
func (m *Monitor) SetPaused(p bool) { m.paused.Store(p) }

// SetSchedule 设置“当前是否处于监控时段”的判断函数。
func (m *Monitor) SetSchedule(active func() bool) { m.active.Store(active) }

// ManualPaused 是否被手动暂停。
func (m *Monitor) ManualPaused() bool { return m.paused.Load() }

// InSchedule 当前是否处于监控时段（未配置时段时始终为 true）。
func (m *Monitor) InSchedule() bool {
	if f, ok := m.active.Load().(func() bool); ok && f != nil {
		return f()
	}
	return true
}

// Paused 是否暂停中（手动暂停或不在监控时段）。
func (m *Monitor) Paused() bool { return m.paused.Load() || !m.InSchedule() }

// PauseReason 返回暂停原因：manual、schedule 或空字符串。
func (m *Monitor) PauseReason() string {
	switch {
	case m.paused.Load():
		return "manual"
	case !m.InSchedule():
		return "schedule"
	}
	return ""
}

// Statuses 返回所有目标的实时状态（按 id 排序）。
func (m *Monitor) Statuses(withSpark bool) []Status { return m.statuses(withSpark, false) }

func (m *Monitor) TCPStatuses(withSpark bool) []Status { return m.statuses(withSpark, true) }

func (m *Monitor) statuses(withSpark, tcp bool) []Status {
	m.mu.RLock()
	ws := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		ws = append(ws, w)
	}
	m.mu.RUnlock()
	out := make([]Status, 0, len(ws))
	for _, w := range ws {
		st := w.status(withSpark)
		if (st.Kind == "tcp") == tcp {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Status 返回单个目标的实时状态。
func (m *Monitor) Status(id string) (Status, bool) {
	m.mu.RLock()
	w, ok := m.workers[id]
	m.mu.RUnlock()
	if !ok {
		return Status{}, false
	}
	return w.status(true), true
}

// Recent 返回 sinceMs（Unix 毫秒）之后的原始样本。
func (m *Monitor) Recent(id string, sinceMs int64) []Sample {
	m.mu.RLock()
	w, ok := m.workers[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return w.recent(sinceMs)
}

// Close 停止所有协程并把未落盘的分钟数据写入。
func (m *Monitor) Close() {
	m.mu.Lock()
	for id, w := range m.workers {
		w.stop()
		delete(m.workers, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
	close(m.flushCh)
	<-m.closed
}

// ---------------------------------------------------------------------------

type minuteAgg struct {
	t             int64
	sent, recv    int
	sum, min, max float64
}

type worker struct {
	sshOpen     bool
	sshChecking bool
	sshChecked  time.Time
	m           *Monitor
	mu          sync.Mutex
	target      Target
	ring        []Sample
	head        int
	n           int

	consecFail int
	consecOK   int
	last       Sample
	hasLast    bool
	state      string
	stateSince int64
	cur        minuteAgg

	quit    chan struct{}
	retimer chan time.Duration
}

func newWorker(t Target, m *Monitor) *worker {
	capacity := int(time.Hour/t.Interval) + 16
	if capacity < 120 {
		capacity = 120
	}
	return &worker{
		m:          m,
		target:     t,
		ring:       make([]Sample, capacity),
		state:      StateUnknown,
		stateSince: time.Now().UnixMilli(),
		quit:       make(chan struct{}),
		retimer:    make(chan time.Duration, 1),
	}
}

func (w *worker) getTarget() Target {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.target
}

func (w *worker) setTarget(t Target) {
	w.mu.Lock()
	old := w.target
	// 更密集的采样需要更大的窗口。只扩容，避免调慢采样时丢失已有历史。
	if capacity := int(time.Hour/t.Interval) + 16; capacity > len(w.ring) {
		ring := make([]Sample, capacity)
		for i := 0; i < w.n; i++ {
			ring[i] = w.ring[(w.head-w.n+i+len(w.ring))%len(w.ring)]
		}
		w.ring = ring
		w.head = w.n
	}
	if old.IP != t.IP {
		w.sshOpen = false
		w.sshChecked = time.Time{}
	}
	w.target = t
	w.mu.Unlock()
	if old.Interval != t.Interval {
		select {
		case w.retimer <- t.Interval:
		default:
		}
	}
}

func (w *worker) stop() { close(w.quit) }

func (w *worker) run() {
	var p pinger.Pinger
	if w.getTarget().Port == 0 {
		var err error
		p, err = pinger.New()
		if err != nil {
			log.Printf("创建 ping 失败 %s: %v", w.target.ID, err)
			return
		}
		defer p.Close()
	}
	defer w.flushMinute(true)

	interval := w.getTarget().Interval
	// 随机错开启动相位，避免所有目标在同一时刻发包。
	select {
	case <-time.After(time.Duration(rand.Int63n(int64(interval)))):
	case <-w.quit:
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if w.m.Paused() {
			w.markPaused()
		} else {
			t := w.getTarget()
			var res pinger.Result
			var err error
			if t.Port > 0 {
				res = tcpPing(t.IP, t.Port, t.Timeout)
			} else {
				res, err = p.Ping(t.IP, t.Timeout, t.Size)
			}
			if err != nil {
				log.Printf("ping %s(%s) 出错: %v", t.ID, t.IP, err)
			}
			w.record(time.Now(), res)
			w.checkSSH()
		}
		select {
		case <-w.quit:
			return
		case d := <-w.retimer:
			ticker.Reset(d)
		case <-ticker.C:
		}
	}
}

// checkSSH probes at most once per minute without delaying ICMP sampling.
func (w *worker) checkSSH() {
	w.mu.Lock()
	if w.target.Kind != "node" || !w.hasLast || w.last.RTT < 0 || w.sshChecking || time.Since(w.sshChecked) < time.Minute {
		w.mu.Unlock()
		return
	}
	ip := w.target.IP
	w.sshChecking = true
	w.mu.Unlock()
	go func() {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), time.Second)
		if conn != nil {
			conn.Close()
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.sshChecking = false
		if w.target.IP == ip {
			w.sshOpen = err == nil
			w.sshChecked = time.Now()
		}
	}()
}

func (w *worker) markPaused() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StatePaused {
		w.state = StatePaused
		w.stateSince = time.Now().UnixMilli()
	}
}

func (w *worker) record(now time.Time, res pinger.Result) {
	s := Sample{T: now.UnixMilli(), RTT: -1}
	if res.OK {
		s.RTT = round2(float64(res.RTT) / float64(time.Millisecond))
	}
	w.mu.Lock()
	w.ring[w.head] = s
	w.head = (w.head + 1) % len(w.ring)
	if w.n < len(w.ring) {
		w.n++
	}
	w.last, w.hasLast = s, true
	if res.OK {
		w.consecOK++
		w.consecFail = 0
	} else {
		w.consecFail++
		w.consecOK = 0
	}

	minute := now.Unix() / 60 * 60
	var flush *flushItem
	if w.cur.t != minute {
		if w.cur.sent > 0 {
			flush = &flushItem{id: w.target.ID, m: w.cur.toMinute()}
		}
		w.cur = minuteAgg{t: minute}
	}
	w.cur.add(s.RTT)

	st := w.computeLocked(now.UnixMilli(), false)
	next := w.classify(st)
	if next != w.state {
		w.state = next
		w.stateSince = s.T
	}
	w.mu.Unlock()

	if flush != nil {
		w.m.enqueue(*flush)
	}
}

func (w *worker) flushMinute(force bool) {
	w.mu.Lock()
	var it *flushItem
	if w.cur.sent > 0 && force {
		it = &flushItem{id: w.target.ID, m: w.cur.toMinute()}
		w.cur = minuteAgg{}
	}
	w.mu.Unlock()
	if it != nil {
		w.m.enqueue(*it)
	}
}

func (m *Monitor) enqueue(it flushItem) {
	select {
	case m.flushCh <- it:
	default:
		log.Printf("历史写入队列已满，丢弃 %s 的一分钟数据", it.id)
	}
}

func (a *minuteAgg) add(rtt float64) {
	a.sent++
	if rtt < 0 {
		return
	}
	if a.recv == 0 || rtt < a.min {
		a.min = rtt
	}
	if rtt > a.max {
		a.max = rtt
	}
	a.recv++
	a.sum += rtt
}

func (a *minuteAgg) toMinute() store.Minute {
	m := store.Minute{T: a.t, Sent: a.sent, Recv: a.recv, Min: a.min, Max: a.max}
	if a.recv > 0 {
		m.Avg = a.sum / float64(a.recv)
	}
	return m
}

func (w *worker) classify(st Status) string {
	if w.m.Paused() {
		return StatePaused
	}
	if !w.hasLast {
		return StateUnknown
	}
	downAfter := 5
	if w.m.downAfter != nil {
		downAfter = w.m.downAfter()
	}
	switch {
	case w.consecFail >= downAfter:
		return StateDown
	case st.Sent1m > 0 && st.Loss1m >= w.target.LossWarnPct:
		return StateLoss
	case st.Avg1m > 0 && st.Avg1m >= w.target.LatencyWarnMs:
		return StateSlow
	default:
		return StateOK
	}
}

func (w *worker) status(withSpark bool) Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.computeLocked(time.Now().UnixMilli(), withSpark)
	st.State = w.state
	st.StateSince = w.stateSince
	if w.m.Paused() {
		st.State = StatePaused
	}
	st.SSHOpen = w.sshOpen && (st.State == StateOK || st.State == StateSlow || st.State == StateLoss) && st.LastRTT >= 0
	return st
}

// computeLocked 遍历环形缓冲计算各窗口指标，调用方需持有锁。
func (w *worker) computeLocked(nowMs int64, withSpark bool) Status {
	st := Status{
		ID: w.target.ID, Kind: w.target.Kind, IP: w.target.IP, ParentID: w.target.ParentID, Port: w.target.Port,
		ConsecFail: w.consecFail, ConsecOK: w.consecOK,
		LatencyWarnMs: w.target.LatencyWarnMs, LossWarnPct: w.target.LossWarnPct,
		LastRTT: -1,
	}
	if w.hasLast {
		st.LastRTT = w.last.RTT
		st.LastTime = w.last.T
	}
	type win struct {
		since      int64
		sent, recv int
		sum        float64
	}
	wins := [3]win{{since: nowMs - 60_000}, {since: nowMs - 300_000}, {since: nowMs - 3_600_000}}
	min1, max1 := math.Inf(1), 0.0
	var jitterSum float64
	var jitterN int
	prev := -1.0
	// 从最旧到最新遍历。
	for i := 0; i < w.n; i++ {
		s := w.ring[(w.head-w.n+i+len(w.ring))%len(w.ring)]
		for k := range wins {
			if s.T >= wins[k].since {
				wins[k].sent++
				if s.RTT >= 0 {
					wins[k].recv++
					wins[k].sum += s.RTT
				}
			}
		}
		if s.T >= wins[0].since && s.RTT >= 0 {
			if s.RTT < min1 {
				min1 = s.RTT
			}
			if s.RTT > max1 {
				max1 = s.RTT
			}
			if prev >= 0 {
				jitterSum += math.Abs(s.RTT - prev)
				jitterN++
			}
			prev = s.RTT
		}
	}
	loss := func(x win) float64 {
		if x.sent == 0 {
			return 0
		}
		return round2(float64(x.sent-x.recv) * 100 / float64(x.sent))
	}
	avg := func(x win) float64 {
		if x.recv == 0 {
			return 0
		}
		return round2(x.sum / float64(x.recv))
	}
	st.Sent1m = wins[0].sent
	st.Loss1m, st.Loss5m, st.Loss1h = loss(wins[0]), loss(wins[1]), loss(wins[2])
	st.Avg1m, st.Avg5m, st.Avg1h = avg(wins[0]), avg(wins[1]), avg(wins[2])
	if wins[0].recv > 0 {
		st.Min1m, st.Max1m = round2(min1), round2(max1)
	}
	if jitterN > 0 {
		st.Jitter1m = round2(jitterSum / float64(jitterN))
	}
	if withSpark {
		k := 60
		if w.n < k {
			k = w.n
		}
		st.Spark = make([]float64, k)
		for i := 0; i < k; i++ {
			st.Spark[i] = w.ring[(w.head-k+i+len(w.ring))%len(w.ring)].RTT
		}
	}
	return st
}

func (w *worker) recent(sinceMs int64) []Sample {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Sample, 0, w.n)
	for i := 0; i < w.n; i++ {
		s := w.ring[(w.head-w.n+i+len(w.ring))%len(w.ring)]
		if s.T >= sinceMs {
			out = append(out, s)
		}
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// The leading underscore is reserved: topology IDs cannot start with it.
func TCPID(id string, port int) string { return "_tcp_" + strconv.Itoa(port) + "_" + id }
func ValidTCPID(id string) bool {
	parts := strings.SplitN(strings.TrimPrefix(id, "_tcp_"), "_", 2)
	if !strings.HasPrefix(id, "_tcp_") || len(parts) != 2 {
		return false
	}
	port, err := strconv.Atoi(parts[0])
	return err == nil && port > 0 && port <= 65535 && topology.ValidID(parts[1]) && id == TCPID(parts[1], port)
}
func tcpPing(ip string, port int, timeout time.Duration) pinger.Result {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
	if err != nil {
		return pinger.Result{}
	}
	elapsed := time.Since(start)
	conn.Close()
	return pinger.Result{OK: true, RTT: elapsed}
}
