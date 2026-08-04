package mattermost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/llm"
)

// Transport implements bot.Transport over a Mattermost event WebSocket plus the REST client.
type Transport struct {
	client    *Client
	botUserID string
	serverURL string
	token     string
	conn      net.Conn
	rw        io.ReadWriter // buffered reader from ws.Dial paired with conn for writes
	// connectFn (re)establishes and authenticates the WebSocket, swapping conn/rw on success.
	// Defaults to (*Transport).connect; overridable in tests.
	connectFn func(ctx context.Context) error
	events    chan bot.Inbound
	done      chan struct{}

	mu          sync.Mutex
	threads     map[string]bool        // root_ids the bot owns or has participated in
	threadOrder []string               // insertion order of threads, for bounded FIFO eviction
	threadSink  func(uint64, []string) // persists the tracked set whenever it grows
	threadSeq   uint64                 // increments per tracked thread; orders concurrent saves
	buffer      *channelBuffer
	debounce    *threadDebouncer
	usernames   map[string]string // user id -> username cache, guarded by mu
	closed      bool

	writeMu sync.Mutex   // serializes WebSocket client writes
	seq     atomic.Int64 // client message seq (auth uses 1)

	// log carries the bot's name. Every bot in the process has its own websocket,
	// and a reconnect line that does not say whose is unreadable in a shared log.
	log *slog.Logger
}

// recoverPanic contains a panic in one of the transport's own goroutines. The bot's event stream
// ends, which its supervisor treats as a stopped bot and restarts - the other bots in the process
// are untouched.
func (t *Transport) recoverPanic(where string) {
	if p := recover(); p != nil {
		t.logger().Error("transport panicked", "where", where, "panic", p, "stack", string(debug.Stack()))
	}
}

// logger is the bot's logger, or the default when none was wired in (tests build
// the transport directly).
func (t *Transport) logger() *slog.Logger {
	if t.log == nil {
		return slog.Default()
	}
	return t.log
}

// readWriter pairs a reader and a writer into an io.ReadWriter.
type readWriter struct {
	io.Reader
	io.Writer
}

// Dial connects to the Mattermost WebSocket, authenticates with the token, and starts
// streaming classified events. The read loop reconnects on its own if the connection later
// drops, so a transient blip or a server restart does not kill the bot.
func Dial(ctx context.Context, serverURL, token, botUserID string, log *slog.Logger) (*Transport, error) {
	if log == nil {
		log = slog.Default()
	}
	t := &Transport{
		log:       log,
		client:    NewClient(serverURL, token),
		botUserID: botUserID,
		serverURL: serverURL,
		token:     token,
		events:    make(chan bot.Inbound, 16),
		done:      make(chan struct{}),
		threads:   map[string]bool{},
		buffer:    newChannelBuffer(100, 24*time.Hour),
		usernames: map[string]string{},
	}
	t.debounce = newThreadDebouncer(threadQuietPeriod, t.fireThreadUpdate)
	t.connectFn = t.connect
	if err := t.connectFn(ctx); err != nil {
		return nil, err
	}
	go t.run(ctx)
	go t.keepalive(ctx)
	return t, nil
}

// wsURL derives the WebSocket endpoint from the REST server URL.
func wsURL(serverURL string) string {
	return strings.Replace(strings.TrimRight(serverURL, "/"), "http", "ws", 1) + "/api/v4/websocket"
}

// connect dials the WebSocket, swaps in the new conn/rw, and authenticates. On any failure it
// closes the dialed conn and returns the error so the caller can retry.
func (t *Transport) connect(ctx context.Context) error {
	conn, br, _, err := ws.Dial(ctx, wsURL(t.serverURL))
	if err != nil {
		return err
	}
	// ws.Dial may return a bufio.Reader holding bytes already read off the socket; read from
	// it when present so no frames are missed. ReadServerText requires io.ReadWriter, so pair
	// the buffered reader with the conn (for control-frame writes).
	var rw io.ReadWriter = conn
	if br != nil {
		rw = readWriter{br, conn}
	}
	t.writeMu.Lock()
	t.conn = conn
	t.rw = rw
	t.seq.Store(1)
	t.writeMu.Unlock()

	authMsg := map[string]any{"action": "authentication_challenge", "seq": 1,
		"data": map[string]string{"token": t.token}}
	data, err := json.Marshal(authMsg)
	if err != nil {
		conn.Close()
		return err
	}
	if err = wsutil.WriteClientText(conn, data); err != nil {
		conn.Close()
		return err
	}
	if err = t.awaitAuth(authTimeout); err != nil {
		conn.Close()
		return err
	}
	return nil
}

const authTimeout = 10 * time.Second

// errTokenRejected marks an authentication failure caused by a bad token, as opposed to a
// transient network error. reconnect logs it loudly because retrying will not help until the
// token is fixed.
var errTokenRejected = errors.New("mattermost rejected the bot token")

// readServerText reads the next text message, mirroring wsutil.ReadServerText but writing the
// automatic Pong reply to a server Ping under writeMu. Without this, that Pong would race the
// keepalive ping and Typing writes on the same conn (two goroutines writing interleaved frames).
func (t *Transport) readServerText() ([]byte, error) {
	rw := t.rw
	base := wsutil.ControlFrameHandler(rw, ws.StateClientSide)
	guarded := func(h ws.Header, r io.Reader) error {
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
		return base(h, r)
	}
	rd := wsutil.Reader{Source: rw, State: ws.StateClientSide, CheckUTF8: true, OnIntermediate: guarded}
	for {
		hdr, err := rd.NextFrame()
		if err != nil {
			return nil, err
		}
		if hdr.OpCode.IsControl() {
			if err := guarded(hdr, &rd); err != nil {
				return nil, err
			}
			continue
		}
		if hdr.OpCode&ws.OpText == 0 {
			if err := rd.Discard(); err != nil {
				return nil, err
			}
			continue
		}
		return io.ReadAll(&rd)
	}
}

// awaitAuth blocks until Mattermost confirms the authentication_challenge, returning an error
// if the server rejects the token or drops the connection first. Mattermost authenticates the
// socket lazily: a dead token is only reported as a FAIL frame the readLoop would discard, so
// without this gate the bot would run mute and only REST calls would reveal the expired session.
func (t *Transport) awaitAuth(timeout time.Duration) error {
	if t.conn != nil {
		_ = t.conn.SetReadDeadline(time.Now().Add(timeout))
		defer func() { _ = t.conn.SetReadDeadline(time.Time{}) }()
	}
	for {
		data, err := t.readServerText()
		if err != nil {
			return fmt.Errorf("mattermost websocket authentication: %w", err)
		}
		ok, aerr := parseAuthFrame(data)
		if aerr != nil {
			return aerr
		}
		if ok {
			return nil
		}
	}
}

// parseAuthFrame inspects one pre-auth WebSocket frame. ok is true once the server confirms
// authentication (an OK response or the hello event); err is set when the server rejects the
// token. A frame that is neither leaves both zero so the caller keeps reading.
func parseAuthFrame(data []byte) (ok bool, err error) {
	var r struct {
		Status string `json:"status"`
		Event  string `json:"event"`
		Error  *struct {
			Message    string `json:"message"`
			StatusCode int    `json:"status_code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &r) != nil {
		return false, nil
	}
	if r.Status == "FAIL" {
		if r.Error != nil && r.Error.Message != "" {
			return false, fmt.Errorf("%w: %s (status %d)",
				errTokenRejected, r.Error.Message, r.Error.StatusCode)
		}
		return false, errTokenRejected
	}
	if r.Status == "OK" || r.Event == "hello" {
		return true, nil
	}
	return false, nil
}

// pingInterval keeps an idle WebSocket alive so the server or an intervening proxy does not
// reap it. maxBackoff caps the reconnect backoff.
const (
	pingInterval = 20 * time.Second // under typical proxy idle timeouts (~60s) with headroom
	maxBackoff   = 30 * time.Second
)

// run consumes frames and reconnects when the connection drops, instead of exiting. It closes
// events (which ends Runtime.Serve and the process) only when Close is called or ctx is done -
// so a network blip or a Mattermost restart no longer silently kills the bot. Note: Mattermost
// does not replay posts missed while disconnected, so a message that arrives during the gap is
// lost; a user who needs an answer can re-send once the bot is back.
func (t *Transport) run(ctx context.Context) {
	// Bots share a process now, so a panic in this goroutine - which parses whatever the chat
	// server sends - would take the whole team down. Contain it here, where it can be recovered,
	// and let the loop end so the supervisor restarts this bot alone.
	defer t.recoverPanic("websocket read loop")
	// Stop the debouncer before closing events, so a thread timer firing during shutdown cannot
	// send on a closed channel. This covers the ctx-cancel path where run returns on its own
	// (Close did not drive it); stop is idempotent, so Close calling it too is harmless.
	defer func() {
		t.debounce.stop()
		close(t.events)
	}()
	for {
		t.consume()
		if t.stopping(ctx) {
			return
		}
		t.logger().Warn("mattermost websocket dropped; reconnecting")
		if !t.reconnect(ctx) {
			return
		}
		t.logger().Info("mattermost websocket reconnected")
	}
}

// consume reads frames into dispatch until the current connection errors.
func (t *Transport) consume() {
	for {
		data, err := t.readServerText()
		if err != nil {
			return
		}
		t.dispatch(data)
	}
}

// reconnect re-dials with capped exponential backoff until it succeeds, returning false if the
// transport is shutting down (Close called or ctx done) before a connection is restored.
func (t *Transport) reconnect(ctx context.Context) bool {
	backoff := time.Second
	for {
		if t.stopping(ctx) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.done:
			return false
		case <-time.After(backoff):
		}
		if err := t.connectFn(ctx); err != nil {
			if errors.Is(err, errTokenRejected) {
				// Retrying will not help until the token is fixed; make it loud, not a quiet WARN.
				t.logger().Error("mattermost rejected the bot token; reconnect will keep retrying",
					"err", err, "retry_in", backoff)
			} else {
				t.logger().Warn("mattermost reconnect failed", "err", err, "retry_in", backoff)
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		return true
	}
}

// stopping reports whether the transport is shutting down.
func (t *Transport) stopping(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// keepalive sends a periodic ping so an idle connection is not closed by the server or a proxy.
// A failed ping is harmless: consume detects the dead conn and run reconnects.
func (t *Transport) keepalive(ctx context.Context) {
	defer t.recoverPanic("websocket keepalive")
	tick := time.NewTicker(pingInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.done:
			return
		case <-tick.C:
			t.writeMu.Lock()
			err := wsutil.WriteClientMessage(t.conn, ws.OpPing, nil)
			t.writeMu.Unlock()
			if err != nil {
				t.logger().Debug("mattermost websocket ping failed", "err", err)
			}
		}
	}
}

// threadQuietPeriod is how long a thread the bot is in must be silent before its accumulated
// replies are delivered as a single thread_update. Long enough that several bots replying in
// quick succession produce one consolidated reaction, not one per message.
const threadQuietPeriod = 45 * time.Second

// dispatch parses one WebSocket frame and routes it: an addressed message (DM, mention,
// broadcast) is forwarded immediately; a plain reply in a thread the bot is in is handed to the
// debouncer so the owner reacts once the thread goes quiet; anything else is buffered as
// ambient channel chatter.
func (t *Transport) dispatch(data []byte) {
	var e wsEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return
	}
	p, ct, ok := parsePost(e)
	if !ok || p.UserID == t.botUserID {
		return
	}
	if in, actionable := classifyPost(p, ct, e.Data["mentions"], t.botUserID); actionable {
		select {
		case t.events <- in:
		case <-t.done:
		}
		return
	}
	if ct != "D" && p.RootID != "" && t.inThread(p.RootID) {
		t.debounce.note(bot.ThreadRef{ChannelID: p.ChannelID, RootID: p.RootID})
		return
	}
	if ct != "D" {
		t.buffer.add(p.ChannelID, p.UserID, p.Message)
	}
}

// fireThreadUpdate delivers a single thread_update once a thread the bot is in has gone quiet.
// It carries no text; the runtime fetches the full thread via ThreadHistory to decide what to do.
func (t *Transport) fireThreadUpdate(ref bot.ThreadRef) {
	select {
	case t.events <- bot.Inbound{Kind: "thread_update", Channel: ref.ChannelID, Thread: ref}:
	case <-t.done:
	}
}

func (t *Transport) inThread(rootID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.threads[rootID]
}

// Events returns the channel of inbound messages.
func (t *Transport) Events() <-chan bot.Inbound { return t.events }

// maxTrackedThreads bounds the set of threads the bot observes. A long-lived bot would otherwise
// accumulate one entry per thread it ever touched; evicting the oldest just means a very old
// thread falls back to mention-only, which is fine.
const maxTrackedThreads = 500

// trackThread records rootID as a thread the bot is in, evicting the oldest once over the cap.
// It reports whether the set changed. Caller holds t.mu.
func (t *Transport) trackThread(rootID string) bool {
	if t.threads[rootID] {
		return false
	}
	t.threads[rootID] = true
	t.threadOrder = append(t.threadOrder, rootID)
	if len(t.threadOrder) > maxTrackedThreads {
		oldest := t.threadOrder[0]
		t.threadOrder = t.threadOrder[1:]
		delete(t.threads, oldest)
	}
	return true
}

// noteThread tracks a thread and hands the new set to the sink, so the bot's thread membership
// survives a restart. Without persistence every thread the bot was following went mention-only
// the moment the process restarted, which looked exactly like the bot ignoring its own threads.
// The sink runs outside t.mu because it writes to disk.
func (t *Transport) noteThread(rootID string) {
	if rootID == "" {
		return
	}
	t.mu.Lock()
	changed := t.trackThread(rootID)
	var (
		sink func(uint64, []string)
		ids  []string
		seq  uint64
	)
	if changed && t.threadSink != nil {
		t.threadSeq++
		sink, seq = t.threadSink, t.threadSeq
		ids = append(ids, t.threadOrder...)
	}
	t.mu.Unlock()
	if sink != nil {
		sink(seq, ids)
	}
}

// SeedThreads restores a previously persisted set of tracked threads. Call it before serving;
// it does not notify the sink, since nothing new was learned.
func (t *Transport) SeedThreads(ids []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		t.trackThread(id)
	}
}

// SetThreadSink installs the callback that persists the tracked thread set. Call it before
// serving. The callback may be invoked concurrently, so it is handed a monotonic version with each
// snapshot and must ignore a version it has already superseded.
func (t *Transport) SetThreadSink(fn func(version uint64, ids []string)) {
	t.mu.Lock()
	t.threadSink = fn
	t.mu.Unlock()
}

// maxPostChars caps a single Mattermost post. The server stores a post's Message in a column that
// holds at most 16383 characters (the MaxPostSize setting); a post above the server limit is
// rejected outright, so long replies are split into several posts under this bound instead of being
// silently dropped. Kept comfortably below the hard cap to tolerate stricter server configs.
const maxPostChars = 16000

// splitForPost breaks text into chunks of at most limit runes, preferring line boundaries so
// markdown stays intact; a single over-long line is hard-split by rune count. Joining the chunks
// reproduces text exactly, because SplitAfter keeps each line's trailing newline.
func splitForPost(text string, limit int) []string {
	if limit <= 0 {
		limit = maxPostChars
	}
	if len([]rune(text)) <= limit {
		return []string{text}
	}
	var chunks []string
	var b strings.Builder
	cur := 0 // runes buffered in b
	flush := func() {
		if b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
			cur = 0
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		lr := []rune(line)
		if len(lr) > limit {
			flush()
			for len(lr) > limit {
				chunks = append(chunks, string(lr[:limit]))
				lr = lr[limit:]
			}
			if len(lr) > 0 {
				b.WriteString(string(lr))
				cur = len(lr)
			}
			continue
		}
		if cur+len(lr) > limit {
			flush()
		}
		b.WriteString(line)
		cur += len(lr)
	}
	flush()
	return chunks
}

// Reply posts text into the given thread and remembers the thread so later replies in it are
// observed (debounced) even when they do not mention the bot. A reply longer than one post is
// split into several posts chained under the same thread root.
func (t *Transport) Reply(thread bot.ThreadRef, text string) error {
	t.noteThread(thread.RootID)
	_, err := t.postChunks(thread.ChannelID, thread.RootID, text, false)
	return err
}

// postChunks writes text as one or more posts, chaining every chunk after the
// first under the first one, and returns the id of that first post. noteNewRoot
// records a newly opened thread as one the bot owns; a reply into an existing
// thread does not need it, because the caller already noted that thread.
func (t *Transport) postChunks(channelID, rootID, text string, noteNewRoot bool) (string, error) {
	ids, err := t.postChunkIDs(channelID, rootID, text, noteNewRoot)
	if len(ids) == 0 {
		return "", err
	}
	return ids[0], err
}

// postChunkIDs is postChunks, reporting every post it created rather than only the first.
func (t *Transport) postChunkIDs(channelID, rootID, text string, noteNewRoot bool) ([]string, error) {
	var ids []string
	chain := rootID
	for _, chunk := range splitForPost(text, maxPostChars) {
		id, err := t.client.CreatePost(context.Background(), channelID, chain, chunk)
		if err != nil {
			return ids, err
		}
		if id != "" {
			ids = append(ids, id)
		}
		if chain == "" && id != "" {
			chain = id // chain the remaining chunks under the first post
			if noteNewRoot {
				t.noteThread(id)
			}
		}
	}
	return ids, nil
}

// PostWithIDs posts like Post or PostToThread and returns the ids of every post it wrote - one
// per chunk, since a long message is split. The fleet needs all of them: the same message is also
// delivered to the teammate in-process, and each id is what lets them recognise the chat copy of
// something they already acted on. Missing the ids of chunks 2..n would wake them again on a
// partial copy.
func (t *Transport) PostWithIDs(channelID, rootID, text string) ([]string, error) {
	if rootID != "" {
		t.noteThread(rootID)
		return t.postChunkIDs(channelID, rootID, text, false)
	}
	return t.postChunkIDs(channelID, "", text, true)
}

// Post sends text to a channel id at root level and remembers the new post as a thread the bot
// owns, so replies to it are observed even without an @mention - a scheduled run that posts a
// status still hears the answers it asked for.
func (t *Transport) Post(channelID, text string) error {
	_, err := t.postChunks(channelID, "", text, true)
	return err
}

// PostToThread posts text as a reply into an existing thread (rootID) of a channel, chunking and
// tracking it exactly like Reply. It lets a scheduled or deferred run deliver its result back into
// the thread the work was requested in, instead of starting a new one.
func (t *Transport) PostToThread(channelID, rootID, text string) error {
	return t.Reply(bot.ThreadRef{ChannelID: channelID, RootID: rootID}, text)
}

// Close shuts the WebSocket down.
func (t *Transport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	t.mu.Unlock()
	t.debounce.stop()
	t.writeMu.Lock()
	err := t.conn.Close()
	t.writeMu.Unlock()
	return err
}

// History returns a human-readable block of recent buffered messages for a channel,
// resolving and caching author usernames, or "" if the channel has no recent chatter.
func (t *Transport) History(ctx context.Context, channelID string) string {
	msgs := t.buffer.recent(channelID)
	if len(msgs) == 0 {
		return ""
	}
	authors := make([]string, len(msgs))
	texts := make([]string, len(msgs))
	for i, m := range msgs {
		authors[i], texts[i] = m.author, m.text
	}
	return t.formatAuthored(ctx, authors, texts)
}

// maxDigestChars bounds a channel digest handed to the read_chat tool. Smaller than a thread
// budget: a digest is an index the reader uses to pick a thread, not the content itself.
const maxDigestChars = 20000

// ChannelDigest returns a channel's recent posts as one readable block. A post is tagged with its
// thread id wherever the thread changes, so a reader can pick a root id to open next without the
// id being repeated down every line of one conversation. Posts that do not fit the budget are
// dropped before formatting rather than trimmed after, because trimming the text could cut away
// the tag a surviving post depends on.
func (t *Transport) ChannelDigest(ctx context.Context, channelID string, limit int) (string, error) {
	posts, err := t.client.ChannelPosts(ctx, channelID, limit)
	if err != nil {
		return "", err
	}
	if len(posts) == 0 {
		return "", nil
	}
	posts, elided := newestWithinBudget(posts, maxDigestChars)
	authors := make([]string, len(posts))
	texts := make([]string, len(posts))
	prev := ""
	for i, p := range posts {
		root := p.RootID
		if root == "" {
			root = p.ID // a top-level post is the root of its own (possibly empty) thread
		}
		authors[i] = p.UserID
		// Tag only where the thread changes: repeating a 26-char id on every consecutive post in
		// the same thread would eat the digest budget without telling the reader anything new.
		if root == prev {
			texts[i] = p.Message
		} else {
			texts[i] = "[thread " + root + "] " + p.Message
			prev = root
		}
	}
	block := t.formatAuthored(ctx, authors, texts)
	if elided {
		block = channelElidedMarker + block
	}
	return block, nil
}

// newestWithinBudget keeps the most recent posts whose messages fit the rune budget, reporting
// whether anything older was dropped. The budget is approximate: it counts message text, not the
// usernames and thread tags formatting adds.
func newestWithinBudget(posts []ChannelPost, budget int) ([]ChannelPost, bool) {
	total := 0
	for i := len(posts) - 1; i >= 0; i-- {
		total += len([]rune(posts[i].Message))
		if total > budget {
			return posts[i+1:], true
		}
	}
	return posts, false
}

// ThreadText returns a whole thread as a readable block. Unlike ThreadHistory it reports a fetch
// failure, because a reader that explicitly asked for a thread must not be handed silence. When
// channelID is set the thread must actually live in that channel: the root id can come from the
// model, and channel membership is the only boundary on what a bot may read.
func (t *Transport) ThreadText(ctx context.Context, channelID, rootID string) (string, error) {
	return t.threadBlock(ctx, channelID, rootID, false)
}

// threadBlock fetches and formats one thread. dropNoise removes the bots' own run-error echoes,
// which is right when the runtime is handing a thread to an agent unasked (they would invite an
// error cascade) but wrong for an explicit read, where hiding posts would misrepresent the thread.
func (t *Transport) threadBlock(ctx context.Context, channelID, rootID string, dropNoise bool) (string, error) {
	posts, err := t.client.Thread(ctx, rootID)
	if err != nil {
		return "", err
	}
	if len(posts) == 0 {
		return "", nil
	}
	if channelID != "" && posts[0].ChannelID != "" && posts[0].ChannelID != channelID {
		return "", fmt.Errorf("thread %s is not in this channel", rootID)
	}
	if dropNoise {
		if posts = dropErrorNoise(posts); len(posts) == 0 {
			return "", nil
		}
	}
	authors := make([]string, len(posts))
	texts := make([]string, len(posts))
	for i, p := range posts {
		authors[i], texts[i] = p.UserID, p.Message
	}
	return trimToBudget(t.formatAuthored(ctx, authors, texts), maxThreadHistoryChars,
		threadElidedMarker), nil
}

// trimToBudget keeps the most recent tail of a formatted block within a rune budget, prefixing the
// caller's elision marker and dropping the partial line the cut leaves behind.
func trimToBudget(block string, budget int, marker string) string {
	r := []rune(block)
	if len(r) <= budget {
		return block
	}
	tail := string(r[len(r)-budget:])
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return marker + tail
}

// maxThreadHistoryChars bounds the thread text handed to the model on a thread_update. The whole
// thread goes in as one input, so a long-running thread would otherwise blow the model's context
// window (the provider then rejects the request with context_length_exceeded). Only the most
// recent messages up to this budget are kept; older ones are elided.
const maxThreadHistoryChars = 60000

// threadElidedMarker and channelElidedMarker prefix a block that was trimmed to its budget, so the
// agent knows it is seeing only the recent tail.
const threadElidedMarker = "[… earlier messages in this thread omitted …]\n"
const channelElidedMarker = "[… earlier messages in this channel omitted …]\n"

// ThreadHistory returns the thread rooted at rootID as a "username: message" block, or "" when it
// cannot be fetched. Run-error echoes are dropped and the block is trimmed to the most recent
// messages within maxThreadHistoryChars.
func (t *Transport) ThreadHistory(ctx context.Context, channelID, rootID string) string {
	block, err := t.threadBlock(ctx, channelID, rootID, true)
	if err != nil {
		// Surface it: a swallowed fetch error would silently hand the agent an empty thread to
		// "decide" on, making the whole thread_update a no-op no operator could explain.
		t.logger().Warn("could not fetch thread for thread_update", "root", rootID, "err", err)
		return ""
	}
	return block
}

// dropErrorNoise removes posts that are just a bot's run-error echo (e.g. a context_length_exceeded
// dump). They carry no task content, waste context budget, and - fed back to the bots observing
// the thread - are exactly what turns one failure into a thread-wide error cascade.
func dropErrorNoise(posts []ThreadPost) []ThreadPost {
	out := posts[:0]
	for _, p := range posts {
		if isErrorNoise(p.Message) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// isErrorNoise reports whether a post is a run-error echo rather than real conversation. It matches
// the "error: ..." replies the runtime used to post for provider/stream failures.
func isErrorNoise(msg string) bool {
	s := strings.TrimSpace(msg)
	if !strings.HasPrefix(s, "error: ") {
		return false
	}
	l := strings.ToLower(s)
	return strings.Contains(l, "stream error") ||
		strings.Contains(l, "context_length_exceeded") ||
		strings.Contains(l, "invalid_request_error") ||
		strings.Contains(l, "context window")
}

// formatAuthored renders parallel author-id/text slices as "username: text" lines, resolving
// and caching any usernames not seen before.
func (t *Transport) formatAuthored(ctx context.Context, authors, texts []string) string {
	t.mu.Lock()
	missing := map[string]struct{}{}
	for _, a := range authors {
		if _, ok := t.usernames[a]; !ok {
			missing[a] = struct{}{}
		}
	}
	t.mu.Unlock()

	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		if resolved, err := t.client.Usernames(ctx, ids); err != nil {
			t.logger().Debug("could not resolve usernames; falling back to ids", "err", err)
		} else {
			t.mu.Lock()
			for id, name := range resolved {
				t.usernames[id] = name
			}
			t.mu.Unlock()
		}
	}

	var b strings.Builder
	t.mu.Lock()
	for i, a := range authors {
		name := t.usernames[a]
		if name == "" {
			name = a
		}
		fmt.Fprintf(&b, "%s: %s\n", name, texts[i])
	}
	t.mu.Unlock()
	return strings.TrimRight(b.String(), "\n")
}

// Attachment limits: how many images one message may hand the model and how
// large a single image download may be (base64 inflates payloads by 4/3, and
// the provider caps the whole request). Larger or non-image files are only
// described in the note, never downloaded.
const (
	maxAttachmentImages = 4
	maxImageBytes       = 3 << 20
)

// viewableImageMimes are the attachment types multimodal models accept.
var viewableImageMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// Attachments resolves a post's file ids: viewable images are downloaded and
// returned for a multimodal turn, and every attachment - including ones that
// were skipped - is described in the note so the model knows what arrived and
// never has to guess whether it "sees" a file.
func (t *Transport) Attachments(ctx context.Context, fileIDs []string) ([]llm.Image, string) {
	var images []llm.Image
	var lines []string
	for _, id := range fileIDs {
		info, err := t.client.FileInfo(ctx, id)
		if err != nil {
			t.logger().Warn("could not resolve attachment", "file", id, "err", err)
			lines = append(lines, "- (attachment unavailable: its metadata could not be fetched)")
			continue
		}
		label := fmt.Sprintf("%s (%s, %s)",
			sanitizeNoteField(info.Name), sanitizeNoteField(info.MimeType), humanSize(info.Size))
		switch {
		case !viewableImageMimes[strings.ToLower(info.MimeType)]:
			lines = append(lines, "- "+label+" - not an image, so its contents are unavailable")
		case info.Size > maxImageBytes:
			lines = append(lines, "- "+label+" - image too large, not attached")
		case len(images) >= maxAttachmentImages:
			lines = append(lines, "- "+label+" - per-message image limit reached, not attached")
		default:
			data, derr := t.client.DownloadFile(ctx, id, maxImageBytes)
			if derr != nil {
				t.logger().Warn("could not download attachment", "file", id, "err", derr)
				lines = append(lines, "- "+label+" - could not be downloaded")
				continue
			}
			// Mattermost derives mime_type from the file extension; sniff the actual
			// bytes so a renamed non-image cannot ride into the model request and
			// fail the whole turn with a provider-side invalid_request. The sniffed
			// type is also what gets declared to the provider - a JPEG renamed to
			// .png must not ship as data:image/png.
			sniffed := http.DetectContentType(data)
			if !viewableImageMimes[sniffed] {
				lines = append(lines, "- "+label+" - contents are not an image, not attached")
				continue
			}
			images = append(images, llm.Image{
				MediaType: sniffed,
				Data:      base64.StdEncoding.EncodeToString(data),
			})
			lines = append(lines, "- "+label+" - attached as an image")
		}
	}
	if len(lines) == 0 {
		return images, ""
	}
	return images, "Attachments on this message:\n" + strings.Join(lines, "\n")
}

// sanitizeNoteField makes a server-provided string safe to embed in the
// runtime-authored attachment note: control characters and newlines (which
// could forge extra note lines that look runtime-authored) are stripped and the
// length is capped.
func sanitizeNoteField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	const max = 120
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// typingPayload builds a user_typing client action frame body.
func typingPayload(seq int64, thread bot.ThreadRef) ([]byte, error) {
	return json.Marshal(map[string]any{
		"action": "user_typing",
		"seq":    seq,
		"data": map[string]string{
			"channel_id": thread.ChannelID,
			"parent_id":  thread.RootID,
		},
	})
}

// Typing sends a single user_typing signal for the thread. Mattermost expires it after a
// few seconds, so callers re-send it while work is in progress.
func (t *Transport) Typing(thread bot.ThreadRef) error {
	data, err := typingPayload(t.seq.Add(1), thread)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return wsutil.WriteClientText(t.conn, data)
}

var _ bot.Transport = (*Transport)(nil)
var _ bot.HistoryReader = (*Transport)(nil)
var _ bot.ThreadReader = (*Transport)(nil)
var _ bot.Typist = (*Transport)(nil)
var _ bot.AttachmentFetcher = (*Transport)(nil)

// AuthorName implements bot.AuthorNamer, reusing the username cache the thread
// and channel formatters fill.
func (t *Transport) AuthorName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	t.mu.Lock()
	name := t.usernames[userID]
	t.mu.Unlock()
	if name != "" {
		return name
	}
	resolved, err := t.client.Usernames(ctx, []string{userID})
	if err != nil {
		t.logger().Debug("could not resolve username", "err", err)
		return ""
	}
	t.mu.Lock()
	for id, n := range resolved {
		t.usernames[id] = n
	}
	name = t.usernames[userID]
	t.mu.Unlock()
	return name
}
