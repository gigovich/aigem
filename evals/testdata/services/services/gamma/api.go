// Package gamma serves the notification API.
package gamma

import (
	"encoding/json"
	"net/http"
	"time"
)

// Channels are the delivery backends gamma can target.
var Channels = []string{"email", "sms", "webhook"}

// sendTimeout bounds one delivery attempt to a channel.
const sendTimeout = 8 * time.Second

// Notification is one queued message.
type Notification struct {
	ID      string    `json:"id"`
	Channel string    `json:"channel"`
	To      string    `json:"to"`
	Body    string    `json:"body"`
	Queued  time.Time `json:"queued"`
	State   string    `json:"state"`
}

// Server exposes the notification endpoints.
type Server struct {
	queue *Queue
	cfg   Config
}

// NewServer wires a server to its queue.
func NewServer(queue *Queue, cfg Config) *Server {
	return &Server{queue: queue, cfg: cfg}
}

// Routes registers gamma's public endpoints.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/notify", s.notify)
	mux.HandleFunc("GET /v1/notify/{id}", s.status)
	mux.HandleFunc("GET /v1/channels", s.listChannels)
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	var n Notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if n.Channel == "" {
		n.Channel = s.cfg.DefaultChannel
	}
	if !knownChannel(n.Channel) {
		http.Error(w, "unknown channel", http.StatusBadRequest)
		return
	}
	if n.To == "" {
		http.Error(w, "recipient is required", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, s.queue.Enqueue(n))
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	n, ok := s.queue.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) listChannels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Channels)
}

func knownChannel(name string) bool {
	for _, c := range Channels {
		if c == name {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
