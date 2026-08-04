# Chat bots

Beyond the interactive terminal agent, aigem can run **unattended bots** that live
in a chat workspace, answer when addressed, and hand work to each other. The only
transport implemented today is Mattermost.

Each bot is one account and one working directory. One process runs one bot or
the whole team.

```sh
aigem bot create <name>   # define a bot interactively
aigem bot list            # list configured bots
aigem bot rm <name>       # delete one
aigem bot start           # run every configured bot
aigem bot start jane      # run one
aigem bot start jane kate # run only these
aigem bot model [<name>] [<ref>]   # show or switch the model a bot runs on
aigem bot prompt <name>   # print the bot's full assembled system prompt
```

## Running the whole team in one process

`aigem bot start` with no name runs every configured bot in one process. They
start one at a time and each is connected before the next begins, because one
Mattermost account allows one websocket and opening several at once is what the
server rate-limits.

What the bots then share:

- **A cap on concurrent turns.** Each bot limits its own threads, but those limits
  multiply: five bots at four threads each is twenty conversations aimed at one
  provider account. The fleet cap bounds the total, and a scheduled run costs a
  slot just like a chat turn does.
- **One connection pool and one token refresh per provider.** Separate processes
  each kept their own pool, and each refreshed the same OAuth token - a real
  problem where refresh tokens are single-use.
- **A cap on concurrent browsers.** Chrome is started for one tool call and closed
  after, so this bounds a peak rather than a resident cost. Profiles stay per bot:
  they hold logins, and one shared profile would put every search in the team
  behind a single browser.

Both caps are configurable in `~/.config/aigem/fleet.json`; the defaults suit a
five-bot team on one provider account:

```json
{
  "max_concurrent_turns": 6,
  "max_concurrent_browsers": 2
}
```

A negative value means no cap. A malformed file is an error rather than a silent
fallback - running an unbounded fleet because a comma slipped is what the caps
exist to prevent.

What they do not share: memory, skills, threads, schedules, or browser profiles.
Those are per bot exactly as before.

A bot that crashes is restarted on its own, with a backoff, while the rest keep
running. A panic inside one bot's turn does not reach the others.

### Reaching a teammate

A handoff still writes to chat - that post is the record a human reads - but chat
is no longer what wakes the teammate. When the recipient runs in the same process
the message is handed to them directly, so a handoff survives a websocket that is
down or reconnecting. Both copies carry the same post id and the teammate acts on
whichever arrives first, never twice.

The `team_status` tool lists the teammates in the process and whether each is
working right now. A bot that has not answered because it is mid-turn looks
exactly like one that never got the message; this is what tells them apart, and
it is what a bot should check before pinging anyone a second time.

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
    expr: "0 9 * * *"
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

Each role has its own base budget - `developer`, for instance, starts at 45
minutes, 120 model rounds, and 300 tool calls rather than the `-p` defaults. The
`turnBudget:` block overrides whichever fields it sets; set any numeric budget to
`0`, or `maxDuration: "0"`, to disable it explicitly. The `-p` budget flags do not
apply to bots.

### Cron

A `cron` entry fires a prompt on a schedule. Each run is a normal turn, subject
to the same budgets. A job carries exactly one of:

- **`expr:`** - a cron expression, for anything recurring. `"0 9 * * *"` is every
  day at 09:00.
- **`at:`** - a single RFC3339 instant, for a one-shot. It fires once and then
  deletes itself from `bot.yaml`, so it needs a full timestamp
  (`"2026-08-01T09:00:00+04:00"`), not a time of day.

Schedules are evaluated in the bot process's local timezone. A container sets no
`TZ` by default, so a bot in Docker runs its cron in **UTC** - set `TZ` on the
container if you want local times.

A job whose `at:` or `expr:` does not parse is dropped with a warning at startup
rather than failing the bot. Treat that warning as urgent: the dropped job is no
longer in the scheduler's list, so the next time aigem rewrites `bot.yaml` the
entry is **removed from the file**. Fix the expression before anything triggers a
save, or keep a copy.

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

## Running as a service

`deploy/systemd/aigem-bots.service` runs the whole fleet as a systemd **user**
unit - the config and credentials it needs live under your home directory, so it
does not want to be a system unit running as root:

```sh
mkdir -p ~/.config/systemd/user
cp deploy/systemd/aigem-bots.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now aigem-bots
loginctl enable-linger "$USER"     # keep running when you are not logged in
```

Without `enable-linger` the fleet stops when your last session ends and does not
come back at boot.

```sh
systemctl --user status aigem-bots
journalctl --user -u aigem-bots -f                      # all bots
journalctl --user -u aigem-bots -f | grep 'bot=jane'    # one bot
systemctl --user restart aigem-bots
```

Three things the unit has to say out loud, because a service does not inherit your
login shell:

- **`PATH`.** The `bash` tool runs non-interactively, so whatever the bots build
  and test with must be on the unit's `PATH`. A missing toolchain reaches a bot as
  "command not found", which it cannot tell apart from a missing tool. Add version
  managers and language toolchains to the `Environment=PATH=` line.
- **`WorkingDirectory`.** A bot with `workdir: .` resolves it against the unit's
  working directory, not the one you installed from.
- **`SSH_AUTH_SOCK`.** A bot that pushes over SSH needs an agent holding the key.
  The unit inherits this variable from the systemd user manager, which on a
  desktop commonly names gnome-keyring's agent while your keys are in the OpenSSH
  one. The symptom is a bot reporting "the agent has no identities" for a key
  `ssh-add -l` shows you in your terminal. Find the socket that has the key and
  uncomment the matching `Environment=SSH_AUTH_SOCK=` line in the unit:

    ```sh
    systemctl --user show-environment | grep SSH_AUTH_SOCK  # what the unit gets
    SSH_AUTH_SOCK=$XDG_RUNTIME_DIR/openssh_agent ssh-add -l # OpenSSH agent
    SSH_AUTH_SOCK=$XDG_RUNTIME_DIR/gcr/ssh ssh-add -l       # gnome-keyring
    ```

Pointing the unit at the right socket is only half of it. An agent starts empty
and a plain `ssh-add` lasts only as long as that agent, so after a reboot the bot
breaks the same way - and nothing prompts you, because it is headless. Neither
agent fixes this for you: OpenSSH's has no config file and never loads a key on
its own, and `AddKeysToAgent yes` only fires when *you* run `ssh` interactively,
against your login shell's agent rather than the socket the unit pins.

So make the load explicit. Either run `ssh-add` at login against that same socket,
or skip the agent entirely and name a passphrase-less key in `~/.ssh/config`:

```
Host github.com
  IdentityFile ~/.config/keys/deploy_ed25519
  IdentitiesOnly yes
```

A key with no passphrase is a real trade-off - it is a credential sitting on
disk - so scope it to one repository as a deploy key rather than reusing your
personal key. See [Security](security.md).

Restarting a single bot is not what this unit is for: aigem already restarts a
crashed bot on its own and leaves the others running, and `systemctl restart`
takes the whole fleet down and up. When you genuinely need per-bot lifecycle
control, use `deploy/systemd/aigem-bot@.service` instead - one unit per bot:

```sh
cp deploy/systemd/aigem-bot@.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now aigem-bot@jane aigem-bot@kate
```

The cost is that separate processes share nothing: no common connection pool, no
single token source, and the caps on concurrent turns and browsers apply per bot
instead of across the team. The template unit declares `Conflicts=aigem-bots`, so
systemd will not let both run - they would otherwise open two websockets for the
same account.

## Running in a container

See [Docker](docker.md) for the image, volume layout, and how to move config and
state to a server.

> Do not run the same bot in two places at once. Two websocket connections for one
> Mattermost account cause duplicate replies and authentication errors. That
> applies across processes as much as within one: `aigem bot start` with no name
> already runs every bot, so a second command naming one of them starts it twice.
