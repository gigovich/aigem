---
name: scheduling
description: >-
  Mechanics of the schedule and post_message tools. Load BEFORE creating, replacing, or
  removing a scheduled job, writing a job's prompt, or posting proactively into a thread,
  person, or thread.
---
Use the schedule tool to run work on a timer:
- set: create or replace a job - a short id, a prompt, and either "expr" (a 5-field cron
  expression "minute hour day-of-month month day-of-week", recurring) or "delay" (one-shot,
  like "10m"/"2h"; runs once and deletes itself). Setting an existing id replaces it.
- remove: delete a job by id.
- list: show your jobs. Entries marked [built-in] are installed by the runtime; set/remove
  refuse their ids.
Each run starts a FRESH agent with no conversation history, only your memory, so write every
job prompt as a self-contained instruction and rely on memory for state. Avoid overlapping
jobs.

Two built-in jobs are always present: `work-heartbeat`, which wakes you about every half hour
to advance what you own (slowing down while you answer it `IDLE`), and `memory-review`, the
daily memory pass. You cannot remove them, so you can never leave yourself with no way to wake
up. Your own jobs only need to cover wakes that must be sooner or more specific, such as
checking a CI run that finishes in fifteen minutes. Do not schedule minute-scale continuations
of your own work: each fire rebuilds a fresh agent from memory, which costs far more than it
advances.

A job due while a turn of yours is running waits for the next free minute, and only one
scheduled job runs at a time, so a job set for "15m" may run later if you are still busy. Two
exceptions: a message arriving mid-run always wakes you, and work held back about an hour and
a half by a turn that never ended is released anyway.

Two rules govern what a job may post:
- A recurring status or follow-up job must post only when something changed or an action is
  needed now. Keep the last reported state in memory, compare it with live state, and when
  nothing changed, finish the run without calling post_message at all. Re-posting an unchanged
  status is noise that teaches everyone to ignore you.
- Do not poll for something that will arrive as a message: that answer wakes you by itself.
  Schedule a continuation only for work YOU will advance when it fires, and keep at most one
  slow check to notice a wait has gone on too long.

Use the post_message tool to post into a thread you are in, or to open a new one: this is how a
scheduled run delivers its result, and how you start a conversation outside of replying to
someone. It has no default thread, so always name the target - a scheduled job has no incoming
message to infer one from, so state the target in the job's prompt or keep it in memory. To land
a deferred result back in the thread the work was requested in, pass that thread's id as
"thread" - so record the thread id in the job's prompt when you schedule follow-up work. With no
thread to return to, name participants instead and the job opens one.
