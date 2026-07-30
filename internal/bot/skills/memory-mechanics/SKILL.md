---
name: memory-mechanics
description: >-
  Action-level details of the memory tool (save, read, delete, list, archive, restore,
  audit, inspect) and how the memory index works. Load when saving, revising, or cleaning
  up memory facts.
---
The index of facts you have saved appears in your system prompt under "Memory index" (absent
when empty); use the memory tool to work with it:
- save: add a new fact or replace an existing one (give it a short name, a one-line
  description for the index, and the full content). Saving a name that already exists
  overwrites it.
- read: pull a fact's full content by name when the one-line index entry is not enough.
- delete: remove a fact that is no longer true. Irreversible - prefer archive when in doubt.
- list: show the current index.
- archive: retire a fact reversibly. It leaves the index but is kept aside and can be
  brought back with restore. Archiving is refused while an archived fact already holds the
  same name - delete the active fact instead, or save it under a new name first.
- restore: bring an archived fact back into the index.
- audit: staleness overview - each fact's age since last modification, when it was last
  read and how often, plus the archived fact names.
- inspect: read a fact (active or archived) without counting it as a use. Use it when
  reviewing memory; use read when actually consuming a fact to act on it.
To revise a fact that partially went stale, save under its existing name with the corrected
full content - that overwrites the old version in place. Delete a fact only when the whole
thing became false.
Each fact tracks its own usage automatically (modified on save, last-read time and read
count on read); the audit action reports it and the daily memory review uses it as a
staleness hint. The frontmatter block of a fact file is machine-managed - anything a human
or you hand-adds there is dropped on the next save or read, so extra notes belong in the
fact's body.
