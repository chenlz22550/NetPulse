package server

// AI 诊断自检：模型写完初稿后，程序先按拓扑和实时状态核对回答里提到的设备，再让模型以审核人身份
// 逐条核对事实与证据，必要时调用工具实测，最后给出「通过」或修正后的完整回答。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"netping/internal/config"
	"netping/internal/llm"
	"netping/internal/topology"
)

const (
	reviewPassMark   = "【通过】"
	reviewReviseMark = "【修正】"
	aiReviewRounds   = 4                // 自检最多几轮（最后一轮不再给工具）
	aiReviewCalls    = 8                // 自检最多追加调用工具次数
	aiReviewMinDraft = 40               // 初稿太短（寒暄、无实质结论）时不自检
	aiReviewMinTime  = 15 * time.Second // 剩余时间不足时跳过自检
	aiReviewMaxFacts = 20               // 自动核对最多列出的设备数
)

var reviewText = map[string]string{"off": "关闭", "skipped": "跳过", "pass": "通过", "revised": "已修正", "failed": "未完成"}

// 现场核实类工具：调用过这些工具的设备算“已实测”。
var probeToolNames = map[string]bool{"ping_now": true, "tcp_check": true, "arp_lookup": true, "trace_route": true, "get_switch_ports": true}

type aiReviewResult struct {
	Result string // pass / revised / failed / skipped
	Note   string
	Final  string // 自检后的最终回答
	Calls  int
	Usage  llm.Usage
}

func reviewPrompt(facts string) string {
	return `【自检】你是这份诊断回答的审核人。请对照本次对话中的「当前概况」和全部工具结果，逐条核对上面的回答：
1. 事实：每个设备名、IP、端口、数值、时间（包括 ` + "```chart" + ` 图表中的节点、状态、数值与连线）是否都能在数据里找到原文，有没有张冠李戴、算错或编造；
2. 证据：关键判断有没有现场核实——说“现在离线/不通”要有 ping_now（怀疑只是禁 ping 时再用 tcp_check 或 arp_lookup）；归因到端口、网线、双工、拥塞或交换机重启要有 get_switch_ports 的读数（未配置 SNMP 时应在回答里说明无法核实）；说“多台同时掉线/停电”要有 correlate_events 或 analyze_impact；外网或跨网段不通要有 trace_route；
3. 推理：有没有忽略更可能的原因，或把推测说成定论；数据不足时是否如实说明；
4. 下面是程序自动核对的结果，其中的不一致必须处理：
` + facts + `

缺少关键证据且工具能补上时，直接调用工具核实（最多 ` + fmt.Sprint(aiReviewCalls) + ` 次，只查真正影响结论的）。核对完毕后，只用以下两种格式之一回复：
- 全部成立：第一行以「` + reviewPassMark + `」开头，后面用一句话（不超过 60 字）说明核实了什么，不要重复原回答；
- 需要修改：第一行只写「` + reviewReviseMark + `」，从第二行开始给出修正后的完整回答（沿用原来的结构和篇幅要求，保留或同步修正 ` + "```chart" + ` 可视化图表，直接面向用户，不要提“初稿”“自检”这些过程）。`
}

// ---------------------------------------------------------------------------
// 程序自动核对

var ipCandRe = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// extractIPv4 找出文本中的 IPv4 地址（排除版本号等更长的点分数字）。
func extractIPv4(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ipCandRe.FindAllStringIndex(text, -1) {
		a, b := m[0], m[1]
		if a > 0 && (isDigit(text[a-1]) || text[a-1] == '.') {
			continue
		}
		if b < len(text) && (isDigit(text[b]) || (text[b] == '.' && b+1 < len(text) && isDigit(text[b+1]))) {
			continue
		}
		ip := text[a:b]
		if net.ParseIP(ip).To4() == nil || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

// probedIPs 统计对话中用现场核实类工具实测过的地址：ip → 工具名列表。
func probedIPs(t *topology.Topology, msgs []llm.Message) (map[string][]string, []string) {
	probed := map[string][]string{}
	var called []string
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			var a map[string]json.RawMessage
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			label := name
			if len(a) > 0 {
				label += " " + truncRunes(tc.Function.Arguments, 80)
			}
			if !seen[label] {
				seen[label] = true
				called = append(called, label)
			}
			if !probeToolNames[name] {
				continue
			}
			ip := argStr(a, "ip")
			if id := argStr(a, "id"); id != "" {
				if _, rid, err := resolveDevice(t, id, ""); err == nil {
					_, ip, _ = deviceLabel(t, rid)
				}
			}
			if ip != "" && !containsStr(probed[ip], name) {
				probed[ip] = append(probed[ip], name)
			}
		}
	}
	return probed, called
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// aiFactSheet 按拓扑与实时状态核对回答中提到的设备，生成给模型的核对表。
func (s *Server) aiFactSheet(draft string, msgs []llm.Message) string {
	t, _ := s.Topo.Snapshot()
	sts := s.statusMap()
	var corpus strings.Builder
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "tool" {
			corpus.WriteString(m.Content)
		}
	}
	probed, called := probedIPs(t, msgs)

	type mention struct{ ip, id string }
	var list []mention
	got := map[string]bool{}
	for _, ip := range extractIPv4(draft) {
		_, id := t.FindByIP(ip)
		key := ip
		if id != "" {
			key = id
		}
		if !got[key] {
			got[key] = true
			list = append(list, mention{ip, id})
		}
	}
	// 按名称提到的设备（名称至少 3 个字符，避免误匹配）
	type named struct{ name, id, ip string }
	var names []named
	for _, sw := range t.Switches {
		if sw.Name != "" {
			names = append(names, named{sw.Name, sw.ID, sw.IP})
		}
	}
	for _, n := range t.Nodes {
		if n.Name != "" {
			names = append(names, named{n.Name, n.ID, n.IP})
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return len(names[i].name) > len(names[j].name) })
	for _, nm := range names {
		if utf8.RuneCountInString(nm.name) >= 3 && !got[nm.id] && strings.Contains(draft, nm.name) {
			got[nm.id] = true
			list = append(list, mention{nm.ip, nm.id})
		}
	}
	if len(list) > aiReviewMaxFacts {
		list = list[:aiReviewMaxFacts]
	}

	var b strings.Builder
	if len(list) == 0 {
		b.WriteString("- 回答中没有提到可核对的具体设备（IP 或名称）。\n")
	} else {
		b.WriteString("回答中提到的设备（按拓扑与此刻的监控状态核对）：\n")
	}
	for _, m := range list {
		if m.id == "" {
			fmt.Fprintf(&b, "- %s：拓扑中没有这个地址", m.ip)
			if strings.Contains(corpus.String(), m.ip) {
				b.WriteString("，但在工具结果中出现过")
			} else {
				b.WriteString("，本次所有数据和工具结果中也都没有出现过——请确认是否写错或属于臆测")
			}
			if p := probed[m.ip]; len(p) > 0 {
				b.WriteString("；已实测：" + strings.Join(p, "、"))
			}
			b.WriteString("\n")
			continue
		}
		name, ip, attach := deviceLabel(t, m.id)
		fmt.Fprintf(&b, "- %s", name)
		if ip != "" && ip != name {
			fmt.Fprintf(&b, "（%s）", ip)
		}
		if attach != "" {
			b.WriteString("，" + attach)
		}
		if st, ok := sts[m.id]; ok {
			fmt.Fprintf(&b, "：此刻状态 %s（自 %s 起），1 分钟平均 %.1fms、丢包 %.1f%%，1 小时丢包 %.1f%%", st.State, fmtLocal(st.StateSince), st.Avg1m, st.Loss1m, st.Loss1h)
		} else {
			b.WriteString("：未监控（没有 ping 数据）")
		}
		if p := probed[ip]; len(p) > 0 {
			b.WriteString("；本次已实测：" + strings.Join(p, "、"))
		} else if ip != "" {
			b.WriteString("；本次未做现场核实")
		}
		b.WriteString("\n")
	}
	counts := map[string]int{}
	for _, st := range sts {
		counts[st.State]++
	}
	fmt.Fprintf(&b, "- 此刻全网：离线 %d、丢包 %d、延迟高 %d、正常 %d。\n", counts["down"], counts["loss"], counts["slow"], counts["ok"])
	if len(called) == 0 {
		b.WriteString("- 本次没有调用任何工具，结论只依据「当前概况」。")
	} else {
		if len(called) > 30 {
			called = called[:30]
		}
		b.WriteString("- 本次已调用的工具：" + strings.Join(called, "；"))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 自检输出的判定：先缓冲，确认是「通过」还是「修正」后再决定是否把修正稿流式发给网页。

type reviewGate struct {
	emit     func(map[string]interface{})
	buf      strings.Builder // 本轮尚未判定的输出
	revising bool            // 已判定为修正并开始输出
	text     strings.Builder // 已输出的修正稿
}

func (g *reviewGate) reset() { g.buf.Reset() }

func (g *reviewGate) send(d string) {
	if d == "" {
		return
	}
	g.text.WriteString(d)
	g.emit(map[string]interface{}{"t": "delta", "text": d})
}

func (g *reviewGate) startRevise(content string) {
	g.revising = true
	g.emit(map[string]interface{}{"t": "revise"})
	g.send(strings.TrimLeft(content, "\r\n"))
}

// stripMark 去掉标记及包裹它的符号，保留同一行里的正文。
func stripMark(line, mark string) string {
	line = strings.Replace(line, mark, "", 1)
	return strings.Trim(line, " \t\r*:：#>")
}

func (g *reviewGate) delta(d string) {
	if g.revising {
		g.send(d)
		return
	}
	g.buf.WriteString(d)
	s := strings.TrimLeft(g.buf.String(), " \t\r\n*#>")
	if strings.HasPrefix(s, reviewReviseMark) {
		if i := strings.IndexByte(s, '\n'); i >= 0 { // 等标记所在行结束再开始输出
			g.startRevise(stripMark(s[:i], reviewReviseMark) + s[i:])
		}
	}
}

// cleanNote 自检通过时的一句话说明。
func cleanNote(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Trim(l, " \t\r*:：#>-"); l != "" {
			return truncRunes(l, 100)
		}
	}
	return ""
}

// finish 在最后一轮结束时给出判定：结果、说明、最终回答。
func (g *reviewGate) finish(draft string) (string, string, string) {
	if g.revising {
		if final := strings.TrimSpace(g.text.String()); final != "" {
			return "revised", "", final
		}
		return "failed", "修正稿为空，保留原回答", draft
	}
	s := strings.TrimSpace(g.buf.String())
	st := strings.TrimLeft(s, " \t\r\n*#>")
	switch {
	case s == "":
		return "failed", "模型没有给出自检结论，保留原回答", draft
	case strings.HasPrefix(st, reviewPassMark):
		return "pass", cleanNote(strings.Replace(st, reviewPassMark, "", 1)), draft
	case strings.HasPrefix(st, reviewReviseMark):
		content := strings.TrimSpace(stripMark(st, reviewReviseMark))
		if content == "" {
			return "failed", "修正稿为空，保留原回答", draft
		}
		g.startRevise(content)
		return "revised", "", strings.TrimSpace(g.text.String())
	}
	// 标记不在开头：按出现的标记处理
	if i := strings.Index(s, reviewReviseMark); i >= 0 {
		if content := strings.TrimSpace(strings.TrimLeft(s[i+len(reviewReviseMark):], " \t*:：")); utf8.RuneCountInString(content) >= aiReviewMinDraft {
			g.startRevise(content)
			return "revised", "", strings.TrimSpace(g.text.String())
		}
	}
	if i := strings.Index(s, reviewPassMark); i >= 0 {
		note := cleanNote(s[i+len(reviewPassMark):])
		if note == "" {
			note = cleanNote(s[:i])
		}
		return "pass", note, draft
	}
	// 没按格式回复：短的当作核对说明，长的当作修正后的回答
	if utf8.RuneCountInString(s) < 150 {
		return "pass", cleanNote(s), draft
	}
	g.startRevise(s)
	return "revised", "", strings.TrimSpace(g.text.String())
}

// ---------------------------------------------------------------------------

// aiSelfCheck 对初稿做自检。出错或超时时保留初稿，不影响已经给出的回答。
func (s *Server) aiSelfCheck(ctx context.Context, c config.Config, run aiRunResult, remote string, emit func(map[string]interface{})) aiReviewResult {
	draft := strings.TrimSpace(run.Final)
	out := aiReviewResult{Result: "skipped", Final: run.Final}
	if utf8.RuneCountInString(draft) < aiReviewMinDraft {
		return out
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < aiReviewMinTime {
		out.Note = "剩余时间不足，跳过自检"
		emit(map[string]interface{}{"t": "review_end", "result": "skipped", "text": out.Note})
		return out
	}
	emit(map[string]interface{}{"t": "review"})
	emit(map[string]interface{}{"t": "status", "text": "正在自检：核对结论与数据…"})
	msgs := append(append([]llm.Message{}, run.Msgs...),
		llm.Message{Role: "assistant", Content: run.Final},
		llm.Message{Role: "user", Content: reviewPrompt(s.aiFactSheet(draft, run.Msgs))})
	cl := s.aiClient(c)
	tools := aiTools()
	g := &reviewGate{emit: emit}
	end := func() aiReviewResult {
		ev := map[string]interface{}{"t": "review_end", "result": out.Result, "text": out.Note, "calls": out.Calls}
		if out.Result == "failed" && g.revising {
			ev["restore"] = true // 修正稿没写完：网页恢复原回答
			out.Final = run.Final
		}
		emit(ev)
		return out
	}
	for round := 0; ; round++ {
		if err := s.checkProfileEpoch(ctx); err != nil {
			out.Result = "failed"
			out.Note = err.Error()
			return end()
		}
		useTools := tools
		if round >= aiReviewRounds-1 || out.Calls >= aiReviewCalls {
			useTools = nil
		}
		if round > 0 {
			emit(map[string]interface{}{"t": "status", "text": "正在自检：根据核实结果复核…"})
		}
		g.reset()
		msg, usage, err := cl.Stream(ctx, llm.Request{Messages: msgs, Tools: useTools}, g.delta)
		out.Usage.Add(usage)
		if err != nil {
			out.Result = "failed"
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				out.Note = "自检超时，保留原回答"
			case errors.Is(err, context.Canceled):
				out.Note = "已取消"
			default:
				out.Note = "自检未完成：" + truncRunes(err.Error(), 120)
			}
			return end()
		}
		if len(msg.ToolCalls) > 0 && useTools != nil {
			msgs = append(msgs, msg)
			out.Calls += len(msg.ToolCalls)
			toolMsgs, err := s.aiExecCallsInCtx(ctx, aiToolNames, msg.ToolCalls, remote, "review", emit)
			msgs = append(msgs, toolMsgs...)
			if err != nil {
				out.Result = "failed"
				out.Note = err.Error()
				return end()
			}
			if g.revising {
				emit(map[string]interface{}{"t": "break"})
			}
			continue
		}
		out.Result, out.Note, out.Final = g.finish(run.Final)
		return end()
	}
}
