// Command mockmodel serves a scripted OpenAI-compatible endpoint so the demo
// recording is deterministic and needs no credentials.
//
// It replies to /v1/chat/completions with a fixed sequence: the first turn asks
// for a tool, the next turn answers. Which reply comes back is decided by what
// is already in the conversation, so a re-record produces the same GIF.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// step is one scripted assistant turn: either a tool call or a final answer.
type step struct {
	tool string // tool name, empty for a plain answer
	args string // tool arguments as JSON
	text string // answer text, streamed word by word
}

// script is played in order, one entry per assistant turn.
var script = []step{
	{tool: "list_dir", args: `{"path":"."}`},
	{tool: "grep", args: `{"pattern":"maxAttempts|backoff","path":"."}`},
	{tool: "read_file", args: `{"path":"retry.go"}`},
	{text: "Retries are bounded in two places.\n\n" +
		"**`maxAttempts`** (`retry.go`) caps the number of tries at 5. " +
		"`Flush` loops up to that and then gives up rather than retrying forever.\n\n" +
		"**`backoff(n)`** (`retry.go`) sets the delay before attempt *n*: it starts at " +
		"one second and doubles, but stops doubling once it reaches 30s.\n\n" +
		"| Attempt | Delay |\n" +
		"| --- | --- |\n" +
		"| 1 | 1s |\n" +
		"| 2 | 2s |\n" +
		"| 3 | 4s |\n" +
		"| 4 | 8s |\n" +
		"| 5 | 16s |\n\n" +
		"So a note is dropped after roughly 31 seconds of retrying.\n"},
}

type chatRequest struct {
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9411", "listen address")
	delay := flag.Duration("delay", 45*time.Millisecond, "pause between streamed words")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stream(w, script[min(assistantTurns(req), len(script)-1)], *delay)
	})

	log.Printf("mock model on http://%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// assistantTurns counts prior assistant messages, which is how far into the
// script the conversation already is.
func assistantTurns(req chatRequest) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func stream(w http.ResponseWriter, s step, delay time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	if s.tool != "" {
		send(chunk{Choices: []choice{{Delta: delta{ToolCalls: []toolCall{{
			Index: 0, ID: "call_" + s.tool, Type: "function",
			Function: toolFn{Name: s.tool, Arguments: s.args},
		}}}}}})
		send(chunk{Choices: []choice{{FinishReason: "tool_calls"}}})
	} else {
		// Word by word, so the recording shows real streaming rather than a
		// finished block appearing at once.
		for i, word := range strings.SplitAfter(s.text, " ") {
			if word == "" {
				continue
			}
			if i > 0 {
				time.Sleep(delay)
			}
			send(chunk{Choices: []choice{{Delta: delta{Content: word}}}})
		}
		send(chunk{Choices: []choice{{FinishReason: "stop"}}})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type chunk struct {
	Choices []choice `json:"choices"`
}

type choice struct {
	Delta        delta  `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type delta struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function toolFn `json:"function"`
}

type toolFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
