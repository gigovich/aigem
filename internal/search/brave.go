package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// braveEndpoint is the Brave Search web-search API.
const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// brave queries the Brave Search API with a subscription token.
type brave struct {
	apiKey   string
	endpoint string
	http     *http.Client
}

func newBrave(apiKey string) *brave {
	return &brave{apiKey: apiKey, endpoint: braveEndpoint, http: &http.Client{Timeout: 20 * time.Second}}
}

// braveResponse captures only the web-results we surface; other blocks (news,
// videos, infobox) are ignored.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// Search implements Searcher against the Brave web-search API.
func (b *brave) Search(ctx context.Context, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.apiKey)

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read brave response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, braveStatusError(resp.StatusCode, body)
	}

	var parsed braveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse brave response: %w", err)
	}
	results := make([]Result, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		results = append(results, Result{
			Title:       cleanText(r.Title),
			URL:         r.URL,
			Description: cleanText(r.Description),
		})
	}
	return results, nil
}

// braveStatusError turns a non-200 into an actionable message, calling out the
// common auth/rate-limit cases without leaking the key.
func braveStatusError(code int, body []byte) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("brave search rejected the API key (HTTP %d) - check it with `aigem search status`", code)
	case http.StatusTooManyRequests:
		return fmt.Errorf("brave search rate limit reached (HTTP 429) - wait and retry")
	default:
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("brave search failed (HTTP %d): %s", code, snippet)
	}
}

// cleanText strips the <strong> highlight tags Brave wraps around query matches
// and collapses whitespace.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "<strong>", "")
	s = strings.ReplaceAll(s, "</strong>", "")
	return strings.Join(strings.Fields(s), " ")
}
