// Package alert 根据实时指标判定离线/丢包/延迟高等事件，并合并发送到已配置渠道。
package alert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/topology"
)

// 事件类型。
const (
	EvDown        = "down"
	EvStillDown   = "still_down"
	EvRecover     = "recover"
	EvLoss        = "loss"
	EvLossRecover = "loss_recover"
	EvSlow        = "slow"
	EvSlowRecover = "slow_recover"
	EvTest        = "test"
)

// Event 一条告警事件。
type Event struct {
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	TargetID string    `json:"target_id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	Location string    `json:"location"`
	Detail   string    `json:"detail"`
	Sent     bool      `json:"sent"`
	Error    string    `json:"error,omitempty"`
}

// targetInfo 来自拓扑的展示信息。
type targetInfo struct {
	Name     string
	Vendor   string
	Location string
	Parent   string // 所接交换机 id（交换机自身则为上联交换机 id）
	Critical bool
	Alert    bool // 是否推送告警（默认关闭）
}

type tstate struct {
	down           bool
	downSince      time.Time
	lastDownNotify time.Time
	lossSince      time.Time
	lossAlerted    bool
	lastLossNotify time.Time
	slowSince      time.Time
	slowAlerted    bool
	lastSlowNotify time.Time
}

// Sender 发送消息的函数，便于测试替换。
type Sender func(cfg config.AlertConfig, content string) error

// Engine 告警引擎。
type Engine struct {
	// HoldDown 返回 true 时暂缓该设备的离线告警（网段扫描正在重新解析其 IP）。
	HoldDown func(id string) bool
	// OnEvents 在一批告警事件写入历史后调用（含发送结果），用于插件等扩展；在引擎协程中调用，不应阻塞。
	OnEvents func(evs []Event, notified bool)

	mu       sync.Mutex
	cfg      func() config.Config
	mon      *monitor.Monitor
	send     Sender
	histPath string

	site       string
	info       map[string]targetInfo
	switchName map[string]string
	children   map[string]int // 交换机 id -> 下挂的被监控目标数
	states     map[string]*tstate
	pending    []Event
	firstPend  time.Time
	history    []Event
}

const historyKeep = 500

// NewEngine 创建告警引擎。
func NewEngine(cfg func() config.Config, mon *monitor.Monitor, histPath string) *Engine {
	e := &Engine{
		cfg:      cfg,
		mon:      mon,
		send:     SendNotifications,
		histPath: histPath,
		info:     map[string]targetInfo{},
		states:   map[string]*tstate{},
	}
	e.loadHistory()
	return e
}

// SetSender 替换消息发送函数（默认 SendNotifications），用于追加插件通知渠道。须在 Run 之前调用。
func (e *Engine) SetSender(s Sender) {
	if s != nil {
		e.send = s
	}
}

// SetTopology 在拓扑变化后更新展示信息。
func (e *Engine) SetTopology(t *topology.Topology) {
	info := map[string]targetInfo{}
	names := map[string]string{}
	children := map[string]int{}
	upstream := map[string]string{}
	for _, l := range t.Links {
		// 约定 from 为上游、to 为下游。
		upstream[l.To.Switch] = l.From.Switch
	}
	for _, s := range t.Switches {
		names[s.ID] = s.Name
		loc := s.Location
		if loc == "" {
			loc = "交换机"
		}
		info[s.ID] = targetInfo{Name: s.Name, Vendor: s.Vendor, Location: loc, Parent: upstream[s.ID], Critical: true, Alert: s.Monitor.Alert}
	}
	for _, n := range t.Nodes {
		loc := t.AttachLabel(n.Attach)
		if n.Location != "" {
			loc += " / " + n.Location
		}
		name := n.Name
		if name == "" {
			name = n.Vendor
		}
		info[n.ID] = targetInfo{Name: name, Vendor: n.Vendor, Location: loc, Parent: n.Attach.Switch,
			Critical: n.HasFlag("critical") || n.HasFlag("gateway"), Alert: n.Monitor.Alert}
		// 只统计开启了告警的下挂节点，用于判断“交换机下全部离线”。
		if n.IP != "" && n.Monitor.IsEnabled() && n.Monitor.Alert && n.Attach.Switch != "" {
			children[n.Attach.Switch]++
		}
	}
	for _, n := range t.Nodes {
		for _, port := range n.Monitor.TCPPorts {
			inf := info[n.ID]
			inf.Name += fmt.Sprintf(" TCP:%d", port)
			inf.Parent = ""
			info[monitor.TCPID(n.ID, port)] = inf
		}
	}
	for _, sw := range t.Switches {
		for _, port := range sw.Monitor.TCPPorts {
			inf := info[sw.ID]
			inf.Name += fmt.Sprintf(" TCP:%d", port)
			inf.Parent = ""
			info[monitor.TCPID(sw.ID, port)] = inf
		}
	}
	e.mu.Lock()
	e.site = t.Site.Name
	e.info = info
	e.switchName = names
	e.children = children
	e.mu.Unlock()
}

// Run 每秒评估一次，直到 stop 关闭。
func (e *Engine) Run(stop <-chan struct{}) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tk.C:
			if e.mon.Paused() {
				continue
			}
			e.Evaluate(now, append(e.mon.Statuses(false), e.mon.TCPStatuses(false)...))
			e.flushIfDue(now)
		}
	}
}

// Evaluate 根据当前状态产生事件（加入待发送队列）。
func (e *Engine) Evaluate(now time.Time, statuses []monitor.Status) {
	c := e.cfg().Alert
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := map[string]bool{}
	repeat := time.Duration(c.RepeatIntervalMin) * time.Minute
	for _, st := range statuses {
		// 未开启告警的设备不参与判定；之后开启时从当前状态重新开始（已离线会立即告警）。
		if !e.info[st.ID].Alert {
			continue
		}
		seen[st.ID] = true
		ts := e.states[st.ID]
		if ts == nil {
			ts = &tstate{}
			e.states[st.ID] = ts
		}
		// ---- 离线 / 恢复 ----
		switch {
		case !ts.down && st.ConsecFail >= c.DownAfterFailures && !(e.HoldDown != nil && e.HoldDown(st.ID)):
			ts.down, ts.downSince, ts.lastDownNotify = true, now, now
			ts.lossSince, ts.lossAlerted = time.Time{}, false
			ts.slowSince, ts.slowAlerted = time.Time{}, false
			e.push(now, EvDown, st, fmt.Sprintf("连续 %d 次无响应", st.ConsecFail))
		case ts.down && st.ConsecOK >= c.UpAfterSuccesses:
			ts.down = false
			e.push(now, EvRecover, st, fmt.Sprintf("离线时长 %s，当前延迟 %s", fmtDur(now.Sub(ts.downSince)), fmtMs(st.LastRTT)))
		case ts.down && repeat > 0 && now.Sub(ts.lastDownNotify) >= repeat:
			ts.lastDownNotify = now
			e.push(now, EvStillDown, st, fmt.Sprintf("已离线 %s", fmtDur(now.Sub(ts.downSince))))
		}
		if ts.down {
			continue
		}
		// ---- 丢包 ----
		lossOn := st.Sent1m >= 10 && st.Loss1m >= st.LossWarnPct
		ts.lossSince, ts.lossAlerted, ts.lastLossNotify = e.sustain(now, st, lossOn, ts.lossSince, ts.lossAlerted, ts.lastLossNotify,
			time.Duration(c.LossDurationMin)*time.Minute, repeat, EvLoss, EvLossRecover,
			fmt.Sprintf("1 分钟丢包 %.0f%%（阈值 %.0f%%），5 分钟 %.0f%%", st.Loss1m, st.LossWarnPct, st.Loss5m),
			fmt.Sprintf("1 分钟丢包降至 %.0f%%", st.Loss1m))
		// ---- 延迟高 ----
		slowOn := st.Avg1m > 0 && st.Avg1m >= st.LatencyWarnMs
		ts.slowSince, ts.slowAlerted, ts.lastSlowNotify = e.sustain(now, st, slowOn, ts.slowSince, ts.slowAlerted, ts.lastSlowNotify,
			time.Duration(c.LatencyDurationMin)*time.Minute, repeat, EvSlow, EvSlowRecover,
			fmt.Sprintf("1 分钟平均 %s（阈值 %s），最大 %s", fmtMs(st.Avg1m), fmtMs(st.LatencyWarnMs), fmtMs(st.Max1m)),
			fmt.Sprintf("1 分钟平均降至 %s", fmtMs(st.Avg1m)))
	}
	for id := range e.states {
		if !seen[id] {
			delete(e.states, id)
		}
	}
}

// sustain 处理“持续超过阈值 N 分钟才告警，低于阈值即恢复”的通用逻辑。
func (e *Engine) sustain(now time.Time, st monitor.Status, on bool, since time.Time, alerted bool, last time.Time,
	need, repeat time.Duration, evOn, evOff, detailOn, detailOff string) (time.Time, bool, time.Time) {
	if on {
		if since.IsZero() {
			since = now
		}
		if !alerted && now.Sub(since) >= need {
			e.push(now, evOn, st, detailOn)
			return since, true, now
		}
		if alerted && repeat > 0 && now.Sub(last) >= repeat {
			e.push(now, evOn, st, detailOn+"（持续 "+fmtDur(now.Sub(since))+"）")
			return since, true, now
		}
		return since, alerted, last
	}
	if alerted {
		e.push(now, evOff, st, detailOff)
	}
	return time.Time{}, false, last
}

func (e *Engine) push(now time.Time, typ string, st monitor.Status, detail string) {
	inf := e.info[st.ID]
	ev := Event{Time: now, Type: typ, TargetID: st.ID, Kind: st.Kind, Name: inf.Name, IP: st.IP, Location: inf.Location, Detail: detail}
	if len(e.pending) == 0 {
		e.firstPend = now
	}
	e.pending = append(e.pending, ev)
}

func (e *Engine) flushIfDue(now time.Time) {
	cfg := e.cfg().Alert
	e.mu.Lock()
	if len(e.pending) == 0 || now.Sub(e.firstPend) < time.Duration(cfg.MergeWindowSec)*time.Second {
		e.mu.Unlock()
		return
	}
	evs := e.pending
	e.pending = nil
	content, toSend := e.composeLocked(evs, cfg)
	e.mu.Unlock()

	var sendErr error
	if cfg.Enabled && toSend > 0 {
		sendErr = e.send(cfg, content)
		if sendErr != nil {
			log.Printf("发送告警通知失败: %v", sendErr)
		}
	}
	for i := range evs {
		evs[i].Sent = cfg.Enabled && toSend > 0 && sendErr == nil
		if sendErr != nil {
			evs[i].Error = sendErr.Error()
		}
	}
	e.appendHistory(evs)
	if e.OnEvents != nil {
		e.OnEvents(append([]Event(nil), evs...), cfg.Enabled && toSend > 0 && sendErr == nil)
	}
}

var typeLabel = map[string]string{
	EvDown:        `<font color="warning">● 离线</font>`,
	EvStillDown:   `<font color="warning">● 仍离线</font>`,
	EvRecover:     `<font color="info">● 恢复</font>`,
	EvLoss:        `<font color="warning">● 丢包</font>`,
	EvLossRecover: `<font color="info">● 丢包恢复</font>`,
	EvSlow:        `<font color="warning">● 延迟高</font>`,
	EvSlowRecover: `<font color="info">● 延迟恢复</font>`,
}

func isRecovery(t string) bool { return t == EvRecover || t == EvLossRecover || t == EvSlowRecover }

// composeLocked 生成 markdown 消息，并做上游交换机故障合并/抑制。返回内容和有效条数。
func (e *Engine) composeLocked(evs []Event, cfg config.AlertConfig) (string, int) {
	// 按交换机归并离线事件。
	downBySwitch := map[string][]int{}
	switchDown := map[string]bool{}
	for i, ev := range evs {
		if ev.Type != EvDown {
			continue
		}
		if ev.Kind == "switch" {
			switchDown[ev.TargetID] = true
		}
		if p := e.info[ev.TargetID].Parent; p != "" && ev.Kind == "node" {
			downBySwitch[p] = append(downBySwitch[p], i)
		}
	}
	skip := map[int]bool{}
	var summary []string
	swIDs := make([]string, 0, len(downBySwitch))
	for id := range downBySwitch {
		swIDs = append(swIDs, id)
	}
	sort.Strings(swIDs)
	for _, sw := range swIDs {
		idx := downBySwitch[sw]
		total := e.children[sw]
		already := e.states[sw] != nil && e.states[sw].down && !switchDown[sw]
		name := e.switchName[sw]
		switch {
		case switchDown[sw]:
			summary = append(summary, fmt.Sprintf("**交换机 %s 不可达**，下挂 %d 台中 %d 台同时离线", name, total, len(idx)))
		case already:
			summary = append(summary, fmt.Sprintf("交换机 %s 已处于离线状态，另有 %d 台下挂设备离线（已合并）", name, len(idx)))
		case len(idx) >= 3 && len(idx) == total:
			summary = append(summary, fmt.Sprintf("**疑似交换机 %s 故障或上联中断**：下挂 %d 台全部离线", name, total))
		default:
			continue
		}
		for _, i := range idx {
			skip[i] = true
		}
	}

	var lines []string
	count := 0
	for i, ev := range evs {
		if skip[i] {
			count++
			continue
		}
		if isRecovery(ev.Type) && !cfg.SendRecovery {
			continue
		}
		count++
		name := ev.Name
		if name == "" {
			name = ev.TargetID
		}
		crit := ""
		if e.info[ev.TargetID].Critical {
			crit = " **[关键]**"
		}
		lines = append(lines, fmt.Sprintf("%s **%s** %s%s\n>位置：%s\n>%s",
			typeLabel[ev.Type], ev.IP, name, crit, ev.Location, ev.Detail))
	}
	if count == 0 {
		return "", 0
	}
	title := cfg.SiteTitle
	if e.site != "" {
		title += " · " + e.site
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n", title)
	for _, s := range summary {
		b.WriteString("<font color=\"warning\">" + s + "</font>\n")
	}
	omitted := 0
	for i, l := range lines {
		if b.Len()+len(l)+80 > wecomMaxBytes {
			omitted = len(lines) - i
			break
		}
		b.WriteString(l + "\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "…另有 %d 条，请在管理页面查看\n", omitted)
	}
	fmt.Fprintf(&b, "<font color=\"comment\">%s</font>", evs[len(evs)-1].Time.Format("2006-01-02 15:04:05"))
	return b.String(), count
}

// SendTest 发送一条测试消息。
func (e *Engine) SendTest(cfg config.AlertConfig) error {
	e.mu.Lock()
	site := e.site
	e.mu.Unlock()
	content := fmt.Sprintf("### %s · %s\n<font color=\"info\">● 测试消息</font>\n>netPing 告警通道配置正常\n<font color=\"comment\">%s</font>",
		cfg.SiteTitle, site, time.Now().Format("2006-01-02 15:04:05"))
	err := e.send(cfg, content)
	ev := Event{Time: time.Now(), Type: EvTest, Detail: "测试消息", Sent: err == nil}
	if err != nil {
		ev.Error = err.Error()
	}
	e.appendHistory([]Event{ev})
	return err
}

// History 返回最近的告警（新的在前）。
func (e *Engine) History(limit int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := len(e.history)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Event, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, e.history[i])
	}
	return out
}

// ActiveDown 返回当前处于离线告警状态的目标 id。
func (e *Engine) ActiveDown() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for id, s := range e.states {
		if s.down {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) appendHistory(evs []Event) {
	e.mu.Lock()
	e.history = append(e.history, evs...)
	if len(e.history) > historyKeep {
		e.history = append([]Event(nil), e.history[len(e.history)-historyKeep:]...)
	}
	e.mu.Unlock()
	if e.histPath == "" {
		return
	}
	if fi, err := os.Stat(e.histPath); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(e.histPath, e.histPath+".1")
	}
	f, err := os.OpenFile(e.histPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("写告警历史失败: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, ev := range evs {
		_ = enc.Encode(ev)
	}
}

func (e *Engine) loadHistory() {
	if e.histPath == "" {
		return
	}
	f, err := os.Open(e.histPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			e.history = append(e.history, ev)
			if len(e.history) > 2*historyKeep {
				e.history = e.history[historyKeep:]
			}
		}
	}
	if len(e.history) > historyKeep {
		e.history = e.history[len(e.history)-historyKeep:]
	}
}

func fmtMs(v float64) string {
	if v < 0 {
		return "超时"
	}
	if v < 10 {
		return fmt.Sprintf("%.2fms", v)
	}
	return fmt.Sprintf("%.0fms", v)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%d小时%d分", h, m)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, s)
	default:
		return fmt.Sprintf("%d秒", s)
	}
}
