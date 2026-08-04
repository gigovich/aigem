---
name: memory-mechanics
description: >-
  Action-level details of the memory tool (save, read, delete, list, archive, restore,
  audit, inspect) and how the memory index works. Load when saving, revising, or cleaning
  up memory facts.
---
The index of facts you saved appears in your system prompt under "Memory index" (absent when
empty). Use the memory tool to work with it:
- save: add a fact or replace one - a short name, a one-line description for the index, and the
  full content. Saving an existing name overwrites it.
- read: pull a fact's full content when the one-line index entry is not enough.
- delete: remove a fact that is no longer true. Irreversible; prefer archive when in doubt.
- list: show the current index.
- archive: retire a fact reversibly. It leaves the index but can be brought back with restore.
  Archiving is refused while an archived fact already holds that name - delete the active fact,
  or save it under a new name first.
- restore: bring an archived fact back into the index.
- audit: staleness overview - each fact's age, when it was last read and how often, plus the
  archived names.
- inspect: read a fact, active or archived, without counting it as a use. Use it when reviewing
  memory; use read when actually consuming a fact to act on it.

To revise a fact that partly went stale, save under its existing name with the corrected full
content: that overwrites it in place. Delete only when the whole fact became false.

Each fact tracks its own usage automatically, and the daily review uses that as a staleness hint.
A fact file's frontmatter is machine-managed - anything hand-added there is dropped on the next
save or read, so extra notes belong in the fact's body.
