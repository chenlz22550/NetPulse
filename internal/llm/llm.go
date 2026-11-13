// Package llm 是 OpenAI 兼容 Chat Completions 接口的最小客户端（流式输出 + 工具调用），用于 AI 诊断。
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

// Client 一个接口端点。
type Client struct {
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
	data, _ := json.Marshal(body)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return Message{}, usage, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	hr.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc, err := c.httpClient()
	if err != nil {
		return Message{}, usage, err
	}
	resp, err := hc.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, usage, ctx.Err()
		}
		return Message{}, usage, fmt.Errorf("连接模型接口失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return Message{}, usage, &APIError{Status: resp.StatusCode, Msg: msg}
	}

	out := Message{Role: "assistant"}
	var content, reasoning strings.Builder
	var calls []ToolCall
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
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
