package chat

import (
	"context"
	"fmt"
	"strings"
)

// Transcript renders a thread the way a model should read it: oldest first,
// every line attributed by name.
//
// It lives here rather than in the bot package because who is speaking is part
// of what a message means - a hold from a person is not a suggestion from a
// peer bot - and the names are in this store. Having two callers build the
// block themselves is how they came to disagree about it.
//
// budget bounds the result in bytes. When the thread is longer than that, the
// oldest messages are dropped and the block says so: silently handing a model
// two thirds of a conversation is how it confidently answers the wrong
// question.
func (s *Store) Transcript(ctx context.Context, actor, threadID string, budget int) (string, error) {
	if budget <= 0 {
		budget = 20 << 10
	}
	view, err := s.ThreadFor(ctx, actor, threadID)
	if err != nil {
		return "", err
	}
	msgs, err := s.Messages(ctx, actor, threadID, 0, 500)
	if err != nil {
		return "", err
	}
	names, err := s.names(ctx)
	if err != nil {
		return "", err
	}

	// Messages arrive newest first, which is what a paging UI wants. Walking
	// them in that order is also how the budget keeps the newest rather than the
	// oldest, which is the half that matters.
	var lines []string
	used, dropped := 0, 0
	for _, m := range msgs {
		line := transcriptLine(m, names)
		if used+len(line) > budget && len(lines) > 0 {
			dropped = len(msgs) - len(lines)
			break
		}
		used += len(line)
		lines = append(lines, line)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Thread %s", threadID)
	if view.Title != "" {
		fmt.Fprintf(&b, " - %s", view.Title)
	}
	fmt.Fprintf(&b, "\nParticipants: %s\n", strings.Join(displayNames(view.Participants, names), ", "))
	if dropped > 0 {
		fmt.Fprintf(&b, "(%d older messages not shown)\n", dropped)
	}
	for _, line := range lines {
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func transcriptLine(m Message, names map[string]string) string {
	who := displayName(m.Author, names)
	if m.Kind == MsgSystem {
		return fmt.Sprintf("- %s\n", m.Body)
	}
	body := m.Body
	if m.Await {
		body += "  [awaiting your reply]"
	}
	if len(m.Attachments) > 0 {
		body += fmt.Sprintf("  [%d attachment(s)]", len(m.Attachments))
	}
	return fmt.Sprintf("%s: %s\n", who, body)
}

// names maps actor ids to display names, so a transcript is not a wall of
// "bot:amiran".
func (s *Store) names(ctx context.Context) (map[string]string, error) {
	actors, err := s.Actors(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(actors))
	for _, a := range actors {
		out[a.ID] = a.Name
	}
	return out, nil
}

// AuthorName resolves one actor id to a display name.
func (s *Store) AuthorName(ctx context.Context, actorID string) string {
	names, err := s.names(ctx)
	if err != nil {
		_, name := ActorName(actorID)
		return name
	}
	return displayName(actorID, names)
}

func displayName(id string, names map[string]string) string {
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	_, name := ActorName(id)
	return name
}

func displayNames(ids []string, names map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, displayName(id, names))
	}
	return out
}

// Digest renders an actor's threads as a short list, for a bot deciding which
// conversation to look at. It is the replacement for the ambient channel
// history a bot used to be handed unasked: on demand, scoped to its own
// threads, and saying what state each one is in.
func (s *Store) Digest(ctx context.Context, actor, state string, limit int) (string, error) {
	views, err := s.Inbox(ctx, actor, state, false, limit)
	if err != nil {
		return "", err
	}
	if len(views) == 0 {
		return "You are in no threads.", nil
	}
	names, err := s.names(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, v := range views {
		title := v.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%s  [%s]  %s\n", v.ID, v.State, title)
		fmt.Fprintf(&b, "    with %s\n", strings.Join(displayNames(v.Participants, names), ", "))
		if v.LastText != "" {
			fmt.Fprintf(&b, "    %s: %s\n", displayName(v.LastAuthor, names), oneLine(v.LastText))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// oneLine reduces a preview to a single bounded line, so a digest stays a list.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 120
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
