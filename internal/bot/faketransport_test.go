package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// fakeWriter is a ThreadWriter and ThreadReader, which is what the three chat
// tools are built on. It records what it was asked to do so a test can assert
// on the request rather than on a rendered string.
type fakeWriter struct {
	mu sync.Mutex

	// actors is what ActorFor answers, keyed by lowercase name.
	actors map[string]string
	// threads a Say is allowed into. A Say elsewhere fails the way the store
	// fails it: a thread you are not in does not exist.
	threads map[string]bool

	said     []saidMessage
	opened   []openedThread
	joined   []joinedActor
	sayErr   error
	openErr  error
	joinErr  error
	digest   string
	thread   string
	search   string
	readErr  error
	gotState string
	gotQuery string
	gotLimit int
	gotRead  ThreadID
}

type saidMessage struct {
	Thread ThreadID
	Text   string
	Opts   SayOpts
}

type openedThread struct {
	Title        string
	Participants []ThreadActor
	Text         string
}

type joinedActor struct {
	Thread ThreadID
	Actor  ThreadActor
}

func newFakeWriter(threads ...string) *fakeWriter {
	f := &fakeWriter{
		actors:  map[string]string{"operator": "human:operator"},
		threads: map[string]bool{},
	}
	for _, th := range threads {
		f.threads[th] = true
	}
	for _, name := range []string{"amiran", "demetre", "jane"} {
		f.actors[name] = "bot:" + name
	}
	return f
}

func (f *fakeWriter) Say(_ context.Context, thread ThreadID, text string, o SayOpts) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sayErr != nil {
		return 0, f.sayErr
	}
	if !f.threads[string(thread)] {
		return 0, errors.New("chat: no such thread")
	}
	f.said = append(f.said, saidMessage{Thread: thread, Text: text, Opts: o})
	return uint64(len(f.said)), nil
}

func (f *fakeWriter) Open(_ context.Context, title string, participants []ThreadActor, text string) (ThreadID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return "", f.openErr
	}
	id := fmt.Sprintf("t_%016x", len(f.opened)+1)
	f.opened = append(f.opened, openedThread{Title: title, Participants: participants, Text: text})
	f.threads[id] = true
	if text != "" {
		f.said = append(f.said, saidMessage{Thread: ThreadID(id), Text: text})
	}
	return ThreadID(id), nil
}

func (f *fakeWriter) Join(_ context.Context, thread ThreadID, actor ThreadActor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.joinErr != nil {
		return f.joinErr
	}
	f.joined = append(f.joined, joinedActor{Thread: thread, Actor: actor})
	return nil
}

func (f *fakeWriter) ActorFor(name string) ThreadActor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.actors[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))]
}

func (f *fakeWriter) ThreadHistory(_ context.Context, thread ThreadID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRead = thread
	return f.thread
}

func (f *fakeWriter) ThreadText(_ context.Context, thread ThreadID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRead = thread
	return f.thread, f.readErr
}

func (f *fakeWriter) Threads(_ context.Context, state string, limit int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotState, f.gotLimit = state, limit
	return f.digest, f.readErr
}

func (f *fakeWriter) Search(_ context.Context, query string, limit int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotQuery, f.gotLimit = query, limit
	return f.search, f.readErr
}

// lastSaid is the most recent message, for a test that only cares about one.
func (f *fakeWriter) lastSaid() saidMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.said) == 0 {
		return saidMessage{}
	}
	return f.said[len(f.said)-1]
}

var (
	_ ThreadWriter = (*fakeWriter)(nil)
	_ ThreadReader = (*fakeWriter)(nil)
)
