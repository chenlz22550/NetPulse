package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientEdgeErrorsAndBranches(t *testing.T) {
	// ProxyURL invalid
	c := Client{APIKey: "k", ProxyURL: "://bad-proxy", BaseURL: "http://127.0.0.1:1"}
	if _, _, err := c.Stream(context.Background(), Request{}, nil); err == nil || !strings.Contains(err.Error(), "代理") {
		t.Fatalf("bad proxy error = %v", err)
	}

	// APIError hints for 402, 404, 503
	for _, tc := range []struct {
		code int
		want string
	}{
		{402, "余额不足"},
		{404, "接口地址或模型名"},
		{503, "暂时不可用"},
	} {
		err := (&APIError{Status: tc.code, Msg: "detail"}).Error()
		if !strings.Contains(err, tc.want) {
			t.Fatalf("APIError(%d) = %q, want %q", tc.code, err, tc.want)
		}
	}

	// OpenAI Responses error event + response.failed + output_item.done
	t.Run("responses error and item done", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(r.URL.RawQuery, "fail") || r.Header.Get("Authorization") == "Bearer fail" {
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"quota exceeded\"}}}\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc1\",\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"fn1\",\"arguments\":\"{\\\"k\\\":1}\"}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")
		}))
		defer ts.Close()
		cl := Client{Protocol: ProtocolOpenAIResponses, BaseURL: ts.URL, APIKey: "fail", Model: "gpt-5"}
		if _, _, err := cl.Stream(context.Background(), Request{}, nil); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("expected response.failed error, got %v", err)
		}
		cl.APIKey = "ok"
		msg, u, err := cl.Stream(context.Background(), Request{}, nil)
		if err != nil || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "c1" || u.TotalTokens != 7 {
			t.Fatalf("responses item.done: msg=%+v u=%+v err=%v", msg, u, err)
		}
	})

	// Anthropic error event, non-thinking mode, /v1 suffix handling
	t.Run("anthropic error and v1 base", func(t *testing.T) {
		if got := anthropicEndpoint("https://api.anthropic.com/v1"); got != "https://api.anthropic.com/v1/messages" {
			t.Fatalf("anthropicEndpoint = %q", got)
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")
		}))
		defer ts.Close()
		cl := Client{Protocol: ProtocolAnthropic, BaseURL: ts.URL + "/v1", APIKey: "k", Model: "claude", Thinking: false, TLSSkipVerify: true}
		if _, _, err := cl.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil || !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("anthropic error = %v", err)
		}
	})

	// Gemini error event, non-thinking mode, /v1beta suffix handling, non-map tool result
	t.Run("gemini error and v1beta base", func(t *testing.T) {
		if got := geminiEndpoint("https://generativelanguage.googleapis.com/v1beta", "models/gemini-2.5-flash"); !strings.Contains(got, "/v1beta/models/gemini-2.5-flash:streamGenerateContent") {
			t.Fatalf("geminiEndpoint = %q", got)
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"safety block\"}}\n\n")
		}))
		defer ts.Close()
		cl := Client{Protocol: ProtocolGemini, BaseURL: ts.URL + "/v1beta", APIKey: "k", Model: "gemini-2.5-flash"}
		if _, _, err := cl.Stream(context.Background(), Request{MaxTokens: 64, Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "tool", ToolCallID: "t1", Content: `[1,2,3]`},
			{Role: "tool", ToolCallID: "t2", Content: `plain text`},
		}}, nil); err == nil || !strings.Contains(err.Error(), "safety block") {
			t.Fatalf("gemini error = %v", err)
		}
	})

	// parseErrMsg plain fallback & parseToolArgsMap invalid json
	if parseErrMsg([]byte("raw text error")) != "raw text error" {
		t.Fatal("parseErrMsg raw fallback failed")
	}
	if parseErrMsg([]byte(`{"message":"top msg"}`)) != "top msg" {
		t.Fatal("parseErrMsg message fallback failed")
	}
	if m := parseToolArgsMap("not-json"); len(m) != 0 {
		t.Fatalf("parseToolArgsMap invalid = %v", m)
	}
}
