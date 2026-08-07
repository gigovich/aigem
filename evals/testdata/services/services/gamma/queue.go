package gamma

import (
	"sync"
	"time"
)

// States a queued notification moves through.
const (
	StateQueued    = "queued"
	StateDelivered = "delivered"
	StateDropped   = "dropped"
)

// deliveryAttempts bounds how often one notification is re-sent before it is
// dropped.
const deliveryAttempts = 6

// deliveryBackoff is the wait before attempt n of a delivery.
func deliveryBackoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < 60*time.Second; i++ {
		d *= 2
	}
	return d
}

// Queue holds notifications and delivers them through a sender.
type Queue struct {
	mu    sync.Mutex
	items map[string]Notification
	next  int
	send  func(Notification) error
}

// NewQueue returns a queue that delivers through send.
func NewQueue(send func(Notification) error) *Queue {
	return &Queue{items: map[string]Notification{}, send: send}
}

// Enqueue accepts a notification for delivery.
func (q *Queue) Enqueue(n Notification) Notification {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next++
	n.ID = notificationID(q.next)
	n.Queued = time.Now()
	n.State = StateQueued
	q.items[n.ID] = n
	return n
}

// Get returns one notification by id.
func (q *Queue) Get(id string) (Notification, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n, ok := q.items[id]
	return n, ok
}

// Pending returns the notifications still awaiting delivery.
func (q *Queue) Pending() []Notification {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Notification
	for _, n := range q.items {
		if n.State == StateQueued {
			out = append(out, n)
		}
	}
	return out
}

// Deliver attempts one notification, retrying with a backoff. A notification
// that never lands is marked dropped.
func (q *Queue) Deliver(id string) error {
	n, ok := q.Get(id)
	if !ok {
		return nil
	}
	var err error
	for attempt := 1; attempt <= deliveryAttempts; attempt++ {
		if err = q.send(n); err == nil {
			q.setState(id, StateDelivered)
			return nil
		}
		time.Sleep(deliveryBackoff(attempt))
	}
	q.setState(id, StateDropped)
	return err
}

func (q *Queue) setState(id, state string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n, ok := q.items[id]; ok {
		n.State = state
		q.items[id] = n
	}
}

func notificationID(n int) string {
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if digits == "" {
		digits = "0"
	}
	return "ntf_" + digits
}
