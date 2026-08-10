package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The list is opened far more often than any one diff is read, so it carries
// filenames only. A session that rewrote a large tree would otherwise ship all
// of it to draw a sidebar.
func TestArtifactListOmitsContentUntilAsked(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	e, ok := srv.lookup(id)
	if !ok {
		t.Fatal("session vanished")
	}
	e.sess.RecordFileChange("/tmp/a.go", "before", "after", false)

	res := srv.get(t, "/api/sessions/"+id+"/artifacts")
	defer res.Body.Close()
	var list []artifactView
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v, want one entry", list)
	}
	if list[0].Old != "" || list[0].New != "" {
		t.Fatalf("the list carried content: %+v", list[0])
	}

	res2 := srv.get(t, "/api/sessions/"+id+"/artifacts?path="+list[0].Path)
	defer res2.Body.Close()
	var one []artifactView
	if err := json.NewDecoder(res2.Body).Decode(&one); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Old != "before" || one[0].New != "after" {
		t.Fatalf("asking for a path returned %+v, want the content", one)
	}
}

// A path outside the session's root stays absolute. Turning it into a trail of
// ".." reads as an escape when it is a file the user approved by name.
func TestRelTo(t *testing.T) {
	for _, c := range []struct{ root, path, want string }{
		{"/proj", "/proj/a/b.go", "a/b.go"},
		{"/proj", "/etc/hosts", "/etc/hosts"},
		{"", "/proj/a.go", "/proj/a.go"},
	} {
		if got := relTo(c.root, c.path); got != c.want {
			t.Errorf("relTo(%q, %q) = %q, want %q", c.root, c.path, got, c.want)
		}
	}
}

func TestUsageIsAlwaysAList(t *testing.T) {
	srv := testServer(t)
	res := srv.get(t, "/api/usage")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", res.Status)
	}
	var out []usageView
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("usage was null, not an empty list")
	}
}
