// Package mattermost implements the bot Transport over Mattermost's REST v4 API
// and event WebSocket.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type apiError struct {
	msg string
}

func (e *apiError) Error() string { return e.msg }

// Client is a thin Mattermost REST v4 client authenticated with a bot token.
type Client struct {
	base  string // server URL without trailing slash
	token string
	http  *http.Client

	mu   sync.Mutex
	meID string // lazily cached own user id (the token's identity never changes)
}

// NewClient builds a client for the given server URL and bot token.
func NewClient(serverURL, token string) *Client {
	return &Client{
		base:  strings.TrimRight(serverURL, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &apiError{
			msg: fmt.Sprintf("mattermost %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg))),
		}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Me returns the authenticated bot's user id (verifies the token).
func (c *Client) Me(ctx context.Context) (string, error) {
	var u struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/me", nil, &u); err != nil {
		return "", err
	}
	return u.ID, nil
}

// TeamID resolves a team name to its id.
func (c *Client) TeamID(ctx context.Context, name string) (string, error) {
	var t struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/teams/name/"+name, nil, &t); err != nil {
		return "", err
	}
	return t.ID, nil
}

// ChannelIDByName resolves a channel to its id by matching the given name against
// either the URL slug or the display name, case-insensitively, among the channels
// the bot belongs to. Restricting the search to member channels means a match also
// proves membership and yields an actionable error when the bot was never invited -
// far clearer than the raw 404 the per-name endpoint returns for an unknown slug or
// an unjoined private channel.
func (c *Client) ChannelIDByName(ctx context.Context, teamID, name string) (string, error) {
	var channels []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/me/teams/"+teamID+"/channels", nil, &channels); err != nil {
		return "", err
	}
	// Models habitually write channels chat-style ("#Tasks"); the hash is not
	// part of the stored name, so strip it rather than failing membership.
	want := strings.ToLower(strings.TrimPrefix(name, "#"))
	for _, ch := range channels {
		if strings.ToLower(ch.Name) == want || strings.ToLower(ch.DisplayName) == want {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("bot is not a member of a channel named %q; invite it to the channel first", name)
}

// MemberChannelNames lists the display names of the channels the bot belongs to in the team,
// for surfacing the valid choices when a post names no channel or an unknown one.
func (c *Client) MemberChannelNames(ctx context.Context, teamID string) ([]string, error) {
	var channels []struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/me/teams/"+teamID+"/channels", nil, &channels); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		if ch.DisplayName != "" {
			names = append(names, ch.DisplayName)
		} else {
			names = append(names, ch.Name)
		}
	}
	return names, nil
}

// myID returns the bot's own user id, fetching it once and caching it; a failed
// fetch is not cached, so a transient error does not poison later calls.
func (c *Client) myID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.meID == "" {
		id, err := c.Me(ctx)
		if err != nil {
			return "", err
		}
		c.meID = id
	}
	return c.meID, nil
}

// DirectChannelWith opens (or returns the existing) direct-message channel between the bot
// and the named user. Mattermost's POST /channels/direct is idempotent, so this doubles as
// a lookup for an already-open DM.
func (c *Client) DirectChannelWith(ctx context.Context, username string) (string, error) {
	// Mattermost usernames are stored lowercase and the endpoint 400s on
	// anything else; models habitually write display-cased names ("Amiran")
	// and often an @ prefix, so normalize instead of failing the send.
	username = strings.ToLower(strings.TrimPrefix(username, "@"))
	var u struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/username/"+url.PathEscape(username), nil, &u); err != nil {
		return "", fmt.Errorf("resolve user %q: %w", username, err)
	}
	me, err := c.myID(ctx)
	if err != nil {
		return "", err
	}
	var ch struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/channels/direct", [2]string{me, u.ID}, &ch); err != nil {
		return "", fmt.Errorf("open direct channel with %q: %w", username, err)
	}
	return ch.ID, nil
}

// CreatePost posts a message to a channel; rootID, when non-empty, makes it a thread reply.
// It returns the new post's id, which for a root-level post is the id of the thread it starts.
func (c *Client) CreatePost(ctx context.Context, channelID, rootID, message string) (string, error) {
	body := map[string]string{"channel_id": channelID, "message": message}
	if rootID != "" {
		body["root_id"] = rootID
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/posts", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ThreadPost is one post in a fetched thread. ChannelID lets a caller confirm the thread really
// belongs to the channel it expected, which is what keeps a model-supplied root id from reaching a
// conversation the bot is not a member of.
type ThreadPost struct {
	UserID    string
	ChannelID string
	Message   string
	CreateAt  int64
}

// Thread returns the posts of the thread rooted at rootID, oldest first.
func (c *Client) Thread(ctx context.Context, rootID string) ([]ThreadPost, error) {
	var resp struct {
		Order []string `json:"order"`
		Posts map[string]struct {
			UserID    string `json:"user_id"`
			ChannelID string `json:"channel_id"`
			Message   string `json:"message"`
			CreateAt  int64  `json:"create_at"`
		} `json:"posts"`
	}
	path := "/posts/" + url.PathEscape(rootID) + "/thread"
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	// Build from the canonical `order` list so same-millisecond posts keep a deterministic
	// relative order, then stable-sort oldest-first.
	out := make([]ThreadPost, 0, len(resp.Order))
	for _, id := range resp.Order {
		p := resp.Posts[id]
		out = append(out, ThreadPost{
			UserID: p.UserID, ChannelID: p.ChannelID, Message: p.Message, CreateAt: p.CreateAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreateAt < out[j].CreateAt })
	return out, nil
}

// ChannelPost is one post fetched from a channel's recent history. RootID is empty for a
// top-level post and otherwise names the thread the post belongs to, which is what lets a reader
// ask for that thread next.
type ChannelPost struct {
	ID       string
	RootID   string
	UserID   string
	Message  string
	CreateAt int64
}

// ChannelPosts returns up to limit of a channel's most recent posts, oldest first.
func (c *Client) ChannelPosts(ctx context.Context, channelID string, limit int) ([]ChannelPost, error) {
	if limit <= 0 {
		limit = 60
	}
	var resp struct {
		Order []string `json:"order"`
		Posts map[string]struct {
			ID       string `json:"id"`
			RootID   string `json:"root_id"`
			UserID   string `json:"user_id"`
			Message  string `json:"message"`
			CreateAt int64  `json:"create_at"`
			Type     string `json:"type"`
		} `json:"posts"`
	}
	path := fmt.Sprintf("/channels/%s/posts?per_page=%d", url.PathEscape(channelID), limit)
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]ChannelPost, 0, len(resp.Order))
	for _, id := range resp.Order {
		p := resp.Posts[id]
		if strings.HasPrefix(p.Type, "system_") {
			continue // joins, leaves, header changes: noise for a reader
		}
		out = append(out, ChannelPost{
			ID: p.ID, RootID: p.RootID, UserID: p.UserID, Message: p.Message, CreateAt: p.CreateAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreateAt < out[j].CreateAt })
	return out, nil
}

// FileInfo describes an uploaded Mattermost file.
type FileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// FileInfo fetches a file's metadata (name, mime type, size) by id.
func (c *Client) FileInfo(ctx context.Context, id string) (FileInfo, error) {
	var info FileInfo
	if err := c.do(ctx, http.MethodGet, "/files/"+url.PathEscape(id)+"/info", nil, &info); err != nil {
		return FileInfo{}, err
	}
	return info, nil
}

// DownloadFile fetches a file's raw contents by id, refusing bodies over limit
// bytes so an oversized upload cannot balloon the turn.
func (c *Client) DownloadFile(ctx context.Context, id string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v4/files/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &apiError{
			msg: fmt.Sprintf("mattermost GET /files/%s: %s: %s", id, resp.Status, strings.TrimSpace(string(msg))),
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file %s exceeds the %d-byte download limit", id, limit)
	}
	return data, nil
}

// Usernames resolves user ids to usernames via POST /users/ids. Unknown ids are simply
// absent from the result map.
func (c *Client) Usernames(
	ctx context.Context,
	ids []string,
) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := c.do(ctx, http.MethodPost, "/users/ids", ids, &users); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.ID] = u.Username
	}
	return m, nil
}
