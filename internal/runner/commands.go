package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/skill"
)

// HandleCommands registers the slash commands that run inside the conversation
// itself: a compaction, a skill, an MCP prompt. Each is a turn, so it is
// refused while one is running, like a typed message would be.
//
// The rest of the catalogue - /new, /model, /login, /resume - is a front-end's
// own to carry out, and a front-end that sends one here is told it is unknown.
func (s *Session) HandleCommands(skills *skill.Registry, m *mcp.Manager) {
	l := s.Local
	l.Handle("compact", func(args string) error {
		display := strings.TrimSpace("/compact " + args)
		return l.Run(display, l.Meta().Title, func(ctx context.Context, ev agent.Events) (string, error) {
			_, err := l.Agent().Compact(ctx, args, ev)
			return "", err
		})
	})
	l.Handle("skill:", func(rest string) error {
		name, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
		args = strings.TrimSpace(args)
		var sk *skill.Skill
		if skills != nil {
			sk, _ = skills.Get(name)
		}
		if sk == nil || !sk.UserInvocable {
			return fmt.Errorf("no such skill: %s", skill.DisplaySafe(name))
		}
		display := strings.TrimSpace(name + " " + args)
		return l.Run("/skill:"+display, session.Title("/skill:"+display),
			func(ctx context.Context, ev agent.Events) (string, error) {
				body, err := sk.Render(ctx, args, skill.RenderOpts{SessionID: l.Meta().ID})
				if err != nil {
					return "", err
				}
				return l.Agent().Run(ctx, body, ev)
			})
	})
	for _, p := range m.PromptCommands() {
		l.Handle(p.Name, func(args string) error {
			display := strings.TrimSpace(p.Name + " " + args)
			return l.Run("/"+display, session.Title("/"+display),
				func(ctx context.Context, ev agent.Events) (string, error) {
					body, err := m.RenderPrompt(ctx, p.Name, args)
					if err != nil {
						return "", err
					}
					return l.Agent().Run(ctx, body, ev)
				})
		})
	}
}
