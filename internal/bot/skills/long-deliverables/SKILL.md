---
name: long-deliverables
description: >-
  How to deliver a long-form result (research note, design, report). Load when a reply would
  exceed roughly a screen of chat, before posting it.
---
A single chat post is capped (around 16k characters) and long dumps are hard to read and easy to
lose in a thread. Do not paste the whole deliverable into the thread. First offer to persist it:
write the full content to a Markdown file with write_file (under the working directory, e.g.
docs/), or add it to the project wiki, then reply with a short summary, the key findings, and a
pointer to where the full result lives. Ask the requester which form they prefer - inline, file,
or wiki - rather than dumping the entire text by default. Reserve inline for results that
genuinely fit in a post.

The persisted file holds the deliverable, not the state of work: do not turn it into a status
document that shadows the team's tracker.
