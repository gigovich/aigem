package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// scriptedClient answers with one confirm-gated tool call, then prose, so a
// test can drive a whole turn - including the approval - over the socket.
type scriptedClient struct{ calls int }

func (s *scriptedClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	s.calls++
	if toolDefs != nil && s.calls == 1 {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function",
			Function: llm.FunctionCall{Name: "bash", Arguments: `{"cmd":"echo hi"}`},
		}}}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
}

func testServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv, err := New(Config{Factory: func(Spec) (*uisession.Local, error) {
		reg, err := tools.NewRegistry(t.TempDir())
		if err != nil {
			return nil, err
		}
		return uisession.New(uisession.Config{
			Tools: reg,
			NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
				return agent.New(&scriptedClient{}, reg, 0.3, confirm, "")
			},
			Ring: 256,
		}), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func (s *Server) get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr().String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (s *Server) newSession(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr().String()+"/api/sessions",
		bytes.NewReader([]byte(`{"cwd":"/tmp"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("create session: %s %s", res.Status, body)
	}
	var v View
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// wsClient is a dialled socket. It keeps the reader the handshake left behind
// as well as the connection: the dialer reads ahead, so frames the server sent
// immediately after the upgrade are already sitting in that buffer. Reading
// from the connection instead loses them, and picks the stream up mid-frame -
// which surfaces later as a missing prefix or a reserved opcode rather than as
// anything that points here.
type wsClient struct {
	conn net.Conn
	r    io.ReadWriter
}

// dial opens the socket the way a browser would: token in the query string,
// since browsers cannot set headers on a handshake.
func (s *Server) dial(t *testing.T, id string, since uint64, origin string) *wsClient {
	t.Helper()
	u := "ws://" + s.Addr().String() + "/api/sessions/" + id + "/socket" +
		"?token=" + s.token + "&since=" + itoa(since)
	d := ws.Dialer{}
	if origin != "" {
		d.Header = ws.HandshakeHeaderHTTP(http.Header{"Origin": []string{origin}})
	}
	conn, br, _, err := d.Dial(context.Background(), u)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &wsClient{conn: conn, r: conn}
	if br != nil {
		// Reads come from the buffer the handshake filled, writes still go to the
		// connection: the frame reader wants both on one value.
		c.r = readWriter{r: br, w: conn}
	}
	return c
}

// readWriter pairs the handshake's read-ahead buffer with the connection.
type readWriter struct {
	r io.Reader
	w io.Writer
}

func (rw readWriter) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }

func (c *wsClient) deadline(t *testing.T, d time.Duration) {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func send(t *testing.T, c *wsClient, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsutil.WriteClientText(c.conn, b); err != nil {
		t.Fatal(err)
	}
}

// await reads until an event of the given kind arrives, so a test names what it
// is waiting for rather than counting frames.
func await(t *testing.T, c *wsClient, kind uisession.Kind) uisession.Event {
	t.Helper()
	c.deadline(t, 5*time.Second)
	for {
		data, err := wsutil.ReadServerText(c.r)
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		var ev uisession.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		if ev.Kind == kind {
			return ev
		}
	}
}

// The whole point of the daemon: a client drives a turn end to end over one
// socket, including answering the approval that blocks it.
func TestSocketDrivesAWholeTurn(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	conn := srv.dial(t, id, 0, "")

	send(t, conn, map[string]any{"op": "submit", "text": "say hi"})

	if ev := await(t, conn, uisession.KindUserMessage); ev.Text != "say hi" {
		t.Fatalf("user message = %q", ev.Text)
	}
	// The call is announced before it asks: the agent publishes the start, then
	// parks on the confirmation.
	start := await(t, conn, uisession.KindToolStart)
	req := await(t, conn, uisession.KindApprovalRequest)
	if req.Approval == nil || req.Approval.Tool != "bash" {
		t.Fatalf("expected a bash approval, got %+v", req.Approval)
	}
	send(t, conn, map[string]any{"op": "resolve", "id": req.ID, "decision": "once", "label": "test"})

	if ev := await(t, conn, uisession.KindApprovalResolved); ev.By != "test" {
		t.Fatalf("resolution attributed to %q, want the label the client gave", ev.By)
	}
	end := await(t, conn, uisession.KindToolEnd)
	if start.ID == "" || end.ID != start.ID {
		t.Fatalf("tool call %q ended as %q; the pair must share an id", start.ID, end.ID)
	}
	if ev := await(t, conn, uisession.KindTurnEnd); ev.Error != "" || ev.Text != "all done" {
		t.Fatalf("turn end = %+v", ev)
	}
}

// A second client resuming from a sequence number sees exactly what the first
// one saw after that point - no gap, no repeat - which is what makes closing a
// laptop and opening a phone work.
func TestSecondConnectionResumesIdentically(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	first := srv.dial(t, id, 0, "")

	send(t, first, map[string]any{"op": "submit", "text": "hello"})
	req := await(t, first, uisession.KindApprovalRequest)
	send(t, first, map[string]any{"op": "resolve", "id": req.ID, "decision": "deny"})
	done := await(t, first, uisession.KindTurnEnd)

	// Everything from the top, on a connection that was not there for any of it.
	// The same history over HTTP, which has no socket timing in it: if these two
	// ever disagree, the replay is at fault rather than the test.
	res := srv.get(t, "/api/sessions/"+id+"/events?since=0")
	defer res.Body.Close()
	var replayed []uisession.Event
	if err := json.NewDecoder(res.Body).Decode(&replayed); err != nil {
		t.Fatal(err)
	}
	if len(replayed) == 0 || replayed[0].Seq != 1 {
		t.Fatalf("http replay starts at %+v, want seq 1", replayed)
	}
	for i, ev := range replayed {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("http replay is not contiguous at %d: %+v", i, ev)
		}
	}

	second := srv.dial(t, id, 0, "")
	var kinds, trace []string
	for {
		ev := await2(t, second)
		kinds = append(kinds, string(ev.Kind))
		trace = append(trace, string(ev.Kind)+"#"+itoa(ev.Seq))
		if ev.Seq >= done.Seq {
			break
		}
	}
	for _, want := range []string{"user_message", "turn_start", "approval_request", "turn_end"} {
		if !contains(kinds, want) {
			t.Fatalf("socket replay is missing %s; got %v", want, trace)
		}
	}

	// And resuming from the end yields nothing already seen.
	third := srv.dial(t, id, done.Seq, "")
	send(t, third, map[string]any{"op": "ping"})
	third.deadline(t, 500*time.Millisecond)
	for {
		data, err := wsutil.ReadServerText(third.r)
		if err != nil {
			break // the deadline: nothing replayed, which is the assertion
		}
		var ev uisession.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Seq <= done.Seq {
			t.Fatalf("event %d was replayed to a client that had already seen it", ev.Seq)
		}
		if ev.Kind != uisession.KindPresence {
			t.Fatalf("unexpected replay: %+v", ev)
		}
	}
}

func await2(t *testing.T, c *wsClient) uisession.Event {
	t.Helper()
	c.deadline(t, 5*time.Second)
	data, err := wsutil.ReadServerText(c.r)
	if err != nil {
		t.Fatal(err)
	}
	var ev uisession.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Behind this endpoint is an agent with bash and the credential store. Loopback
// keeps the network out but not browsers, so both gates are checked here rather
// than trusted to the bind address.
func TestUnauthorizedAndCrossOriginAreRefused(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	base := "http://" + srv.Addr().String()

	res, err := http.Get(base + "/api/sessions") //nolint:bodyclose // closed below
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %s, want 401", res.Status)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %s, want 401", res2.Status)
	}

	// A page on another origin, with a stolen token, is still refused.
	req3, _ := http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
	req3.Header.Set("Authorization", "Bearer "+srv.token)
	req3.Header.Set("Origin", "http://evil.example")
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %s, want 403", res3.Status)
	}

	// The same on the socket, which is the one that would matter.
	u := "ws://" + srv.Addr().String() + "/api/sessions/" + id + "/socket?token=" + srv.token
	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Origin": []string{"http://evil.example"},
	})}
	if _, _, _, err := d.Dial(context.Background(), u); err == nil {
		t.Fatal("a cross-origin websocket handshake was accepted")
	}
}

// A name that merely contains the one we answer to is a different name. This is
// the shape of a DNS rebinding attack, and a prefix or suffix test lets it in.
func TestHostMatchIsExact(t *testing.T) {
	srv := testServer(t)
	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{
		"127.0.0.1.evil.example:" + port,
		"evil.example:" + port,
		"localhost:1",
	} {
		if slices.Contains(srv.allowed.hosts, host) {
			t.Errorf("host %q was allowed", host)
		}
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		if !slices.Contains(srv.allowed.hosts, host) {
			t.Errorf("host %q was refused", host)
		}
	}
}

// A build with no assets says what to do instead of serving a blank page.
func TestNoAssetsExplainsItself(t *testing.T) {
	srv := testServer(t)
	res := srv.get(t, "/")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %s, want 501", res.Status)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "make web") {
		t.Fatalf("the message does not say how to get a UI: %q", body)
	}
}

// The session list and its lifecycle, which is what a multi-session UI is built
// on.
func TestSessionLifecycle(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)

	res := srv.get(t, "/api/sessions")
	defer res.Body.Close()
	var list []View
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].Cwd != "/tmp" {
		t.Fatalf("session list = %+v", list)
	}

	req, _ := http.NewRequest(http.MethodDelete, "http://"+srv.Addr().String()+"/api/sessions/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+srv.token)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %s, want 204", del.Status)
	}

	res2 := srv.get(t, "/api/sessions")
	defer res2.Body.Close()
	var after []View
	if err := json.NewDecoder(res2.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("session survived deletion: %+v", after)
	}
}

// MaxSessions is still honoured for a caller that wants one conversation at a
// time, but it is a choice now rather than the only safe setting.
func TestSessionCapIsHonoured(t *testing.T) {
	srv := testServer(t)
	srv.maxSessions = 1
	srv.newSession(t)

	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr().String()+"/api/sessions",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+srv.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second session: %s, want 409", res.Status)
	}
}

// Two conversations at once, each answering its own questions. The failure this
// guards against is not a crash: with a shared tool registry the delegation and
// skill tools carry whichever confirmation function was registered last, so a
// tool call in one conversation asks the other one's clients for approval - and
// the wrong person clicks Once.
func TestConcurrentSessionsAreIndependent(t *testing.T) {
	srv := testServer(t)
	a, b := srv.newSession(t), srv.newSession(t)
	if a == b {
		t.Fatal("the daemon reused a handle")
	}
	ca, cb := srv.dial(t, a, 0, ""), srv.dial(t, b, 0, "")

	send(t, ca, map[string]any{"op": "submit", "text": "in a"})
	reqA := await(t, ca, uisession.KindApprovalRequest)

	// b has been asked nothing, and must not be offered a's question.
	cb.deadline(t, 300*time.Millisecond)
	for {
		data, err := wsutil.ReadServerText(cb.r)
		if err != nil {
			break
		}
		var ev uisession.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == uisession.KindApprovalRequest {
			t.Fatal("one conversation's approval was offered to another")
		}
	}

	// Answering in a lets a run, and leaves b untouched.
	send(t, ca, map[string]any{"op": "resolve", "id": reqA.ID, "decision": "once", "label": "a"})
	if ev := await(t, ca, uisession.KindTurnEnd); ev.Error != "" {
		t.Fatalf("a's turn = %+v", ev)
	}

	send(t, cb, map[string]any{"op": "submit", "text": "in b"})
	reqB := await(t, cb, uisession.KindApprovalRequest)
	if reqB.ID == "" {
		t.Fatal("b never got its own question")
	}
	send(t, cb, map[string]any{"op": "resolve", "id": reqB.ID, "decision": "deny", "label": "b"})
	if ev := await(t, cb, uisession.KindTurnEnd); ev.Error != "" {
		t.Fatalf("b's turn = %+v", ev)
	}
}

// Shutting down must not hang on a client that is connected and idle. The
// websocket is hijacked, so the http server cannot close it; the write side
// ending has to take the read side with it.
func TestShutdownClosesIdleSockets(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	conn := srv.dial(t, id, 0, "")

	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an idle websocket")
	}
	conn.deadline(t, 3*time.Second)
	for {
		if _, err := wsutil.ReadServerText(conn.r); err != nil {
			return // the connection was closed from the server side, as it must be
		}
	}
}
