package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolName renders the Claude-Code-style namespaced tool name.
func toolName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// emptyObjectSchema is the argument schema for a no-argument tool.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// mcpTool adapts one server tool to tools.Tool.
type mcpTool struct {
	sc      *serverConn
	tool    string // bare tool name on the server
	name    string // namespaced name exposed to the model
	desc    string
	schema  json.RawMessage
	confirm bool
}

func newMCPTool(sc *serverConn, t *mcpsdk.Tool) *mcpTool {
	schema := emptyObjectSchema
	if t.InputSchema != nil {
		if raw, err := json.Marshal(t.InputSchema); err == nil {
			schema = raw
		}
	}
	return &mcpTool{
		sc:      sc,
		tool:    t.Name,
		name:    toolName(sc.name, t.Name),
		desc:    t.Description,
		schema:  schema,
		confirm: confirmFor(t.Annotations, sc.cfg.AutoApprove, t.Name),
	}
}

// confirmFor decides whether a tool needs confirmation. A tool the server marks
// destructive always confirms (even if listed in autoApprove). Otherwise an
// auto-approved or read-only tool skips the prompt; everything else confirms.
func confirmFor(ann *mcpsdk.ToolAnnotations, autoApprove []string, name string) bool {
	if ann != nil && ann.DestructiveHint != nil && *ann.DestructiveHint {
		return true
	}
	for _, a := range autoApprove {
		if a == name {
			return false
		}
	}
	if ann != nil && ann.ReadOnlyHint {
		return false
	}
	return true
}

func (t *mcpTool) Name() string            { return t.name }
func (t *mcpTool) Description() string     { return t.desc }
func (t *mcpTool) Schema() json.RawMessage { return t.schema }
func (t *mcpTool) NeedsConfirm() bool      { return t.confirm }

func (t *mcpTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var argv any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argv); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	res, err := t.sc.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.tool, Arguments: argv})
	if err != nil {
		return "", err
	}
	text := flatten(res.Content)
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

// flatten renders a tool/resource result's content blocks to text the model
// can read.
func flatten(content []mcpsdk.Content) string {
	var parts []string
	for _, c := range content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		case *mcpsdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image: %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcpsdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio: %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcpsdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource: %s (%s)]", v.URI, v.Name))
		case *mcpsdk.EmbeddedResource:
			if v.Resource != nil {
				if v.Resource.Text != "" {
					parts = append(parts, v.Resource.Text)
				} else {
					parts = append(parts, fmt.Sprintf("[embedded resource: %s]", v.Resource.URI))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// ---- resource read tools ----

// listResourcesTool exposes a server's resources to the model.
type listResourcesTool struct{ sc *serverConn }

func newListResourcesTool(sc *serverConn) *listResourcesTool { return &listResourcesTool{sc: sc} }

func (t *listResourcesTool) Name() string { return toolName(t.sc.name, "list_resources") }
func (t *listResourcesTool) Description() string {
	return "List the resources available from the " + t.sc.name +
		" MCP server: each entry has a uri, name, and description. Use read_resource to fetch one."
}
func (t *listResourcesTool) Schema() json.RawMessage { return emptyObjectSchema }
func (t *listResourcesTool) NeedsConfirm() bool      { return false }

func (t *listResourcesTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	res, err := t.sc.session.ListResources(ctx, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range res.Resources {
		fmt.Fprintf(&b, "- %s", r.URI)
		if r.Name != "" {
			fmt.Fprintf(&b, " (%s)", r.Name)
		}
		if r.Description != "" {
			fmt.Fprintf(&b, ": %s", r.Description)
		}
		b.WriteByte('\n')
	}
	for _, rt := range t.sc.templates {
		fmt.Fprintf(&b, "- %s [template]", rt.URITemplate)
		if rt.Description != "" {
			fmt.Fprintf(&b, ": %s", rt.Description)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(no resources)", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// readResourceTool reads a single resource by URI (covering templates).
type readResourceTool struct{ sc *serverConn }

func newReadResourceTool(sc *serverConn) *readResourceTool { return &readResourceTool{sc: sc} }

func (t *readResourceTool) Name() string { return toolName(t.sc.name, "read_resource") }
func (t *readResourceTool) Description() string {
	return "Read a resource from the " + t.sc.name + " MCP server by its uri " +
		"(from list_resources, or filled into a resource template)."
}
func (t *readResourceTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":` +
		`{"uri":{"type":"string","description":"The resource URI to read."}},"required":["uri"]}`)
}
func (t *readResourceTool) NeedsConfirm() bool { return false }

func (t *readResourceTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.URI == "" {
		return "", fmt.Errorf("uri is required")
	}
	res, err := t.sc.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: a.URI})
	if err != nil {
		return "", err
	}
	return formatContents(res.Contents), nil
}

// formatContents renders resource contents (text or a binary placeholder).
func formatContents(contents []*mcpsdk.ResourceContents) string {
	var parts []string
	for _, c := range contents {
		if c.Text != "" {
			parts = append(parts, c.Text)
		} else if len(c.Blob) > 0 {
			parts = append(parts, fmt.Sprintf("[binary resource: %s, %d bytes]", c.MIMEType, len(c.Blob)))
		}
	}
	if len(parts) == 0 {
		return "(empty resource)"
	}
	return strings.Join(parts, "\n")
}
