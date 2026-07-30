# Chat bots

Beyond the interactive terminal agent, aigem can run **unattended bots** that live
in a chat workspace, answer when addressed, and hand work to each other. The only
transport implemented today is Mattermost.

Each bot is one process, one account, and one working directory.

```sh
aigem bot create <name>   # define a bot interactively
aigem bot list            # list configured bots
aigem bot rm <name>       # delete one
aigem bot start <name>    # run its chat loop
aigem bot model [<name>] [<ref>]   # show or switch the model a bot runs on
aigem bot prompt <name>   # print the bot's full assembled system prompt
```

## Roles

A bot is created with a role, which decides its system prompt and its posture:

| Role         | What it is for                                          |
| ------------ | ------------------------------------------------------- |
| `manager`    | coordinates, delegates, and chases work to completion   |
| `researcher` | gathers and summarizes information                      |
| `architect`  | designs before anything gets built                      |
| `developer`  | implements changes in its working directory             |
| `tester`     | verifies, reproduces, and reports                       |

## Configuration

Bots live in `~/.config/aigem/bots/<name>/bot.yaml`:

```yaml
name: jane
role: developer
persona: "female; speaks Russian with feminine forms"
model: openai/gpt-5.6-sol
workdir: /path/to/repo
capabilityProfile: workspace-write
transport:
  kind: mattermost
  serverURL: https://mattermost.example.com
  team: YourTeam
  botUserID: "..."
turnBudget:
  maxDuration: 45m
  maxModelRounds: 80
  maxToolCalls: 240
  maxRepeatedToolCalls: 12
cron:
  - id: standup
    at: "09:00"
    prompt: "Post a short status summary for the team."
```

### Capability profile

`capabilityProfile` defaults to `workspace-write`, and `aigem bot create` prompts
for it. Existing configs without the field get that same safe default. A bot with
no human to ask never escalates on its own - see
[the security model](security.md#capability-profiles-and-approvals).

Bots do not read the path-grant file at all, so a directory you approved
interactively never opens for a bot in the same working directory. Bot mode also
does not connect MCP servers.

### Turn budgets

Bots use the same unattended defaults as `-p` and can override them per bot. Set
any numeric budget to `0`, or `maxDuration: "0"`, to disable it explicitly.

### Cron

A `cron` entry fires a prompt on a schedule - either `at:` for a daily time or
`expr:` for a cron expression. Each run is a normal turn, subject to the same
budgets.

## Choosing a bot's model

Without a `model` field a bot opens the first authenticated provider, so adding a
login can silently move every bot to another model. Pin it per bot instead:

```sh
aigem bot model                        # what each bot runs on now
aigem bot model amiran                 # just this one
aigem bot model amiran openai/gpt-5.6-sol
aigem bot model --all xai/grok-4.3     # or any provider from your own models.json
aigem bot model amiran --clear         # back to the auto-picked default
aigem bot model --all --clear
```

The ref must resolve and open with the stored credentials; validation happens
before any config is written, so a ref one bot rejects cannot half-switch the
fleet.

Only the built-in providers and `~/.config/aigem/models.json` can be pinned - a
repo's own `.aigem/models.json` is untrusted and is not consulted here, and a bot
opens its model from its own working directory rather than the one you ran the
command in.

A bot whose configured model cannot be opened refuses to start rather than
falling back. A running bot keeps its current model until it is restarted.

> Switching rewrites `bot.yaml` from its parsed form, so hand-written comments and
> keys aigem does not know are dropped.

## Observability

Each bot logs `msg="llm usage"` for every model call: the tokens it cost, the
running totals, and the tightest quota window. That is what makes burn rate
comparable between models. See [Models and providers](models.md#usage-and-quota).

## Running in a container

See [Docker](docker.md) for the image, volume layout, and how to move config and
state to a server.

> Do not run the same bot in two places at once. Two websocket connections for one
> Mattermost account cause duplicate replies and authentication errors.
