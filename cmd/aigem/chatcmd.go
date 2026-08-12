package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/chat"
)

const chatUsage = `usage:
  aigem chat threads [--state <state>] [--archived]   list the inbox
  aigem chat new --with <bot>[,<bot>] [--title t] <text>
                                                     open a thread; prints its id
  aigem chat send <thread> <text>                     post into a thread
  aigem chat read <thread>                            print a thread
  aigem chat tail [<thread>]                          follow live (blocks)
  aigem chat fleet                                    roster and who is running

States: needs_you, working, waiting, idle.
Threads are the fleet's conversations; the daemon is whichever "aigem bot start"
is running.`

// runChatCommand is the operator's terminal client for the fleet's
// conversations. It speaks the same HTTP API the browser does, so there is
// nothing here to keep in step with the daemon beyond the API itself.
func runChatCommand(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(chatUsage)
		return nil
	}
	c, err := dialChat()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "threads":
		return c.threads(ctx, args[1:])
	case "new":
		return c.newThread(ctx, args[1:])
	case "send":
		return c.send(ctx, args[1:])
	case "read":
		return c.read(ctx, args[1:])
	case "tail":
		return c.tail(ctx, args[1:])
	case "fleet":
		return c.fleet(ctx)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], chatUsage)
	}
}

// chatClient talks to the running fleet daemon.
type chatClient struct {
	base  string
	token string
}

func dialChat() (*chatClient, error) {
	state, running, err := chat.LoadState()
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, errors.New("no fleet is running; start one with `aigem bot start`")
	}
	return &chatClient{base: "http://" + state.Addr, token: state.Token}, nil
}

func (c *chatClient) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(res.Body).Decode(&e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *chatClient) threads(ctx context.Context, args []string) error {
	q := url.Values{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--state":
			if i+1 >= len(args) {
				return errors.New("--state needs a value")
			}
			i++
			q.Set("state", args[i])
		case "--archived":
			q.Set("archived", "true")
		default:
			return fmt.Errorf("unknown flag %q\n\n%s", args[i], chatUsage)
		}
	}
	var views []chat.ThreadView
	if err := c.do(ctx, http.MethodGet, "/api/chat/threads?"+q.Encode(), nil, &views); err != nil {
		return err
	}
	if len(views) == 0 {
		fmt.Println("no threads")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, v := range views {
		unread := ""
		if v.Unread > 0 {
			unread = fmt.Sprintf("%d unread", v.Unread)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			v.ID, v.State, unread, names(v.Participants), title(v))
	}
	return w.Flush()
}

func (c *chatClient) newThread(ctx context.Context, args []string) error {
	var with []string
	var head string
	text := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--with":
			if i+1 >= len(args) {
				return errors.New("--with needs at least one bot name")
			}
			i++
			for _, name := range strings.Split(args[i], ",") {
				if name = strings.TrimSpace(name); name != "" {
					with = append(with, chat.BotActor(name))
				}
			}
		case "--title":
			if i+1 >= len(args) {
				return errors.New("--title needs a value")
			}
			i++
			head = args[i]
		default:
			text = append(text, args[i])
		}
	}
	if len(with) == 0 {
		return errors.New("name at least one bot with --with")
	}
	body := strings.Join(text, " ")
	if head == "" {
		head = oneLine(body)
	}
	var view chat.ThreadView
	if err := c.do(ctx, http.MethodPost, "/api/chat/threads", map[string]any{
		"title": head, "participants": with, "text": body,
	}, &view); err != nil {
		return err
	}
	fmt.Println(view.ID)
	return nil
}

func (c *chatClient) send(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: aigem chat send <thread> <text>")
	}
	return c.do(ctx, http.MethodPost, "/api/chat/threads/"+args[0]+"/messages",
		map[string]any{"text": strings.Join(args[1:], " ")}, nil)
}

func (c *chatClient) read(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: aigem chat read <thread>")
	}
	var msgs []chat.Message
	if err := c.do(ctx, http.MethodGet,
		"/api/chat/threads/"+args[0]+"/messages?limit=200", nil, &msgs); err != nil {
		return err
	}
	// Newest first over the wire, because that is what a paging UI wants; a
	// transcript reads the other way.
	for i := len(msgs) - 1; i >= 0; i-- {
		printMessage(msgs[i])
	}
	return nil
}

// tail follows the live stream. With a thread id it also prints that thread's
// agent timeline, which is the part Mattermost could never show.
func (c *chatClient) tail(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	watch := ""
	if len(args) > 0 {
		watch = args[0]
	}
	u := "ws" + strings.TrimPrefix(c.base, "http") + "/api/chat/socket?token=" +
		url.QueryEscape(c.token)
	conn, _, _, err := ws.Dial(ctx, u)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if watch != "" {
		op, err := json.Marshal(map[string]any{"op": "watch", "thread": watch})
		if err != nil {
			return err
		}
		if err := wsutil.WriteClientText(conn, op); err != nil {
			return err
		}
	}
	// Closing the connection is what unblocks the read below; the read itself
	// has no context to cancel.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var f chat.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch {
		case f.Stream == chat.StreamMessage && f.Message != nil:
			if watch == "" || f.ThreadID == watch {
				printMessage(*f.Message)
			}
		case f.Stream == chat.StreamEvent:
			printEvent(f.Event)
		case f.Stream == chat.StreamDesync:
			fmt.Printf("-- reconnect from %d: the stream ran ahead of this client\n", f.From)
		}
	}
}

func (c *chatClient) fleet(ctx context.Context) error {
	var actors []chat.Actor
	if err := c.do(ctx, http.MethodGet, "/api/chat/fleet", nil, &actors); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, a := range actors {
		state := "stopped"
		switch {
		case a.Kind == chat.KindHuman:
			state = "you"
		case a.Present:
			state = "running"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.Name, a.Role, state)
	}
	return w.Flush()
}

func printMessage(m chat.Message) {
	_, who := chat.ActorName(m.Author)
	stamp := m.Created.Local().Format("15:04")
	if m.Kind == chat.MsgSystem {
		fmt.Printf("%s  -- %s\n", stamp, m.Body)
		return
	}
	mark := ""
	if m.Await {
		mark = " [needs you]"
	}
	fmt.Printf("%s  %s%s: %s\n", stamp, who, mark, m.Body)
}

// printEvent renders one step of a turn on a single line. The terminal gets the
// shape of the work - which tool, on what - and the browser gets the rest.
func printEvent(payload []byte) {
	var ev struct {
		Kind  string          `json:"kind"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Args  json.RawMessage `json:"args"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Kind {
	case "tool_start":
		fmt.Printf("        %s %s\n", ev.Name, oneLine(string(ev.Args)))
	case "tool_end":
		if ev.Error != "" {
			fmt.Printf("        %s failed: %s\n", ev.Name, oneLine(ev.Error))
		}
	case "notice", "error":
		fmt.Printf("        %s\n", oneLine(ev.Text))
	}
}

// oneLine reduces a value to one bounded line. The package already has a
// firstLine, but it marks a truncated line with " ..." and does not bound the
// length; a tool argument or a thread preview needs both.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 100
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func names(actors []string) string {
	out := make([]string, 0, len(actors))
	for _, a := range actors {
		kind, name := chat.ActorName(a)
		if kind == chat.KindHuman {
			continue
		}
		out = append(out, name)
	}
	return strings.Join(out, ",")
}

func title(v chat.ThreadView) string {
	if v.Title != "" {
		return v.Title
	}
	if v.LastText != "" {
		return oneLine(v.LastText)
	}
	return "(no messages)"
}
