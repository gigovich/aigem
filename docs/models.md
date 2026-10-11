# Models and providers

aigem starts with no models. You add the ones you want from these providers, or
from any OpenAI-compatible server:

| Provider | Authentication                                                    |
| -------- | ----------------------------------------------------------------- |
| `local`  | none - a llama.cpp server you run                                 |
| `openai` | an API key, or a ChatGPT Plus/Pro subscription via OAuth          |
| `xai`    | an API key, or a Grok subscription via device-code OAuth          |

The list is what you added, kept in `~/.config/aigem/models.json`, plus a
project's `.aigem/models.json` and the local model once it is set up. aigem
knows the context window and output limit of the OpenAI GPT-5.x and xAI Grok
models it ships with, and fills them in when you add one of those.

> A project file may add providers and tweak the models of an existing one, but
> never its `base_url`, `api`, `auth`, or `headers`. It ships with a
> possibly-cloned repository and must not be able to point a credential you stored
> at another host.

## CLI

```sh
aigem models                       # list the models you added; * marks ones needing login
aigem auth status                  # show which providers are authenticated
aigem auth login openai            # ChatGPT subscription: opens the browser (OAuth)
aigem auth login openai --api-key-stdin    # OpenAI API key from stdin (safer than argv)
aigem auth login openai --api-key sk-...   # API key via argv (or set $OPENAI_API_KEY)
aigem auth login xai               # Grok subscription: device code, approve in any browser
aigem auth login xai --api-key-stdin       # xAI API key from stdin (or set $XAI_API_KEY)
aigem auth logout openai           # clear the stored credential
aigem --model openai/gpt-5.6-sol   # start on a model you added
aigem --model xai/grok-4.3         # ... or an xAI one
```

`/login` in the TUI covers the `openai` provider only; authenticate `xai` from
the CLI.

In the TUI, `/model` opens a fuzzy picker of the models you added. A model whose
provider is not authenticated shows a lock and routes to `/login` on select. Del
removes the highlighted model. Selecting an authenticated model switches the live
session - the conversation continues, and the context gauge and compaction window
follow the new model. `/login [provider]` runs the OAuth flow; `/logout [provider]` clears the
token.

### Add models

In the TUI, choose **Add model…** in `/model`, or run `/model add`. A TUI with no
model offers it on start. In the browser, use **Add models** on the Models screen;
a daemon with no models of your own shows the same form in place of every screen.

1. Choose where to add from:
   - a provider you can use - `openai`, `xai`, or one you added. One that needs a
     sign-in shows a lock; in the TUI choosing it signs in to `openai` first.
   - a model server found on this machine. aigem checks `127.0.0.1` on the
     default ports of Ollama (11434), LM Studio (1234), llama.cpp (9931, 8080),
     vLLM (8000), Jan (1337) and KoboldCpp (5001). It never scans the network.
   - in the browser, a server URL you type. It is never sent a stored key.
   - in the TUI, **Set up a local model (llama.cpp)**, the wizard below, or
     **Enter by hand…**, the form for anything a server does not list.
2. aigem asks the provider for its models (`GET <base>/v1/models`) and lists
   the ones not added yet. OpenAI's listing is narrowed to chat models. A ChatGPT
   subscription has no listing and offers the models it supports; so does a Grok
   subscription the listing refuses.
3. Check the models to add (Space in the TUI) and confirm. A model whose context
   window nobody reported needs one from you.

A server becomes a provider needing no credential, named after it (`ollama`,
`lmstudio`, ...); adding more from the same URL reuses it.

The manual form (**Enter by hand…**) takes one model:

1. Choose an existing built-in/user provider, or **New OpenAI-compatible
   provider**. Existing providers keep their endpoint and authentication. A new
   provider needs a unique ID, an HTTP(S) base URL, and `apikey` or `none`
   authentication.
2. Enter the model ID (including any `/` in the provider's model name). Display
   name, context window, and output limit are optional; blank limits use the
   session's startup fallbacks. Supplied limits must be positive, and the output
   limit cannot exceed a supplied context window.
3. Optionally enter a masked API key. It is stored separately in the private
   `auth.json`, never in `models.json` or input history. Replacing a stored
   credential, including a subscription login, requires explicit confirmation
   and affects every model on that provider. Leave the field blank to keep the
   existing credential/environment behavior.
4. Review the destination and choose **Save** or **Save & select**. Save returns
   to the picker with the new model highlighted, without changing the active
   model or default. Save & select switches the live conversation and remembers
   the selection for next startup. If authentication prevents selection, the
   model remains saved and the current model stays active.

Use Enter/Tab to advance, Shift+Tab to go back, and Esc to cancel before saving.
The review scrolls with Up/Down; Left/Right selects its save action.

Additions go to the user-level `models.json` (respecting `XDG_CONFIG_HOME`) and
are available immediately and after restart. Existing entries and unknown
configuration fields are preserved; duplicate model references and malformed
configuration are refused rather than overwritten.

Removing a model takes it out of the user-level `models.json` and keeps its
provider entry, so a project file can never take over that provider's id and the
credential stored for it. Models from a project file and the local model are
not removed this way; use `/model reset` for the local one.

Project-only providers are not offered for user-level registration, and never
get a stored key: an endpoint from a cloned repository must not silently become a
destination for saved credentials. To use a key with one, add it to your own
`models.json`.

## Which model gets picked

With no `--model`, aigem first reuses **the model you last selected**, if it still
resolves and is usable. Otherwise it takes the first model you added of the first
authenticated provider that requires authentication - `openai`, then `xai`, then
anything you added - and else the first model you added. A model only a project
file declares is never picked this way. With no model at all, the TUI starts and
asks you to add one, and `-p` exits with an error.

A bare `--model <name>` keeps the old behavior: the local provider with that model
name.

The ChatGPT subscription (Codex backend) only accepts Codex-supported models -
currently `gpt-5.6-sol` (the default), `gpt-5.6-terra`, and `gpt-5.6-luna`.
Other models, and any you add via `models.json`, require an API key. If you have
both a stored ChatGPT login and `$OPENAI_API_KEY`, aigem uses
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

The local model is listed once it is set up. Choose **Set up a local model** when
adding a model in the TUI (or run `/model init` or `aigem models init`), and aigem
runs a short wizard and then spawns `llama-server` as a detached daemon - it
survives aigem exiting and is reused next launch.

Startup never silently downloads anything. If you start on a local model that is
not set up (`--model <name>`), the TUI shows a "Local model not set up" alert
naming the file, with a **Set up & download** action you have to choose - the
multi-gigabyte download never starts on its own.

```sh
aigem models                 # list the models you added
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

The TUI status bar shows the tightest window next to the context gauge. A call
the provider reported no counts for (an aborted runaway, a backend that sends
none) is logged as `uncounted` rather than folded into the totals.

Coverage follows what each provider sends. A ChatGPT subscription reports plan,
percent used per rolling window, per-model buckets, and credits. An API key
reports the classic `x-ratelimit-*` remaining counts, which appear as remaining
units rather than a percentage. Providers that send neither still get token counts
and report no quota.
