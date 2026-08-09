package uisession

import (
	"strings"

	"github.com/sahilm/fuzzy"

	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/skill"
)

// Command is one thing a user can invoke by name. The catalogue lives here
// because it is not a terminal concept: a slash menu and a command palette are
// two renderings of the same list, and a session that offers a skill in one and
// not the other would be a bug nobody notices until both exist.
type Command struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// Commands lists the built-in commands plus whatever the loaded skills and MCP
// servers contribute. Names carry their leading slash: they are what the user
// types, and a front-end that shows them some other way strips it.
func Commands(skills *skill.Registry, mcpMgr *mcp.Manager) []Command {
	cmds := []Command{
		{"/new", "Start a fresh session (clears the conversation)"},
		{"/model", "Switch the active model"},
		{"/login", "Authenticate a provider (e.g. /login openai)"},
		{"/logout", "Clear a provider's stored credential"},
		{"/resume", "Resume a saved session"},
		{"/skills", "Browse available skills"},
		{"/agents", "Browse and configure agents"},
		{"/artifacts", "Review files changed this session"},
		{"/compact", "Summarize the conversation to free context"},
	}
	if skills != nil {
		for _, s := range skills.List() {
			if s.UserInvocable {
				cmds = append(cmds, Command{"/skill:" + s.Name, oneLine(s.Description)})
			}
		}
	}
	if mcpMgr != nil && !mcpMgr.Empty() {
		cmds = append(cmds, Command{"/mcp", "Browse MCP servers and resources"})
		for _, p := range mcpMgr.PromptCommands() {
			desc := oneLine(p.Desc)
			if len(p.Args) > 0 {
				desc = "args: " + strings.Join(p.Args, " ") + "  " + desc
			}
			cmds = append(cmds, Command{"/" + p.Name, desc})
		}
	}
	return cmds
}

// FilterCommands ranks the catalogue against a typed query with a fuzzy match.
// A bare "/" (or an empty query) is not a search - it is the request to see
// everything, in the order the catalogue defines rather than a ranked one.
func FilterCommands(cmds []Command, query string) []Command {
	if query == "" || query == "/" {
		return cmds
	}
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	out := make([]Command, 0, len(cmds))
	for _, mt := range fuzzy.Find(query, names) {
		out = append(out, cmds[mt.Index])
	}
	return out
}

// oneLine flattens a description onto a single bounded line, so a skill whose
// front matter runs to a paragraph cannot push the menu open.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 100 {
		s = string([]rune(s)[:100]) + "…"
	}
	return s
}
