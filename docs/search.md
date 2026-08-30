# Web search

The optional `web_search` tool can be backed by the Brave Search API or by an
automated local-browser flow. Neither is configured by default - aigem makes no
network calls you did not set up.

```sh
aigem search status
aigem search set brave --api-key-stdin
aigem search set browser                    # DuckDuckGo by default
aigem search set browser --engine duckduckgo --profile-dir ~/.local/state/aigem/browser-profile
aigem search clear
```

## Brave

Returns ranked results directly from the API. The key is stored in aigem's state
directory.

## Browser

Drives Chrome/Chromium through the DevTools protocol:

- `web_search` opens the search-results page and returns ranked results (title,
  URL, snippet).
- `open_url` opens a single URL in that same profile and returns the page text
  plus the page's links and search forms - so the model can read a found page and
  drive the site's own navigation instead of re-querying the engine.

A third tool, `browser_action`, drives a page directly - clicking, typing, and
observing named selectors - for flows a plain fetch cannot reach. Each call is an
ephemeral session that has to start with its own `navigate` step; only the
profile's cookies carry over between calls.

`open_url` only opens public web addresses; internal hosts (localhost, private
and link-local ranges) are refused. `browser_action` honors the same rule, with
one deliberate exception: hosts you allowlist explicitly with
`aigem search set browser --test-host HOST` are reachable, so the agent can drive
an internal staging site you pointed it at.

It uses an isolated profile; if `--profile-dir` is omitted, aigem creates one in
its private state directory. Chrome/Chromium is auto-detected, and `--executable`
is only an advanced override.

Supported engines: `duckduckgo` (default), `google`, `bing`.

## Configuring from the TUI

`/agents` lists the subagents and the configurable `web-search` capability.
Selecting `web-search` opens an inline editor for the provider and its
settings.
