package alpha

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when no account matches the id.
var ErrNotFound = errors.New("alpha: account not found")

// queryTimeout bounds a single store operation.
const queryTimeout = 3 * time.Second

// maxRetries is how often a failed store operation is retried.
const maxRetries = 3

// Store keeps accounts in memory behind a mutex.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]Account
	nextID   int
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{accounts: map[string]Account{}}
}

// Create stores a new account and returns it with its assigned id.
func (s *Store) Create(ctx context.Context, a Account) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	a.ID = accountID(s.nextID)
	a.Created = time.Now()
	s.accounts[a.ID] = a
	return a, nil
}

// Get returns one account by id.
func (s *Store) Get(ctx context.Context, id string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return a, nil
}

// List returns up to limit accounts in unspecified order.
func (s *Store) List(ctx context.Context, limit int) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, limit)
	for _, a := range s.accounts {
		if len(out) == limit {
			break
		}
		out = append(out, a)
	}
	return out, nil
}

// Delete removes an account.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return ErrNotFound
	}
	delete(s.accounts, id)
	return nil
}

func accountID(n int) string {
	const prefix = "acct_"
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if digits == "" {
		digits = "0"
	}
	return prefix + digits
}
