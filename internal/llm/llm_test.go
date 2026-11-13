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
			var deltas []string
			msg, usage, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "诊断"}}, Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "overview"}}}, MaxTokens: 100}, func(s string) { deltas = append(deltas, s) })
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
