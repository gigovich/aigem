# Demo recording

`docs/assets/demo.gif` in the README is generated from this directory, so it can
be regenerated whenever the TUI changes rather than going stale.

```sh
demo/record.sh
```

Needs [`vhs`](https://github.com/charmbracelet/vhs), `ttyd`, `ffmpeg`, and a Go
toolchain:

```sh
sudo apt install ttyd ffmpeg          # or: brew install ttyd ffmpeg
go install github.com/charmbracelet/vhs@latest
```

## What is real, and what is not

The TUI, the tools, the sandbox, and the markdown renderer in the recording are
all the real thing. Only the **model** is scripted.

| Piece | What it is |
| --- | --- |
| [`mockmodel/`](mockmodel) | A scripted OpenAI-compatible endpoint. It replies with a fixed sequence - `list_dir`, `grep`, `read_file`, then the answer - chosen by how many assistant turns are already in the conversation. |
| [`workspace/`](workspace) | The tiny sample project the recording explores. The tool output in the GIF is genuinely read from these files. |
| [`demo.tape`](demo.tape) | The VHS script: what gets typed, and how long each beat lasts. |
| [`record.sh`](record.sh) | Builds everything, prepares an isolated environment, records, cleans up. |

A scripted model is what makes the recording **deterministic** - the same GIF
comes out every time - and means regenerating it costs no tokens and needs no
credentials.

## Why it runs in a throwaway directory

`record.sh` copies `workspace/` to a temp directory and points
`XDG_CONFIG_HOME`/`XDG_STATE_HOME` at throwaway paths. Two reasons:

- **Nothing personal leaks in.** A recording made against a real install would
  put that machine's provider list, model names, and project skills on screen.
- **Run in place and the project root would be this repository**, so aigem would
  report on aigem's own skills and settings instead of the sample project.

It also writes an explicit "no search backend" `search.json`. Without it the
first interactive launch runs the web-search setup wizard, which would swallow
the recording's first keystrokes before the TUI ever opened.
