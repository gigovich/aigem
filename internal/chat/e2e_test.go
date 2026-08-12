package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// The whole point of stage two is that the fleet is drivable from a terminal
// before a single pixel is drawn. This walks the path the operator's CLI takes:
// open a thread, watch a bot work in it, and read the result back - all through
// the HTTP and websocket API, with nothing reaching into the store.
func TestOperatorCanDriveAThreadThroughTheAPIAlone(t *testing.T) {
	store, srv := testAPI(t)
	ctx := t.Context()

	// 1. Open a thread with a bot in it and say something.
	res := do(t, srv, http.MethodPost, "/api/chat/threads", map[string]any{
		"title": "refresh-token rotation", "participants": []string{amiran},
		"text": "the logout at 03:00 is back",
	})
	th := decode[ThreadView](t, res)

	// 2. Follow it, the way `aigem chat tail <thread>` does.
	conn, _, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/chat/socket")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	watch, err := json.Marshal(clientOp{Op: "watch", Thread: th.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := wsutil.WriteClientText(conn, watch); err != nil {
		t.Fatal(err)
	}

	// 3. The bot works: a turn, some steps, an answer. This is the only part
	//    that goes through the store, because it is what stage three will wire
	//    the transport to.
	go botTurn(ctx, t, store, th.ID)

	// 4. The operator sees the work and the answer, in order, over one socket.
	var sawWorking, sawTool, sawAnswer bool
	deadline := time.Now().Add(10 * time.Second)
	for (!sawWorking || !sawTool || !sawAnswer) && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch {
		case f.Stream == StreamThread && f.Thread != nil && f.Thread.Working:
			sawWorking = true
		case f.Stream == StreamEvent && strings.Contains(string(f.Event), "go test"):
			sawTool = true
		case f.Stream == StreamMessage && f.Message != nil &&
			strings.Contains(f.Message.Body, "auth.Refresh"):
			sawAnswer = true
		}
	}
	if !sawWorking || !sawTool || !sawAnswer {
		t.Fatalf("the operator saw working=%v tool=%v answer=%v; want all three",
			sawWorking, sawTool, sawAnswer)
	}

	// 5. And the thread reads back the same way over plain HTTP, which is what
	//    `aigem chat read` does.
	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID+"/messages", nil)
	msgs := decode[[]Message](t, res)
	if len(msgs) != 2 {
		t.Fatalf("the thread has %d messages, want the ask and the answer", len(msgs))
	}
	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID+"/timeline", nil)
	if frames := decode[[]Frame](t, res); len(frames) != 3 {
		t.Fatalf("the timeline has %d events, want the three the turn produced", len(frames))
	}
	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID+"/turns", nil)
	turns := decode[[]Turn](t, res)
	if len(turns) != 1 || turns[0].Usage.Calls != 1 {
		t.Fatalf("turns = %+v, want one with its spend recorded", turns)
	}
	if turns[0].Ended.IsZero() {
		t.Fatal("the turn is still open after the bot answered")
	}

	// 6. The thread is no longer working, and the operator is not owed anything.
	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID, nil)
	final := decode[ThreadView](t, res)
	if final.Working {
		t.Fatal("the thread still reports working")
	}
	if final.Unread != 1 {
		t.Fatalf("unread = %d, want the bot's one answer", final.Unread)
	}
}

// botTurn is what stage three's transport will do: open a turn, record the
// steps, answer, close the turn.
func botTurn(ctx context.Context, t *testing.T, s *Store, threadID string) {
	turn, err := s.BeginTurn(ctx, threadID, amiran)
	if err != nil {
		t.Error(err)
		return
	}
	for _, payload := range []string{
		`{"kind":"tool_start","name":"grep","args":{"pattern":"Refresh("}}`,
		`{"kind":"tool_start","name":"bash","args":{"cmd":"go test ./internal/auth"}}`,
		`{"kind":"tool_end","name":"bash","text":"FAIL"}`,
	} {
		if _, err := s.AppendEvent(ctx, EventRecord{
			Thread: threadID, Actor: amiran, TurnSeq: turn,
			Kind: "tool", Payload: []byte(payload),
		}); err != nil {
			t.Error(err)
			return
		}
	}
	if err := s.AddUsage(ctx, amiran, turn,
		Usage{InputTokens: 4210, OutputTokens: 380}, "grok-4.3"); err != nil {
		t.Error(err)
		return
	}
	if _, err := s.Say(ctx, threadID, Draft{
		Author: amiran,
		Body:   "Reproduced. `auth.Refresh` rotates before the old token is revoked.",
	}); err != nil {
		t.Error(err)
		return
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Error(err)
	}
}

// A daemon serving the chat needs no session factory, and the routes have to
// arrive under the same guard as everything else.
func TestTheAPIMountsIntoAServerWithNoSessions(t *testing.T) {
	s := newStore(t)
	hub := NewHub()
	_ = s.AddPublisher("hub", hub.Publish)

	var guarded int
	mux := http.NewServeMux()
	NewAPI(s, hub).Mount(mux, func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			guarded++
			h(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := do(t, srv, http.MethodGet, "/api/chat/threads", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mounted route answered %d", res.StatusCode)
	}
	if guarded != 1 {
		t.Fatalf("the guard ran %d times, want once per request", guarded)
	}
}
