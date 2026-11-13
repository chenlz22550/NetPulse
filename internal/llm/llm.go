// Package llm 是大模型接口的最小流式客户端（支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages、Google Gemini 等常见协议与工具调用），用于 AI 诊断。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Message 对话消息。
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall 模型发起的工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool 提供给模型的函数定义。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 函数名、说明与 JSON Schema 参数。
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheHitTokens   int `json:"prompt_cache_hit_tokens,omitempty"`
}

// Add 累加用量。
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.CacheHitTokens += o.CacheHitTokens
}

// 协议类型。
const (
	ProtocolOpenAI          = "openai"           // OpenAI Chat Completions（/chat/completions）
	ProtocolOpenAIResponses = "openai-responses" // OpenAI Responses API（/responses）
	ProtocolAnthropic       = "anthropic"        // Anthropic Messages API（/v1/messages）
	ProtocolGemini          = "gemini"           // Google Gemini generateContent（/models/{model}:streamGenerateContent）
)

// Protocols 支持的协议标识。
var Protocols = []string{ProtocolOpenAI, ProtocolOpenAIResponses, ProtocolAnthropic, ProtocolGemini}

// NormalizeProtocol 归一化协议名，未知或空值返回 ProtocolOpenAI。
func NormalizeProtocol(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case ProtocolOpenAIResponses, "responses", "openai_responses":
		return ProtocolOpenAIResponses
	case ProtocolAnthropic, "claude":
		return ProtocolAnthropic
	case ProtocolGemini, "google":
		return ProtocolGemini
	default:
		return ProtocolOpenAI
	}
}

// Client 一个接口端点。
type Client struct {
	Protocol      string
	BaseURL       string
	APIKey        string
	Model         string
	Thinking      bool
	ProxyURL      string
	TLSSkipVerify bool
	Timeout       time.Duration
}

// Request 一次补全请求。
type Request struct {
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

func (c *Client) httpClient() (*http.Client, error) {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
	if c.ProxyURL != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("代理地址无效: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if c.TLSSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式开启
	}
	return &http.Client{Transport: tr, Timeout: c.Timeout}, nil
}

// APIError 接口返回的错误。
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	hint := ""
	switch e.Status {
	case 401:
		hint = "API Key 无效或已失效"
	case 402:
		hint = "账户余额不足"
	case 404:
		hint = "接口地址或模型名不正确"
	case 429:
		hint = "请求过于频繁，请稍后再试"
	case 500, 502, 503:
		hint = "模型服务暂时不可用，请稍后再试"
	}
	msg := e.Msg
	if hint != "" {
		msg = hint + "（" + msg + "）"
	}
	return fmt.Sprintf("模型接口返回 HTTP %d：%s", e.Status, msg)
}

// Stream 发送流式请求：正文增量通过 onDelta 回调，返回完整的助手消息（含工具调用）与用量。
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(string)) (Message, Usage, error) {
	var usage Usage
	if c.APIKey == "" {
		return Message{}, usage, errors.New("尚未设置 API Key")
	}
	switch NormalizeProtocol(c.Protocol) {
	case ProtocolOpenAIResponses:
		return c.streamOpenAIResponses(ctx, req, onDelta)
	case ProtocolAnthropic:
		return c.streamAnthropic(ctx, req, onDelta)
	case ProtocolGemini:
		return c.streamGemini(ctx, req, onDelta)
	default:
		return c.streamOpenAI(ctx, req, onDelta)
	}
}

func (c *Client) doStreamReq(ctx context.Context, method, endpoint string, headers map[string]string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		hr.Header.Set(k, v)
	}
	hc, err := c.httpClient()
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("连接模型接口失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Msg: parseErrMsg(raw)}
	}
	return resp, nil
}

func parseErrMsg(raw []byte) string {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil {
		if e.Error != nil && e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return msg
}

func joinEndpoint(base, suffix string) string {
	return strings.TrimRight(base, "/") + suffix
}

func newSSEScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return sc
}

func parseToolArgsMap(args string) map[string]interface{} {
	m := map[string]interface{}{}
	if strings.TrimSpace(args) != "" {
		_ = json.Unmarshal([]byte(args), &m)
	}
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. OpenAI Chat Completions (/chat/completions)

func (c *Client) streamOpenAI(ctx context.Context, req Request, onDelta func(string)) (Message, Usage, error) {
	var usage Usage
	body := map[string]interface{}{
		"model":          c.Model,
		"messages":       req.Messages,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if c.Thinking {
		body["thinking"] = map[string]string{"type": "enabled"}
	} else {
		body["thinking"] = map[string]string{"type": "disabled"}
		body["temperature"] = 0.3
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	resp, err := c.doStreamReq(ctx, http.MethodPost, joinEndpoint(c.BaseURL, "/chat/completions"), map[string]string{
		"Authorization": "Bearer " + c.APIKey,
	}, body)
	if err != nil {
		return Message{}, usage, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	var content, reasoning strings.Builder
	var calls []ToolCall
	sc := newSSEScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return out, usage, fmt.Errorf("模型接口错误：%s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			d := ch.Delta
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				if onDelta != nil {
					onDelta(d.Content)
				}
			}
			for _, tc := range d.ToolCalls {
				// 索引来自模型服务，限制范围以避免负下标崩溃或异常内存分配。
				if tc.Index < 0 || tc.Index >= 128 {
					return out, usage, fmt.Errorf("模型工具调用索引无效: %d（允许 0~127）", tc.Index)
				}
				for len(calls) <= tc.Index {
					calls = append(calls, ToolCall{Type: "function"})
				}
				t := &calls[tc.Index]
				if tc.ID != "" {
					t.ID = tc.ID
				}
				if tc.Function.Name != "" {
					t.Function.Name += tc.Function.Name
				}
				t.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, usage, ctx.Err()
		}
		return out, usage, fmt.Errorf("读取模型输出中断: %w", err)
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	out.ToolCalls = calls
	return out, usage, nil
}

// ---------------------------------------------------------------------------
// 2. OpenAI Responses API (/responses)

func (c *Client) streamOpenAIResponses(ctx context.Context, req Request, onDelta func(string)) (Message, Usage, error) {
	var usage Usage
	var sysParts []string
	var input []interface{}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if m.Content != "" {
				sysParts = append(sysParts, m.Content)
			}
		case "user":
			input = append(input, map[string]interface{}{"role": "user", "content": m.Content})
		case "assistant":
			if m.Content != "" {
				input = append(input, map[string]interface{}{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				callID := tc.ID
				if callID == "" {
					callID = "call_0"
				}
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				input = append(input, map[string]interface{}{
					"type":      "function_call",
					"call_id":   callID,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}
		case "tool":
			input = append(input, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		}
	}
	body := map[string]interface{}{
		"model":  c.Model,
		"input":  input,
		"stream": true,
	}
	if len(sysParts) > 0 {
		body["instructions"] = strings.Join(sysParts, "\n\n")
	}
	if c.Thinking {
		body["reasoning"] = map[string]string{"effort": "medium"}
	} else {
		body["temperature"] = 0.3
	}
	if len(req.Tools) > 0 {
		var tools []map[string]interface{}
		for _, tl := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        tl.Function.Name,
				"description": tl.Function.Description,
				"parameters":  tl.Function.Parameters,
			})
		}
		body["tools"] = tools
	}
	if req.MaxTokens > 0 {
		// Responses API 对 max_output_tokens 有最小 16 的限制，避免 400。
		maxOut := req.MaxTokens
		if maxOut < 16 {
			maxOut = 16
		}
		body["max_output_tokens"] = maxOut
	}
	resp, err := c.doStreamReq(ctx, http.MethodPost, joinEndpoint(c.BaseURL, "/responses"), map[string]string{
		"Authorization": "Bearer " + c.APIKey,
	}, body)
	if err != nil {
		return Message{}, usage, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	var content, reasoning strings.Builder
	var calls []ToolCall
	byItemID := map[string]int{}
	byCallID := map[string]int{}
	ensureCall := func(itemID, callID, name string) *ToolCall {
		if itemID != "" {
			if idx, ok := byItemID[itemID]; ok {
				return &calls[idx]
			}
		}
		if callID != "" {
			if idx, ok := byCallID[callID]; ok {
				if itemID != "" {
					byItemID[itemID] = idx
				}
				return &calls[idx]
			}
		}
		if len(calls) >= 128 {
			return nil
		}
		tc := ToolCall{ID: callID, Type: "function"}
		tc.Function.Name = name
		idx := len(calls)
		calls = append(calls, tc)
		if itemID != "" {
			byItemID[itemID] = idx
		}
		if callID != "" {
			byCallID[callID] = idx
		}
		return &calls[idx]
	}

	sc := newSSEScanner(resp.Body)
	var curEvent string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			curEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ev struct {
			Type      string `json:"type"`
			Delta     string `json:"delta"`
			ItemID    string `json:"item_id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Item      *struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Usage *struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					TotalTokens        int `json:"total_tokens"`
					InputTokensDetails *struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		typ := ev.Type
		if typ == "" {
			typ = curEvent
		}
		switch typ {
		case "error":
			msg := ev.Message
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			if msg == "" {
				msg = "模型服务返回错误"
			}
			return out, usage, fmt.Errorf("模型接口错误：%s", msg)
		case "response.failed":
			msg := "请求失败"
			if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
				msg = ev.Response.Error.Message
			}
			return out, usage, fmt.Errorf("模型接口错误：%s", msg)
		case "response.output_text.delta":
			if ev.Delta != "" {
				content.WriteString(ev.Delta)
				if onDelta != nil {
					onDelta(ev.Delta)
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if ev.Delta != "" {
				reasoning.WriteString(ev.Delta)
			}
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				t := ensureCall(ev.Item.ID, ev.Item.CallID, ev.Item.Name)
				if t != nil && ev.Item.Arguments != "" && t.Function.Arguments == "" {
					t.Function.Arguments = ev.Item.Arguments
				}
			}
		case "response.function_call_arguments.delta":
			if t := ensureCall(ev.ItemID, ev.CallID, ev.Name); t != nil {
				t.Function.Arguments += ev.Delta
			}
		case "response.function_call_arguments.done":
			if t := ensureCall(ev.ItemID, ev.CallID, ev.Name); t != nil {
				if ev.Name != "" && t.Function.Name == "" {
					t.Function.Name = ev.Name
				}
				if ev.Arguments != "" {
					t.Function.Arguments = ev.Arguments
				}
			}
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				if t := ensureCall(ev.Item.ID, ev.Item.CallID, ev.Item.Name); t != nil {
					if ev.Item.CallID != "" {
						t.ID = ev.Item.CallID
					}
					if ev.Item.Name != "" {
						t.Function.Name = ev.Item.Name
					}
					if ev.Item.Arguments != "" {
						t.Function.Arguments = ev.Item.Arguments
					}
				}
			}
		case "response.completed", "response.incomplete":
			if ev.Response != nil && ev.Response.Usage != nil {
				u := ev.Response.Usage
				usage = Usage{
					PromptTokens:     u.InputTokens,
					CompletionTokens: u.OutputTokens,
					TotalTokens:      u.TotalTokens,
				}
				if usage.TotalTokens == 0 {
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				}
				if u.InputTokensDetails != nil {
					usage.CacheHitTokens = u.InputTokensDetails.CachedTokens
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, usage, ctx.Err()
		}
		return out, usage, fmt.Errorf("读取模型输出中断: %w", err)
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	out.ToolCalls = calls
	return out, usage, nil
}

// ---------------------------------------------------------------------------
// 3. Anthropic Messages API (/v1/messages 或 /messages)

func anthropicEndpoint(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/messages"
	}
	return b + "/v1/messages"
}

func (c *Client) streamAnthropic(ctx context.Context, req Request, onDelta func(string)) (Message, Usage, error) {
	var usage Usage
	var sysParts []string
	var messages []map[string]interface{}

	appendMsg := func(role string, blocks []map[string]interface{}) {
		if len(blocks) == 0 {
			return
		}
		// Anthropic 要求 user 与 assistant 严格交替；合并同角色连续消息。
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			prev := messages[n-1]["content"].([]map[string]interface{})
			messages[n-1]["content"] = append(prev, blocks...)
			return
		}
		messages = append(messages, map[string]interface{}{"role": role, "content": blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				sysParts = append(sysParts, m.Content)
			}
		case "user":
			if m.Content != "" {
				appendMsg("user", []map[string]interface{}{{"type": "text", "text": m.Content}})
			}
		case "assistant":
			var blocks []map[string]interface{}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					id = "call_0"
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  tc.Function.Name,
					"input": parseToolArgsMap(tc.Function.Arguments),
				})
			}
			appendMsg("assistant", blocks)
		case "tool":
			appendMsg("user", []map[string]interface{}{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}})
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := map[string]interface{}{
		"model":      c.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if len(sysParts) > 0 {
		body["system"] = strings.Join(sysParts, "\n\n")
	}
	if c.Thinking {
		budget := 2048
		if maxTokens <= budget {
			budget = maxTokens / 2
			if budget < 1024 {
				budget = 1024
			}
			if maxTokens <= budget {
				maxTokens = budget + 1024
				body["max_tokens"] = maxTokens
			}
		}
		body["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
	} else {
		body["temperature"] = 0.3
	}
	if len(req.Tools) > 0 {
		var tools []map[string]interface{}
		for _, tl := range req.Tools {
			schema := tl.Function.Parameters
			if schema == nil {
				schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			tools = append(tools, map[string]interface{}{
				"name":         tl.Function.Name,
				"description":  tl.Function.Description,
				"input_schema": schema,
			})
		}
		body["tools"] = tools
	}

	resp, err := c.doStreamReq(ctx, http.MethodPost, anthropicEndpoint(c.BaseURL), map[string]string{
		"x-api-key":         c.APIKey,
		"anthropic-version": "2023-06-01",
	}, body)
	if err != nil {
		return Message{}, usage, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	var content, reasoning strings.Builder
	var calls []ToolCall
	blockToCall := map[int]int{}

	sc := newSSEScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type  string                 `json:"type"`
				ID    string                 `json:"id"`
				Name  string                 `json:"name"`
				Input map[string]interface{} `json:"input"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Message *struct {
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "error":
			msg := "模型服务返回错误"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			return out, usage, fmt.Errorf("模型接口错误：%s", msg)
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				usage.PromptTokens += u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				usage.CacheHitTokens += u.CacheReadInputTokens
				usage.CompletionTokens += u.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
		case "message_delta":
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					usage.PromptTokens = ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens + ev.Usage.CacheCreationInputTokens
				}
				if ev.Usage.CacheReadInputTokens > 0 {
					usage.CacheHitTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.CompletionTokens = ev.Usage.OutputTokens
				}
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" && len(calls) < 128 {
				tc := ToolCall{ID: ev.ContentBlock.ID, Type: "function"}
				tc.Function.Name = ev.ContentBlock.Name
				if len(ev.ContentBlock.Input) > 0 {
					if b, err := json.Marshal(ev.ContentBlock.Input); err == nil {
						tc.Function.Arguments = string(b)
					}
				}
				blockToCall[ev.Index] = len(calls)
				calls = append(calls, tc)
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					content.WriteString(ev.Delta.Text)
					if onDelta != nil {
						onDelta(ev.Delta.Text)
					}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					reasoning.WriteString(ev.Delta.Thinking)
				}
			case "input_json_delta":
				if idx, ok := blockToCall[ev.Index]; ok {
					calls[idx].Function.Arguments += ev.Delta.PartialJSON
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, usage, ctx.Err()
		}
		return out, usage, fmt.Errorf("读取模型输出中断: %w", err)
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
		if strings.TrimSpace(calls[i].Function.Arguments) == "" {
			calls[i].Function.Arguments = "{}"
		}
	}
	out.ToolCalls = calls
	return out, usage, nil
}

// ---------------------------------------------------------------------------
// 4. Google Gemini generateContent (/v1beta/models/{model}:streamGenerateContent?alt=sse)

func geminiEndpoint(base, model string) string {
	b := strings.TrimRight(base, "/")
	if !strings.Contains(b, "/v1") {
		b += "/v1beta"
	}
	m := strings.TrimPrefix(model, "models/")
	return b + "/models/" + url.PathEscape(m) + ":streamGenerateContent?alt=sse"
}

func (c *Client) streamGemini(ctx context.Context, req Request, onDelta func(string)) (Message, Usage, error) {
	var usage Usage
	var sysParts []map[string]string
	var contents []map[string]interface{}
	callNameByID := map[string]string{}

	appendContent := func(role string, parts []map[string]interface{}) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			prev := contents[n-1]["parts"].([]map[string]interface{})
			contents[n-1]["parts"] = append(prev, parts...)
			return
		}
		contents = append(contents, map[string]interface{}{"role": role, "parts": parts})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				sysParts = append(sysParts, map[string]string{"text": m.Content})
			}
		case "user":
			if m.Content != "" {
				appendContent("user", []map[string]interface{}{{"text": m.Content}})
			}
		case "assistant":
			var parts []map[string]interface{}
			if m.Content != "" {
				parts = append(parts, map[string]interface{}{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					callNameByID[tc.ID] = tc.Function.Name
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{
						"name": tc.Function.Name,
						"args": parseToolArgsMap(tc.Function.Arguments),
					},
				})
			}
			appendContent("model", parts)
		case "tool":
			name := callNameByID[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID
			}
			var respObj interface{}
			if json.Unmarshal([]byte(m.Content), &respObj) != nil {
				respObj = map[string]interface{}{"result": m.Content}
			} else if _, isMap := respObj.(map[string]interface{}); !isMap {
				respObj = map[string]interface{}{"result": respObj}
			}
			appendContent("user", []map[string]interface{}{{
				"functionResponse": map[string]interface{}{
					"name":     name,
					"response": respObj,
				},
			}})
		}
	}

	genCfg := map[string]interface{}{}
	if c.Thinking {
		genCfg["thinkingConfig"] = map[string]interface{}{"thinkingBudget": -1}
	} else {
		genCfg["temperature"] = 0.3
	}
	if req.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = req.MaxTokens
	}
	body := map[string]interface{}{
		"contents":         contents,
		"generationConfig": genCfg,
	}
	if len(sysParts) > 0 {
		body["systemInstruction"] = map[string]interface{}{"parts": sysParts}
	}
	if len(req.Tools) > 0 {
		var decls []map[string]interface{}
		for _, tl := range req.Tools {
			decls = append(decls, map[string]interface{}{
				"name":        tl.Function.Name,
				"description": tl.Function.Description,
				"parameters":  tl.Function.Parameters,
			})
		}
		body["tools"] = []map[string]interface{}{{"functionDeclarations": decls}}
	}

	resp, err := c.doStreamReq(ctx, http.MethodPost, geminiEndpoint(c.BaseURL, c.Model), map[string]string{
		"x-goog-api-key": c.APIKey,
	}, body)
	if err != nil {
		return Message{}, usage, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	var content, reasoning strings.Builder
	var calls []ToolCall

	sc := newSSEScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						Thought      bool   `json:"thought"`
						FunctionCall *struct {
							ID   string                 `json:"id"`
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
				TotalTokenCount         int `json:"totalTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return out, usage, fmt.Errorf("模型接口错误：%s", chunk.Error.Message)
		}
		if um := chunk.UsageMetadata; um != nil {
			comp := um.CandidatesTokenCount + um.ThoughtsTokenCount
			total := um.TotalTokenCount
			if total == 0 {
				total = um.PromptTokenCount + comp
			}
			usage = Usage{
				PromptTokens:     um.PromptTokenCount,
				CompletionTokens: comp,
				TotalTokens:      total,
				CacheHitTokens:   um.CachedContentTokenCount,
			}
		}
		for _, cand := range chunk.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					if p.Thought {
						reasoning.WriteString(p.Text)
					} else {
						content.WriteString(p.Text)
						if onDelta != nil {
							onDelta(p.Text)
						}
					}
				}
				if p.FunctionCall != nil && len(calls) < 128 {
					tc := ToolCall{ID: p.FunctionCall.ID, Type: "function"}
					tc.Function.Name = p.FunctionCall.Name
					b, _ := json.Marshal(p.FunctionCall.Args)
					if len(b) == 0 || string(b) == "null" {
						tc.Function.Arguments = "{}"
					} else {
						tc.Function.Arguments = string(b)
					}
					calls = append(calls, tc)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, usage, ctx.Err()
		}
		return out, usage, fmt.Errorf("读取模型输出中断: %w", err)
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	out.ToolCalls = calls
	return out, usage, nil
}
