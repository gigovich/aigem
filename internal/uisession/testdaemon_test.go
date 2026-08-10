package uisession

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// testDaemon is the smallest thing that speaks the daemon's protocol. The real
// one lives in internal/web, which imports this package, so exercising Remote
// against it here would be a cycle. It is deliberately literal about the
// protocol rather than sharing code with the server: a client and a server that
// agree because they call the same function do not prove much.
type testDaemon struct {
	base  string
	id    string
	token string
}

func newTestDaemon(t *testing.T, sess *Local) *testDaemon {
	t.Helper()
	const id, token = "s-1", "tok"
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		meta := sess.Meta()
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": id, "title": meta.Title, "model": meta.Model},
		})
	})

	mux.HandleFunc("GET /api/sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		evs, err := sess.Replay(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		_ = json.NewEncoder(w).Encode(evs)
	})

	mux.HandleFunc("GET /api/sessions/{id}/socket", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		events, detach, err := sess.Subscribe(Client{Kind: r.URL.Query().Get("kind")}, since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			detach()
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range events {
				b, err := json.Marshal(ev)
				if err != nil {
					return
				}
				if err := wsutil.WriteServerText(conn, b); err != nil {
					return
				}
			}
			_ = conn.Close()
		}()
		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				break
			}
			if op != ws.OpText {
				continue
			}
			var in struct {
				Op       string   `json:"op"`
				Text     string   `json:"text"`
				ID       string   `json:"id"`
				Decision Decision `json:"decision"`
				Name     string   `json:"name"`
				Args     string   `json:"args"`
				Label    string   `json:"label"`
			}
			if json.Unmarshal(data, &in) != nil {
				continue
			}
			switch in.Op {
			case "submit":
				_ = sess.Submit(in.Text, nil)
			case "interrupt":
				sess.Interrupt()
			case "resolve":
				_ = sess.Resolve(in.ID, in.Decision, in.Label)
			case "command":
				_ = sess.Command(in.Name, in.Args)
			}
		}
		detach()
		<-done
		_ = conn.Close()
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testDaemon{base: srv.URL, id: id, token: token}
}
