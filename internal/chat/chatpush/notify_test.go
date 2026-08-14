package chatpush

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/push"
	"github.com/gigovich/aigem/internal/push/pushtest"
)

const (
	amiran = "bot:amiran"
	// The RFC 8291 example's subscription keys: a well-formed P-256 point and a
	// 16-byte secret, which is all the store and the sender require.
	subKey  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	subPriv = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	subAuth = "BTBZMqHH6r4Tts7J_aSIgg"
)

// service is a stand-in push service that reports what it was asked to deliver.
type service struct {
	*httptest.Server
	sent   chan *http.Request
	status int

	mu sync.Mutex
	// topics is every delivery in the order the service saw it, and acked is
	// how many of those a test has already claimed with waitForPush.
	topics []string
	bodies [][]byte
	acked  int
}

// body is what the delivery a test just claimed carried.
func (s *service) body(n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[n]
}

// read decrypts what a delivery actually carried. Asserting on the alert struct
// alone leaves everything between it and the browser untested: the wrong view,
// an empty author, a payload that never made it into the body.
func read(t *testing.T, r *http.Request, body []byte) map[string]string {
	t.Helper()
	uaPriv, err := ecdh.P256().NewPrivateKey(mustDecode(t, subPriv))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := pushtest.Decrypt(uaPriv, mustDecode(t, subAuth), body)
	if err != nil {
		t.Fatalf("decrypting what was pushed: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatalf("the payload is not the alert: %v", err)
	}
	_ = r
	return got
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newService(t *testing.T) *service {
	t.Helper()
	svc := &service{sent: make(chan *http.Request, 64), status: http.StatusCreated}
	// TLS, because a subscription endpoint is https or it is refused: the
	// endpoint is the capability to notify that browser.
	svc.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		svc.mu.Lock()
		svc.bodies = append(svc.bodies, body)
		svc.mu.Unlock()
		svc.mu.Lock()
		svc.topics = append(svc.topics, r.Header.Get("Topic"))
		svc.mu.Unlock()
		w.WriteHeader(svc.status)
		// Never blocks. A test that stops reading - or one that fires more
		// deliveries than it waits for - would otherwise wedge the handler, and
		// httptest.Server.Close waits for handlers to return.
		select {
		case svc.sent <- r:
		default:
		}
	}))
	t.Cleanup(svc.Close)
	return svc
}

// waitForPush returns the next delivery, or fails: a notification that never
// arrives is the failure this whole package exists to prevent.
func (s *service) waitForPush(t *testing.T) *http.Request {
	t.Helper()
	select {
	case r := <-s.sent:
		s.mu.Lock()
		s.acked++
		s.mu.Unlock()
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no push arrived")
		return nil
	}
}

// quietUntil is the end-to-end half of "nothing was pushed for that": it makes
// a second thread ask - which must push - waits for that delivery, and then
// asserts that it is the only one the service ever saw.
//
// It is not the whole assertion. Two deliveries started by two writes have no
// ordering between them, so an unwanted one that lost the race would land after
// the barrier and be missed here; the deterministic half is asserting what the
// notifier believes, which onFrames has finished updating by the time the write
// that produced it returns. Callers do both.
func (s *service) quietUntil(t *testing.T, store *chat.Store) {
	t.Helper()
	th, err := store.NewThread(t.Context(), "the barrier", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "anything?")
	s.waitForPush(t)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Everything a test claimed with waitForPush is a delivery it asked for.
	// What follows must be the barrier and nothing else.
	if rest := s.topics[s.acked-1:]; len(rest) != 1 || rest[0] != th.ID {
		t.Fatalf("the service was pushed %v after the last expected delivery, want only "+
			"the barrier thread %s", rest, th.ID)
	}
}

// pushed is how many deliveries the service has seen that no test claimed.
func (s *service) pushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.topics) - s.acked
}

// isAsking is what the notifier believes about a thread. onFrames runs inside
// the publish of the write that produced the frames, so this is settled by the
// time the call that wrote them has returned.
func (n *Notifier) isAsking(thread string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.asking[thread]
}

func newStore(t *testing.T) *chat.Store {
	t.Helper()
	store, err := chat.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, a := range []chat.Actor{
		{ID: chat.Operator, Kind: chat.KindHuman, Name: "operator"},
		{ID: amiran, Kind: chat.KindBot, Name: "amiran", Role: "developer"},
	} {
		if err := store.PutActor(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func newNotifier(t *testing.T, store *chat.Store, hc *http.Client) *Notifier {
	t.Helper()
	keys, err := push.LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := New(store, push.NewClient(keys, push.WithTransport(hc.Transport)), slog.New(slog.DiscardHandler))
	if err := n.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func subscribe(t *testing.T, store *chat.Store, endpoint string) push.Subscription {
	t.Helper()
	sub := push.Subscription{Endpoint: endpoint, P256dh: subKey, Auth: subAuth}
	if err := store.AddPushSub(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	return sub
}

// askFor makes a bot ask the operator something, which is the one event that
// earns a notification.
func askFor(t *testing.T, store *chat.Store, thread, body string) {
	t.Helper()
	if _, err := store.Say(t.Context(), thread,
		chat.Draft{Author: amiran, Body: body, AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
}

func TestPushesTheTransitionIntoNeedsYou(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")

	req := svc.waitForPush(t)
	if req.Header.Get("Topic") != th.ID {
		t.Errorf("Topic is %q, want the thread id so a second alert replaces the first",
			req.Header.Get("Topic"))
	}
	if enc := req.Header.Get("Content-Encoding"); enc != "aes128gcm" {
		t.Errorf("Content-Encoding is %q", enc)
	}

	// Still asking: a second message in the same thread is not a second
	// interruption.
	if _, err := store.Say(t.Context(), th.ID,
		chat.Draft{Author: amiran, Body: "still waiting", AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
	svc.quietUntil(t, store)
}

func TestPushesAgainAfterTheOperatorAnswers(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	svc.waitForPush(t)

	if _, err := store.Say(t.Context(), th.ID,
		chat.Draft{Author: chat.Operator, Body: "eu-west"}); err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "and which account?")
	svc.waitForPush(t)
}

// A restart must not re-announce what the operator already knows about.
func TestDoesNotPushWhatWasAlreadyAsking(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")

	// Only now does the notifier start, with the thread already in needs_you.
	newNotifier(t, store, svc.Client())
	if err := store.MarkRead(t.Context(), chat.Operator, th.ID, 1); err != nil {
		t.Fatal(err)
	}
	svc.quietUntil(t, store)
}

func TestForgetsASubscriptionThePushServiceHasDropped(t *testing.T) {
	svc := newService(t)
	svc.status = http.StatusGone
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	svc.waitForPush(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		subs, err := store.PushSubs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a subscription answering 410 was kept: %+v", subs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A refusal that is not "gone" is a service problem, not the browser's: the
// subscription stays.
func TestKeepsASubscriptionAServiceMerelyRefused(t *testing.T) {
	svc := newService(t)
	svc.status = http.StatusInternalServerError
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	svc.waitForPush(t)

	// Long enough for a delete to have happened if one were going to.
	time.Sleep(200 * time.Millisecond)
	subs, err := store.PushSubs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("a 500 dropped the subscription: %+v", subs)
	}
}

func TestPayloadNamesTheThreadAndWhoIsAsking(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	req := svc.waitForPush(t)

	// Read back off the wire, decrypted with the subscription's own key: this
	// is the payload a phone will show, not the struct the notifier built.
	got := read(t, req, svc.body(0))
	want := map[string]string{
		"thread": th.ID,
		"title":  "the deploy",
		"body":   "amiran needs you",
		"url":    "/chat/" + th.ID,
	}
	if !maps.Equal(got, want) {
		t.Errorf("the phone is shown %v, want %v", got, want)
	}
	// Exactly those four keys, so adding a "preview" of what was said breaks a
	// test rather than sending the conversation to a locked screen.
	if len(got) != 4 {
		t.Errorf("the payload carries %d fields: %v", len(got), got)
	}
}

// A thread with no title still has to say something, and a bot that is not
// named is still asking.
func TestAlertForFillsInWhatIsMissing(t *testing.T) {
	for name, c := range map[string]struct {
		view   chat.ThreadView
		author string
		title  string
		body   string
	}{
		"no title": {
			view:   chat.ThreadView{Thread: chat.Thread{ID: "t_1"}},
			author: amiran,
			title:  "aigem",
			body:   "amiran needs you",
		},
		"no author": {
			view:   chat.ThreadView{Thread: chat.Thread{ID: "t_1", Title: "the deploy"}},
			author: "",
			title:  "the deploy",
			body:   "needs you",
		},
		"the operator's own message": {
			view:   chat.ThreadView{Thread: chat.Thread{ID: "t_1", Title: "the deploy"}},
			author: chat.Operator,
			title:  "the deploy",
			body:   "needs you",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := alertFor(c.view, c.author)
			if a.Title != c.title || a.Body != c.body {
				t.Errorf("alert is %q / %q, want %q / %q", a.Title, a.Body, c.title, c.body)
			}
		})
	}
}

// A thread the operator is not in cannot be opened from a notification, so it
// is never announced. The store already refuses to park a bot-only thread in
// needs_you, which makes this a guard rather than a path - and a guard is worth
// testing where it is, since the frame is what carries the audience.
func TestIgnoresAThreadTheOperatorIsNotIn(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	n := newNotifier(t, store, svc.Client())

	frames := askFrames("t_botsonly")
	frames[1].Thread.Participants = []string{amiran, "bot:kate"}
	n.onFrames(frames)
	svc.quietUntil(t, store)
}

func TestCloseLetsADeliveryLand(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	answered := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusCreated)
		answered <- struct{}{}
	}))
	defer srv.Close()

	n := blockedDelivery(t, srv, arrived)

	closed := make(chan struct{})
	go func() {
		n.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close abandoned a delivery it could have finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the delivery finished")
	}
	select {
	case <-answered:
	default:
		t.Error("the push service never answered the delivery Close waited for")
	}
}

// A service that accepts a connection and then says nothing is the case the
// grace period exists to bound: shutting the fleet down must not wait out the
// request timeout.
func TestCloseGivesUpOnAServiceThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	n := blockedDelivery(t, srv, arrived)

	start := time.Now()
	n.Close()
	if waited := time.Since(start); waited > closeGrace+3*time.Second {
		t.Errorf("Close waited %s on a service that never answered", waited)
	}
}

// blockedDelivery gets one push as far as the service and no further, and
// returns the notifier that is waiting on it.
func blockedDelivery(t *testing.T, srv *httptest.Server, arrived <-chan struct{}) *Notifier {
	t.Helper()
	store := newStore(t)
	subscribe(t, store, srv.URL+"/send/phone")
	keys, err := push.LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := New(store, push.NewClient(keys, push.WithTransport(srv.Client().Transport)), slog.New(slog.DiscardHandler))
	if err := n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no push reached the service")
	}
	return n
}

// The state on a frame is the effective one: while any turn is open it reads
// "working" and hides what the thread is actually parked in. A bot writes
// inside a turn, so without this the second turn over an unanswered question -
// another bot answering, a heartbeat, a cron - reads as a fresh transition and
// rings the phone again for a question the operator has already seen.
func TestDoesNotPushAgainForATurnOverAnUnansweredQuestion(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	// The ask, as a bot actually writes it: inside a turn.
	turn, err := store.BeginTurn(t.Context(), th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Say(t.Context(), th.ID, chat.Draft{
		Author: amiran, Body: "which region?", AwaitReply: true, TurnSeq: turn,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndTurn(t.Context(), amiran, turn, ""); err != nil {
		t.Fatal(err)
	}
	if got := svc.waitForPush(t).Header.Get("Topic"); got != th.ID {
		t.Fatalf("the ask pushed for %s, want %s", got, th.ID)
	}

	// A later turn in the same thread that answers nothing: a heartbeat, or a
	// second bot working. The question is still unanswered, and the operator
	// has already been told.
	second, err := store.BeginTurn(t.Context(), th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndTurn(t.Context(), amiran, second, ""); err != nil {
		t.Fatal(err)
	}
	svc.quietUntil(t, store)
}

// An archived thread is one the operator has put away. The seed does not read
// archived threads, so alerting on one would be the two halves disagreeing -
// and a restart would then re-announce it on the next unrelated frame.
func TestDoesNotPushAnArchivedThread(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	n := newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetArchived(t.Context(), chat.Operator, th.ID, true); err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	if n.isAsking(th.ID) {
		t.Error("an archived thread was recorded as asking, so it will never ask again")
	}
	svc.quietUntil(t, store)
}

// One dead subscription must not cost the live ones their notification.
func TestKeepsDeliveringPastASubscriptionThatIsGone(t *testing.T) {
	live := newService(t)
	dead := newService(t)
	dead.status = http.StatusGone
	store := newStore(t)
	// The dead one first, so the live one is only reached by continuing.
	subscribe(t, store, dead.URL+"/send/old")
	subscribe(t, store, live.URL+"/send/phone")
	// One client trusts both stand-ins: a browser's endpoint host is not the
	// daemon's business, and two subscriptions can name two services.
	newNotifier(t, store, bothOf(t, dead, live))

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")

	dead.waitForPush(t)
	live.waitForPush(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		subs, err := store.PushSubs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) == 1 {
			if !strings.HasSuffix(subs[0].Endpoint, "/send/phone") {
				t.Fatalf("the wrong subscription was forgotten: %+v", subs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriptions after a 410: %+v", subs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// bothOf is an HTTP client that trusts two stand-in services at once.
func bothOf(t *testing.T, a, b *service) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	for _, svc := range []*service{a, b} {
		pool.AddCert(svc.Certificate())
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

func TestTopicForRefusesWhatAServiceWouldNotAccept(t *testing.T) {
	for topic, want := range map[string]string{
		"t_db0b588a96bd081b":    "t_db0b588a96bd081b",
		"":                      "",
		strings.Repeat("a", 33): "",
		"t/1":                   "",
	} {
		if got := topicFor(topic); got != want {
			t.Errorf("topicFor(%q) = %q, want %q", topic, got, want)
		}
	}
}

// The store calls its publishers outside its own lock, so a write already in
// flight can reach onFrames after Close removed the registration. Adding to the
// wait group at that point is a panic, not a late delivery.
func TestFramesArrivingAfterCloseAreDropped(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	n := newNotifier(t, store, svc.Client())
	n.Close()

	n.onFrames(askFrames("t_late"))

	// The guard, not the cancelled context: a frame that got as far as being
	// recorded also got as far as wg.Add, which is the panic this prevents.
	if n.isAsking("t_late") {
		t.Error("a closed notifier acted on a frame")
	}
	select {
	case r := <-svc.sent:
		t.Fatalf("a closed notifier delivered %s", r.URL.Path)
	case <-time.After(100 * time.Millisecond):
	}
}

// askFrames is what a write produces when a bot asks the operator something:
// the message, then the thread it left behind.
func askFrames(thread string) []chat.Frame {
	return []chat.Frame{
		{
			Seq: 1, Stream: chat.StreamMessage, ThreadID: thread,
			Message: &chat.Message{
				Seq: 1, Thread: thread, Author: amiran, Body: "which region?",
				Kind: chat.MsgMessage, Await: true,
			},
		},
		{
			Seq: 2, Stream: chat.StreamThread, ThreadID: thread,
			Thread: &chat.ThreadView{
				Thread:       chat.Thread{ID: thread, Title: "late", State: chat.StateNeedsYou},
				Participants: []string{chat.Operator, amiran},
			},
		},
	}
}

// The operator answering does not always land with the thread at rest: a
// heartbeat or a second bot can have a turn open, and the frame then reads
// "working" whatever the thread is parked in. An answer that is not noticed
// leaves this believing the thread is still asking, and the bot's next question
// is then silently swallowed.
func TestPushesAgainWhenTheOperatorAnsweredDuringSomeoneElsesTurn(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	svc.waitForPush(t)

	// A turn is open for the whole of what follows, so every frame in it reads
	// "working".
	turn, err := store.BeginTurn(t.Context(), th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Say(t.Context(), th.ID,
		chat.Draft{Author: chat.Operator, Body: "eu-west"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Say(t.Context(), th.ID, chat.Draft{
		Author: amiran, Body: "and which account?", AwaitReply: true, TurnSeq: turn,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndTurn(t.Context(), amiran, turn, ""); err != nil {
		t.Fatal(err)
	}

	if got := svc.waitForPush(t).Header.Get("Topic"); got != th.ID {
		t.Fatalf("pushed for %s, want the thread with the new question (%s)", got, th.ID)
	}
}

// Un-archiving is the operator's own doing, and after a restart the thread is
// not in the seed - so a rule that read the thread's state would announce it as
// though a bot had just asked.
func TestDoesNotPushWhenAnArchivedThreadIsBroughtBack(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	if err := store.SetArchived(t.Context(), chat.Operator, th.ID, true); err != nil {
		t.Fatal(err)
	}
	// The restart: a notifier that never saw any of that, seeded from a store
	// whose archived threads it does not read.
	newNotifier(t, store, svc.Client())
	if err := store.SetArchived(t.Context(), chat.Operator, th.ID, false); err != nil {
		t.Fatal(err)
	}
	svc.quietUntil(t, store)
}

// The seed is what stops a restart re-announcing every unanswered question. It
// is exercised by a bot asking again in a thread that was already asking: the
// state that says "they already know" can only have come from the seed.
func TestSeedsWhatIsAlreadyAsking(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")

	// The restart.
	n := newNotifier(t, store, svc.Client())
	if !n.isAsking(th.ID) {
		t.Fatal("a thread that was already asking was not seeded")
	}
	askFor(t, store, th.ID, "still: which region?")
	if svc.pushed() != 0 {
		t.Error("a repeated question in a thread already asking was announced again")
	}
	svc.quietUntil(t, store)
}

// A thread that is gone takes what was remembered about it with it. Otherwise
// an id reused after a delete starts out believing it is already asking.
func TestForgetsADeletedThread(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	n := newNotifier(t, store, svc.Client())

	th, err := store.NewThread(t.Context(), "the deploy", chat.Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	askFor(t, store, th.ID, "which region?")
	svc.waitForPush(t)
	if !n.isAsking(th.ID) {
		t.Fatal("a thread that asked is not recorded as asking")
	}

	if err := store.DeleteThread(t.Context(), chat.Operator, th.ID); err != nil {
		t.Fatal(err)
	}
	if n.isAsking(th.ID) {
		t.Error("a deleted thread is still remembered as asking")
	}
}

// Deliveries are bounded: a fleet that turns several threads over to the
// operator at once must not open a socket per thread per subscription.
func TestDeliversNoMoreThanTheCapAtOnce(t *testing.T) {
	var mu sync.Mutex
	var inFlight, peak int
	release := make(chan struct{})
	arrived := make(chan struct{}, 32)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer close(release)

	store := newStore(t)
	subscribe(t, store, srv.URL+"/send/phone")
	newNotifier(t, store, srv.Client())

	const asks = maxSending + 4
	for i := range asks {
		th, err := store.NewThread(t.Context(), fmt.Sprintf("thread %d", i), chat.Operator,
			[]string{amiran})
		if err != nil {
			t.Fatal(err)
		}
		askFor(t, store, th.ID, "which region?")
	}
	for range maxSending {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("fewer deliveries were started than the cap allows")
		}
	}
	// Long enough for a fifth to have started if nothing bounded it.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if peak > maxSending {
		t.Errorf("%d deliveries were in flight at once, want at most %d", peak, maxSending)
	}
}

// Close has to unregister as well as stop acting: a notifier left registered is
// called on every write for the life of the process.
func TestCloseUnregistersFromTheStore(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	n := newNotifier(t, store, svc.Client())

	removed := false
	inner := n.remove
	n.remove = func() {
		removed = true
		inner()
	}
	n.Close()
	if !removed {
		t.Error("Close left the notifier registered as a publisher")
	}
}

// The store calls its publishers outside its own lock, so onFrames and Close
// genuinely overlap. Adding to the wait group after Close began waiting is a
// panic rather than a late delivery; this hammers the window.
func TestOnFramesAndCloseOverlapSafely(t *testing.T) {
	svc := newService(t)
	store := newStore(t)
	subscribe(t, store, svc.URL+"/send/phone")
	n := newNotifier(t, store, svc.Client())

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.onFrames(askFrames(fmt.Sprintf("t_%d", i)))
		}()
	}
	n.Close()
	wg.Wait()
}
