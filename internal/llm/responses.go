package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ResponsesConfig configures the ChatGPT subscription (Codex) Responses adapter.
// There is no max-tokens knob: the Codex backend manages output length and
// rejects max_output_tokens.
type ResponsesConfig struct {
	BaseURL   string
	Info      ModelInfo
	Token     func(context.Context) (string, error) // bearer access token
	AccountID string                                // chatgpt-account-id
	Effort    string                                // reasoning effort: low | medium | high (default medium)
}

// ResponsesClient speaks the OpenAI Responses API against the Codex subscription
// backend (store:false, streamed, with encrypted reasoning echoed between turns
// to preserve the chain). It implements Backend.
type ResponsesClient struct {
	cfg       ResponsesConfig
	HTTP      *http.Client
	sessionID string
	usageTracker
}

// NewResponsesClient builds the subscription adapter.
func NewResponsesClient(cfg ResponsesConfig) *ResponsesClient {
	if cfg.Effort == "" {
		cfg.Effort = "medium"
	}
	return &ResponsesClient{
		cfg:       cfg,
		HTTP:      &http.Client{Timeout: 10 * time.Minute},
		sessionID: newUUID(),
	}
}

// Model returns the adapter's model metadata.
func (c *ResponsesClient) Model() ModelInfo { return c.cfg.Info }

// Endpoint returns the Codex backend base URL this adapter calls.
func (c *ResponsesClient) Endpoint() string { return c.cfg.BaseURL }

// Tokenize falls back to the chars/4 estimate; the Responses backend has no
// tokenizer endpoint.
func (c *ResponsesClient) Tokenize(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

// ---- request encoding ----

// responsesRequest is restricted to the parameters the Codex subscription
// backend accepts. It rejects client-side output/sampling controls
// (max_output_tokens, temperature, metadata, user, ...), so only this allowlist
// is sent.
type responsesRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Input        []json.RawMessage `json:"input"`
	Tools        []responsesTool   `json:"tools,omitempty"`
	ToolChoice   string            `json:"tool_choice,omitempty"`
	Reasoning    *reasoningParam   `json:"reasoning,omitempty"`
	Include      []string          `json:"include,omitempty"`
	Store        bool              `json:"store"`
	Stream       bool              `json:"stream"`
}

type reasoningParam struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// encodeRequest bridges chat-shaped history to a Responses request. The first
// system message becomes the instructions slot; everything else becomes input
// items, with each assistant turn's replayed reasoning (ProviderState) spliced
// in ahead of its function calls so the encrypted chain survives store:false.
func (c *ResponsesClient) encodeRequest(messages []Message, tools []Tool) responsesRequest {
	var instructions string
	var input []json.RawMessage
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			if instructions == "" {
				instructions = m.Content
			} else {
				instructions += "\n\n" + m.Content
			}
		case RoleUser:
			if m.Content != "" || len(m.Images) > 0 {
				input = append(input, messageItemWithImages("user", "input_text", m.Content, m.Images))
			}
		case RoleAssistant:
			input = append(input, replayReasoning(m.ProviderState, c.cfg.Info.Provider)...)
			if strings.TrimSpace(m.Content) != "" {
				input = append(input, messageItem("assistant", "output_text", m.Content))
			}
			for _, tc := range m.ToolCalls {
				input = append(input, functionCallItem(tc.ID, tc.Function.Name, tc.Function.Arguments))
			}
		case RoleTool:
			input = append(input, functionOutputItem(m.ToolCallID, m.Content))
		}
	}

	rt := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		rt = append(rt, responsesTool{
			Type: "function", Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}

	req := responsesRequest{
		Model:        c.cfg.Info.ID,
		Instructions: instructions,
		Input:        input,
		Tools:        rt,
		Include:      []string{"reasoning.encrypted_content"},
		Store:        false,
		Stream:       true,
	}
	if c.cfg.Info.Reasoning {
		req.Reasoning = &reasoningParam{Effort: reasoningEffort(c.cfg.Effort), Summary: "auto"}
	}
	return req
}

// reasoningEffort normalizes the effort to what the Codex backend accepts; it
// rejects "minimal", which is clamped to "low".
func reasoningEffort(e string) string {
	switch e {
	case "":
		return "medium"
	case "minimal":
		return "low"
	default:
		return e
	}
}

// providerState is the tagged container stashed in Message.ProviderState: the
// encrypted reasoning items plus the provider that produced them, so replay only
// ever happens back to the same provider (invariant 5).
type providerState struct {
	Provider string            `json:"provider"`
	Items    []json.RawMessage `json:"items"`
}

// replayReasoning returns the stored encrypted reasoning items as input items,
// but only when they were produced by provider; foreign or malformed state is
// dropped, never sent cross-provider.
func replayReasoning(state json.RawMessage, provider string) []json.RawMessage {
	if len(state) == 0 {
		return nil
	}
	var ps providerState
	if json.Unmarshal(state, &ps) != nil || ps.Provider != provider {
		return nil
	}
	return ps.Items
}

// The item builders marshal a fixed map of strings, which cannot fail, so the
// error is dropped.
func messageItem(role, contentType, text string) json.RawMessage {
	return messageItemWithImages(role, contentType, text, nil)
}

func messageItemWithImages(role, contentType, text string, images []Image) json.RawMessage {
	content := make([]map[string]any, 0, 1+len(images))
	if text != "" {
		content = append(content, map[string]any{"type": contentType, "text": text})
	}
	if role == "user" {
		for _, img := range images {
			content = append(content, map[string]any{
				"type":      "input_image",
				"image_url": "data:" + img.MediaType + ";base64," + img.Data,
			})
		}
	}
	b, _ := json.Marshal(map[string]any{
		"type": "message", "role": role,
		"content": content,
	})
	return b
}

func functionCallItem(callID, name, args string) json.RawMessage {
	if args == "" {
		args = "{}"
	}
	b, _ := json.Marshal(map[string]any{
		"type": "function_call", "call_id": callID, "name": name, "arguments": args,
	})
	return b
}

func functionOutputItem(callID, output string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type": "function_call_output", "call_id": callID, "output": output,
	})
	return b
}

// Stream sends a Responses request and assembles the streamed reply into the
// same Message/StreamEvent shape the agent consumes for chat-completions.
func (c *ResponsesClient) Stream(ctx context.Context, messages []Message, tools []Tool, _ float64,
	onEvent func(StreamEvent)) (Message, error) {
	buf, err := json.Marshal(c.encodeRequest(messages, tools))
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/codex/responses", bytes.NewReader(buf))
	if err != nil {
		return Message{}, err
	}
	tok, err := c.cfg.Token(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("responses: auth: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	if c.cfg.AccountID != "" {
		httpReq.Header.Set("chatgpt-account-id", c.cfg.AccountID)
	}
	httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
	httpReq.Header.Set("originator", "codex_cli_rs")
	httpReq.Header.Set("session_id", c.sessionID)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	// The subscription's quota headers ride on every response, including the
	// failures - a 429 is exactly when the percentage matters most.
	c.recordLimits(ParseLimits(resp.Header, c.cfg.Info.Provider, c.cfg.Info.ID, time.Now()))
	if resp.StatusCode != http.StatusOK {
		body := errBody(resp.Body)
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(body, "not supported") {
			body += " [the ChatGPT subscription supports only Codex models (" +
				strings.Join(CodexSubscriptionModels(), ", ") +
				"); switch with /model, or use an API key for other models]"
		}
		return Message{}, fmt.Errorf("responses: status %d: %s", resp.StatusCode, body)
	}
	msg, usage, err := parseResponsesStream(resp.Body, c.cfg.Info.Provider, onEvent)
	c.recordUsage(usage)
	return msg, err
}

// ---- SSE decoding ----

func readSSEData(r io.Reader, maxToken int) func(func(string, error) bool) {
	return func(yield func(string, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), maxToken)
		var data []string
		flush := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := strings.Join(data, "\n")
			data = nil
			return yield(payload, nil)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			field, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimPrefix(value, " ")
			if field == "data" {
				data = append(data, value)
			}
		}
		if err := scanner.Err(); err != nil {
			yield("", err)
			return
		}
		flush()
	}
}

type respEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Item        json.RawMessage `json:"item"`
	OutputIndex int             `json:"output_index"`
	Arguments   string          `json:"arguments"`
	Response    json.RawMessage `json:"response"`
}

type respItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func parseResponsesStream(r io.Reader, provider string, onEvent func(StreamEvent)) (Message, Usage, error) {
	out := Message{Role: RoleAssistant}
	var usage Usage
	calls := map[int]*ToolCall{} // keyed by output_index
	var order []int
	var reasoning []json.RawMessage // raw reasoning items, for next-turn replay
	var loop loopDetector

	for data, err := range readSSEData(r, 8*1024*1024) {
		if err != nil {
			return out, usage, fmt.Errorf("responses: read stream: %w", err)
		}
		if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var ev respEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return out, usage, fmt.Errorf("responses: decode event: %w", err)
		}
		var contentDelta string
		switch ev.Type {
		case "response.output_text.delta":
			out.Content += ev.Delta
			contentDelta = ev.Delta
			onEvent(StreamEvent{Content: ev.Delta})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			out.ReasoningContent += ev.Delta
			onEvent(StreamEvent{Reasoning: ev.Delta})
		case "response.output_item.added":
			it := decodeItem(ev.Item)
			if it.Type == "function_call" {
				c, ok := calls[ev.OutputIndex]
				if !ok {
					c = &ToolCall{Type: "function"}
					calls[ev.OutputIndex] = c
					order = append(order, ev.OutputIndex)
				}
				c.ID = firstNonEmptyStr(it.CallID, c.ID)
				if it.Name != "" {
					c.Function.Name = it.Name
				}
				c.Function.Arguments += it.Arguments
			}
		case "response.function_call_arguments.delta":
			if c, ok := calls[ev.OutputIndex]; ok {
				c.Function.Arguments += ev.Delta
			}
		case "response.output_item.done":
			it := decodeItem(ev.Item)
			switch it.Type {
			case "function_call":
				c, ok := calls[ev.OutputIndex]
				if !ok {
					c = &ToolCall{Type: "function"}
					calls[ev.OutputIndex] = c
					order = append(order, ev.OutputIndex)
				}
				c.ID = firstNonEmptyStr(it.CallID, c.ID)
				if it.Name != "" {
					c.Function.Name = it.Name
				}
				if it.Arguments != "" {
					c.Function.Arguments = it.Arguments // authoritative final args
				}
			case "reasoning":
				reasoning = append(reasoning, append(json.RawMessage(nil), ev.Item...))
			}
		case "response.completed":
			out.FinishReason = "stop"
			usage = decodeResponseUsage(ev.Response)
		case "response.failed", "response.error", "error":
			return out, usage, fmt.Errorf("responses: stream error: %s", data)
		}
		// Only final-answer text feeds the loop detector; reasoning summaries
		// legitimately repeat phrasing and must not trip it.
		if loop.feed(contentDelta) {
			out.FinishReason = "repetition"
			onEvent(StreamEvent{FinishReason: "repetition"})
			break
		}
	}

	for _, idx := range order {
		if calls[idx].Function.Arguments == "" {
			calls[idx].Function.Arguments = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, *calls[idx])
	}
	if len(reasoning) > 0 {
		if b, err := json.Marshal(providerState{Provider: provider, Items: reasoning}); err == nil {
			out.ProviderState = b
		}
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	onEvent(StreamEvent{FinishReason: out.FinishReason})
	return out, usage, nil
}

// decodeResponseUsage reads the token counts the terminal response.completed
// event carries. Its input_tokens already include the cached part.
func decodeResponseUsage(raw json.RawMessage) Usage {
	var r struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Usage{}
	}
	return Usage{
		InputTokens:  r.Usage.InputTokens,
		CachedTokens: r.Usage.InputTokensDetails.CachedTokens,
		OutputTokens: r.Usage.OutputTokens,
	}
}

func decodeItem(raw json.RawMessage) respItem {
	var it respItem
	_ = json.Unmarshal(raw, &it)
	return it
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// newUUID returns a random RFC-4122 v4 UUID string for the session_id header.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
