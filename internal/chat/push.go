package chat

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/gigovich/aigem/internal/push"
)

// maxPushSubs bounds how many subscriptions are kept.
//
// A browser subscribes once and keeps the same endpoint for as long as its
// site data survives, so the honest number of rows is one per device. The cap
// exists for the failure mode where that is not true - a browser that clears
// storage on every close mints a new endpoint each time, and each dead one is
// only discovered by pushing to it - and it drops the oldest, which is the one
// least likely to still be somebody's phone.
const maxPushSubs = 20

// AddPushSub records a browser's subscription, replacing an identical endpoint
// rather than duplicating it: re-subscribing is what a page does on every load,
// and it must be free.
func (s *Store) AddPushSub(ctx context.Context, sub push.Subscription) error {
	if err := sub.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return s.write(ctx, "add push subscription", func(tx *sql.Tx, _ *[]Frame) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO push_subs (endpoint, p256dh, auth, created_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(endpoint) DO UPDATE SET
			   p256dh = excluded.p256dh, auth = excluded.auth,
			   created_at = excluded.created_at`,
			sub.Endpoint, sub.P256dh, sub.Auth, s.now().UnixMilli()); err != nil {
			return err
		}
		// created_at moves on a repeat, so it orders by when this browser last
		// said it was there rather than by when it first did. The cap keeps the
		// newest, and a phone that loads the page every day must not be the
		// first row evicted by a browser minting a fresh endpoint per load.
		//
		// Trimmed here rather than by a sweep: this is the only place the table
		// grows, and the cap is meaningless if it is enforced later.
		_, err := tx.ExecContext(ctx,
			`DELETE FROM push_subs WHERE endpoint IN (
			   SELECT endpoint FROM push_subs ORDER BY created_at DESC, endpoint LIMIT -1 OFFSET ?
			 )`, maxPushSubs)
		return err
	})
}

// PushSubs lists every subscription to deliver to.
func (s *Store) PushSubs(ctx context.Context) ([]push.Subscription, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT endpoint, p256dh, auth FROM push_subs ORDER BY created_at, endpoint`)
	if err != nil {
		return nil, fmt.Errorf("chat: read push subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []push.Subscription{}
	for rows.Next() {
		var sub push.Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// DeletePushSub forgets one endpoint. It is not an error to forget one that is
// already gone: the two callers are a browser unsubscribing and a push service
// answering 410, and both can arrive twice.
func (s *Store) DeletePushSub(ctx context.Context, endpoint string) error {
	return s.write(ctx, "delete push subscription", func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM push_subs WHERE endpoint = ?`, endpoint)
		return err
	})
}
