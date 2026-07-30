package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// PromptCommand describes an MCP prompt exposed as a slash command.
type PromptCommand struct {
	Name string // namespaced: mcp__<server>__<prompt>
	Desc string
	Args []string // declared argument names, for the hint
}

// PromptCommands returns one entry per prompt across connected servers.
func (m *Manager) PromptCommands() []PromptCommand {
	if m == nil {
		return nil
	}
	var out []PromptCommand
	for _, sc := range m.servers {
		if !sc.connected {
			continue
		}
		for _, p := range sc.prompts {
			args := make([]string, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				args = append(args, a.Name)
			}
			out = append(out, PromptCommand{
				Name: toolName(sc.name, p.Name),
				Desc: p.Description,
				Args: args,
			})
		}
	}
	return out
}

// RenderPrompt fetches the named prompt (mcp__<server>__<prompt>) with the
// positional argument string mapped onto its declared arguments, and returns
// the prompt messages flattened into text to inject as a turn.
func (m *Manager) RenderPrompt(ctx context.Context, name, argString string) (string, error) {
	sc, p := m.findPrompt(name)
	if p == nil {
		return "", fmt.Errorf("no such MCP prompt: %s", name)
	}
	fields := splitArgs(argString)
	args := map[string]string{}
	for i, a := range p.Arguments {
		if i < len(fields) {
			args[a.Name] = fields[i]
		}
	}
	res, err := sc.session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: p.Name, Arguments: args})
	if err != nil {
		return "", err
	}
	return flattenMessages(res.Messages), nil
}

// findPrompt resolves a namespaced prompt name to its server and prompt.
func (m *Manager) findPrompt(name string) (*serverConn, *mcpsdk.Prompt) {
	for _, sc := range m.servers {
		if !sc.connected {
			continue
		}
		for _, p := range sc.prompts {
			if toolName(sc.name, p.Name) == name {
				return sc, p
			}
		}
	}
	return nil, nil
}

// flattenMessages joins prompt messages into a single text block.
func flattenMessages(msgs []*mcpsdk.PromptMessage) string {
	var parts []string
	for _, msg := range msgs {
		if t, ok := msg.Content.(*mcpsdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// splitArgs splits on whitespace, honoring single and double quotes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return out
}
