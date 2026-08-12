// Package llm is a thin client for a llama.cpp OpenAI-compatible server.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content"`
	Images           []Image    `json:"images,omitempty"`
	ReasoningContent string     `json:"-"` // shown live, never sent back to the server
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	FinishReason     string     `json:"-"` // transient: why generation stopped
	// ProviderState is opaque, provider-specific state an adapter stashes on an
	// assistant message and replays on the next request (e.g. the Responses API's
	// encrypted reasoning items). It is ignored by the chat-completions path and
	// dropped on a cross-provider switch, so history stays provider-neutral.
	ProviderState json.RawMessage `json:"provider_state,omitempty"`
}

type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is an OpenAI-style function tool definition sent in a request.
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Client is the chat-completions transport. The same type serves the local
// llama.cpp server AND OpenAI-API-key: only the base URL, the optional bearer
// auth, extra headers, and whether a /tokenize endpoint exists differ.
type Client struct {
	BaseURL   string
	MaxTokens int // cap per response; 0 means no cap (server default)
	HTTP      *http.Client

	info        ModelInfo
	auth        func(context.Context) (string, error) // bearer token; nil = no Authorization header
	headers     map[string]string                     // extra request headers
	tokenizeURL string                                // POST endpoint for exact counts; "" => chars/4 estimate
	// noUsageOptIn latches once a server has rejected stream_options, so the
	// fallback costs one 400 per process rather than one per call.
	noUsageOptIn atomic.Bool
	usageTracker
}

// ClientConfig configures a chat-completions Client.
type ClientConfig struct {
	BaseURL string
	Info    ModelInfo
	// Auth returns the bearer token for the Authorization header (nil = none).
	Auth      func(context.Context) (string, error)
	Headers   map[string]string
	MaxTokens int
	// TokenizeURL, when set, is a POST endpoint returning {"tokens":[...]} for
	// exact counts (llama.cpp's /tokenize). Empty falls back to chars/4.
	TokenizeURL string
}

// NewClient builds a chat-completions Client from cfg.
func NewClient(cfg ClientConfig) *Client {
	return &Client{
		BaseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		MaxTokens:   cfg.MaxTokens,
		HTTP:        sharedHTTPClient(),
		info:        cfg.Info,
		auth:        cfg.Auth,
		headers:     cfg.Headers,
		tokenizeURL: cfg.TokenizeURL,
	}
}

// New builds a client for a local llama.cpp server (no auth, /tokenize enabled).
func New(baseURL, model string) *Client {
	base := strings.TrimRight(baseURL, "/")
	return NewClient(ClientConfig{
		BaseURL:     base,
		Info:        ModelInfo{Provider: "local", ID: model},
		TokenizeURL: base + "/tokenize",
	})
}

// Model returns the client's model metadata.
func (c *Client) Model() ModelInfo { return c.info }

// Endpoint returns the base URL this client calls.
func (c *Client) Endpoint() string { return c.BaseURL }

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []Tool         `json:"tools,omitempty"`
	Temperature   float64        `json:"temperature"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions asks for a final usage-only chunk. Without it a streamed
// chat-completions response reports no token counts at all.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       Role       `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// wireMessages converts our provider-neutral messages to OpenAI chat-completions
// wire shape. ProviderState is deliberately not represented here, and user turns
// with images use multimodal content parts instead of the legacy string content.
func wireMessages(msgs []Message) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.Images) > 0 && m.Role == RoleUser {
			cm.Content = chatContentParts(m.Content, m.Images)
		}
		out = append(out, cm)
	}
	return out
}

func chatContentParts(text string, images []Image) []map[string]any {
	parts := make([]map[string]any, 0, 1+len(images))
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, img := range images {
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + img.MediaType + ";base64," + img.Data,
			},
		})
	}
	return parts
}

// StreamEvent is one incremental update emitted while streaming a response.
type StreamEvent struct {
	Content      string // delta of the final answer
	Reasoning    string // delta of reasoning_content
	FinishReason string // set on the terminating event
}

// Stream sends a chat request and invokes onEvent for each delta. It returns the
// fully assembled assistant message (content, reasoning, tool calls).
func (c *Client) Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
	onEvent func(StreamEvent)) (Message, error) {
	msg, err := c.stream(ctx, messages, tools, temperature, onEvent)
	if errors.Is(err, errUsageOptInRejected) {
		// The opt-in is now off for this client, so the retry cannot loop.
		return c.stream(ctx, messages, tools, temperature, onEvent)
	}
	return msg, err
}

func (c *Client) stream(
	ctx context.Context,
	messages []Message,
	tools []Tool,
	temperature float64,
	onEvent func(StreamEvent),
) (Message, error) {
	if c.info.Temperature != nil {
		temperature = *c.info.Temperature
	}
	reqBody := chatRequest{
		Model:       c.info.ID,
		Messages:    wireMessages(messages),
		Tools:       tools,
		Temperature: temperature,
		MaxTokens:   c.MaxTokens,
		Stream:      true,
	}
	if !c.noUsageOptIn.Load() {
		reqBody.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(buf),
	)
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	if c.auth != nil {
		tok, err := c.auth(ctx)
		if err != nil {
			return Message{}, fmt.Errorf("llm: auth: %w", err)
		}
		if tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	// Quota headers ride on every response, including the failures - a 429 is
	// exactly when the remaining-tokens count matters most.
	c.recordLimits(ParseLimits(resp.Header, c.info.Provider, c.info.ID, time.Now()))
	if resp.StatusCode != http.StatusOK {
		body := errBody(resp.Body)
		// A strict OpenAI-compatible gateway may reject the usage opt-in as an
		// unknown argument. Losing token counts is a fair price for still working;
		// failing every call is not, and the error would be classified terminal.
		// The caller retries after this returns, so the rejected response is closed
		// first rather than held open for the length of the next stream.
		if resp.StatusCode == http.StatusBadRequest && reqBody.StreamOptions != nil &&
			strings.Contains(body, "stream_options") {
			c.noUsageOptIn.Store(true)
			return Message{}, errUsageOptInRejected
		}
		return Message{}, fmt.Errorf("llm: status %d: %s", resp.StatusCode, body)
	}

	msg, usage, err := parseStream(resp.Body, onEvent)
	c.recordUsage(ctx, usage)
	return msg, err
}

// errUsageOptInRejected signals that the only thing wrong with the request was
// the usage opt-in, which the client has now stopped sending.
var errUsageOptInRejected = errors.New("llm: server rejected stream_options")

// Tokenize returns the number of tokens text encodes to, via llama-server's
// POST /tokenize endpoint. It is used at the compaction decision point where an
// accurate count matters more than the cheap chars/4 estimate. Backends without
// such an endpoint (OpenAI) fall back to the chars/4 estimate.
func (c *Client) Tokenize(ctx context.Context, text string) (int, error) {
	if c.tokenizeURL == "" {
		return len(text) / 4, nil
	}
	buf, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: text})
	if err != nil {
		return 0, err
	}
	// Tokenizing is a tiny, frequent call; cap it well under the client's
	// generation timeout so a slow server cannot stall the compaction decision.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.tokenizeURL, bytes.NewReader(buf),
	)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("llm: tokenize status %d: %s", resp.StatusCode, errBody(resp.Body))
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("llm: decode tokenize: %w", err)
	}
	return len(out.Tokens), nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseStream(r io.Reader, onEvent func(StreamEvent)) (Message, Usage, error) {
	out := Message{Role: RoleAssistant}
	var usage Usage
	// tool calls arrive fragmented across chunks, keyed by index.
	calls := map[int]*ToolCall{}
	var order []int
	var loop loopDetector

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return out, usage, fmt.Errorf("llm: decode chunk: %w", err)
		}
		// The usage chunk is the one with no choices, so it must be read before
		// the empty-choices skip below.
		if chunk.Usage != nil {
			usage = Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				CachedTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" {
			out.Content += ch.Delta.Content
		}
		if ch.Delta.ReasoningContent != "" {
			out.ReasoningContent += ch.Delta.ReasoningContent
		}
		for _, tc := range ch.Delta.ToolCalls {
			c, ok := calls[tc.Index]
			if !ok {
				c = &ToolCall{Type: "function"}
				calls[tc.Index] = c
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				c.ID = tc.ID
			}
			if tc.Type != "" {
				c.Type = tc.Type
			}
			if tc.Function.Name != "" {
				c.Function.Name = tc.Function.Name
			}
			c.Function.Arguments += tc.Function.Arguments
		}
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" {
			onEvent(StreamEvent{Content: ch.Delta.Content, Reasoning: ch.Delta.ReasoningContent})
		}
		// Abort early if the model is emitting the same line over and over.
		if loop.feed(ch.Delta.Content) || loop.feed(ch.Delta.ReasoningContent) {
			out.FinishReason = "repetition"
			onEvent(StreamEvent{FinishReason: "repetition"})
			break
		}
		if ch.FinishReason != "" {
			out.FinishReason = ch.FinishReason
			onEvent(StreamEvent{FinishReason: ch.FinishReason})
		}
	}
	if err := scanner.Err(); err != nil {
		return out, usage, fmt.Errorf("llm: read stream: %w", err)
	}

	for _, idx := range order {
		out.ToolCalls = append(out.ToolCalls, *calls[idx])
	}
	return out, usage, nil
}

// loopDetector flags runaway generation where the model repeats the same
// non-empty line many times in a row.
type loopDetector struct {
	cur     strings.Builder
	last    string
	count   int
	tripped bool
}

const repeatLineLimit = 6

// feed consumes a streamed text delta and reports true once the same line has
// repeated repeatLineLimit times consecutively.
func (d *loopDetector) feed(s string) bool {
	if d.tripped || s == "" {
		return d.tripped
	}
	for _, r := range s {
		if r != '\n' {
			d.cur.WriteRune(r)
			continue
		}
		line := strings.TrimSpace(d.cur.String())
		d.cur.Reset()
		if line == "" {
			continue
		}
		if line == d.last {
			d.count++
		} else {
			d.last = line
			d.count = 1
		}
		if d.count >= repeatLineLimit {
			d.tripped = true
			return true
		}
	}
	return false
}
