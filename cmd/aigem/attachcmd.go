package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gigovich/aigem/internal/uisession"
	"github.com/gigovich/aigem/internal/web"
)

const attachUsage = `usage:
  aigem attach [<session-id>]   follow a conversation running in the daemon

With no id, the daemon's current conversation is used. Type to send; an
approval is answered by its number. Detaching leaves the conversation running,
which is the point of it living in the daemon.`

// runAttachCommand connects a terminal to a conversation the daemon owns. It
// renders the same event stream the browser does - the stream is the protocol,
// so there is nothing here to keep in step with the daemon beyond it.
func runAttachCommand(args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println(attachUsage)
			return nil
		}
	}
	state, running, err := web.LoadState()
	if err != nil {
		return err
	}
	if !running {
		return errors.New("no daemon is running; start one with `aigem web run`")
	}
	base := "http://" + state.Addr

	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	if id == "" {
		id, err = firstSession(base, state.Token)
		if err != nil {
			return err
		}
	}

	sess, err := uisession.Dial(base, id, state.Token)
	if err != nil {
		return err
	}
	defer sess.Close()

	events, detach, err := sess.Subscribe(uisession.Client{Kind: "tui", Label: hostLabel()}, 0)
	if err != nil {
		return err
	}
	defer detach()

	fmt.Printf("attached to %s on %s - Ctrl+C to detach\n\n", id, state.Addr)
	go renderStream(events, sess)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	lines := make(chan string)
	go func() {
		defer close(lines)
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- in.Text()
		}
	}()

	for {
		select {
		case <-stop:
			fmt.Println("\ndetached; the conversation keeps running")
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := handleAttachLine(sess, strings.TrimSpace(line)); err != nil {
				fmt.Fprintln(os.Stderr, "  ⚠", err)
			}
		}
	}
}

// handleAttachLine routes a typed line: a number answers the open approval, a
// slash word is a command the daemon's session handles, anything else is a
// message.
func handleAttachLine(sess *uisession.Remote, line string) error {
	if line == "" {
		return nil
	}
	if id, req := sess.Pending(); req != nil {
		if n := optionIndex(line, len(req.Options)); n >= 0 {
			return sess.Resolve(id, req.Options[n].Value, "attach")
		}
	}
	if line == "/interrupt" {
		sess.Interrupt()
		return nil
	}
	if after, ok := strings.CutPrefix(line, "/"); ok {
		name, args, _ := strings.Cut(after, " ")
		return sess.Command(name, strings.TrimSpace(args))
	}
	return sess.Submit(line, nil)
}

// optionIndex reads "1".."n" as a choice, and -1 for anything else, so typing a
// message that happens to start with a digit is not mistaken for an answer.
func optionIndex(line string, n int) int {
	if len(line) != 1 || line[0] < '1' || line[0] > '9' {
		return -1
	}
	i := int(line[0] - '1')
	if i >= n {
		return -1
	}
	return i
}

func renderStream(events <-chan uisession.Event, sess *uisession.Remote) {
	for ev := range events {
		switch ev.Kind {
		case uisession.KindUserMessage:
			fmt.Printf("\n› %s\n", ev.Text)
		case uisession.KindContent:
			fmt.Print(ev.Text)
		case uisession.KindAssistantMessage:
			fmt.Printf("%s\n", strings.TrimRight(ev.Text, "\n"))
		case uisession.KindToolStart:
			fmt.Printf("\n  · %s %s\n", ev.Name, oneLineArgs(ev.Args))
		case uisession.KindToolEnd:
			if ev.Error != "" {
				fmt.Printf("  ⤷ error: %s\n", ev.Error)
				continue
			}
			fmt.Printf("  ⤷ %s\n", firstLine(ev.Text))
		case uisession.KindAgentStart:
			fmt.Printf("  ▸ %s: %s\n", ev.Agent, firstLine(ev.Text))
		case uisession.KindSubToolStart:
			fmt.Printf("    ▸ %s:%s\n", ev.Agent, ev.Name)
		case uisession.KindNotice, uisession.KindBudgetExhausted:
			fmt.Printf("\n  ⚠ %s\n", ev.Text)
		case uisession.KindApprovalRequest:
			printApproval(ev)
		case uisession.KindApprovalResolved:
			fmt.Printf("  → %s (by %s)\n", ev.Decision, ev.By)
		case uisession.KindTurnEnd:
			switch {
			case ev.Interrupted:
				fmt.Print("\n  ⚠ interrupted\n\n")
			case ev.Error != "":
				fmt.Printf("\n  ⚠ %s\n\n", ev.Error)
			default:
				fmt.Print("\n\n")
			}
		}
	}
	// The stream ends when the daemon goes away or this client detaches; the
	// caller is already on its way out either way.
	_ = sess
}

func printApproval(ev uisession.Event) {
	a := ev.Approval
	if a == nil {
		return
	}
	what := a.Tool
	if a.Kind == uisession.ApprovalPath {
		verb := "read"
		if a.Write {
			verb = "modify"
		}
		what = fmt.Sprintf("%s wants to %s %s", a.Tool, verb, a.Path)
	}
	fmt.Printf("\n  ? %s\n", what)
	for i, o := range a.Options {
		fmt.Printf("    %d) %s\n", i+1, o.Label)
	}
	fmt.Print("  answer with a number: ")
}

func oneLineArgs(args any) string {
	s := fmt.Sprintf("%s", args)
	return firstLine(truncateTo(s, 120))
}

func truncateTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstSession(base, token string) (string, error) {
	r, err := uisession.ListSessions(base, token)
	if err != nil {
		return "", err
	}
	if len(r) == 0 {
		return "", errors.New("the daemon has no conversation open yet")
	}
	return r[0], nil
}

func hostLabel() string {
	h, err := os.Hostname()
	if err != nil {
		return "terminal"
	}
	return h
}
