package beta

import (
	"errors"
	"sync"
	"time"
)

// Errors the ledger can return for a payment attempt.
var (
	ErrNoInvoice   = errors.New("beta: no such invoice")
	ErrAlreadyPaid = errors.New("beta: invoice already paid")
	ErrDeclined    = errors.New("beta: payment declined")
)

// payAttempts is how often a declined charge is re-submitted before the
// invoice is left open for the client to retry.
const payAttempts = 5

// payBackoff is the wait before attempt n of a charge.
func payBackoff(n int) time.Duration {
	d := 500 * time.Millisecond
	for i := 1; i < n && d < 10*time.Second; i++ {
		d *= 2
	}
	return d
}

// Ledger holds invoices and settles them through a charger.
type Ledger struct {
	mu       sync.Mutex
	invoices map[string]Invoice
	next     int
	charge   func(Invoice) error
}

// NewLedger returns a ledger that settles through charge.
func NewLedger(charge func(Invoice) error) *Ledger {
	return &Ledger{invoices: map[string]Invoice{}, charge: charge}
}

// Add stores a new invoice and returns it with its id.
func (l *Ledger) Add(in Invoice) Invoice {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	in.ID = invoiceID(l.next)
	if in.Due.IsZero() {
		in.Due = time.Now().Add(14 * 24 * time.Hour)
	}
	l.invoices[in.ID] = in
	return in
}

// Open returns the unpaid invoices for one account, or for all accounts when
// account is empty.
func (l *Ledger) Open(account string) []Invoice {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Invoice
	for _, in := range l.invoices {
		if in.Paid {
			continue
		}
		if account == "" || in.Account == account {
			out = append(out, in)
		}
	}
	return out
}

// Pay settles an invoice, retrying a declined charge with a backoff.
func (l *Ledger) Pay(id string) error {
	l.mu.Lock()
	in, ok := l.invoices[id]
	l.mu.Unlock()
	if !ok {
		return ErrNoInvoice
	}
	if in.Paid {
		return ErrAlreadyPaid
	}

	var err error
	for attempt := 1; attempt <= payAttempts; attempt++ {
		if err = l.charge(in); err == nil {
			l.mu.Lock()
			in.Paid = true
			l.invoices[id] = in
			l.mu.Unlock()
			return nil
		}
		time.Sleep(payBackoff(attempt))
	}
	return ErrDeclined
}

// Void removes an unpaid invoice.
func (l *Ledger) Void(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	in, ok := l.invoices[id]
	if !ok {
		return ErrNoInvoice
	}
	if in.Paid {
		return ErrAlreadyPaid
	}
	delete(l.invoices, id)
	return nil
}

func invoiceID(n int) string {
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if digits == "" {
		digits = "0"
	}
	return "inv_" + digits
}
