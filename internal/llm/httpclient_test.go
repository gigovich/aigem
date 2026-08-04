package llm

import "testing"

// TestBackendsShareOneHTTPClient pins the reason the shared client exists: with a bot per
// backend in one process, a client each would mean a connection pool each, and every bot paying
// its own TLS handshakes to the same provider host.
func TestBackendsShareOneHTTPClient(t *testing.T) {
	a := NewClient(ClientConfig{BaseURL: "https://example.invalid"})
	b := NewClient(ClientConfig{BaseURL: "https://example.invalid"})
	if a.HTTP != b.HTTP {
		t.Fatal("two backends built their own HTTP clients, so they do not share a connection pool")
	}
	r := NewResponsesClient(ResponsesConfig{})
	if r.HTTP != a.HTTP {
		t.Fatal("the Responses adapter does not share the process client")
	}
	if a.HTTP.Timeout != callTimeout {
		t.Fatalf("client timeout = %v, want %v", a.HTTP.Timeout, callTimeout)
	}
}
