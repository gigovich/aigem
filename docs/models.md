# Models and providers

aigem talks to a local llama.cpp server by default, and can also use hosted
providers:

| Provider | Authentication                                                    |
| -------- | ----------------------------------------------------------------- |
| `local`  | none - a llama.cpp server you run                                 |
| `openai` | an API key, or a ChatGPT Plus/Pro subscription via OAuth          |
| `xai`    | an API key, or a Grok subscription via device-code OAuth          |

Models live in a registry: built-in presets (`local/...`, the OpenAI GPT-5.x
family, the xAI Grok family) plus anything you add in
`~/.config/aigem/models.json` or a project `.aigem/models.json`.

> A project file may add providers and tweak the models of an existing one, but
> never its `base_url`, `api`, `auth`, or `headers`. It ships with a
> possibly-cloned repository and must not be able to point a credential you stored
> at another host.

## CLI

```sh
aigem models                       # list providers/models; * marks ones needing login
aigem auth status                  # show which providers are authenticated
aigem auth login openai            # ChatGPT subscription: opens the browser (OAuth)
aigem auth login openai --api-key-stdin    # OpenAI API key from stdin (safer than argv)
aigem auth login openai --api-key sk-...   # API key via argv (or set $OPENAI_API_KEY)
aigem auth login xai               # Grok subscription: device code, approve in any browser
aigem auth login xai --api-key-stdin       # xAI API key from stdin (or set $XAI_API_KEY)
aigem auth logout openai           # clear the stored credential
aigem --model openai/gpt-5.6-sol   # start on a specific model
aigem --model xai/grok-4.3         # ... or an xAI one
```

`/login` in the TUI covers the `openai` provider only; authenticate `xai` from
the CLI.

In the TUI, `/model` opens a fuzzy picker of every model grouped by provider. A
model whose provider is not authenticated shows a lock and routes to `/login` on
select. Selecting an authenticated model switches the live session - the
conversation continues, and the context gauge and compaction window follow the new
model. `/login [provider]` runs the OAuth flow; `/logout [provider]` clears the
token.

## Which model gets picked

With no `--model`, aigem first reuses **the model you last selected**, if it still
resolves and is usable. Otherwise it takes the first authenticated provider that
requires authentication, in registry order - `openai`, then `xai`, then anything
you added - and falls back to the local model when none is authenticated.

A bare `--model <name>` keeps the old behavior: the local provider with that model
name.

The ChatGPT subscription (Codex backend) only accepts Codex-supported models -
currently `gpt-5.6-sol` (the default), `gpt-5.4`, `gpt-5.4-mini`, and
`gpt-5.3-codex-spark`. Other models, and any you add via `models.json`, require an
API key. If you have both a stored ChatGPT login and `$OPENAI_API_KEY`, aigem uses
the API key automatically for models outside the Codex allow-list.

## Subscription logins: read this first

Both subscription paths - ChatGPT and Grok - work by reusing the vendor's own CLI
OAuth client, and the ChatGPT path additionally identifies itself to OpenAI's
undocumented Codex endpoint as that CLI (an `originator: codex_cli_rs` header).

That means:

- You are using an **undocumented endpoint** that the vendor can change or close
  at any time, and whose use is governed by their terms of service rather than a
  published API contract.
- Your **subscription account** carries the risk. If a vendor decides this is not
  acceptable use, the consequence lands on the account you logged in with, up to
  suspension.

The **API-key paths are the supported alternatives** and carry none of this. Use
`--api-key-stdin`, or `$OPENAI_API_KEY` / `$XAI_API_KEY`, if that matters to you -
which for anything you depend on, it probably should.

Credentials are stored `0600` in `~/.local/state/aigem/auth.json` and are never
logged.

## Local model setup

The local model is set up the first time you select it. Pick `local/...` via
`/model` in the TUI (or run `aigem models init`) and, if it has not been
initialized, aigem runs a short wizard and then spawns `llama-server` as a
detached daemon - it survives aigem exiting and is reused next launch.

Startup never silently downloads anything. If you have other models configured,
just use them. If the local model is the startup selection but is not set up, the
TUI shows a "Local model not set up" alert naming the file, with a
**Set up & download** action you have to choose - the multi-gigabyte download
never starts on its own.

```sh
aigem models                 # list providers/models
aigem models init            # set up the local model and start the server
aigem models status          # initialized? running? reachable?
aigem models start           # start the saved local server
aigem models stop            # stop it
aigem models reset           # stop and clear setup (re-run init next time)
```

The same commands exist in the TUI as `/model init`, `/model status`,
`/model start`, `/model stop`, and `/model reset`.

The wizard asks only the essentials - HF download vs a local `.gguf`, and the
`llama-server` binary. Everything else uses defaults in
`~/.local/state/aigem/local.json`, which you can edit: host/port, context size,
GPU layers (`-ngl`), flash attention, and extra args. The default targets the
Unsloth Gemma 4 12B QAT GGUF with `-hf unsloth/gemma-4-12B-it-qat-GGUF:UD-Q4_K_XL`.
`--jinja` is always added.

The first launch downloads the GGUF (several GB) into llama.cpp's cache before
serving, so aigem shows a live download indicator (percent, size, speed) and waits
as long as the download keeps making progress - there is no fixed time limit. The
wait only aborts after a couple of minutes with no progress at all. If
`llama-server` is not on your `PATH`, setup prints OS-specific install
instructions.

In `-p` mode an initialized-but-stopped server is auto-started; an uninitialized
local model exits with guidance to run `aigem models init`.

## Usage and quota

Providers report what a call cost, and how much of the account's quota is gone, on
the responses to ordinary calls: token counts in the body, quota state in headers.
aigem records both.

```sh
aigem usage                   # each provider's last known quota state
aigem usage --refresh         # send one small request first, then report
aigem usage --refresh <ref>   # refresh a specific provider's model
```

```
openai                         plan prolite            as of just now
  primary                      2% used of a 7d window  resets in 5d21h (Aug 2 09:00)
  GPT-5.3-Codex-Spark primary  0% used of a 7d window  resets in 6d23h (Aug 3 11:17)
  credits                      0 (none available)
```

There is no endpoint to poll for this - the numbers only ride on real calls - so a
plain `aigem usage` reports the last reading and how old it is. `--refresh` spends
a few tokens on one small request to update a single provider; the report itself
covers all of them.

The TUI status bar shows the tightest window next to the context gauge. Each bot
logs `msg="llm usage"` for every model call: the tokens it cost, the running
totals, and the tightest quota window - which is what makes burn rate comparable
between models. A call the provider reported no counts for (an aborted runaway, a
backend that sends none) is logged as `uncounted` rather than folded into the
totals.

Coverage follows what each provider sends. A ChatGPT subscription reports plan,
percent used per rolling window, per-model buckets, and credits. An API key
reports the classic `x-ratelimit-*` remaining counts, which appear as remaining
units rather than a percentage. Providers that send neither still get token counts
and report no quota.
