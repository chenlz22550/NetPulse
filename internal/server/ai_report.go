package server

// AI 运维报告：程序先按统计周期（日报 24h / 周报 7d / 月报 30d）汇总全网可用性、丢包、延迟、
// 告警与当前异常，再交给模型撰写结构化的 Markdown 运维报告（可调用只读工具补充数据、插入图表）。
// 报告末尾附程序统计的关键数据，保存在 data/reports/，可在网页查看、下载 Markdown 或导出 HTML/PDF。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/llm"
)

const (
	reportMaxRounds   = 6
	reportMaxCalls    = 12
	reportMaxTokens   = 8192
	reportMaxContext  = 60000
	reportKeep        = 100 // 最多保留多少份报告
	reportMaxNoteRune = 500
)

// reportRanges 支持的统计周期。
var reportRanges = map[string]struct {
	dur    time.Duration
	label  string
	kind   string
	bucket time.Duration // 趋势分桶
}{
	"24h": {24 * time.Hour, "近 24 小时", "日报", time.Hour},
	"7d":  {7 * 24 * time.Hour, "近 7 天", "周报", 24 * time.Hour},
	"30d": {30 * 24 * time.Hour, "近 30 天", "月报", 24 * time.Hour},
}

var reportIDRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9]{1,3})?$`)

// Report 一份已生成的运维报告。
type Report struct {
	ID         string    `json:"id"`
	Time       string    `json:"time"`
	Range      string    `json:"range"`
	RangeLabel string    `json:"range_label"`
	Kind       string    `json:"kind"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Site       string    `json:"site"`
	Title      string    `json:"title"`
	Note       string    `json:"note,omitempty"`
	Markdown   string    `json:"markdown"`
	Model      string    `json:"model"`
	Tools      int       `json:"tools"`
	Ms         int64     `json:"ms"`
	Usage      llm.Usage `json:"usage"`
	Summary    rptKPI    `json:"summary"`
}

// rptKPI 报告的关键指标（程序统计，不经模型）。
type rptKPI struct {
	SuccessSamples  int     `json:"success_samples"`
	HasData         bool    `json:"has_data"`
	ObservedMinutes int     `json:"observed_minutes"`
	FaultEpisodes   int     `json:"fault_episodes"`
	Monitored       int     `json:"monitored"`
	WithData        int     `json:"with_data"`
	Availability    float64 `json:"availability_pct"`
	LossPct         float64 `json:"loss_pct"`
	AvgMs           float64 `json:"avg_ms"`
	DownMinutes     int     `json:"down_minutes"`
	Abnormal        int     `json:"abnormal_now"`
	Alerts          int     `json:"alerts"`
}

type rptDev struct {
	ID, Name, IP, Attach, State string
	Switch                      bool
	Alert                       bool
	Loss, Avg, Max, P95, Jitter float64
	Minutes, DownMin, Episodes  int
}

func (d rptDev) avail() float64 {
	if d.Minutes == 0 {
		return 100
	}
	return r2(100 - float64(d.DownMin)*100/float64(d.Minutes))
}

func (d rptDev) label() string { return devText(d.Name, d.IP) }

type rptBucket struct {
	Start      time.Time
	Sent, Recv int
	Sum        float64
	Abnormal   int // 本时段内有中断分钟的设备数
}

func (b rptBucket) loss() float64 {
	if b.Sent == 0 {
		return 0
	}
	return r2(float64(b.Sent-b.Recv) * 100 / float64(b.Sent))
}

func (b rptBucket) avg() float64 {
	if b.Recv == 0 {
		return 0
	}
	return r2(b.Sum / float64(b.Recv))
}

// rptData 报告所需的全部统计。
type rptData struct {
	Range, Label, Kind string
	From, To           time.Time
	Site               string
	Switches, Nodes    int
	Monitored          int
	AlertOn, Unlocated int
	Categories         map[string]int
	StateCounts        map[string]int
	Abnormal           []deviceRow
	Devs               []rptDev // 周期内有数据的设备
	Sent, Recv         int
	Sum                float64
	DownMin, Minutes   int
	Prev               *rptBucket // 上一周期整体（无数据时为 nil）
	Buckets            []rptBucket
	Alerts             []alert.Event
	AlertCounts        map[string]int
	AlertFail          int
	Truncated          int // 超过分析上限未统计的设备数
	RetentionShort     bool
}

func (d *rptData) kpi() rptKPI {
	k := rptKPI{Monitored: d.Monitored, WithData: len(d.Devs), DownMinutes: d.DownMin, Abnormal: len(d.Abnormal), Alerts: len(d.Alerts), HasData: d.Minutes > 0, ObservedMinutes: d.Minutes, SuccessSamples: d.Recv}
	for _, dev := range d.Devs {
		k.FaultEpisodes += dev.Episodes
	}
	if d.Sent > 0 {
		k.LossPct = r2(float64(d.Sent-d.Recv) * 100 / float64(d.Sent))
	}
	if d.Recv > 0 {
		k.AvgMs = r2(d.Sum / float64(d.Recv))
	}
	if d.Minutes > 0 {
		k.Availability = r2(100 - float64(d.DownMin)*100/float64(d.Minutes))
	}
	return k
}

// collectReport 统计 [now-dur, now) 内的全网数据。
func (s *Server) collectReport(rg string, now time.Time) (*rptData, error) {
	spec, ok := reportRanges[rg]
	if !ok {
		return nil, badRequest("统计周期只能是 24h、7d 或 30d")
	}
	t, _ := s.Topo.Snapshot()
	cfg := s.Cfg.Get()
	d := &rptData{Range: rg, Label: spec.label, Kind: spec.kind, From: now.Add(-spec.dur), To: now,
		Switches: len(t.Switches), Nodes: len(t.Nodes), Categories: map[string]int{}, StateCounts: map[string]int{}, AlertCounts: map[string]int{}}
	d.Site = t.Site.Name
	if t.Site.Subnet != "" {
		d.Site += "（" + t.Site.Subnet + "）"
	}
	d.RetentionShort = cfg.RetentionDays > 0 && time.Duration(cfg.RetentionDays)*24*time.Hour < spec.dur

	for _, n := range t.Nodes {
		if n.Monitor.Alert {
			d.AlertOn++
		}
		if n.Attach.Switch == "" {
			d.Unlocated++
		}
		if c := nodeCategory(n); c != "" {
			d.Categories[c]++
		}
	}
	for _, sw := range t.Switches {
		if sw.Monitor.Alert {
			d.AlertOn++
		}
	}
	sts := s.statusMap()
	for _, r := range buildRows(t, sts) {
		d.StateCounts[r.State]++
		if isAbnormal(r.State) {
			d.Abnormal = append(d.Abnormal, r)
		}
	}
	sort.Slice(d.Abnormal, func(i, j int) bool { return ipKey(d.Abnormal[i].IP) < ipKey(d.Abnormal[j].IP) })

	// 趋势分桶起点：按小时或本地零点对齐
	first := d.From.Truncate(time.Hour)
	if spec.bucket >= 24*time.Hour {
		y, m, dd := d.From.Date()
		first = time.Date(y, m, dd, 0, 0, 0, 0, d.From.Location())
	}
	var starts []int64
	for b := first; b.Before(now); b = b.Add(spec.bucket) {
		if spec.bucket >= 24*time.Hour {
			y, m, dd := b.Date() // 跨夏令时也保持零点
			b = time.Date(y, m, dd, 0, 0, 0, 0, b.Location())
		}
		d.Buckets = append(d.Buckets, rptBucket{Start: b})
		starts = append(starts, b.Unix())
	}
	bucketOf := func(ts int64) int {
		i := sort.Search(len(starts), func(k int) bool { return starts[k] > ts }) - 1
		if i < 0 {
			i = 0
		}
		return i
	}

	ids := monitoredIDs(t, "")
	d.Monitored = len(ids)
	if len(ids) > analysisMaxDevices {
		d.Truncated = len(ids) - analysisMaxDevices
		ids = ids[:analysisMaxDevices]
	}
	prev := &rptBucket{}
	hit := make([]bool, len(d.Buckets))
	for _, id := range ids {
		ms, err := s.minuteSeries(id, d.From, now)
		if err != nil {
			return nil, err
		}
		st := statMinutes(ms)
		if st.sent > 0 {
			name, ip, attach := deviceLabel(t, id)
			dv := rptDev{ID: id, Name: name, IP: ip, Attach: attach, Switch: t.FindSwitch(id) != nil,
				Loss: st.lossPct(), Avg: st.avg(), P95: st.p95(), Jitter: st.jitter(), Minutes: st.minutes, DownMin: st.downMin}
			if st.recv > 0 {
				dv.Max = r2(st.max)
			}
			if x, ok := sts[id]; ok {
				dv.State = x.State
			}
			if k, mon, _ := devMon(t, id); k != "" {
				dv.Alert = mon.Alert
			}
			for i := range hit {
				hit[i] = false
			}
			var prevLoss int64 = -1
			for _, m := range ms {
				if m.Sent <= 0 {
					continue
				}
				bi := bucketOf(m.T)
				b := &d.Buckets[bi]
				b.Sent += m.Sent
				b.Recv += m.Recv
				b.Sum += m.Avg * float64(m.Recv)
				if m.Recv == 0 && !hit[bi] {
					hit[bi] = true
					b.Abnormal++
				}
				if m.Recv < m.Sent { // 连续的丢包分钟记为一次异常
					if m.T-prevLoss > 60 {
						dv.Episodes++
					}
					prevLoss = m.T
				}
			}
			d.Devs = append(d.Devs, dv)
			d.Sent += st.sent
			d.Recv += st.recv
			d.Sum += st.sum
			d.DownMin += st.downMin
			d.Minutes += st.minutes
		}
		// 上一周期（同样长度）整体对比
		if pm, err := s.minuteSeries(id, d.From.Add(-spec.dur), d.From); err == nil {
			ps := statMinutes(pm)
			prev.Sent += ps.sent
			prev.Recv += ps.recv
			prev.Sum += ps.sum
		}
	}
	if prev.Sent > 0 {
		d.Prev = prev
	}

	for _, ev := range s.Alerts.History(0) {
		if ev.Time.Before(d.From) || !ev.Time.Before(d.To) || ev.Type == alert.EvTest {
			continue
		}
		d.Alerts = append(d.Alerts, ev)
		d.AlertCounts[ev.Type]++
		if !ev.Sent && ev.Error != "" {
			d.AlertFail++
		}
	}
	return d, nil
}

var alertTypeText = map[string]string{
	"down": "离线", "still_down": "仍离线", "recover": "恢复", "loss": "丢包", "loss_recover": "丢包恢复",
	"slow": "延迟高", "slow_recover": "延迟恢复", "test": "测试",
}

func stateText(st string) string {
	m := map[string]string{"ok": "正常", "slow": "延迟高", "loss": "丢包", "down": "离线", "unknown": "等待", "paused": "暂停", "disabled": "未监控", "unmonitored": "未监控"}
	if v, ok := m[st]; ok {
		return v
	}
	return orDefault(st, "-")
}

// topDevs 按 key 降序取前 n 台（key<=0 的不列）。
func topDevs(devs []rptDev, n int, key func(rptDev) float64) []rptDev {
	out := make([]rptDev, 0, len(devs))
	for _, d := range devs {
		if key(d) > 0 {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return key(out[i]) > key(out[j]) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func mdCell(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ")
	return orDefault(strings.TrimSpace(s), "-")
}

func (d *rptData) bucketLabel(b rptBucket) string {
	if reportRanges[d.Range].bucket >= 24*time.Hour {
		return b.Start.Format("01-02")
	}
	return b.Start.Format("15:04")
}

// contextText 给模型的统计资料。
func (d *rptData) contextText(s *Server) string {
	cfg := s.Cfg.Get()
	k := d.kpi()
	var b strings.Builder
	if !k.HasData {
		b.WriteString("本周期无观测样本，所有可用率、丢包和延迟数值均不可用于判断健康状态。\n")
	}
	fmt.Fprintf(&b, "【报告类型】%s（%s）：%s ~ %s\n", d.Kind, d.Label, d.From.Format("2006-01-02 15:04"), d.To.Format("2006-01-02 15:04"))
	if d.RetentionShort {
		fmt.Fprintf(&b, "注意：历史数据只保留 %d 天，统计周期早于此的部分没有数据。\n", cfg.RetentionDays)
	}
	fmt.Fprintf(&b, "【监控配置】默认 ping 间隔 %dms，延迟阈值 %dms，丢包阈值 %.0f%%；企业微信告警：%v；监控时段：%s；监控暂停：%v\n",
		cfg.Ping.IntervalMs, cfg.Ping.LatencyWarnMs, cfg.Ping.LossWarnPct, cfg.Alert.Enabled && cfg.Alert.WebhookURL != "", cfg.Schedule.Describe(), s.Mon.Paused())
	fmt.Fprintf(&b, "【资产】网络设备 %d 台，终端 %d 台，启用监控 %d 台（周期内有数据 %d 台），已开告警 %d 台，未定位终端 %d 台",
		d.Switches, d.Nodes, d.Monitored, len(d.Devs), d.AlertOn, d.Unlocated)
	if d.Truncated > 0 {
		fmt.Fprintf(&b, "（设备过多，另有 %d 台未统计）", d.Truncated)
	}
	b.WriteString("\n")
	if len(d.Categories) > 0 {
		var cs []string
		for c, n := range d.Categories {
			cs = append(cs, fmt.Sprintf("%s %d", c, n))
		}
		sort.Strings(cs)
		b.WriteString("终端类别：" + strings.Join(cs, "，") + "\n")
	}
	fmt.Fprintf(&b, "\n【全网指标（程序统计）】可用率 %.2f%%（按整分钟全部丢包计中断），整体丢包率 %.2f%%，平均延迟 %.1fms，累计中断 %d 设备·分钟，告警事件 %d 条",
		k.Availability, k.LossPct, k.AvgMs, k.DownMinutes, k.Alerts)
	if d.AlertFail > 0 {
		fmt.Fprintf(&b, "（其中 %d 条推送失败）", d.AlertFail)
	}
	b.WriteString("\n")
	if d.Prev != nil {
		fmt.Fprintf(&b, "上一周期（同样时长）：整体丢包率 %.2f%%，平均延迟 %.1fms\n", d.Prev.loss(), d.Prev.avg())
	} else {
		b.WriteString("上一周期：没有历史数据，无法对比\n")
	}
	var dist [4]int
	for _, dv := range d.Devs {
		switch {
		case dv.Loss == 0:
			dist[0]++
		case dv.Loss < 1:
			dist[1]++
		case dv.Loss < 5:
			dist[2]++
		default:
			dist[3]++
		}
	}
	fmt.Fprintf(&b, "丢包分布：无丢包 %d 台，<1%% %d 台，1~5%% %d 台，≥5%% %d 台\n", dist[0], dist[1], dist[2], dist[3])

	b.WriteString("\n【趋势】时段 | 丢包率% | 平均延迟ms | 有中断的设备数\n")
	for _, bk := range d.Buckets {
		fmt.Fprintf(&b, "%s | %.2f | %.1f | %d\n", d.bucketLabel(bk), bk.loss(), bk.avg(), bk.Abnormal)
	}

	row := func(dv rptDev) string {
		return fmt.Sprintf("%s | %s | %s | %s | %s | %.2f | %.1f | %.1f | %.1f | %d | %d | %.2f | %v\n", dv.ID, dv.Name, orDefault(dv.IP, "-"), dv.Attach,
			stateText(dv.State), dv.Loss, dv.Avg, dv.Max, dv.P95, dv.DownMin, dv.Episodes, dv.avail(), dv.Alert)
	}
	head := "id | 名称 | IP | 接线 | 当前状态 | 丢包% | 平均ms | 峰值ms | P95ms | 中断分钟 | 异常次数 | 可用率% | 告警\n"
	b.WriteString("\n【丢包率排行 Top 15】" + head)
	for _, dv := range topDevs(d.Devs, 15, func(x rptDev) float64 { return x.Loss }) {
		b.WriteString(row(dv))
	}
	b.WriteString("\n【中断时长排行 Top 10】" + head)
	for _, dv := range topDevs(d.Devs, 10, func(x rptDev) float64 { return float64(x.DownMin) }) {
		b.WriteString(row(dv))
	}
	b.WriteString("\n【平均延迟排行 Top 10】" + head)
	for _, dv := range topDevs(d.Devs, 10, func(x rptDev) float64 { return x.Avg }) {
		b.WriteString(row(dv))
	}
	var sw []rptDev
	for _, dv := range d.Devs {
		if dv.Switch {
			sw = append(sw, dv)
		}
	}
	if len(sw) > 0 {
		b.WriteString("\n【网络设备（交换机/路由器等）】" + head)
		for _, dv := range sw {
			b.WriteString(row(dv))
		}
	}

	fmt.Fprintf(&b, "\n【当前异常设备 %d 台】\n", len(d.Abnormal))
	for i, r := range d.Abnormal {
		if i >= 40 {
			fmt.Fprintf(&b, "……另有 %d 台\n", len(d.Abnormal)-i)
			break
		}
		loss := "-"
		if r.Loss5m != nil {
			loss = fmt.Sprintf("%.0f%%", *r.Loss5m)
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | 5分钟丢包 %s | 自 %s\n", r.ID, r.Name, r.IP, r.Attach, stateText(r.State), loss, orDefault(r.StateSince, "-"))
	}

	var ac []string
	for ty, n := range d.AlertCounts {
		ac = append(ac, fmt.Sprintf("%s %d", orDefault(alertTypeText[ty], ty), n))
	}
	sort.Strings(ac)
	fmt.Fprintf(&b, "\n【告警事件】共 %d 条：%s\n", len(d.Alerts), orDefault(strings.Join(ac, "，"), "无"))
	per := map[string]int{}
	names := map[string]string{}
	for _, ev := range d.Alerts {
		if ev.Type == alert.EvDown || ev.Type == alert.EvLoss || ev.Type == alert.EvSlow {
			per[ev.TargetID]++
			names[ev.TargetID] = devText(ev.Name, ev.IP)
		}
	}
	type kv struct {
		id string
		n  int
	}
	var freq []kv
	for id, n := range per {
		freq = append(freq, kv{id, n})
	}
	sort.Slice(freq, func(i, j int) bool { return freq[i].n > freq[j].n || freq[i].n == freq[j].n && freq[i].id < freq[j].id })
	if len(freq) > 0 {
		b.WriteString("告警最多的设备：")
		for i, f := range freq {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "%s(%s) %d 次；", names[f.id], f.id, f.n)
		}
		b.WriteString("\n")
	}
	b.WriteString("最近告警（新的在前）：\n")
	for i, ev := range d.Alerts {
		if i >= 40 {
			break
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", ev.Time.Format("01-02 15:04"), orDefault(alertTypeText[ev.Type], ev.Type), devText(ev.Name, ev.IP), truncRunes(strings.ReplaceAll(ev.Detail, "\n", " "), 80))
	}
	text := b.String()
	if len(text) > reportMaxContext {
		cut := text[:reportMaxContext]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		text = cut + "\n……（数据过多，后面的已省略）"
	}
	return text
}

// appendix 程序统计的关键数据，附在报告末尾，保证报告中有可核对的原始数字。
func (d *rptData) appendix() string {
	k := d.kpi()
	var b strings.Builder
	b.WriteString("\n\n---\n\n## 附录：程序统计数据\n\n")
	fmt.Fprintf(&b, "以下数据由 netPing 直接统计，未经 AI 加工。统计周期 %s ~ %s。\n\n", d.From.Format("2006-01-02 15:04"), d.To.Format("2006-01-02 15:04"))
	b.WriteString("| 指标 | 数值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 启用监控设备 | %d 台（周期内有数据 %d 台） |\n", d.Monitored, len(d.Devs))
	if k.HasData {
		fmt.Fprintf(&b, "| 全网可用率 | %.2f%% |\n", k.Availability)
	} else {
		b.WriteString("| 全网可用率 | 无数据 |\n")
	}
	if k.HasData {
		fmt.Fprintf(&b, "| 整体丢包率 | %.2f%% |\n", k.LossPct)
	} else {
		b.WriteString("| 整体丢包率 | 无数据 |\n")
	}
	if d.Recv > 0 {
		fmt.Fprintf(&b, "| 平均延迟 | %.1f ms |\n", k.AvgMs)
	} else {
		b.WriteString("| 平均延迟 | 无成功样本 |\n")
	}
	fmt.Fprintf(&b, "| 累计中断 | %d 设备·分钟 |\n", k.DownMinutes)
	fmt.Fprintf(&b, "| 告警事件 | %d 条 |\n| 故障次数（连续丢包分钟段） | %d 次 |\n", k.Alerts, k.FaultEpisodes)
	fmt.Fprintf(&b, "| 当前异常 | %d 台 |\n", k.Abnormal)
	if d.Prev != nil {
		fmt.Fprintf(&b, "| 上一周期丢包率 / 平均延迟 | %.2f%% / %.1f ms |\n", d.Prev.loss(), d.Prev.avg())
	}
	top := topDevs(d.Devs, 10, func(x rptDev) float64 { return x.Loss + float64(x.DownMin)/1e6 })
	if len(top) > 0 {
		b.WriteString("\n**丢包率最高的设备**\n\n| 设备 | 接线 | 丢包率 | 平均延迟 | 中断分钟 | 可用率 |\n|---|---|---|---|---|---|\n")
		for _, dv := range top {
			fmt.Fprintf(&b, "| %s | %s | %.2f%% | %.1f ms | %d | %.2f%% |\n", mdCell(dv.label()), mdCell(dv.Attach), dv.Loss, dv.Avg, dv.DownMin, dv.avail())
		}
	} else if len(d.Devs) > 0 {
		b.WriteString("\n周期内所有设备均无丢包。\n")
	}
	b.WriteString("\n仅统计被激活机房的实际观测时段；非激活时段和缺失数据不计为故障。\n")
	b.WriteString("\n口径：可用率 = 1 − 整分钟全部丢包的分钟数 ÷ 有数据的分钟数；暂停监控、非监控时段没有数据，不计入。\n")
	return b.String()
}

func reportSystemPrompt(d *rptData, note string) string {
	return `你是 netPing 的机房网络运维报告撰写助手。netPing 持续 ping 机房里的交换机和终端，记录延迟、抖动、丢包，并维护交换机-端口-设备的拓扑。
机房：` + d.Site + `；当前时间：` + time.Now().Format("2006-01-02 15:04:05 (Mon)") + `。
用户要导出一份「` + d.Kind + `」（` + d.Label + `），交给主管或存档。下面给出程序已经统计好的全网数据。你可以调用只读工具补充细节（如 find_outages 看某台设备的中断时段、correlate_events 找多台同时掉线、analyze_impact 归纳当前异常、get_uplink_path、compare_baseline），但大部分结论应直接基于给出的统计，工具调用不超过 8 次。你不能修改任何配置。

写作要求：
1. 只输出报告正文（Markdown），不要寒暄、不要说“以下是报告”。第一行是一级标题：# ` + d.Site + ` 网络运维` + d.Kind + `（日期或日期范围）。
2. 结构（使用二级标题，按顺序）：
   ## 一、总体结论 —— 3~5 句，给出健康评级（优 / 良 / 中 / 差，并说明依据），本周期最重要的 1~3 件事。
   ## 二、关键指标 —— Markdown 表格：指标、本期、上期（没有就写“-”）、变化；包含可用率、丢包率、平均延迟、累计中断、告警数、当前异常数。
   ## 三、稳定性分析 —— 按交换机/端口/设备类别归纳问题，列出最不稳定的设备（表格：设备、接线、丢包率、中断分钟、异常次数、判断）；指出多台设备同时异常、固定时段异常等规律。
   ## 四、告警与故障事件 —— 本期重要故障事件的时间、影响范围、持续时间和可能原因；告警推送是否正常；反复告警的设备。没有就如实说明。
   ## 五、风险与隐患 —— 如关键设备未开告警、交换机未监控、大量未定位设备、延迟持续偏高、单点故障、数据缺失等。
   ## 六、处理建议 —— Markdown 表格：优先级（高/中/低）、事项、涉及设备、具体操作、建议完成时间。
3. 插入 1~3 个 ` + "```chart" + ` 图表帮助阅读（代码块内只放一个 JSON 对象），推荐：用【趋势】画丢包率/延迟趋势曲线（line，points 的 loss 填丢包率%），用排行画丢包率或中断时长柱状图（bar），有公共故障时画时间线（timeline）或故障路径（path）。图中的数据、设备名、IP 必须来自给出的统计或工具结果，严禁编造。支持的格式：
` + aiChartFormats + `
4. 所有数字必须来自给出的统计或工具结果，不要编造；推断要写明“可能”“推测”。周期内数据不足时如实说明。全网都正常时报告可以简短，但仍要给出巡检建议。
5. 状态含义：ok 正常、slow 延迟高、loss 丢包、down 离线、unknown 等待、paused 暂停、unmonitored 未监控。手机平板、笔记本离线通常是正常离开；仅 ARP、停用监控、无 IP 的设备没有 ping 数据，不算故障。
6. 用简体中文，面向管理者和运维人员，语言简洁专业；设备写“名称（IP）”。总篇幅 800~2000 字（不含图表）。
7. 不要写附录：程序会在报告末尾自动附上原始统计数据。` + func() string {
		if note == "" {
			return ""
		}
		return "\n\n用户对本次报告的额外要求：" + note
	}()
}

// reportTitle 取 Markdown 中的一级标题。
func reportTitle(md, def string) string {
	for _, l := range strings.Split(md, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "# ") {
			return truncRunes(strings.TrimSpace(strings.TrimPrefix(l, "# ")), 80)
		}
		if l != "" && !strings.HasPrefix(l, "#") {
			break
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// 存储

func (s *Server) reportDir() string {
	if s.Alerts != nil {
		if dir := s.Alerts.DataDir(); dir != "" {
			return filepath.Join(dir, "reports")
		}
	}
	return filepath.Join(s.BaseDir, "data", "reports")
}

var reportSaveMu sync.Mutex

func (s *Server) saveReport(rp *Report) error {
	reportSaveMu.Lock()
	defer reportSaveMu.Unlock()
	dir := s.reportDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	base := rp.ID
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, rp.ID+".json")); os.IsNotExist(err) {
			break
		} else if err != nil {
			return err
		}
		if i > 999 {
			return errors.New("同一秒生成报告过多，请稍后重试")
		}
		rp.ID = fmt.Sprintf("%s-%d", base, i)
	}
	b, err := json.MarshalIndent(rp, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, rp.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, rp.ID+".json")); err != nil {
		return err
	}
	// 只保留最近 reportKeep 份
	ids := s.reportIDs()
	for i := reportKeep; i < len(ids); i++ {
		_ = os.Remove(filepath.Join(dir, ids[i]+".json"))
	}
	return nil
}

// reportIDs 已保存报告的 id，新的在前。
func (s *Server) reportIDs() []string {
	ents, _ := os.ReadDir(s.reportDir())
	var ids []string
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), ".json")
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && reportIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

func (s *Server) loadReport(id string) (*Report, error) {
	if !reportIDRe.MatchString(id) {
		return nil, notFound("报告")
	}
	b, err := os.ReadFile(filepath.Join(s.reportDir(), id+".json"))
	if err != nil {
		return nil, notFound("报告")
	}
	var rp Report
	if err := json.Unmarshal(b, &rp); err != nil {
		return nil, fmt.Errorf("报告文件损坏：%v", err)
	}
	return &rp, nil
}

// ---------------------------------------------------------------------------
// HTTP

func (s *Server) collectReportBoundLocked(wantProfileID string, wantEpoch *int64, rg string, now time.Time) (*rptData, string, string, int64, error) {
	done := s.Topo.ProfileWork()
	defer done()
	activeID, epoch, err := s.checkActiveBindingLocked(wantProfileID, wantEpoch)
	if err != nil {
		return nil, "", activeID, epoch, err
	}
	d, err := s.collectReport(rg, now)
	if err != nil {
		return nil, "", activeID, epoch, err
	}
	return d, d.contextText(s), activeID, epoch, nil
}

func (s *Server) saveReportWithEpoch(ctx context.Context, rp *Report) (bool, error) {
	done := s.Topo.ProfileWork()
	defer done()
	if err := s.checkProfileEpoch(ctx); err != nil {
		return true, err
	}
	return false, s.saveReport(rp)
}

// generateAIReportWithBinding runs the AI operational report loop atomically bound to the active profile+epoch.
func (s *Server) generateAIReportWithBinding(ctx context.Context, wantProfileID string, wantEpoch *int64, rg, note, remote string, emit func(map[string]interface{})) (*Report, llm.Usage, int, int64, string, string, int64, error) {
	c := s.Cfg.Get()
	if !c.AI.Enabled {
		return nil, llm.Usage{}, 0, 0, "", "", 0, badRequest("AI 诊断未启用（在「AI 诊断」页开启）")
	}
	if c.AI.APIKey == "" {
		return nil, llm.Usage{}, 0, 0, "", "", 0, badRequest("尚未设置 API Key，请先在「AI 诊断」页右侧填写")
	}
	if rg == "" {
		rg = "24h"
	}
	if _, ok := reportRanges[rg]; !ok {
		return nil, llm.Usage{}, 0, 0, "", "", 0, badRequest("统计周期只能是 24h、7d 或 30d")
	}
	select {
	case aiSem <- struct{}{}:
		defer func() { <-aiSem }()
	default:
		return nil, llm.Usage{}, 0, 0, "", "", 0, &apiError{status: http.StatusTooManyRequests, msg: "同时进行的 AI 分析太多，请稍后再试"}
	}
	return s.generateAIReportCore(ctx, c, wantProfileID, wantEpoch, rg, note, remote, emit)
}

func (s *Server) generateAIReportCore(ctx context.Context, c config.Config, wantProfileID string, wantEpoch *int64, rg, note, remote string, emit func(map[string]interface{})) (*Report, llm.Usage, int, int64, string, string, int64, error) {
	note = truncRunes(strings.TrimSpace(note), reportMaxNoteRune)
	timeout := time.Duration(c.AI.TimeoutSec) * time.Second
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute // 报告较长
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	if emit != nil {
		emit(map[string]interface{}{"t": "status", "text": "正在统计" + reportRanges[rg].label + "的监控数据…"})
	}
	d, ctxText, boundID, boundEpoch, err := s.collectReportBoundLocked(wantProfileID, wantEpoch, rg, start)
	if err != nil {
		return nil, llm.Usage{}, 0, 0, c.AI.Model, boundID, boundEpoch, err
	}
	ctx = s.withBoundProfileEpoch(ctx, boundID, boundEpoch)
	k := d.kpi()
	if emit != nil {
		emit(map[string]interface{}{"t": "stats", "stats": k})
	}
	msgs := []llm.Message{
		{Role: "system", Content: reportSystemPrompt(d, note)},
		{Role: "user", Content: ctxText + "\n请按要求撰写运维报告。"},
	}
	res, err := s.aiLoopWith(ctx, c, msgs, aiToolNames, reportMaxRounds, reportMaxCalls, reportMaxTokens, remote, emit)
	if err != nil {
		return nil, llm.Usage{}, 0, 0, c.AI.Model, boundID, boundEpoch, err
	}
	body := strings.TrimSpace(res.Final)
	if body == "" {
		return nil, res.Usage, res.Calls, time.Since(start).Milliseconds(), c.AI.Model, boundID, boundEpoch, errors.New("模型没有返回报告内容，请重试。")
	}
	// 去掉模型偶尔包裹整篇报告的 ```markdown 代码块
	if strings.HasPrefix(body, "```markdown") || strings.HasPrefix(body, "```md") {
		if nl := strings.Index(body, "\n"); nl >= 0 {
			body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body[nl+1:]), "```"))
		}
	}
	defTitle := d.Site + " 网络运维" + d.Kind
	if !strings.HasPrefix(strings.TrimSpace(body), "# ") {
		body = "# " + defTitle + "\n\n" + body
	}
	ms := time.Since(start).Milliseconds()
	rp := &Report{
		ID: start.Format("20060102-150405"), Time: start.Format("2006-01-02 15:04:05"),
		Range: d.Range, RangeLabel: d.Label, Kind: d.Kind, Site: d.Site, Note: note,
		From: d.From.Format("2006-01-02 15:04"), To: d.To.Format("2006-01-02 15:04"),
		Title: reportTitle(body, defTitle), Markdown: body + d.appendix(),
		Model: c.AI.Model, Tools: res.Calls, Ms: ms, Usage: res.Usage, Summary: k,
	}
	if epochErr, err := s.saveReportWithEpoch(ctx, rp); err != nil {
		if epochErr {
			return nil, res.Usage, res.Calls, ms, c.AI.Model, boundID, boundEpoch, err
		}
		return rp, res.Usage, res.Calls, ms, c.AI.Model, boundID, boundEpoch, err
	}
	return rp, res.Usage, res.Calls, ms, c.AI.Model, boundID, boundEpoch, nil
}

func (s *Server) listReportSummaries(limit int) []map[string]interface{} {
	list := []map[string]interface{}{}
	for _, rid := range s.reportIDs() {
		if limit > 0 && len(list) >= limit {
			break
		}
		rp, err := s.loadReport(rid)
		if err != nil {
			continue
		}
		list = append(list, map[string]interface{}{
			"id":          rp.ID,
			"time":        rp.Time,
			"title":       rp.Title,
			"range":       rp.Range,
			"range_label": rp.RangeLabel,
			"kind":        rp.Kind,
			"model":       rp.Model,
			"summary":     rp.Summary,
			"note":        rp.Note,
		})
	}
	return list
}

func exportReportMarkdown(rp *Report) string {
	generator := "netPing AI（" + rp.Model + "）"
	if rp.Model == "deterministic" {
		generator = "netPing 程序统计"
	}
	return rp.Markdown + "\n\n> 由 " + generator + " 生成，生成时间 " + rp.Time + "\n"
}

func exportReportHTML(rp *Report) string {
	md := exportReportMarkdown(rp)
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<title>")
	b.WriteString(html.EscapeString(rp.Title))
	b.WriteString("</title>")
	b.WriteString("<style>body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;max-width:960px;margin:24px auto;padding:0 16px;line-height:1.6;color:#111827}pre{background:#f3f4f6;padding:12px;border-radius:6px;overflow:auto;white-space:pre-wrap}table{border-collapse:collapse;width:100%;margin:12px 0}th,td{border:1px solid #d1d5db;padding:6px 10px;text-align:left}</style>")
	b.WriteString("</head><body>")
	fmt.Fprintf(&b, "<header><p><strong>%s</strong> | %s (%s) | %s ~ %s</p></header>",
		html.EscapeString(rp.Title),
		html.EscapeString(rp.Site),
		html.EscapeString(rp.RangeLabel),
		html.EscapeString(rp.From),
		html.EscapeString(rp.To),
	)
	b.WriteString("<article><pre>")
	b.WriteString(html.EscapeString(md))
	b.WriteString("</pre></article></body></html>")
	return b.String()
}

func (s *Server) deleteReport(id string) error {
	if !reportIDRe.MatchString(id) {
		return notFound("报告")
	}
	if err := os.Remove(filepath.Join(s.reportDir(), id+".json")); err != nil {
		return notFound("报告")
	}
	return nil
}

// handleAIReport HTTP NDJSON handler wrapper around generateAIReport.
func (s *Server) handleAIReport(w http.ResponseWriter, r *http.Request) error {
	c := s.Cfg.Get()
	if !c.AI.Enabled {
		return badRequest("AI 诊断未启用（在「AI 诊断」页开启）")
	}
	if c.AI.APIKey == "" {
		return badRequest("尚未设置 API Key，请先在「AI 诊断」页右侧填写")
	}
	var req struct {
		Range string `json:"range"`
		Note  string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Range == "" {
		req.Range = "24h"
	}
	if _, ok := reportRanges[req.Range]; !ok {
		return badRequest("统计周期只能是 24h、7d 或 30d")
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
	remote := "AI报告/" + clientAddr(r)
	rp, usage, calls, ms, model, _, _, err := s.generateAIReportCore(r.Context(), c, "", nil, req.Range, req.Note, remote, emit)
	if err != nil {
		if rp != nil {
			log.Printf("保存运维报告失败：%v", err)
			emit(map[string]interface{}{"t": "warn", "text": "报告已生成，但保存到 data/reports 失败：" + err.Error()})
			log.Printf("AI 运维报告（%s，%s，%s）：%s —— 工具 %d 次，%d tokens，%.1f 秒", clientAddr(r), model, rp.Kind, rp.ID, calls, usage.TotalTokens, float64(ms)/1000)
			emit(map[string]interface{}{"t": "report", "report": rp})
			emit(map[string]interface{}{"t": "done", "usage": usage, "tools": calls, "ms": ms, "model": model})
			return nil
		}
		if errors.Is(err, context.Canceled) {
			log.Printf("AI 运维报告已取消（%s）", clientAddr(r))
			return nil
		}
		log.Printf("AI 运维报告失败（%s）：%v", clientAddr(r), err)
		emit(map[string]interface{}{"t": "error", "error": err.Error()})
		return nil
	}
	log.Printf("AI 运维报告（%s，%s，%s）：%s —— 工具 %d 次，%d tokens，%.1f 秒", clientAddr(r), model, rp.Kind, rp.ID, calls, usage.TotalTokens, float64(ms)/1000)
	emit(map[string]interface{}{"t": "report", "report": rp})
	emit(map[string]interface{}{"t": "done", "usage": usage, "tools": calls, "ms": ms, "model": model})
	return nil
}

// handleReports 报告列表、查看、下载与删除：
// GET reports、GET reports/<id>、GET reports/<id>.md、DELETE reports/<id>。
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request, arg string) error {
	id := strings.TrimPrefix(strings.TrimPrefix(arg, "reports"), "/")
	if id == "" {
		if r.Method != http.MethodGet {
			return notFound("接口")
		}
		writeJSON(w, s.listReportSummaries(0))
		return nil
	}
	switch r.Method {
	case http.MethodGet:
		md := strings.HasSuffix(id, ".md")
		rp, err := s.loadReport(strings.TrimSuffix(id, ".md"))
		if err != nil {
			return err
		}
		if !md {
			writeJSON(w, rp)
			return nil
		}
		name := "netPing-运维报告-" + rp.ID + ".md"
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="netPing-report-`+rp.ID+`.md"; filename*=UTF-8''`+urlPathEscape(name))
		_, _ = w.Write([]byte(exportReportMarkdown(rp)))
		return nil
	case http.MethodDelete:
		if err := s.deleteReport(id); err != nil {
			return err
		}
		log.Printf("删除运维报告 %s（来源 %s）", id, clientAddr(r))
		writeJSON(w, map[string]interface{}{"ok": true})
		return nil
	}
	return notFound("接口")
}

// urlPathEscape 按 RFC 5987 编码文件名。
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
