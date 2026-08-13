# Design System: aigem Session Workspace

The single source of truth for generating and reviewing screens in the aigem web UI.
This is an aspirational target, not a description of the current build: where the shipped
code disagrees, the code is wrong.

Scope: the browser workspace that drives a terminal coding agent. Dark surface only.
There is no light theme and none is planned; a second palette would double the review
surface for an interface that is looked at in a dim room next to a terminal.

---

## 1. Visual Theme & Atmosphere

An instrument panel for watching an agent work on your machine, in Catppuccin Mocha.
The mood is a well-run control room: quiet, unlit, everything at rest until something
actually happens, and then exactly one thing moves. Mocha's lilac-tinted neutrals keep
that room warm rather than clinical, and a single peach accent is the only thing in it
asking for a decision.

Calibration for this product:

- **Density 8 (Cockpit Dense).** A running session shows a conversation list, a tool-call
  stream, a plan, a changed-file list and a diff at once. Whitespace is spent on separating
  those zones, never on padding rows.
- **Variance 5 (Offset Asymmetric).** The three-zone workspace is deliberately unequal:
  a narrow rail, a wide stream, a medium panel. Nothing is centered and nothing is thirds.
- **Motion 3 (Static Restrained).** In an agent monitor, movement means work is happening.
  Decorative motion is a lie about system state, so motion is rationed to live states only.
- **Creativity 9.** Expressed through restraint and typographic precision, not ornament:
  hairline structure, monospaced data, near-zero chrome.

The interface should read as a tool that respects the user's attention, closer to a
logic analyzer than to a chat product.

---

## 2. Color Palette & Roles

**The palette is Catppuccin Mocha** (upstream `catppuccin/palette`, v1.8.0), with Peach as
the accent. Values are copied from upstream, never hand-mixed: the point of adopting a
published palette is that it stays the published palette. To move flavors, re-fetch the
JSON and re-map the roles below.

Token names say the **role**, not the color, so a future palette swap is one file. Do not
invent intermediate shades; use opacity of an existing token instead.

### Surfaces and neutrals

| Role       | Mocha      | Hex       | Used for                                        |
| ---------- | ---------- | --------- | ----------------------------------------------- |
| `canvas`   | `crust`    | `#11111B` | Application canvas, the deepest surface         |
| `panel`    | `base`     | `#1E1E2E` | Rails, side panels, composer, sticky headers    |
| `raised`   | `surface0` | `#313244` | Hover, selection, inline code, tool-call bodies |
| `line`     | `surface1` | `#45475A` | 1px structure, diff gutters, panel edges        |
| `fg`       | `text`     | `#CDD6F4` | Primary text, headings, active labels           |
| `muted`    | `subtext0` | `#A6ADC8` | Secondary text, metadata, icons at rest         |
| `disabled` | `overlay0` | `#6C7086` | Disabled controls and decorative glyphs only    |

`line-faint` is `line` at 55% alpha, for dividers inside a single component where a
full-strength rule would over-segment. `raised` is the only step up from `panel`; there is
no third elevation.

Catppuccin's neutrals are deliberately lilac-tinted rather than gray. That is the palette
working as designed, and it is not the banned "AI purple": the ban below is about
**accents**, not about the temperature of a neutral ramp.

### Accent (exactly one)

- **`accent`** = Mocha `peach` (`#FAB387`) - Primary buttons, focus rings, the active
  conversation marker, the "awaiting your approval" state, links.

It carries a single meaning: **you are the one who has to act**. A pending tool approval
and the primary submit button are the same color on purpose, because in this product they
are the same event. Nothing decorative may use it.

Mocha offers fourteen accents. Exactly one is in play at a time. Peach is the chosen one
because it sits 40 degrees from `red` and 93 from `green`, so an accent dot and a status
dot are never confusable at 6px.

### State signals (semantic, not accents)

- **`good`** = Mocha `green` (`#A6E3A1`, hue 116) - Applied, passed, completed plan item,
  diff additions.
- **`bad`** = Mocha `red` (`#F38BA8`, hue 343) - Failed, denied, errored, diff deletions.
- **`muted`** - Skipped, cancelled, not started.

**Color never carries meaning alone**: every state that uses a signal color also carries an
icon or a text label, so a 6px status dot is never the only cue.

### Measured contrast on `canvas` (#11111B)

`fg` 14.6:1, `muted` 8.4:1, `accent` 10.6:1, `good` 12.6:1, `bad` 8.1:1. All pass AA
comfortably - Mocha on `crust` has more headroom than the palette it replaced, and `bad`
is no longer the tight one.

`disabled` measures 3.9:1 and **fails AA**. It is therefore banned for placeholder text and
for anything a reader must read, including diff line numbers; those use `muted`.

### Backgrounds derived from signal colors

Tinted fills use the signal at 12% over `panel`, with a 1px border of the same
signal at 35%. Diff rows are the one exception at 16%, because they must survive being
read as a continuous block.

### Banned in color

- No `#000000` and no pure `#FFFFFF`.
- No purple, violet, indigo, or periwinkle "AI blue" (`#7AA2F7` and neighbors) **as the
  accent**. This is the single most common tell in generated agent UIs. Mocha's `mauve`,
  `lavender`, `blue` and `sapphire` are therefore off the table as accents, however
  idiomatic they are elsewhere in Catppuccin. The lilac cast of the neutral ramp is fine.
- No outer glows, no `box-shadow` in a hue, no neon.
- No gradients on surfaces, text, or borders. Flat fills only.
- No second accent introduced for "variety".

---

## 3. Typography Rules

- **Display and UI: `Geist`** - Headings, labels, body, buttons. Track-tight at large sizes
  (`-0.015em` above 20px), normal below. Hierarchy comes from weight (500 vs 400) and from
  `fg` vs `muted`, never from size alone. Nothing in this interface is larger than 24px.
- **Data and code: `Geist Mono`** - Required, not optional, for: file paths, diffs, token
  counts, costs, durations, model names, session ids, tool names, tool arguments, shell
  output, timestamps. At density 8, **every number is monospaced** so columns align without
  a table.
- **Body copy: `Geist`, 14px, line-height 1.6, max 68 characters.** Assistant markdown is
  the only long-form text; it gets a measure constraint even inside a wide stream.

### Type scale

| Role            | Size | Weight | Color   | Notes                        |
| --------------- | ---- | ------ | ------- | ---------------------------- |
| Section title   | 15px | 500    | `fg`    | Panel and rail headers       |
| Body / message  | 14px | 400    | `fg`    | 1.6 leading, 68ch measure    |
| Dense row label | 13px | 400    | `fg`    | Conversation rows, file rows |
| Metadata        | 12px | 400    | `muted` | Timestamps, counts, model    |
| Code and diff   | 12px | 400    | `fg`    | Geist Mono, 1.45 leading     |
| Micro label     | 11px | 500    | `muted` | Badges, no uppercase needed  |

### Banned in typography

- `Inter`, and system-font stacks used as the primary face.
- All serif faces. This is a dashboard; serif is banned here without exception.
- ALL-CAPS for anything longer than a two-word badge.
- Letter-spacing added to body text.
- Font sizes below 11px anywhere, including badges.

---

## 4. Component Stylings

### Radii and structure

A tight radius scale, because generous rounding at this density turns a row list into a
bag of pills: **4px** (badges, inline code, dots), **6px** (buttons, inputs, rows),
**8px** (panels, dialogs, diff containers). Nothing above 8px.

### Buttons (`ui.tsx`)

- **Primary:** `accent` fill, `canvas` text, no border. One primary per view.
- **Outline:** `panel` fill, `line` border, `fg` text. Hover to `raised`.
- **Ghost:** transparent, `muted` text. Hover to `raised` with `fg` text.
- **Destructive:** `bad` at 12% fill, `bad` at 40% border, `bad` text. Closing a
  conversation is not undoable, so it is always destructive-styled and always labeled.
- Active state is a tactile 1px downward translate. No scale, no glow, no ring on click.
- Focus-visible is a 2px `accent` ring at 60% opacity, offset 1px. Keyboard only.
- Minimum tap target 36px on desktop rows, 44px anywhere reachable on a phone.

### Cards: mostly banned

At density 8, cards fragment the stream. Use **hairline dividers and negative space**
instead. Nested panels are permitted in exactly four places, where elevation encodes a
real containment relationship:

1. A tool call inside a timeline turn.
2. A diff inside the side panel.
3. A modal or drawer over the workspace.
4. A popover for a choice that has to be made in place: the participant picker in the
   thread header, the `@mention` list above the composer. It is not a dialog - it takes no
   backdrop and traps no focus - and Escape or moving away closes it. A choice that
   warrants covering the conversation is a drawer instead.

   Anchored to a control (the picker), it returns focus to that control on close. Anchored
   to the caret (the mention list), there is no control to return to: it is driven by the
   arrow keys, closes on blur, and announces itself as a combobox on the field it belongs
   to, because a list a screen reader is never told about is a list only some readers get.

A panel may not sit directly inside another panel: `raised` is the only step up from
`panel`, so a `panel` fill inside a `panel` fill erases the boundary it was drawn for. An
expanded agent trace is therefore indented under its summary line rather than boxed - the
tool calls inside it are the panels.

Everywhere else - conversation rows, changed-file rows, plan items, usage figures - use a
`border-top` hairline and no background until hover.

### Rows and lists

The unit of this interface. A row is 32px to 36px tall, has a 2px left marker slot that is
`accent` when active and transparent otherwise, a truncating label, and a right-aligned
metadata cluster. Row actions (close, copy, open) are hidden at `opacity: 0` **only** under
`@media (hover: hover)`; on touch they are always visible, because an invisible control
that still accepts a tap is a trap.

### Timeline (`timeline.tsx`)

The stream is chronological and left-aligned with a continuous 1px `line` spine. User turns
and assistant turns are distinguished by label and by an `fg` vs `muted` label color, not by
opposing alignment and not by chat bubbles. Tool calls are collapsed by default to a
single monospaced line - `tool_name`, its one readable argument, and the outcome - and
expand in place.

No duration on that line. The daemon does not send one, and a figure timed in the browser
would be wrong for every replayed turn, which is most of them. If the protocol grows a
duration it belongs here; inventing one to fill the column does not.

### Plan (`plan.tsx`)

A checklist with an inline progress figure in Geist Mono (`3/7`), and no bar at all: the
count is exact, the header already carries the same figure, and a bar would be a third
rendering of one number. Never a percentage. Completed items get `good` and a check glyph,
plus a struck-through label so the state survives without color.

### Diff (`files.tsx`)

Monospaced, unified, with a mono line-number gutter separated by a `line`. Additions on
`good` at 16%, deletions on `bad` at 16%, with a leading `+` / `-` character so the diff is
readable in grayscale. Truncation past `MAX_ROWS` is stated explicitly in a row that says
how many rows were dropped; silent truncation is banned.

### Inputs and the composer

Label above the field, helper text optional, error text below in `bad`. `raised`
fill, `line` border, `accent` focus ring. No floating labels, no placeholder-as-label.
The composer is pinned to the bottom edge with `env(safe-area-inset-bottom)` padding and
grows to a maximum of 6 lines before it scrolls internally.

### Loading states

**The circular spinner is banned.** Replace it with:

- **Skeletal shimmer** matching the exact dimensions of the content that will arrive
  (row heights, gutter widths, panel padding), for anything being fetched.
- **A pulsing 6px `accent` dot** for "session is running", the one live indicator.
- **A blinking 2px `fg` caret** at the tail of streaming text.

### Empty and first-run states

There is no hero section in this product; the empty workspace does that job. It is a
composed, left-aligned block naming what the pane will hold, and nothing else. No
illustrations, no mascots, no "Scroll to explore", no arrows, no chevrons.

It carries no call to action of its own: the composer is pinned below it with the caret
already in reach, and a button that only moves focus there would be a second primary
action competing with the real one.

### Error states

Inline and adjacent to the thing that failed, in a `bad`-tinted block with the actual
error text in Geist Mono. Never a toast for an error the user must act on, and never a
generic "Something went wrong".

---

## 5. Layout Principles

The workspace is a three-zone asymmetric grid and fills the viewport at every width. There
is no max-width and nothing is centered: the stream is the elastic zone, so a wider window
buys more room for diffs and long tool output rather than more empty margin.

```
[ rail 260px ][ stream 1fr, min 0 ][ panel 420px ]
```

- CSS Grid for the workspace and for every row. No flexbox percentage math, no `calc()`
  percentage hacks.
- Every scroll container gets `min-height: 0` and `min-width: 0` so a long tool output
  scrolls inside its zone instead of stretching the grid.
- Full-height regions use `min-h-[100dvh]`. `h-screen` is banned; it breaks on iOS Safari.
- No overlapping elements. Nothing is absolutely positioned over content except the modal
  layer, a popover anchored to its trigger (§4), and the row-action cluster inside its own
  row.
- Zone gutters are `line` rules, not gaps and not shadows.
- Spacing scale: 2, 4, 6, 8, 12, 16, 24, 32. Nothing between, nothing above 32 inside a
  panel.

### Responsive

- **Below 1280px:** the side panel becomes a right-side drawer over the stream.
- **Below 768px:** single column. The rail becomes a left drawer, the panel a bottom sheet,
  and the stream is the whole viewport. No exceptions, no two-column fallback.
- Horizontal page overflow at any width is a critical failure. Diffs and shell output
  scroll inside their own container, never the document.
- Body text never drops below 14px. Touch targets never below 44px, keyed off
  `pointer: coarse` rather than the viewport: a touch laptop is a wide screen someone still
  taps. Rows that contain a control grow with it, so a 44px target never overhangs into the
  row above.
- Vertical section gaps scale with `clamp(1rem, 3vw, 2rem)`.

---

## 6. Motion & Interaction

Motion budget is 3 out of 10. Everything below is the complete permitted list.

- **Spring physics** (`stiffness: 100, damping: 20`) for drawer, sheet and panel transitions.
  No linear easing anywhere.
- **State color and background transitions:** 120ms, ease-out. Hover, focus, selection.
- **No mount reveal.** An earlier draft of this document specified a staggered cascade for
  newly streamed timeline items. It contradicts the rule above it: in an agent monitor
  movement means work is happening, and a row fading in means only that a row was added.
  Items appear in place, instantly.
- **Perpetual loops are restricted to live state**, and there are exactly three: the
  running-session dot pulse, the streaming caret blink, and the skeleton shimmer. When the
  session is idle, nothing on screen moves.
- Animate `transform` and `opacity` only. Never `top`, `left`, `width`, `height`, or
  `background-position`.
- `prefers-reduced-motion: reduce` disables all three loops and all cascades, and reduces
  transitions to instant. The running dot falls back to a static filled dot.
- Auto-scroll on new content only when the stream is already within 80px of the bottom.
  Yanking a user away from what they are reading is worse than a missed message.

---

## 7. Anti-Patterns (Banned)

Structural:

- No overlapping elements, no stacked absolute positioning outside the modal layer.
- No three equal cards in a row, no equal-thirds grid of any kind.
- No cards where a hairline divider would do.
- No centered layouts.
- No `h-screen`, no `calc()` percentage math.
- No horizontal document scroll at any viewport.

Color and type:

- No `#000000`, no pure `#FFFFFF`.
- No purple, violet, or periwinkle "AI blue" accents. No neon, no glow, no gradient.
- No second accent color.
- No `Inter`. No serif faces. No system-font stack as the primary face.
- No color as the sole carrier of meaning.

Motion:

- No decorative animation. No parallax, no floating elements, no marquee.
- No circular spinners.
- No motion at all while the session is idle.

Content and copy:

- No emoji, anywhere, including in empty states and commit-style summaries.
- No fabricated data. Never invent token counts, costs, durations, uptime percentages or
  success rates for a mockup. Use `[tokens]`, `[cost]`, `[duration]` as visible placeholders.
- No invented metrics panels ("SYSTEM PERFORMANCE", "BY THE NUMBERS").
- No `LABEL // 2026` formatting.
- No generic placeholder names ("John Doe", "Acme", "Nexus", "example-project").
  Use plausible real shapes: `internal/web/ui/src/App.tsx`, `grep`, `read_file`.
- No round fake numbers (`99.99%`, `50%`, `10x`).
- No marketing verbs in UI copy: "Elevate", "Seamless", "Unleash", "Next-Gen", "Supercharge".
- No filler UI text: "Scroll to explore", "Swipe down", scroll arrows, bouncing chevrons.
- No custom mouse cursors.
- No hotlinked images. Placeholders come from `picsum.photos` or inline SVG.
