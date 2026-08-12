package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runReadThreads(t *testing.T, f *fakeWriter, args string) (string, error) {
	t.Helper()
	return NewReadThreadsTool(f).Run(context.Background(), json.RawMessage(args))
}

func TestReadThreadsListsAndFilters(t *testing.T) {
	f := newFakeWriter()
	f.digest = "t_0102030405060708  [needs_you]  retries"

	out, err := runReadThreads(t, f, `{"action":"list","state":"needs_you","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != f.digest {
		t.Fatalf("out = %q", out)
	}
	if f.gotState != "needs_you" || f.gotLimit != 5 {
		t.Fatalf("list asked for state %q limit %d", f.gotState, f.gotLimit)
	}
}

func TestReadThreadsReadsOneThread(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	f.thread = "operator: look please\namiran: on it"

	out, err := runReadThreads(t, f, `{"action":"read","thread":"t_0102030405060708"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != f.thread {
		t.Fatalf("out = %q", out)
	}
	if f.gotRead != "t_0102030405060708" {
		t.Fatalf("read %q", f.gotRead)
	}
}

// A model that invented an id has to be told where ids come from. "Not found"
// alone is something it will try to work around.
func TestReadThreadsRefusesAnIDItCannotHaveGot(t *testing.T) {
	f := newFakeWriter()
	for _, id := range []string{"", "Tasks", "t_zzzz", "abc123", "t_0102030405060708extra"} {
		body, err := json.Marshal(map[string]string{"action": "read", "thread": id})
		if err != nil {
			t.Fatal(err)
		}
		_, err = runReadThreads(t, f, string(body))
		if err == nil {
			t.Fatalf("read accepted %q as a thread id", id)
		}
		if !strings.Contains(err.Error(), "list") {
			t.Fatalf("error for %q does not say where ids come from: %v", id, err)
		}
	}
	if f.gotRead != "" {
		t.Fatalf("an invented id reached the store as %q", f.gotRead)
	}
}

func TestReadThreadsSearches(t *testing.T) {
	f := newFakeWriter()
	f.search = "t_0102030405060708  amiran: the rotation drops sessions"

	out, err := runReadThreads(t, f, `{"action":"search","query":"rotation","limit":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != f.search {
		t.Fatalf("out = %q", out)
	}
	if f.gotQuery != "rotation" || f.gotLimit != 3 {
		t.Fatalf("search asked for %q limit %d", f.gotQuery, f.gotLimit)
	}
	if _, err := runReadThreads(t, f, `{"action":"search"}`); err == nil {
		t.Fatal("search with no query was accepted")
	}
}

func TestReadThreadsRejectsAnUnknownAction(t *testing.T) {
	f := newFakeWriter()
	_, err := runReadThreads(t, f, `{"action":"digest"}`)
	if err == nil {
		t.Fatal("an unknown action was accepted")
	}
	if !strings.Contains(err.Error(), "list") {
		t.Fatalf("error does not name the actions there are: %v", err)
	}
}

func TestReadThreadsSurfacesTheStoresError(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	f.readErr = errors.New("chat: no such thread")

	if _, err := runReadThreads(t, f, `{"action":"read","thread":"t_0102030405060708"}`); err == nil {
		t.Fatal("a refused read reported success")
	}
}
