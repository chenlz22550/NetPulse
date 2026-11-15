package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStreamAssemblesResponse(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		t.Run(fmt.Sprint(thinking), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
				}
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["model"] != "test-model" || body["stream"] != true || body["max_tokens"] != float64(100) || len(body["tools"].([]interface{})) != 1 || len(body["messages"].([]interface{})) != 1 || body["stream_options"].(map[string]interface{})["include_usage"] != true {
					t.Errorf("body: %v", body)
				}
				wantThinking := "disabled"
				if thinking {
					wantThinking = "enabled"
				}
				if body["thinking"].(map[string]interface{})["type"] != wantThinking {
					t.Errorf("thinking: %v", body)
				}
				if _, ok := body["temperature"]; ok == thinking {
					t.Errorf("temperature: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, ": keepalive\n\ndata: invalid-json\n\n")
				for _, delta := range []string{
					`{"reasoning_content":"检查链路"}`,
					`{"content":"网络"}`,
					`{"content":"正常","tool_calls":[{"index":1,"function":{"name":"get_","arguments":"{\"id\":"}},{"index":0,"id":"first","function":{"name":"overview","arguments":"{}"}}]}`,
					`{"tool_calls":[{"index":1,"function":{"name":"device","arguments":"\"n1\"}"}}]}`,
				} {
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\n", delta)
				}
				fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"total_tokens\":20,\"prompt_cache_hit_tokens\":3}}\n\ndata: [DONE]\n\ndata: {\"error\":{\"message\":\"must not read after DONE\"}}\n\n")
			}))
			defer ts.Close()
			c := Client{BaseURL: ts.URL + "/v1", APIKey: "test-key", Model: "test-model", Thinking: thinking}
			var deltas, thoughts []string
			msg, usage, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "诊断"}}, Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "overview"}}}, MaxTokens: 100,
				OnReasoning: func(s string) { thoughts = append(thoughts, s) }}, func(s string) { deltas = append(deltas, s) })
			if !reflect.DeepEqual(thoughts, []string{"检查链路"}) {
				t.Fatalf("OnReasoning: %v", thoughts)
			}
			if err != nil {
				t.Fatal(err)
			}
			if msg.Role != "assistant" || msg.Content != "网络正常" || msg.ReasoningContent != "检查链路" || !reflect.DeepEqual(deltas, []string{"网络", "正常"}) {
				t.Fatalf("message: %+v, deltas: %v", msg, deltas)
			}
			if len(msg.ToolCalls) != 2 || msg.ToolCalls[0].ID != "first" || msg.ToolCalls[1].ID != "call_1" || msg.ToolCalls[1].Type != "function" || msg.ToolCalls[1].Function.Name != "get_device" || msg.ToolCalls[1].Function.Arguments != `{"id":"n1"}` {
				t.Fatalf("calls: %+v", msg.ToolCalls)
			}
			if usage != (Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20, CacheHitTokens: 3}) {
				t.Fatalf("usage: %+v", usage)
			}
		})
	}
}

func TestStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"bad key", 401, `{"error":{"message":"invalid key"}}`, "API Key 无效"},
		{"rate limit", 429, "slow down", "请求过于频繁"},
		{"unknown status", 418, "teapot", "teapot"},
		{"stream error", 200, "data: {\"error\":{\"message\":\"upstream failed\"}}\n\n", "upstream failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer ts.Close()
			c := Client{BaseURL: ts.URL, APIKey: "key"}
			_, _, err := c.Stream(context.Background(), Request{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v", err)
			}
			if tc.status != 200 {
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != tc.status {
					t.Fatalf("APIError: %v", err)
				}
			}
		})
	}
	t.Run("missing key", func(t *testing.T) {
		_, _, err := (&Client{}).Stream(context.Background(), Request{}, nil)
		if err == nil || !strings.Contains(err.Error(), "尚未设置") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := (&Client{BaseURL: "http://127.0.0.1:1", APIKey: "key"}).Stream(ctx, Request{}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error: %v", err)
		}
	})
}

func TestStreamRejectsInvalidToolIndex(t *testing.T) {
	for _, index := range []int{-1, 1000000} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d}]}}]}\n\n", index)
			}))
			defer ts.Close()
			_, _, err := (&Client{BaseURL: ts.URL, APIKey: "key"}).Stream(context.Background(), Request{}, nil)
			if err == nil {
				t.Fatal("expected invalid tool index error")
			}
		})
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14, CacheHitTokens: 2}
	u.Add(Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, CacheHitTokens: 1})
	if u != (Usage{PromptTokens: 13, CompletionTokens: 6, TotalTokens: 19, CacheHitTokens: 3}) {
		t.Fatalf("usage: %+v", u)
	}
}

func TestNormalizeProtocol(t *testing.T) {
	cases := map[string]string{
		"":                 ProtocolOpenAI,
		"openai":           ProtocolOpenAI,
		"unknown":          ProtocolOpenAI,
		"openai-responses": ProtocolOpenAIResponses,
		"responses":        ProtocolOpenAIResponses,
		"anthropic":        ProtocolAnthropic,
		"claude":           ProtocolAnthropic,
		"gemini":           ProtocolGemini,
		"google":           ProtocolGemini,
	}
	for in, want := range cases {
		if got := NormalizeProtocol(in); got != want {
			t.Fatalf("NormalizeProtocol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamOpenAIResponses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer resp-key" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-5" || body["instructions"] != "你是网络助手" || body["stream"] != true {
			t.Fatalf("body: %v", body)
		}
		if body["reasoning"].(map[string]interface{})["effort"] != "medium" || body["reasoning"].(map[string]interface{})["summary"] != "auto" {
			t.Fatalf("reasoning: %v", body["reasoning"])
		}
		input := body["input"].([]interface{})
		if len(input) != 3 {
			t.Fatalf("input len: %d %v", len(input), input)
		}
		if input[1].(map[string]interface{})["type"] != "function_call" || input[2].(map[string]interface{})["type"] != "function_call_output" {
			t.Fatalf("input items: %v", input)
		}
		tools := body["tools"].([]interface{})
		if tools[0].(map[string]interface{})["name"] != "ping_now" {
			t.Fatalf("tools: %v", tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"先核实\"}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"网络\"}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"通畅\"}\n\n")
		fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_abc\",\"name\":\"get_device\",\"arguments\":\"\"}}\n\n")
		fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"id\\\":\"}\n\n")
		fmt.Fprint(w, "event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"id\\\":\\\"sw1\\\"}\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":30,\"output_tokens\":12,\"total_tokens\":42,\"input_tokens_details\":{\"cached_tokens\":10}}}}\n\n")
	}))
	defer ts.Close()

	c := Client{Protocol: ProtocolOpenAIResponses, BaseURL: ts.URL + "/v1", APIKey: "resp-key", Model: "gpt-5", Thinking: true}
	var deltas, thoughts []string
	var prevCall ToolCall
	prevCall.ID = "call_prev"
	prevCall.Type = "function"
	prevCall.Function.Name = "ping_now"
	prevCall.Function.Arguments = `{"ip":"1.1.1.1"}`
	msg, usage, err := c.Stream(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "你是网络助手"},
			{Role: "user", Content: "检查"},
			{Role: "assistant", ToolCalls: []ToolCall{prevCall}},
			{Role: "tool", ToolCallID: "call_prev", Content: `{"ok":true}`},
		},
		Tools:       []Tool{{Type: "function", Function: ToolFunction{Name: "ping_now"}}},
		MaxTokens:   10,
		OnReasoning: func(s string) { thoughts = append(thoughts, s) },
	}, func(s string) { deltas = append(deltas, s) })
	if strings.Join(thoughts, "") != "先核实" {
		t.Fatalf("OnReasoning: %v", thoughts)
	}
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "网络通畅" || msg.ReasoningContent != "先核实" || !reflect.DeepEqual(deltas, []string{"网络", "通畅"}) {
		t.Fatalf("msg: %+v deltas: %v", msg, deltas)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_abc" || msg.ToolCalls[0].Function.Name != "get_device" || msg.ToolCalls[0].Function.Arguments != `{"id":"sw1"}` {
		t.Fatalf("calls: %+v", msg.ToolCalls)
	}
	if usage != (Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42, CacheHitTokens: 10}) {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestStreamAnthropic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "ant-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "claude-sonnet-4-5" || body["system"] != "系统提示" || body["stream"] != true {
			t.Fatalf("body: %v", body)
		}
		if body["thinking"].(map[string]interface{})["type"] != "enabled" {
			t.Fatalf("thinking: %v", body["thinking"])
		}
		msgs := body["messages"].([]interface{})
		if len(msgs) != 3 || msgs[0].(map[string]interface{})["role"] != "user" || msgs[1].(map[string]interface{})["role"] != "assistant" || msgs[2].(map[string]interface{})["role"] != "user" {
			t.Fatalf("messages: %v", msgs)
		}
		tools := body["tools"].([]interface{})
		if tools[0].(map[string]interface{})["input_schema"] == nil {
			t.Fatalf("tools: %v", tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":5,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"分析中\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"一切\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"正常\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_overview\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer ts.Close()

	c := Client{Protocol: ProtocolAnthropic, BaseURL: ts.URL, APIKey: "ant-key", Model: "claude-sonnet-4-5", Thinking: true}
	var deltas, thoughts []string
	var prevCall ToolCall
	prevCall.ID = "toolu_0"
	prevCall.Type = "function"
	prevCall.Function.Name = "ping_now"
	prevCall.Function.Arguments = `{"ip":"1.1.1.1"}`
	msg, usage, err := c.Stream(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "系统提示"},
			{Role: "user", Content: "诊断"},
			{Role: "assistant", ToolCalls: []ToolCall{prevCall}},
			{Role: "tool", ToolCallID: "toolu_0", Content: `{"ok":true}`},
		},
		Tools:       []Tool{{Type: "function", Function: ToolFunction{Name: "get_overview"}}},
		OnReasoning: func(s string) { thoughts = append(thoughts, s) },
	}, func(s string) { deltas = append(deltas, s) })
	if strings.Join(thoughts, "") != "分析中" {
		t.Fatalf("OnReasoning: %v", thoughts)
	}
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "一切正常" || msg.ReasoningContent != "分析中" || !reflect.DeepEqual(deltas, []string{"一切", "正常"}) {
		t.Fatalf("msg: %+v deltas: %v", msg, deltas)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "toolu_1" || msg.ToolCalls[0].Function.Name != "get_overview" || msg.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("calls: %+v", msg.ToolCalls)
	}
	if usage != (Usage{PromptTokens: 25, CompletionTokens: 9, TotalTokens: 34, CacheHitTokens: 5}) {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestStreamGemini(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "gem-key" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["systemInstruction"] == nil || body["tools"] == nil {
			t.Fatalf("body: %v", body)
		}
		if tc, _ := body["generationConfig"].(map[string]interface{})["thinkingConfig"].(map[string]interface{}); tc == nil || tc["includeThoughts"] != true {
			t.Fatalf("thinkingConfig should request thought summaries: %v", body["generationConfig"])
		}
		contents := body["contents"].([]interface{})
		if len(contents) != 3 || contents[1].(map[string]interface{})["role"] != "model" {
			t.Fatalf("contents: %v", contents)
		}
		fnResp := contents[2].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["functionResponse"].(map[string]interface{})
		if fnResp["name"] != "ping_now" {
			t.Fatalf("functionResponse: %v", fnResp)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"思考\",\"thought\":true},{\"text\":\"状态\"}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"良好\"},{\"functionCall\":{\"name\":\"get_device\",\"args\":{\"id\":\"core\"}}}]}}],\"usageMetadata\":{\"promptTokenCount\":15,\"candidatesTokenCount\":5,\"thoughtsTokenCount\":3,\"totalTokenCount\":23,\"cachedContentTokenCount\":4}}\n\n")
	}))
	defer ts.Close()

	c := Client{Protocol: ProtocolGemini, BaseURL: ts.URL, APIKey: "gem-key", Model: "gemini-2.5-flash", Thinking: true}
	var deltas, thoughts []string
	var prevCall ToolCall
	prevCall.ID = "g_0"
	prevCall.Type = "function"
	prevCall.Function.Name = "ping_now"
	prevCall.Function.Arguments = `{"ip":"1.1.1.1"}`
	msg, usage, err := c.Stream(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "系统提示"},
			{Role: "user", Content: "诊断"},
			{Role: "assistant", ToolCalls: []ToolCall{prevCall}},
			{Role: "tool", ToolCallID: "g_0", Content: `{"ok":true}`},
		},
		Tools:       []Tool{{Type: "function", Function: ToolFunction{Name: "get_device"}}},
		OnReasoning: func(s string) { thoughts = append(thoughts, s) },
	}, func(s string) { deltas = append(deltas, s) })
	if strings.Join(thoughts, "") != "思考" {
		t.Fatalf("OnReasoning: %v", thoughts)
	}
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "状态良好" || msg.ReasoningContent != "思考" || !reflect.DeepEqual(deltas, []string{"状态", "良好"}) {
		t.Fatalf("msg: %+v deltas: %v", msg, deltas)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_0" || msg.ToolCalls[0].Function.Name != "get_device" || msg.ToolCalls[0].Function.Arguments != `{"id":"core"}` {
		t.Fatalf("calls: %+v", msg.ToolCalls)
	}
	if usage != (Usage{PromptTokens: 15, CompletionTokens: 8, TotalTokens: 23, CacheHitTokens: 4}) {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestResponsesSummaryFallback(t *testing.T) {
	var bodies []map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if body["reasoning"].(map[string]interface{})["summary"] != nil {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"Your organization must be verified to generate reasoning summaries."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer ts.Close()
	c := Client{Protocol: ProtocolOpenAIResponses, BaseURL: ts.URL, APIKey: "k", Model: "m", Thinking: true}
	msg, _, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil || msg.Content != "ok" || len(bodies) != 2 {
		t.Fatalf("fallback: err=%v msg=%+v requests=%d", err, msg, len(bodies))
	}
	// 其他 400 错误不重试。
	bodies = nil
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, nil)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad model name"}}`)
	}))
	defer ts2.Close()
	c.BaseURL = ts2.URL
	if _, _, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil || len(bodies) != 1 {
		t.Fatalf("unrelated 400 should not retry: err=%v requests=%d", err, len(bodies))
	}
}
