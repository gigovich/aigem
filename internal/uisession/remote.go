package uisession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/session"
)

// Remote is a session running in a daemon. It is the same Session a front-end
// drives locally, which is the whole reason the event stream was made the
// protocol rather than being wrapped in one: a terminal attaching to a
// conversation a browser started needs no second contract.
type Remote struct {
	base   string // http://host:port
	id     string
	token  string
	client *http.Client

	mu      sync.Mutex
	conn    net.Conn
	rw      io.ReadWriter // reads come from the buffer the handshake filled
	subs    map[string]*subscriber
	subSeq  uint64
	seq     uint64
	meta    session.Meta
	pending *Approval
	pendID  string
	closed  bool

	done chan struct{}
	// queue bounds a subscriber's backlog, mirroring the local session so a
	// front-end sees the same behaviour either side of the wire.
	queue int
}

var _ Session = (*Remote)(nil)

// Dial attaches to a session in a daemon and starts following its events. The
// caller subscribes as it would locally; the connection is re-established
// underneath, resuming from the last sequence number delivered.
func Dial(base, id, token string) (*Remote, error) {
	if base == "" || id == "" {
		return nil, errors.New("uisession: no daemon address or session id")
	}
	r := &Remote{
		base:   base,
		id:     id,
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
		subs:   map[string]*subscriber{},
		done:   make(chan struct{}),
		queue:  defaultRing,
	}
	// Fail fast on a daemon that is not there or does not know this session,
	// rather than reporting it as a reconnect that never succeeds.
	if err := r.refreshMeta(); err != nil {
		return nil, err
	}
	go r.follow()
	return r, nil
}

func (r *Remote) url(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("token", r.token)
	return r.base + path + "?" + q.Encode()
}

func (r *Remote) get(path string, q url.Values, out any) error {
	req, err := http.NewRequest(http.MethodGet, r.url(path, q), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, res.Status, body)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// refreshMeta reads the session list rather than a per-session endpoint,
// because that list is also how a caller discovers the session exists at all.
func (r *Remote) refreshMeta() error {
	var list []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Model string `json:"model"`
	}
	if err := r.get("/api/sessions", nil, &list); err != nil {
		return err
	}
	for _, s := range list {
		if s.ID == r.id {
			r.mu.Lock()
			// No id yet: a conversation gets one on its first turn, and the
			// session_meta event is what reports it.
			r.meta = session.Meta{Title: s.Title, Model: s.Model}
			r.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("no session %q on that daemon", r.id)
}

// ListSessions returns the ids a daemon is holding, newest first. It is the
// discovery step before Dial, kept here so a caller needs one package to attach.
func ListSessions(base, token string) ([]string, error) {
	r := &Remote{base: base, token: token, client: &http.Client{Timeout: 10 * time.Second}}
	var list []struct {
		ID string `json:"id"`
	}
	if err := r.get("/api/sessions", nil, &list); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out, nil
}

// Replay asks the daemon for the recorded history, which is the same journal a
// browser catches up from.
func (r *Remote) Replay(since uint64) ([]Event, error) {
	var out []Event
	q := url.Values{"since": []string{strconv.FormatUint(since, 10)}}
	if err := r.get("/api/sessions/"+r.id+"/events", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Subscribe attaches a local consumer to the stream this connection is
// following. The fan-out is the same one the local session uses, so a slow
// renderer is dropped with a resume marker here too rather than stalling the
// socket for everyone attached to it.
func (r *Remote) Subscribe(c Client, since uint64) (<-chan Event, func(), error) {
	backlog, err := r.Replay(since)
	if err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, ErrClosed
	}
	r.subSeq++
	if c.ID == "" {
		c.ID = "r-" + strconv.FormatUint(r.subSeq, 10)
	}
	// Anything the follower has already seen is in the backlog; start the live
	// tail above it so nothing is delivered twice.
	if n := len(backlog); n > 0 && backlog[n-1].Seq > r.seq {
		r.seq = backlog[n-1].Seq
	}
	s := newSubscriber(c, r.queue, backlog)
	r.subs[c.ID] = s
	r.mu.Unlock()

	go s.run()
	var once sync.Once
	return s.out, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, c.ID)
			r.mu.Unlock()
			s.stop()
		})
	}, nil
}

func (r *Remote) fanout(ev Event) {
	r.mu.Lock()
	if ev.Seq > r.seq {
		r.seq = ev.Seq
	}
	switch ev.Kind {
	case KindSessionMeta:
		// The conversation's own id, not the daemon's handle for it: Meta means
		// the same thing here as it does locally, and the handle is how this
		// client addressed the session rather than anything about it.
		r.meta = session.Meta{ID: ev.ID, Title: ev.Text, Model: ev.Name}
	case KindApprovalRequest:
		r.pendID, r.pending = ev.ID, ev.Approval
	case KindApprovalResolved:
		r.pendID, r.pending = "", nil
	}
	subs := make([]*subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		s.push(ev)
	}
}

// follow keeps a socket open, reconnecting from the last event delivered. A
// dropped connection is ordinary - a laptop lid, a tunnel - and losing the
// middle of a conversation to one is not, so the daemon replays the gap.
func (r *Remote) follow() {
	attempt := 0
	for {
		select {
		case <-r.done:
			return
		default:
		}
		if err := r.connect(); err != nil {
			// A daemon that has gone away is worth saying out loud once per
			// attempt rather than silently retrying forever in the dark.
			r.fanout(Event{Kind: KindNotice, Time: time.Now(),
				Text: "attach: " + err.Error()})
		}
		select {
		case <-r.done:
			return
		case <-time.After(backoff(attempt)):
		}
		attempt++
		if attempt > 6 {
			attempt = 6
		}
	}
}

func backoff(attempt int) time.Duration {
	d := 250 * time.Millisecond << attempt
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func (r *Remote) connect() error {
	r.mu.Lock()
	since := r.seq
	r.mu.Unlock()

	q := url.Values{
		"since": []string{strconv.FormatUint(since, 10)},
		"kind":  []string{"tui"},
	}
	wsURL := "ws" + r.url("/api/sessions/"+r.id+"/socket", q)[len("http"):]
	conn, br, _, err := ws.Dialer{}.Dial(context.Background(), wsURL)
	if err != nil {
		return err
	}
	var rw io.ReadWriter = conn
	if br != nil {
		// The dialer reads ahead: frames the daemon sent right after the upgrade
		// are already buffered, and reading from the connection instead loses the
		// start of the stream and resumes mid-frame.
		rw = readWriter{r: br, w: conn}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	r.conn, r.rw = conn, rw
	r.mu.Unlock()

	for {
		data, err := wsutil.ReadServerText(rw)
		if err != nil {
			r.mu.Lock()
			r.conn, r.rw = nil, nil
			r.mu.Unlock()
			_ = conn.Close()
			return nil // a closed socket is expected; follow reconnects
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Kind == KindDesync {
			// Resume from where delivery actually stopped, not from where the
			// daemon had got to.
			r.mu.Lock()
			r.seq = ev.From
			r.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		if ev.Kind == "" {
			continue // a per-request error reply, not something that happened
		}
		r.fanout(ev)
	}
}

type readWriter struct {
	r io.Reader
	w io.Writer
}

func (rw readWriter) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }

// connectWait bounds how long an op waits for a usable connection. Attaching
// and typing immediately is normal, and so is sending a moment after a drop -
// neither should surface as a failure when the follower is about to be back.
const connectWait = 3 * time.Second

func (r *Remote) waitConn() io.ReadWriter {
	deadline := time.Now().Add(connectWait)
	for {
		r.mu.Lock()
		rw, closed := r.rw, r.closed
		r.mu.Unlock()
		if rw != nil || closed || time.Now().After(deadline) {
			return rw
		}
		select {
		case <-r.done:
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (r *Remote) send(op map[string]any) error {
	rw := r.waitConn()
	if rw == nil {
		return errors.New("not connected to the daemon")
	}
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return wsutil.WriteClientMessage(rw, ws.OpText, b)
}

// Submit sends a message. Images are carried in the frame, as the daemon's own
// clients send them.
func (r *Remote) Submit(text string, images []llm.Image) error {
	op := map[string]any{"op": "submit", "text": text}
	if len(images) > 0 {
		op["images"] = images
	}
	return r.send(op)
}

func (r *Remote) Interrupt() { _ = r.send(map[string]any{"op": "interrupt"}) }

func (r *Remote) Command(name, args string) error {
	return r.send(map[string]any{"op": "command", "name": name, "args": args})
}

func (r *Remote) Resolve(id string, d Decision, by string) error {
	return r.send(map[string]any{"op": "resolve", "id": id, "decision": d, "label": by})
}

// Meta is the conversation's identity as last reported. It is read from the
// stream rather than fetched, so it costs nothing to ask.
func (r *Remote) Meta() session.Meta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta
}

// Pending returns the approval waiting for an answer, for a client that
// attached after it was asked.
func (r *Remote) Pending() (string, *Approval) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendID, r.pending
}

// Close detaches. The conversation keeps running in the daemon, which is the
// point of it living there.
func (r *Remote) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	conn := r.conn
	subs := r.subs
	r.subs = map[string]*subscriber{}
	r.mu.Unlock()

	close(r.done)
	if conn != nil {
		_ = conn.Close()
	}
	for _, s := range subs {
		s.stop()
	}
}
