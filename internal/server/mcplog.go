package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MCP 工具调用日志：记录所有由大模型（网页 AI 功能）或外部 MCP 客户端发起的工具调用，
// 包含关联 ID、机房、工具、完整参数与完整结果（脱敏后）、错误、开始时间与耗时。
// 日志按天写入 data/mcp_calls/YYYY-MM-DD.jsonl，单条记录不截断；只按保留天数整文件清理。

const (
	mcpLogRetentionDays = 30
	mcpLogDirName       = "mcp_calls"
	mcpLogMaxLine       = 256 << 20 // 读取时单行上限（远大于单个工具结果）
)

// MCPCallLog 一次工具调用的完整记录。
type MCPCallLog struct {
	ID          string `json:"id"`                     // 本条调用的唯一 ID
	RunID       string `json:"run_id,omitempty"`       // 同一次 AI 任务（诊断/报告等）内所有调用共享
	ToolCallID  string `json:"tool_call_id,omitempty"` // 模型给出的 tool_call id
	RPCID       string `json:"rpc_id,omitempty"`       // 外部 MCP 客户端的 JSON-RPC id
	Source      string `json:"source"`                 // ai_diagnose / ai_review / ai_report / ai_optimize / ai_discover / mcp …
	Phase       string `json:"phase,omitempty"`
	Remote      string `json:"remote,omitempty"`
	ProfileID   string `json:"profile_id"`
	ProfileName string `json:"profile_name,omitempty"`
	Tool        string `json:"tool"`
	Title       string `json:"title,omitempty"`
	Args        string `json:"args"`             // 脱敏后的完整参数
	Result      string `json:"result,omitempty"` // 脱敏后的完整结果
	ResultBytes int    `json:"result_bytes"`
	// ModelTruncated 模型实际收到的结果是否因长度上限被截断（日志中仍保存完整结果）。
	ModelTruncated bool   `json:"model_truncated,omitempty"`
	Status         string `json:"status"` // ok / error / denied
	Error          string `json:"error,omitempty"`
	StartedAt      int64  `json:"started_at"` // Unix 毫秒
	DurationMs     int64  `json:"duration_ms"`
	Redacted       bool   `json:"redacted,omitempty"` // 参数或结果中有字段被脱敏
	// 列表用的预览（写入时生成），列表查询不必解析完整结果。
	ArgsPreview   string `json:"args_preview,omitempty"`
	ResultPreview string `json:"result_preview,omitempty"`
	ArgsBytes     int    `json:"args_bytes"`
}

// mcpCallHead 列表查询时只解码的字段（不含完整参数与结果）。
type mcpCallHead struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	RPCID          string `json:"rpc_id,omitempty"`
	Source         string `json:"source"`
	Phase          string `json:"phase,omitempty"`
	Remote         string `json:"remote,omitempty"`
	ProfileID      string `json:"profile_id"`
	ProfileName    string `json:"profile_name,omitempty"`
	Tool           string `json:"tool"`
	Title          string `json:"title,omitempty"`
	ResultBytes    int    `json:"result_bytes"`
	ModelTruncated bool   `json:"model_truncated,omitempty"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	StartedAt      int64  `json:"started_at"`
	DurationMs     int64  `json:"duration_ms"`
	Redacted       bool   `json:"redacted,omitempty"`
	ArgsPreview    string `json:"args_preview,omitempty"`
	ResultPreview  string `json:"result_preview,omitempty"`
	ArgsBytes      int    `json:"args_bytes"`
}

func (e MCPCallLog) head() mcpCallHead {
	return mcpCallHead{ID: e.ID, RunID: e.RunID, ToolCallID: e.ToolCallID, RPCID: e.RPCID, Source: e.Source, Phase: e.Phase,
		Remote: e.Remote, ProfileID: e.ProfileID, ProfileName: e.ProfileName, Tool: e.Tool, Title: e.Title,
		ResultBytes: e.ResultBytes, ModelTruncated: e.ModelTruncated, Status: e.Status, Error: e.Error,
		StartedAt: e.StartedAt, DurationMs: e.DurationMs, Redacted: e.Redacted,
		ArgsPreview: e.ArgsPreview, ResultPreview: e.ResultPreview, ArgsBytes: e.ArgsBytes}
}

func runePrefix(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

type mcpLogger struct {
	mu        sync.Mutex
	dir       string
	lastPrune string
	mem       []MCPCallLog // 未配置 BaseDir 时（测试）保存在内存
}

func (s *Server) callLogger() *mcpLogger {
	s.callLogOnce.Do(func() {
		s.callLog = &mcpLogger{}
		if s.BaseDir != "" {
			s.callLog.dir = filepath.Join(s.BaseDir, "data", mcpLogDirName)
		}
	})
	return s.callLog
}

func newLogID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + time.Now().Format("20060102150405") + "-" + hex.EncodeToString(b[:])
}

// ---- 调用上下文 ----

type aiRunKey struct{}

type aiRunInfo struct {
	RunID  string
	Source string
}

// withAIRun 为一次 AI 任务分配关联 ID（已有则沿用），source 标识来源功能。
func withAIRun(ctx context.Context, source string) context.Context {
	if ri, ok := ctx.Value(aiRunKey{}).(aiRunInfo); ok && ri.RunID != "" {
		if source != "" && ri.Source == "" {
			ri.Source = source
			return context.WithValue(ctx, aiRunKey{}, ri)
		}
		return ctx
	}
	return context.WithValue(ctx, aiRunKey{}, aiRunInfo{RunID: newLogID("run-"), Source: source})
}

func aiRunFrom(ctx context.Context) aiRunInfo {
	if ctx == nil {
		return aiRunInfo{}
	}
	ri, _ := ctx.Value(aiRunKey{}).(aiRunInfo)
	return ri
}

// aiSourceFromRemote 由 remote 前缀推断来源（兼容未显式设置来源的调用方）。
func aiSourceFromRemote(remote string) string {
	switch {
	case strings.HasPrefix(remote, "AI诊断"):
		return "ai_diagnose"
	case strings.HasPrefix(remote, "AI优化"):
		return "ai_optimize"
	case strings.HasPrefix(remote, "AI发现"), strings.HasPrefix(remote, "AI自主发现"):
		return "ai_discover"
	case strings.HasPrefix(remote, "AI报告"), strings.HasPrefix(remote, "AI运维报告"):
		return "ai_report"
	}
	return "ai"
}

// ---- 脱敏 ----

var (
	sensitiveKeyRe = regexp.MustCompile(`(?i)(api[_-]?key|apikey|token|secret|passw(or)?d|passwd|pwd|community|authorization|cookie|credential|private[_-]?key|access[_-]?key|webhook(_url)?|dingtalk_url|feishu_url|slack_url|generic_webhook_url|signature|^sign$)`)
	// 文本中的 key=value / "Bearer xxx" / URL 查询串中的令牌。
	inlineSecretRe = regexp.MustCompile(`(?i)\b(access_token|token|api[_-]?key|apikey|secret|password|passwd|pwd|sign|signature|key|community)=([^&\s"'\\]+)`)
	bearerRe       = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	skKeyRe        = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{12,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})`)
	// 企业微信/钉钉/飞书/Slack 机器人地址的路径或查询中带有令牌。
	webhookURLRe = regexp.MustCompile(`(?i)https?://(qyapi\.weixin\.qq\.com|oapi\.dingtalk\.com|open\.feishu\.cn|open\.larksuite\.com|hooks\.slack\.com)[^\s"'\\]*`)
	// URL 中的账号密码，例如代理地址 http://user:pass@proxy:3128。
	urlUserinfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@"'\\]+:[^@\s/"'\\]+@`)
)

const redactedMark = "***已脱敏***"

// redactText 对任意文本做脱敏：优先按 JSON 字段名递归脱敏，再处理行内凭据。
func redactText(s string) (string, bool) {
	if strings.TrimSpace(s) == "" {
		return s, false
	}
	changed := false
	trim := strings.TrimSpace(s)
	if (strings.HasPrefix(trim, "{") || strings.HasPrefix(trim, "[")) && json.Valid([]byte(trim)) {
		var v interface{}
		dec := json.NewDecoder(strings.NewReader(trim))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			v2, c := redactValue(v, "")
			if c {
				if b, err := marshalNoEscape(v2); err == nil {
					s = b
					changed = true
				}
			}
		}
	}
	out := redactInline(s)
	if out != s {
		changed = true
	}
	return out, changed
}

func marshalNoEscape(v interface{}) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func redactInline(s string) string {
	s = webhookURLRe.ReplaceAllStringFunc(s, func(m string) string {
		if i := strings.Index(m, "://"); i >= 0 {
			rest := m[i+3:]
			if j := strings.IndexAny(rest, "/?"); j >= 0 {
				return m[:i+3] + rest[:j] + "/" + redactedMark
			}
		}
		return m
	})
	s = urlUserinfoRe.ReplaceAllString(s, "${1}"+redactedMark+"@")
	s = inlineSecretRe.ReplaceAllString(s, "${1}="+redactedMark)
	s = bearerRe.ReplaceAllString(s, "${1} "+redactedMark)
	s = skKeyRe.ReplaceAllString(s, redactedMark)
	return s
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	// 布尔/提示性字段不算凭据本身。
	if strings.HasSuffix(lk, "_set") || strings.HasSuffix(lk, "_hint") || strings.HasSuffix(lk, "_configured") || lk == "token_required" {
		return false
	}
	return sensitiveKeyRe.MatchString(k)
}

func redactValue(v interface{}, key string) (interface{}, bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		changed := false
		for k, val := range x {
			if isSensitiveKey(k) {
				if str, ok := val.(string); ok {
					if str != "" && str != redactedMark {
						x[k] = redactedMark
						changed = true
					}
					continue
				}
				if val != nil {
					if _, isBool := val.(bool); !isBool {
						if _, isNum := val.(json.Number); !isNum {
							x[k] = redactedMark
							changed = true
							continue
						}
					}
				}
			}
			nv, c := redactValue(val, k)
			if c {
				x[k] = nv
				changed = true
			}
		}
		return x, changed
	case []interface{}:
		changed := false
		for i, val := range x {
			nv, c := redactValue(val, key)
			if c {
				x[i] = nv
				changed = true
			}
		}
		return x, changed
	case string:
		// 字符串里可能嵌套 JSON 或行内凭据。
		out, c := redactText(x)
		return out, c
	}
	return v, false
}

// ---- 记录 ----

func (s *Server) profileNameOf(id string) string {
	if s.Profiles == nil {
		return ""
	}
	return s.Profiles.NameOf(id)
}

func (s *Server) recordToolCall(e MCPCallLog) MCPCallLog {
	if e.ID == "" {
		e.ID = newLogID("call-")
	}
	if e.ProfileID == "" {
		e.ProfileID = s.activeProfileID()
	}
	if e.ProfileName == "" {
		e.ProfileName = s.profileNameOf(e.ProfileID)
	}
	var c1, c2, c3 bool
	e.Args, c1 = redactText(e.Args)
	e.Result, c2 = redactText(e.Result)
	e.Error, c3 = redactText(e.Error)
	e.Redacted = e.Redacted || c1 || c2 || c3
	e.ResultBytes = len(e.Result)
	e.ArgsBytes = len(e.Args)
	e.ArgsPreview = runePrefix(e.Args, 300)
	e.ResultPreview = runePrefix(e.Result, 200)
	if err := s.callLogger().append(e); err != nil {
		log.Printf("写入 MCP 调用日志失败：%v（%s %s）", err, e.Tool, e.ID)
	}
	return e
}

func (l *mcpLogger) append(e MCPCallLog) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == "" {
		l.mem = append(l.mem, e)
		return nil
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	day := time.UnixMilli(e.StartedAt).Format("2006-01-02")
	f, err := os.OpenFile(filepath.Join(l.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if l.lastPrune != day {
		l.lastPrune = day
		l.pruneLocked(time.Now())
	}
	if werr != nil {
		return werr
	}
	return cerr
}

func (l *mcpLogger) pruneLocked(now time.Time) {
	cutoff := now.AddDate(0, 0, -mcpLogRetentionDays).Format("2006-01-02")
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if day := strings.TrimSuffix(name, ".jsonl"); len(day) == 10 && day < cutoff {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// ---- 查询 ----

type mcpLogQuery struct {
	ProfileID string // 空 = 所有机房
	Tool      string
	Status    string
	Source    string
	RunID     string
	Q         string
	Limit     int
	BeforeMs  int64
	AfterID   string // 分页游标：从这条记录之后（更早）继续
}

func (q mcpLogQuery) match(e mcpCallHead) bool {
	if q.ProfileID != "" && e.ProfileID != q.ProfileID {
		return false
	}
	if q.Tool != "" && e.Tool != q.Tool {
		return false
	}
	if q.Status != "" && e.Status != q.Status {
		return false
	}
	if q.Source != "" && !strings.HasPrefix(e.Source, q.Source) && !(q.Source == "ai" && strings.HasPrefix(e.Source, "ai")) {
		return false
	}
	if q.RunID != "" && e.RunID != q.RunID {
		return false
	}
	if q.BeforeMs > 0 && e.StartedAt >= q.BeforeMs {
		return false
	}
	if q.Q != "" {
		hay := strings.ToLower(e.Tool + " " + e.Title + " " + e.ArgsPreview + " " + e.Error + " " + e.ID + " " + e.RunID + " " + e.Remote)
		if !strings.Contains(hay, strings.ToLower(q.Q)) {
			return false
		}
	}
	return true
}

// files 返回按日期从新到旧排列的日志文件。
func (l *mcpLogger) files() ([]string, error) {
	ents, err := os.ReadDir(l.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(l.dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files, nil
}

// scanLines 逐行读取文件，fn 返回 false 时停止。
func scanLines(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), mcpLogMaxLine)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if !fn(sc.Bytes()) {
			return nil
		}
	}
	return sc.Err()
}

// eachHead 按写入顺序从新到旧遍历记录摘要（只解码摘要字段），fn 返回 false 时停止。
func (l *mcpLogger) eachHead(fn func(h mcpCallHead) bool) error {
	l.mu.Lock()
	if l.dir == "" {
		mem := append([]MCPCallLog(nil), l.mem...)
		l.mu.Unlock()
		for i := len(mem) - 1; i >= 0; i-- {
			if !fn(mem[i].head()) {
				return nil
			}
		}
		return nil
	}
	l.mu.Unlock()
	files, err := l.files()
	if err != nil {
		return err
	}
	for _, path := range files {
		var heads []mcpCallHead
		if err := scanLines(path, func(line []byte) bool {
			var h mcpCallHead
			if json.Unmarshal(line, &h) == nil && h.ID != "" {
				heads = append(heads, h)
			}
			return true
		}); err != nil {
			return err
		}
		for i := len(heads) - 1; i >= 0; i-- {
			if !fn(heads[i]) {
				return nil
			}
		}
	}
	return nil
}

// each 遍历完整记录（测试与导出用），顺序同 eachHead。
func (l *mcpLogger) each(fn func(e MCPCallLog) bool) error {
	var ids []string
	if err := l.eachHead(func(h mcpCallHead) bool { ids = append(ids, h.ID); return true }); err != nil {
		return err
	}
	for _, id := range ids {
		e, ok, err := l.get(id)
		if err != nil {
			return err
		}
		if ok && !fn(e) {
			return nil
		}
	}
	return nil
}

func (l *mcpLogger) query(q mcpLogQuery) ([]mcpCallHead, bool, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	var out []mcpCallHead
	more := false
	passed := q.AfterID == ""
	err := l.eachHead(func(h mcpCallHead) bool {
		if !passed {
			passed = h.ID == q.AfterID
			return true
		}
		if !q.match(h) {
			return true
		}
		if len(out) >= q.Limit {
			more = true
			return false
		}
		out = append(out, h)
		return true
	})
	return out, more, err
}

// get 读取一条完整记录：逐行查找，只解码 ID 匹配的那一行。
func (l *mcpLogger) get(id string) (MCPCallLog, bool, error) {
	l.mu.Lock()
	if l.dir == "" {
		defer l.mu.Unlock()
		for _, e := range l.mem {
			if e.ID == id {
				return e, true, nil
			}
		}
		return MCPCallLog{}, false, nil
	}
	l.mu.Unlock()
	if id == "" {
		return MCPCallLog{}, false, nil
	}
	needle, _ := json.Marshal(id)
	needle = append([]byte(`"id":`), needle...)
	files, err := l.files()
	if err != nil {
		return MCPCallLog{}, false, err
	}
	var found MCPCallLog
	ok := false
	for _, path := range files {
		if err := scanLines(path, func(line []byte) bool {
			if !bytes.Contains(line, needle) {
				return true
			}
			var e MCPCallLog
			if json.Unmarshal(line, &e) == nil && e.ID == id {
				found, ok = e, true
				return false
			}
			return true
		}); err != nil {
			return MCPCallLog{}, false, err
		}
		if ok {
			break
		}
	}
	return found, ok, nil
}

// handleMCPCalls GET /api/mcp/calls[?profile=current|all|<id>&tool=&status=&source=&run=&q=&limit=&before=]
// GET /api/mcp/calls/<id> 返回单条完整记录；GET /api/mcp/calls/<id>/download 下载完整记录。
func (s *Server) handleMCPCalls(w http.ResponseWriter, r *http.Request, arg string) error {
	if r.Method != http.MethodGet {
		return &apiError{status: http.StatusMethodNotAllowed, msg: "不支持的请求方法"}
	}
	l := s.callLogger()
	arg = strings.Trim(arg, "/")
	if arg != "" {
		parts := strings.Split(arg, "/")
		e, ok, err := l.get(parts[0])
		if err != nil {
			return err
		}
		if !ok {
			return notFound("调用记录")
		}
		if len(parts) == 2 && parts[1] == "download" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, e.ID))
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			return enc.Encode(e)
		}
		writeJSON(w, e)
		return nil
	}
	qv := r.URL.Query()
	q := mcpLogQuery{
		Tool: strings.TrimSpace(qv.Get("tool")), Status: strings.TrimSpace(qv.Get("status")),
		Source: strings.TrimSpace(qv.Get("source")), RunID: strings.TrimSpace(qv.Get("run")),
		Q: strings.TrimSpace(qv.Get("q")),
	}
	switch p := strings.TrimSpace(qv.Get("profile")); p {
	case "", "current":
		q.ProfileID = s.activeProfileID()
	case "all":
	default:
		q.ProfileID = p
	}
	q.Limit, _ = strconv.Atoi(qv.Get("limit"))
	q.AfterID = strings.TrimSpace(qv.Get("after"))
	q.BeforeMs, _ = strconv.ParseInt(qv.Get("before"), 10, 64)
	list, more, err := l.query(q)
	if err != nil {
		return err
	}
	if list == nil {
		list = []mcpCallHead{}
	}
	writeJSON(w, map[string]interface{}{
		"items": list, "more": more, "profile": q.ProfileID,
		"retention_days": mcpLogRetentionDays,
	})
	return nil
}
