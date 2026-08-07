// Package alpha serves the account API.
package alpha

import (
	"encoding/json"
	"net/http"
	"time"
)

// MaxPageSize caps how many accounts one list call returns.
const MaxPageSize = 200

// readTimeout bounds a single inbound request.
const readTimeout = 15 * time.Second

// Account is the API's view of a customer record.
type Account struct {
	ID      string    `json:"id"`
	Email   string    `json:"email"`
	Plan    string    `json:"plan"`
	Created time.Time `json:"created"`
}

// Server exposes the account endpoints over an account store.
type Server struct {
	store *Store
	cfg   Config
}

// NewServer wires a server to its store.
func NewServer(store *Store, cfg Config) *Server {
	return &Server{store: store, cfg: cfg}
}

// Routes registers alpha's public endpoints.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/accounts", s.listAccounts)
	mux.HandleFunc("POST /v1/accounts", s.createAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	mux.HandleFunc("DELETE /v1/accounts/{id}", s.deleteAccount)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	limit := s.cfg.DefaultPageSize
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	accounts, err := s.store.List(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var a Account
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if a.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}
	created, err := s.store.Create(r.Context(), a)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
