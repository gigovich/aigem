# MCP servers

aigem connects to [Model Context Protocol](https://modelcontextprotocol.io)
servers and exposes their tools to the model.

## Configuration

Servers are read from `mcpServers` blocks in project settings, then global
settings, in this order:

1. `<project>/.aigem/settings.json`
2. `<project>/.claude/settings.json`
3. `<project>/.claude/settings.local.json`
4. `<project>/.mcp.json`
5. `~/.config/aigem/settings.json`

```sh
aigem mcp list             # configured servers: name, transport, command/URL
aigem mcp add <name> ...   # add a server
aigem mcp remove <name>    # remove one
aigem mcp login <name>     # run an HTTP server's OAuth flow
aigem mcp logout <name>    # clear its stored token
```

`list`, `add`, and `remove` only manage configuration, and the trust gate is
applied when a chat front-end starts and would actually connect.

`login` is the exception: it dials the named HTTP server to run its OAuth flow,
and it is **not** trust-gated. Only run `aigem mcp login` for a server you
already trust in a project you already trust.

## Trust

Global servers always keep their configured behavior.

**Project-local MCP servers are trust-gated**, whether they use stdio or HTTP. CLI,
REPL, and TUI startup skip them in an untrusted project and print a warning. This
prevents an untrusted repository from starting local commands, connecting to
project-defined HTTP endpoints, sending configured headers or OAuth credentials,
or applying project `autoApprove` semantics before you have approved any of it.

Pass `--trust-project-mcp` to approve each server's current transport
configuration and, **separately**, its current `autoApprove` policy. A transport
can remain allowed while a changed approval policy is invalidated; in that case
the server starts with `autoApprove` disabled until the new policy is approved.

HTTP approval is per named target and fingerprint, and never grants general
network access. See [the security model](security.md) for the details.
