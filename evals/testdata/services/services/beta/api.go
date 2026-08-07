// Package beta serves the billing API.
package beta

import (
	"encoding/json"
	"net/http"
	"time"
)

// RetryAfter is the backoff advertised to a client whose payment was rejected.
const RetryAfter = 30 * time.Second

// requestTimeout bounds a single inbound request.
const requestTimeout = 20 * time.Second

// Invoice is one billable document.
type Invoice struct {
	ID       string    `json:"id"`
	Account  string    `json:"account"`
	Cents    int64     `json:"cents"`
	Currency string    `json:"currency"`
	Due      time.Time `json:"due"`
	Paid     bool      `json:"paid"`
}

// Server exposes the billing endpoints.
type Server struct {
	ledger *Ledger
	cfg    Config
}

// NewServer wires a server to its ledger.
func NewServer(ledger *Ledger, cfg Config) *Server {
	return &Server{ledger: ledger, cfg: cfg}
}

// Routes registers beta's public endpoints.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/invoices", s.listInvoices)
	mux.HandleFunc("POST /v1/invoices", s.createInvoice)
	mux.HandleFunc("POST /v1/invoices/{id}/pay", s.payInvoice)
	mux.HandleFunc("POST /v1/invoices/{id}/void", s.voidInvoice)
}

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
	invoices := s.ledger.Open(r.URL.Query().Get("account"))
	writeJSON(w, http.StatusOK, invoices)
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	var in Invoice
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if in.Cents <= 0 {
		http.Error(w, "amount must be positive", http.StatusBadRequest)
		return
	}
	if in.Currency == "" {
		in.Currency = s.cfg.DefaultCurrency
	}
	writeJSON(w, http.StatusCreated, s.ledger.Add(in))
}

func (s *Server) payInvoice(w http.ResponseWriter, r *http.Request) {
	err := s.ledger.Pay(r.PathValue("id"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case err == ErrAlreadyPaid:
		http.Error(w, err.Error(), http.StatusConflict)
	case err == ErrDeclined:
		w.Header().Set("Retry-After", "30")
		http.Error(w, err.Error(), http.StatusPaymentRequired)
	default:
		http.Error(w, err.Error(), http.StatusNotFound)
	}
}

func (s *Server) voidInvoice(w http.ResponseWriter, r *http.Request) {
	if err := s.ledger.Void(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
