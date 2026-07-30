package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testResponsesClient() *ResponsesClient {
	return NewResponsesClient(ResponsesConfig{
		BaseURL:   CodexResponsesURL,
		Info:      ModelInfo{Provider: "openai", ID: "gpt-5.1-codex", Reasoning: true},
		Token:     func(context.Context) (string, error) { return "tok", nil },
		AccountID: "acct-1",
	})
}

// itemTypes extracts the "type" of each input item for sequence assertions.
func itemTypes(t *testing.T, items []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, raw := range items {
		var x struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &x); err != nil {
			t.Fatalf("decode item: %v", err)
		}
		out = append(out, x.Type)
	}
	return out
}

func TestResponsesEncodeBasic(t *testing.T) {
	c := testResponsesClient()
	msgs := []Message{
		{Role: RoleSystem, Content: "be terse"},
		{Role: RoleUser, Content: "read x.go"},
	}
	tools := []Tool{{Type: "function", Function: ToolDefinition{
		Name: "read_file", Description: "read a file", Parameters: json.RawMessage(`{"type":"object"}`),
	}}}
	req := c.encodeRequest(msgs, tools)

	if req.Instructions != "be terse" {
		t.Fatalf("instructions = %q", req.Instructions)
	}
	if req.Store || !req.Stream {
		t.Fatalf("store=%v stream=%v", req.Store, req.Stream)
	}
	if len(req.Include) != 1 || req.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", req.Include)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "medium" {
		t.Fatalf("reasoning = %+v", req.Reasoning)
	}
	if got := itemTypes(t, req.Input); len(got) != 1 || got[0] != "message" {
		t.Fatalf("input types = %v", got)
	}
	if len(req.Tools) != 1 || req.Tools[0].Type != "function" || req.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %+v", req.Tools)
	}

	// The Codex backend rejects these client-side controls; they must never be
	// serialized as top-level request parameters.
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(wire, &top); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"max_output_tokens", "max_tokens", "parallel_tool_calls",
		"temperature", "metadata", "user"} {
		if _, ok := top[forbidden]; ok {
			t.Fatalf("request contains forbidden parameter %q: %s", forbidden, wire)
		}
	}
}

func TestReasoningEffortClampsMinimal(t *testing.T) {
	if got := reasoningEffort("minimal"); got != "low" {
		t.Fatalf("minimal should clamp to low, got %q", got)
	}
	if got := reasoningEffort(""); got != "medium" {
		t.Fatalf("empty should default to medium, got %q", got)
	}
	if got := reasoningEffort("high"); got != "high" {
		t.Fatalf("high should pass through, got %q", got)
	}
}

const toolCallSSE = `
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_text.delta","delta":"Thinking"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"ENC1"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"x.go\"}"}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","arguments":"{\"path\":\"x.go\"}"}}

data: {"type":"response.completed","response":{}}

data: [DONE]
`

func TestResponsesParseToolCall(t *testing.T) {
	var reasoningSeen string
	msg, _, err := parseResponsesStream(strings.NewReader(toolCallSSE), "openai", func(e StreamEvent) {
		reasoningSeen += e.Reasoning
	})
	if err != nil {
		t.Fatal(err)
	}
	if reasoningSeen != "Thinking" {
		t.Fatalf("reasoning stream = %q", reasoningSeen)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"x.go"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if len(msg.ProviderState) == 0 {
		t.Fatal("expected provider_state with reasoning")
	}
	if !strings.Contains(string(msg.ProviderState), "ENC1") {
		t.Fatalf("provider_state missing encrypted content: %s", msg.ProviderState)
	}
}

func TestResponsesReasoningReplayRoundTrip(t *testing.T) {
	// First turn: parse the streamed assistant message (carries ProviderState).
	assistant, _, err := parseResponsesStream(strings.NewReader(toolCallSSE), "openai", func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}

	// Second turn: that assistant message plus its tool result are replayed.
	c := testResponsesClient()
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "go"},
		assistant,
		{Role: RoleTool, ToolCallID: "call_abc", Content: "file contents"},
	}
	req := c.encodeRequest(msgs, nil)
	types := itemTypes(t, req.Input)
	// Expect: user message, replayed reasoning, function_call, function_call_output.
	want := []string{"message", "reasoning", "function_call", "function_call_output"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("input sequence = %v, want %v", types, want)
	}
	// The encrypted reasoning must be replayed verbatim.
	if !strings.Contains(string(req.Input[1]), "ENC1") {
		t.Fatalf("replayed reasoning missing encrypted content: %s", req.Input[1])
	}
	// The function_call_output must carry the tool result keyed by call_id.
	if !strings.Contains(string(req.Input[3]), "call_abc") ||
		!strings.Contains(string(req.Input[3]), "file contents") {
		t.Fatalf("function_call_output wrong: %s", req.Input[3])
	}
}

func TestReasoningNotReplayedCrossProvider(t *testing.T) {
	assistant, _, err := parseResponsesStream(strings.NewReader(toolCallSSE), "openai", func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	// A Responses client for a DIFFERENT provider must drop the foreign reasoning.
	other := NewResponsesClient(ResponsesConfig{
		BaseURL: CodexResponsesURL,
		Info:    ModelInfo{Provider: "other", ID: "x", Reasoning: true},
		Token:   func(context.Context) (string, error) { return "t", nil },
	})
	req := other.encodeRequest([]Message{
		{Role: RoleUser, Content: "go"}, assistant,
		{Role: RoleTool, ToolCallID: "call_abc", Content: "r"},
	}, nil)
	for _, raw := range req.Input {
		if strings.Contains(string(raw), "ENC1") {
			t.Fatalf("encrypted reasoning leaked cross-provider: %s", raw)
		}
	}
}

const textSSE = `
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"E2"}}

data: {"type":"response.output_text.delta","delta":"Hello "}

data: {"type":"response.output_text.delta","delta":"world"}

data: {"type":"response.completed","response":{}}
`

// The subscription path reports its token counts only on the terminal event, so
// a typo in these field names would silently zero every ChatGPT-side number.
func TestResponsesParseUsage(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":4210,` +
		`"input_tokens_details":{"cached_tokens":3840},"output_tokens":73}}}
`
	_, usage, err := parseResponsesStream(strings.NewReader(sse), "openai", func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: 4210, CachedTokens: 3840, OutputTokens: 73}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	if usage.Total() != 4283 {
		t.Fatalf("total = %d; cached is part of input, not extra", usage.Total())
	}
}

func TestResponsesParseText(t *testing.T) {
	var content string
	msg, _, err := parseResponsesStream(strings.NewReader(textSSE), "openai", func(e StreamEvent) {
		content += e.Content
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Hello world" || content != "Hello world" {
		t.Fatalf("content = %q (stream %q)", msg.Content, content)
	}
	if msg.FinishReason != "stop" {
		t.Fatalf("finish = %q", msg.FinishReason)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", msg.ToolCalls)
	}
}

func TestResponsesParseMultilineSSEData(t *testing.T) {
	sse := "data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"Hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	msg, _, err := parseResponsesStream(strings.NewReader(sse), "openai", func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Hi" {
		t.Fatalf("content = %q", msg.Content)
	}
}

func TestResponsesErrorEvent(t *testing.T) {
	sse := `
data: {"type":"response.failed","response":{"error":{"message":"boom"}}}
`
	if _, _, err := parseResponsesStream(strings.NewReader(sse), "openai", func(StreamEvent) {}); err == nil {
		t.Fatal("expected error from response.failed event")
	}
}

// wireMessages must drop provider_state so it never leaks to chat-completions.
func TestWireMessagesStripsProviderState(t *testing.T) {
	msgs := []Message{{Role: RoleAssistant, Content: "x", ProviderState: json.RawMessage(`[{"a":1}]`)}}
	out := wireMessages(msgs)
	wire, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "provider_state") {
		t.Fatalf("provider_state should be stripped on the wire: %s", wire)
	}
	if msgs[0].ProviderState == nil {
		t.Fatal("original message must not be mutated")
	}
}

func TestWireMessagesEncodesImages(t *testing.T) {
	out := wireMessages([]Message{{Role: RoleUser, Content: "what is this?", Images: []Image{{MediaType: "image/png", Data: "AAA="}}}})
	wire, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"type":"image_url"`) || !strings.Contains(string(wire), "data:image/png;base64,AAA=") {
		t.Fatalf("image not encoded for chat completions: %s", wire)
	}
}

func TestResponsesEncodeImages(t *testing.T) {
	req := testResponsesClient().encodeRequest([]Message{
		{Role: RoleUser, Content: "look", Images: []Image{{MediaType: "image/png", Data: "AAA="}}},
	}, nil)
	if len(req.Input) != 1 {
		t.Fatalf("input = %d", len(req.Input))
	}
	wire := string(req.Input[0])
	if !strings.Contains(wire, `"type":"input_image"`) || !strings.Contains(wire, "data:image/png;base64,AAA=") {
		t.Fatalf("image not encoded for responses: %s", wire)
	}
}

func TestResponsesStreamHeadersAndBody(t *testing.T) {
	var gotAuth, gotAcct, gotBeta, gotOrig, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("chatgpt-account-id")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotOrig = r.Header.Get("originator")
		gotSession = r.Header.Get("session_id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, textSSE)
	}))
	defer srv.Close()

	c := NewResponsesClient(ResponsesConfig{
		BaseURL:   srv.URL,
		Info:      ModelInfo{Provider: "openai", ID: "gpt-5.1-codex", Reasoning: true},
		Token:     func(context.Context) (string, error) { return "tok-xyz", nil },
		AccountID: "acct-9",
	})
	msg, err := c.Stream(context.Background(), []Message{
		{Role: RoleSystem, Content: "s"}, {Role: RoleUser, Content: "hi"},
	}, nil, 0, func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Hello world" {
		t.Fatalf("content = %q", msg.Content)
	}
	if gotAuth != "Bearer tok-xyz" || gotAcct != "acct-9" {
		t.Fatalf("auth=%q acct=%q", gotAuth, gotAcct)
	}
	if gotBeta != "responses=experimental" || gotOrig != "codex_cli_rs" || gotSession == "" {
		t.Fatalf("beta=%q orig=%q session=%q", gotBeta, gotOrig, gotSession)
	}
}
