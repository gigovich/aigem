package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResourceView is one resource (or template) for the human browser.
type ResourceView struct {
	URI      string // concrete URI, or template URI
	Name     string
	Desc     string
	Template bool
}

// ServerView is a snapshot of one server's status and capabilities for /mcp.
type ServerView struct {
	Name      string
	Connected bool
	Err       string
	Tools     []string
	Prompts   []string
	Resources []ResourceView
}

// Servers returns a view of every configured server for the /mcp browser.
func (m *Manager) Servers() []ServerView {
	if m == nil {
		return nil
	}
	out := make([]ServerView, 0, len(m.servers))
	for _, sc := range m.servers {
		v := ServerView{Name: sc.name, Connected: sc.connected}
		if sc.err != nil {
			v.Err = sc.err.Error()
		}
		for _, t := range sc.tools {
			v.Tools = append(v.Tools, t.Name)
		}
		for _, p := range sc.prompts {
			v.Prompts = append(v.Prompts, p.Name)
		}
		for _, r := range sc.resources {
			v.Resources = append(v.Resources, ResourceView{URI: r.URI, Name: r.Name, Desc: r.Description})
		}
		for _, rt := range sc.templates {
			v.Resources = append(v.Resources, ResourceView{
				URI: rt.URITemplate, Name: rt.Name, Desc: rt.Description, Template: true,
			})
		}
		out = append(out, v)
	}
	return out
}

// ReadResource fetches one resource's text for the human preview pane.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (string, error) {
	sc := m.byName[server]
	if sc == nil || !sc.connected {
		return "", fmt.Errorf("mcp server %q is not connected", server)
	}
	res, err := sc.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return "", err
	}
	return formatContents(res.Contents), nil
}
