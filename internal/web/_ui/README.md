# The browser client

The UI `aigem web` serves. React 19, Vite, Tailwind v4, no state library and a
hand-written router.

```sh
make web-dev     # Vite, proxying /api to a running `aigem web`
make web-check   # lint, typecheck and test
make web         # build the bundle the Go binary embeds
```

The wire this talks to - routes, status codes, the two websocket protocols, the
feature map - is documented in [`docs/web.md`](../../../docs/web.md). This file
is about the client's own shape.

## The layers

Imports go one way down this list. Nothing below reaches back up.

- `src/lib/` - the wire and the transport: types, the API client, the two socket
  clients, the router, the store, formatting. Knows nothing about React.
- `src/state/` - the application's state and what is derived from it: a run's
  timeline folded into what screens draw, an event's row, the inspector's
  contents.
- `src/ui/` - presentational components. They are given everything they draw and
  read no store.
- `src/shell/` - the frame: header, sidebar, inspector, status bar, palette,
  quick chat, the keyboard map.
- `src/screens/` - one per route. They own their screen's mutations and publish
  to the inspector.

Two breakpoints, both in `src/state/app.ts`: below 1120px the columns narrow
and the inspector closes; below 720px there is one column - the navigation is a
drawer, the inspector a sheet, and a list-and-detail screen shows one or the
other, chosen by whether the route names an id.

A screen never opens a socket: the shell holds one run stream per tab and hands
the conversation down, because the daemon allows 64 websockets across every tab
and a component that opened its own would spend them by being mounted twice.

## The design is a canvas, not this repository

Phase one's appearance comes from a Claude Design artboard, and the values that
matter are transcribed into `test/design-values.test.ts` - both palettes, the
density metrics, the two column widths - and checked against the CSS. "About the
same colour" is the failure that check exists to catch.

Colours are **roles**, never values: `--fg-subtle`, `--attention`, `--running`.
They are declared in `src/theme/mocha.css` and `src/theme/latte.css`, bridged
into Tailwind by the `@theme inline` block in `src/index.css`, and switched by
`data-theme` / `data-density` on `<html>`. A new role must be added to *both*
palette files before anything uses it; `test/theme-tokens.test.ts` enforces that.

`STATUS` in `src/lib/wire.ts` is the one place a state's label, glyph and colour
are written down. Take them from there rather than inventing them in a screen.

## Adding a screen

1. A name in `SCREENS` (`src/lib/route.ts`) and a case in `Screen` (`src/App.tsx`) -
   the switch is exhaustive, so the compiler will ask for the case.
2. A row in `src/shell/Sidebar.tsx` and, if it is worth reaching by keyboard, an
   entry in `src/shell/commands.ts`.
3. A `feature` on both, if the daemon can be built without whatever it shows. A
   screen that can never hold anything must not be offered.

## What a screen has to keep

These are checked by `src/a11y.test.tsx`, which is where to add the next one.

- Every icon-only control carries an `aria-label`.
- Colour never carries a state on its own. The pattern is a glyph with
  `aria-hidden` and the label beside it, in an `sr-only` span where the design
  shows the glyph alone.
- A live region is in the document before there is anything to announce. One
  inserted together with its text is announced by nothing.
- Anything with a `grid`, `listbox` or `radiogroup` role implements the keyboard
  contract that role promises: one tab stop, arrows inside. `src/ui/roving.ts` is
  that. A row that needs a second control is a list of buttons instead, because
  an option is a leaf and a button inside one cannot be reached.

## Two costs written into the code

`src/state/run.ts` appends to its arrays in place rather than copying them: a run
is tens of thousands of events long and copying per event is quadratic. Nothing
may memoise on the identity of `view.events` or `view.rows` - memoise on `seq`.

`src/ui/Markdown.tsx` renders into React elements and never into HTML. This page
draws model output and the text of skills the agent may have been pointed at by a
page an attacker wrote; a tag that parser cannot build cannot be built. Keep it
that way - `dangerouslySetInnerHTML` with a sanitiser in front of it makes the
sanitiser the only thing standing between that text and the DOM.
