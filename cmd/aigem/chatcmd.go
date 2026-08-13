package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/chat"
)

const chatUsage = `usage:
  aigem chat threads [--state <s>] [--archived]        list the inbox
  aigem chat new --with <bot>[,<bot>] [--title t] <text>
                                                      open a thread; prints its id
  aigem chat send <thread> <text>                      post; prints the message seq
  aigem chat read <thread> [--limit n] [--before seq]  print a thread and its cost
  aigem chat search <words> [--limit n]                search the threads you are in
  aigem chat tail [<thread>]                           follow live (blocks)
  aigem chat fleet                                     roster and who is running

States: needs_you, working, waiting, idle.
Listing commands take --json for scripting.
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
	// The verb is checked before the daemon is dialled, so a typo reports the
	// typo rather than "no fleet is running".
	run, ok := chatVerbs[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q\n\n%s", args[0], chatUsage)
	}
	c, err := dialChat()
	if err != nil {
		return err
	}
	return run(c, context.Background(), args[1:])
}

var chatVerbs = map[string]func(*chatClient, context.Context, []string) error{
	"threads": (*chatClient).threads,
	"new":     (*chatClient).newThread,
	"send":    (*chatClient).send,
	"read":    (*chatClient).read,
	"tail":    (*chatClient).tail,
	"search":  (*chatClient).search,
	"fleet":   (*chatClient).fleet,
}

// flags builds a parser for one subcommand, so every one of them accepts the
// same forms the rest of the CLI does - "--x v", "--x=v", "-h", and "--" to end
// the flags. Hand-rolling this per verb is how they came to disagree.
func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("aigem chat "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
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
	fs := flags("threads")
	state := fs.String("state", "", "only threads in this state")
	archived := fs.Bool("archived", false, "the archive instead of the inbox")
	asJSON := fs.Bool("json", false, "print the raw response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	if *state != "" {
		q.Set("state", *state)
	}
	if *archived {
		q.Set("archived", "true")
	}
	var views []chat.ThreadView
	if err := c.do(ctx, http.MethodGet, "/api/chat/threads?"+q.Encode(), nil, &views); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(views)
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
	fs := flags("new")
	bots := fs.String("with", "", "comma-separated bot names to open the thread with")
	head := fs.String("title", "", "thread title (default: the first line of the text)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var with []string
	for _, name := range strings.Split(*bots, ",") {
		if name = strings.TrimSpace(name); name != "" {
			with = append(with, chat.BotActor(name))
		}
	}
	if len(with) == 0 {
		return errors.New("name at least one bot with --with")
	}
	body := strings.Join(fs.Args(), " ")
	title := *head
	if title == "" {
		title = oneLine(body)
	}
	var view chat.ThreadView
	if err := c.do(ctx, http.MethodPost, "/api/chat/threads", map[string]any{
		"title": title, "participants": with, "text": body,
	}, &view); err != nil {
		return err
	}
	fmt.Println(view.ID)
	return nil
}

// send posts into a thread and prints the message's sequence number, which is
// what a script needs to follow up on what it just wrote.
func (c *chatClient) send(ctx context.Context, args []string) error {
	fs := flags("send")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: aigem chat send <thread> <text>")
	}
	var m chat.Message
	if err := c.do(ctx, http.MethodPost, "/api/chat/threads/"+rest[0]+"/messages",
		map[string]any{"text": strings.Join(rest[1:], " ")}, &m); err != nil {
		return err
	}
	fmt.Println(m.Seq)
	return nil
}

func (c *chatClient) search(ctx context.Context, args []string) error {
	fs := flags("search")
	limit := fs.Int("limit", 20, "how many hits to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		return errors.New("usage: aigem chat search <words>")
	}
	q := url.Values{}
	q.Set("q", strings.Join(fs.Args(), " "))
	q.Set("limit", strconv.Itoa(*limit))
	var hits []chat.Message
	if err := c.do(ctx, http.MethodGet, "/api/chat/search?"+q.Encode(), nil, &hits); err != nil {
		return err
	}
	for _, m := range hits {
		fmt.Printf("%s  ", m.Thread)
		printMessage(m)
	}
	return nil
}

func (c *chatClient) read(ctx context.Context, args []string) error {
	fs := flags("read")
	limit := fs.Int("limit", 200, "how many messages to print")
	before := fs.Uint64("before", 0, "print the messages below this sequence")
	asJSON := fs.Bool("json", false, "print the raw response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) < 1 {
		return errors.New("usage: aigem chat read <thread>")
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(*limit))
	if *before > 0 {
		q.Set("before", strconv.FormatUint(*before, 10))
	}
	var page chat.Page[chat.Message]
	if err := c.do(ctx, http.MethodGet,
		"/api/chat/threads/"+fs.Args()[0]+"/messages?"+q.Encode(), nil, &page); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(page)
	}
	if page.More {
		// Said before the transcript rather than after it, where the reader is
		// already at the newest message and has stopped looking up.
		fmt.Printf("(older messages not shown; --before %d for the rest)\n\n", page.Cursor)
	}
	// Newest first over the wire, because that is what a paging UI wants; a
	// transcript reads the other way.
	for i := len(page.Items) - 1; i >= 0; i-- {
		printMessage(page.Items[i])
	}
	c.printSpend(ctx, fs.Args()[0])
	return nil
}

// printSpend closes a transcript with what the work in the thread cost. A
// thread is a task, and what a task cost the account is part of reading it -
// until now that number reached only the bot's log, where it was attributed to
// a process rather than to any particular piece of work.
//
// The line is labelled as the thread's, because it is: --limit and --before
// page the transcript, and a total that silently followed them would be a
// different number every time the same thread was read.
//
// A failure to read it is not a failure to read the thread, so it is swallowed:
// the transcript is what was asked for.
func (c *chatClient) printSpend(ctx context.Context, threadID string) {
	var sp chat.Spend
	if err := c.do(ctx, http.MethodGet,
		"/api/chat/threads/"+threadID+"/spend", nil, &sp); err != nil {
		return
	}
	if sp.Usage.IsZero() {
		return
	}
	line := fmt.Sprintf("thread total: %s · %s in",
		countOf(sp.Turns, "turn"), humanTokens(sp.Usage.InputTokens))
	if sp.Usage.CachedTokens > 0 {
		line += fmt.Sprintf(" (%s cached)", humanTokens(sp.Usage.CachedTokens))
	}
	line += fmt.Sprintf(" · %s out · %s",
		humanTokens(sp.Usage.OutputTokens), countOf(sp.Usage.Calls, "call"))
	if sp.Usage.Uncounted > 0 {
		// Named as well as counted: Calls includes these, and a reader comparing
		// the figure to the bot log - where it does not - needs to see how many
		// they were.
		line += fmt.Sprintf(" (%d uncounted)", sp.Usage.Uncounted)
	}
	if len(sp.Models) > 0 {
		line += " · " + strings.Join(sp.Models, ", ")
	}
	fmt.Printf("--\n%s\n", line)
}

func humanTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	// The bounds are shy of the round number by half a decimal place, because
	// past that "%.1f" prints "1000.0k", which is not how anyone writes a
	// million.
	case n < 999_950:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 999_950_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// tail follows the live stream. With a thread id it also prints that thread's
// agent timeline, which is the part Mattermost could never show.
func (c *chatClient) tail(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fs := flags("tail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	watch := ""
	if len(fs.Args()) > 0 {
		watch = fs.Args()[0]
	}
	// The token goes in a header, not the query string: only a browser cannot
	// set one on a handshake, and a URL carrying a credential lands in proxy
	// logs and in every process listing.
	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Authorization": []string{"Bearer " + c.token},
	})}
	conn, _, _, err := d.Dial(ctx, "ws"+strings.TrimPrefix(c.base, "http")+"/api/chat/socket")
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
			if watch != "" && f.ThreadID != watch {
				continue
			}
			if watch == "" {
				// Following everything interleaves threads, so each line has to
				// say which one it belongs to.
				fmt.Printf("%s  ", f.ThreadID)
			}
			printMessage(*f.Message)
		case f.Stream == chat.StreamEvent:
			printEvent(f.Event)
		case f.Stream == chat.StreamDesync:
			fmt.Printf("-- dropped at %d: this client fell behind; reconnect from there\n", f.From)
		case f.Stream == chat.StreamTruncated:
			fmt.Printf("-- history continues below %d; `aigem chat read` has the rest\n", f.From)
		}
	}
}

func (c *chatClient) fleet(ctx context.Context, args []string) error {
	fs := flags("fleet")
	asJSON := fs.Bool("json", false, "print the raw response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var members []chat.FleetMember
	if err := c.do(ctx, http.MethodGet, "/api/chat/fleet", nil, &members); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(members)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "bot\trole\tstate\tthreads\theartbeat\tnext job\tmodel")
	for _, m := range members {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", m.Name, m.Role, fleetState(m), m.Threads,
			heartbeatOf(m.Live), nextJobOf(m.Live), dash(modelOf(m.Live)))
	}
	return w.Flush()
}

// fleetState is the one word for what a bot is doing. Working comes from the
// store, so it is the same answer the inbox gives; the rest comes from the
// daemon, and a bot no daemon reported gets "-" rather than a guess.
func fleetState(m chat.FleetMember) string {
	switch {
	case m.Kind == chat.KindHuman:
		return "you"
	case m.Working:
		return "working"
	case m.Live == nil:
		return "-"
	case !m.Live.Running:
		return "stopped"
	default:
		return "idle"
	}
}

func heartbeatOf(l *chat.LiveBot) string {
	if l == nil || !l.Running || l.Heartbeat == "" {
		return "-"
	}
	return fmt.Sprintf("%s (t%d)", l.Heartbeat, l.Tier)
}

func nextJobOf(l *chat.LiveBot) string {
	if l == nil || l.NextJob == "" || l.NextRun == nil {
		return "-"
	}
	return l.NextJob + " " + l.NextRun.Local().Format("15:04")
}

func modelOf(l *chat.LiveBot) string {
	if l == nil {
		return ""
	}
	return l.Model
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printJSON is what a script reads. Every listing verb takes --json so driving
// the fleet from a script does not mean parsing a table.
func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
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
