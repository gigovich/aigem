package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrGone reports a subscription the push service says no longer exists: the
// browser cleared its site data, the app was uninstalled, or the service
// expired it. The caller's only correct response is to forget it - retrying
// produces the same answer forever.
var ErrGone = errors.New("push: subscription is gone")

// ttl is how long a push service holds a message for a device that is offline.
//
// Twelve hours because the fact being announced is sticky: a thread stays in
// needs_you until the operator answers it. A phone that has been off longer
// than that reads the inbox rather than a stale interruption.
const ttl = 12 * time.Hour

// sendTimeout bounds one delivery. The notifier runs off the store's publish
// path, so a push service that accepts a connection and then says nothing must
// not become the fleet's problem.
const sendTimeout = 15 * time.Second

// Client delivers messages to push services.
type Client struct {
	keys *Keys
	http *http.Client
	now  func() time.Time
}

// NewClient builds a client. The HTTP client is its own rather than the default
// one, so a hung push service cannot hold a connection open indefinitely.
func NewClient(keys *Keys, opts ...Option) *Client {
	c := &Client{
		keys: keys,
		http: &http.Client{
			Timeout: sendTimeout,
			// RFC 8030 delivery is one POST to one endpoint, so a redirect is
			// never something to follow: following one lets the push service
			// aim the daemon at any host it likes - a tailnet peer, a metadata
			// service - and read the answer back out of the error this returns.
			// A chain ending in 410 would also delete the operator's
			// subscription on a stranger's say-so.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Option adjusts a client.
type Option func(*Client)

// WithTransport replaces how requests are carried. Its one caller is a test
// standing a push service up on a certificate no root store has heard of:
// subscriptions are https-only, so a stand-in has to serve TLS and something
// has to trust it.
//
// The transport rather than the whole client, so a test still exercises the
// timeout and the redirect policy that make this client safe to point at a
// stranger. Handing over the client replaced those too, and the first test
// written against it reported that redirects were followed - because they were,
// by the test's own client.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) { c.http.Transport = rt }
}

// PublicKey is what a browser subscribes with.
func (c *Client) PublicKey() string { return c.keys.Public }

// Message is one notification: the payload the service worker will receive,
// and the topic that lets a later message about the same thing replace an
// undelivered one.
type Message struct {
	Payload []byte
	// Topic is at most 32 characters from the URL-safe base64 alphabet, which
	// RFC 8030 requires and every thread id already satisfies. Empty means no
	// replacement.
	Topic string
}

// Send delivers one message to one subscription.
//
// Failures are reported, never retried here: the push service has its own
// store-and-forward for a device that is offline, so a failure that reaches
// this far is either the subscription being gone or the service being down,
// and neither is fixed by trying again inside a publish.
func (c *Client) Send(ctx context.Context, sub Subscription, msg Message) error {
	// Checked here as well as when it was stored: the store is not the only way
	// a subscription can reach this, and https is what keeps the endpoint - a
	// bearer capability to notify that browser - off the wire in clear.
	if err := sub.Validate(); err != nil {
		return err
	}
	if err := checkTopic(msg.Topic); err != nil {
		return err
	}
	body, err := Encrypt(sub, msg.Payload)
	if err != nil {
		return err
	}
	auth, err := c.keys.Authorization(sub.Endpoint, c.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: %s: %w", originOrEndpoint(sub.Endpoint), scrub(err))
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	// The daemon pushes for one reason - a bot is blocked on the operator - so
	// everything it sends is the kind a phone should wake for. Nothing else is
	// ever sent at this urgency because nothing else is ever sent.
	req.Header.Set("Urgency", "high")
	if msg.Topic != "" {
		req.Header.Set("Topic", msg.Topic)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("push: %s: %w", originOrEndpoint(sub.Endpoint), scrub(err))
	}
	defer func() { _ = resp.Body.Close() }()
	// Drained but not read back to the caller. A service explains a 400 in its
	// body, which would be worth having - but a WAF or CDN in front of one
	// answers with the request URL in it, and that URL is the capability to
	// notify this browser. Everything else in this package is scrubbed of it;
	// this was the one path that put it back.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	}
	return fmt.Errorf("push: %s answered %s", originOrEndpoint(sub.Endpoint), resp.Status)
}

func checkTopic(topic string) error {
	if topic == "" || ValidTopic(topic) {
		return nil
	}
	return fmt.Errorf("push: topic %q is not at most 32 characters of URL-safe base64", topic)
}

// ValidTopic reports whether a string is one RFC 8030 lets a Topic header
// carry: at most 32 characters from the URL-safe base64 alphabet. It is
// exported because the caller choosing topics has to be able to ask without
// discovering the answer as a failed delivery.
func ValidTopic(topic string) bool {
	if topic == "" || len(topic) > 32 {
		return false
	}
	for _, r := range topic {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// scrub takes the URL back out of a transport error.
//
// net/http wraps every failure in a *url.Error carrying the whole request URL,
// so an ordinary "connection refused" would write the subscription endpoint -
// which is the capability to notify that browser - into the daemon's log every
// time a phone's network hiccups. The wrapped cause says the same thing.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// originOrEndpoint keeps the subscription's path out of a log line: it is the
// capability to notify that browser, and a log is not the place for it.
func originOrEndpoint(endpoint string) string {
	if o, err := originOf(endpoint); err == nil {
		return o
	}
	return "push service"
}
