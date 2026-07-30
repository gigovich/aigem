package mattermost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelIDByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/me/teams/team1/channels" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write([]byte(`[
			{"id":"c-town","name":"town-square","display_name":"Town Square"},
			{"id":"c-tasks","name":"tasks","display_name":"Tasks"}
		]`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	cases := map[string]string{
		"tasks":       "c-tasks", // exact slug
		"Tasks":       "c-tasks", // display name
		"#Tasks":      "c-tasks", // chat-style hash prefix models tend to write
		"#tasks":      "c-tasks",
		"town-square": "c-town",
		"Town Square": "c-town", // display name with space
	}
	for input, want := range cases {
		got, err := c.ChannelIDByName(context.Background(), "team1", input)
		if err != nil || got != want {
			t.Errorf("ChannelIDByName(%q) = %q, %v; want %q", input, got, err, want)
		}
	}

	if _, err := c.ChannelIDByName(context.Background(), "team1", "Secret"); err == nil ||
		!strings.Contains(err.Error(), "Secret") {
		t.Errorf("missing channel: want error mentioning name, got %v", err)
	}
}

func TestAttachments(t *testing.T) {
	pngBytes := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16) // real PNG signature for content sniffing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/files/img1/info":
			w.Write([]byte(`{"id":"img1","name":"shot.png","mime_type":"image/png","size":24}`))
		case "/api/v4/files/img1":
			io.WriteString(w, pngBytes)
		case "/api/v4/files/fake1/info":
			w.Write([]byte(`{"id":"fake1","name":"evil\nstrings.png","mime_type":"image/png","size":9}`))
		case "/api/v4/files/fake1":
			io.WriteString(w, "MZplainexe") // renamed non-image: extension says png, bytes do not
		case "/api/v4/files/doc1/info":
			w.Write([]byte(`{"id":"doc1","name":"spec.pdf","mime_type":"application/pdf","size":2048}`))
		case "/api/v4/files/gone/info":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tr := &Transport{client: NewClient(srv.URL, "tok")}
	images, note := tr.Attachments(context.Background(), []string{"img1", "fake1", "doc1", "gone"})

	if len(images) != 1 || images[0].MediaType != "image/png" {
		t.Fatalf("images = %+v, want only the real png", images)
	}
	for _, want := range []string{
		"shot.png", "attached as an image",
		"contents are not an image",
		"spec.pdf", "not an image", "unavailable",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "evil\nstrings") {
		t.Errorf("newline in a filename must not survive into the note:\n%s", note)
	}
}

func TestDownloadFileEnforcesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	if _, err := c.DownloadFile(context.Background(), "big", 50); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized body must be refused, got err=%v", err)
	}
	data, err := c.DownloadFile(context.Background(), "ok", 100)
	if err != nil || len(data) != 100 {
		t.Fatalf("DownloadFile = %d bytes, %v", len(data), err)
	}
}

func TestMeAndCreatePost(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/v4/users/me":
			w.Write([]byte(`{"id":"bot123","username":"amiran"}`))
		case "/api/v4/posts":
			buf, _ := io.ReadAll(r.Body)
			gotBody = string(buf)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"p1"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	id, err := c.Me(context.Background())
	if err != nil || id != "bot123" {
		t.Fatalf("Me = %q, %v", id, err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	postID, err := c.CreatePost(context.Background(), "chan1", "root1", "hello")
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	if postID != "p1" {
		t.Fatalf("CreatePost id = %q, want p1", postID)
	}
	for _, want := range []string{`"channel_id":"chan1"`, `"root_id":"root1"`, `"message":"hello"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("post body missing %s: %s", want, gotBody)
		}
	}
}

func TestUsernames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/ids" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Write([]byte(`[{"id":"u1","username":"alice"},{"id":"u2","username":"bob"}]`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	got, err := c.Usernames(context.Background(), []string{"u1", "u2"})
	if err != nil || got["u1"] != "alice" || got["u2"] != "bob" {
		t.Fatalf("Usernames = %v, %v", got, err)
	}
	if m, err := c.Usernames(context.Background(), nil); err != nil || len(m) != 0 {
		t.Fatalf("empty ids = %v, %v", m, err)
	}
}

func TestDirectChannelWith(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users/username/mark":
			w.Write([]byte(`{"id":"u-mark"}`))
		case r.URL.Path == "/api/v4/users/me":
			w.Write([]byte(`{"id":"u-bot"}`))
		case r.URL.Path == "/api/v4/channels/direct" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "u-bot") || !strings.Contains(string(body), "u-mark") {
				http.Error(w, "wrong pair", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"id":"dm-1"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	got, err := c.DirectChannelWith(context.Background(), "mark")
	if err != nil || got != "dm-1" {
		t.Fatalf("DirectChannelWith = %q, %v; want dm-1", got, err)
	}
	if _, err := c.DirectChannelWith(context.Background(), "ghost"); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown user: want error naming the user, got %v", err)
	}
}

func TestChannelPostsOrdersAndDropsSystemPosts(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Write([]byte(`{
			"order":["p3","p2","p1","sys"],
			"posts":{
				"p1":{"id":"p1","root_id":"","user_id":"u1","message":"first","create_at":100},
				"p2":{"id":"p2","root_id":"p1","user_id":"u2","message":"reply","create_at":200},
				"p3":{"id":"p3","root_id":"","user_id":"u1","message":"newest","create_at":300},
				"sys":{"id":"sys","root_id":"","user_id":"u3","message":"joined","create_at":150,"type":"system_join_channel"}
			}
		}`))
	}))
	defer srv.Close()

	posts, err := NewClient(srv.URL, "tok").ChannelPosts(context.Background(), "c1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "per_page=25") {
		t.Fatalf("limit not passed through: %s", gotPath)
	}
	if len(posts) != 3 {
		t.Fatalf("got %d posts, want 3 (the system join must be dropped)", len(posts))
	}
	if posts[0].Message != "first" || posts[2].Message != "newest" {
		t.Fatalf("posts should be oldest-first, got %+v", posts)
	}
	if posts[1].RootID != "p1" {
		t.Fatalf("root id lost: %+v", posts[1])
	}
}

func TestChannelPostsDefaultsLimit(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Write([]byte(`{"order":[],"posts":{}}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "tok").ChannelPosts(context.Background(), "c1", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "per_page=60") {
		t.Fatalf("expected a default limit, got %s", gotPath)
	}
}

func TestChannelDigestTagsThreadIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/users/ids") || r.Method == http.MethodPost {
			w.Write([]byte(`[{"id":"u1","username":"amiran"},{"id":"u2","username":"kate"}]`))
			return
		}
		w.Write([]byte(`{
			"order":["p1","p2"],
			"posts":{
				"p1":{"id":"p1","root_id":"","user_id":"u1","message":"кто берёт тикет?","create_at":100},
				"p2":{"id":"p2","root_id":"p1","user_id":"u2","message":"беру DOAML-7","create_at":200}
			}
		}`))
	}))
	defer srv.Close()
	tr := newTestTransport()
	tr.client = NewClient(srv.URL, "tok")

	got, err := tr.ChannelDigest(context.Background(), "c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	// A top-level post is tagged with its own id so the reader can then ask for that thread.
	if !strings.Contains(got, "[thread p1] кто берёт тикет?") {
		t.Fatalf("digest = %q", got)
	}
	// A consecutive post in the same thread is not re-tagged: repeating a 26-char id on every
	// line would eat the digest budget without telling the reader anything new.
	if !strings.Contains(got, "kate: беру DOAML-7") || strings.Count(got, "[thread p1]") != 1 {
		t.Fatalf("expected one tag for the thread, digest = %q", got)
	}
}

func TestThreadTextReportsFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tr := newTestTransport()
	tr.client = NewClient(srv.URL, "tok")

	// Unlike ThreadHistory, an explicit read must not hand back silence on failure.
	if _, err := tr.ThreadText(context.Background(), "c1", "r1"); err == nil {
		t.Fatal("expected an error from a failed thread fetch")
	}
	if got := tr.ThreadHistory(context.Background(), "c1", "r1"); got != "" {
		t.Fatalf("ThreadHistory should stay lenient, got %q", got)
	}
}

func TestTrimToBudgetKeepsRecentTail(t *testing.T) {
	block := "old line\nmiddle line\nnewest line\n"
	if got := trimToBudget(block, 1000, threadElidedMarker); got != block {
		t.Fatalf("a block under budget must be untouched, got %q", got)
	}
	got := trimToBudget(block, 20, threadElidedMarker)
	if !strings.HasPrefix(got, threadElidedMarker) {
		t.Fatalf("a trimmed block must say so, got %q", got)
	}
	if !strings.Contains(got, "newest line") {
		t.Fatalf("trimming must keep the recent tail, got %q", got)
	}
	if strings.Contains(got, "old line") {
		t.Fatalf("trimming should drop the oldest content, got %q", got)
	}
}

// The root id can come from the model, so a thread must be refused when it lives somewhere the
// caller did not name: channel membership is the only boundary on what a bot may read.
func TestThreadTextRefusesAThreadFromAnotherChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || strings.HasPrefix(r.URL.Path, "/api/v4/users/ids") {
			w.Write([]byte(`[{"id":"u1","username":"amiran"}]`))
			return
		}
		w.Write([]byte(`{"order":["p1"],"posts":{"p1":{"id":"p1","channel_id":"private-chan",
			"user_id":"u1","message":"secret","create_at":100}}}`))
	}))
	defer srv.Close()
	tr := newTestTransport()
	tr.client = NewClient(srv.URL, "tok")

	if _, err := tr.ThreadText(context.Background(), "tasks-chan", "p1"); err == nil {
		t.Fatal("a thread from another channel must be refused")
	}
	// The same thread read through its own channel is fine.
	got, err := tr.ThreadText(context.Background(), "private-chan", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "secret") {
		t.Fatalf("got %q", got)
	}
	// An unscoped read is allowed for callers that have no channel to check against.
	if _, err := tr.ThreadText(context.Background(), "", "p1"); err != nil {
		t.Fatal(err)
	}
}

// A model-supplied root id must not be able to steer the request off its endpoint.
func TestThreadEscapesTheRootID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath() + "?" + r.URL.RawQuery
		w.Write([]byte(`{"order":[],"posts":{}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	if _, err := c.Thread(context.Background(), "../channels/other/posts?x="); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotPath, "/channels/other/posts") {
		t.Fatalf("root id escaped its path segment: %s", gotPath)
	}
	if !strings.HasPrefix(gotPath, "/api/v4/posts/") || !strings.Contains(gotPath, "/thread") {
		t.Fatalf("unexpected path %s", gotPath)
	}
}
