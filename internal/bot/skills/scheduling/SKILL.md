---
name: scheduling
description: >-
  Mechanics of the schedule and post_message tools. Load BEFORE creating, replacing, or
  removing a scheduled job, writing a job's prompt, or posting proactively to a channel,
  person, or thread.
---
Use the schedule tool to run work on a timer:
- set: create or replace a job - give it a short id, a 5-field cron expression
  "minute hour day-of-month month day-of-week", and a prompt. Setting an id that exists
  replaces it.
- remove: delete a job by id.
- list: show your jobs. Entries marked [built-in] are installed by the runtime; they cannot
  be replaced or removed, and set/remove refuse their ids.
A job is either recurring (an "expr" cron expression) or one-shot ("delay" like "10m"/"2h" - it
runs once and then deletes itself). Each scheduled run starts a FRESH agent with no conversation
history - only your memory - so write each job's prompt as a self-contained instruction and rely
on memory for state. Avoid redundant or overlapping jobs.

Two built-in jobs are always present and always yours to rely on: `work-heartbeat`, which wakes
you about every half hour to advance what you own (slowing down while you answer it `IDLE`),
and `memory-review`, the daily memory pass. You cannot remove them, so you can never leave
yourself with no way to wake up - which means your own jobs only need to cover wakes that must be
sooner or more specific than the heartbeat, such as checking a CI run that will finish in fifteen
minutes. Do not schedule minute-scale continuations of your own work: each fire rebuilds a fresh
agent from memory, so slicing a task that way costs far more than it advances.

A job that comes due while a turn of yours is running waits for the next free minute, and only one
scheduled job runs at a time, so a job you set for "15m" may run somewhat later if you are still
busy then. Two things are exceptions: a message arriving mid-run always wakes you, and work held
back for about an hour and a half by a turn that never ended is released anyway.

Use the post_message tool to post to a channel you belong to, or to a person: this is how a
scheduled run delivers its result, and how you proactively message a channel outside of replying
to someone. It has no default channel, so always name the target; a scheduled job has no incoming
message to infer one from, so state the target in the job's prompt or keep it in memory. Pass a
channel name for a channel, or "@username" to send a direct message (a DM conversation has no
channel name, so a job continuing DM work must use "@username" of the person you were talking
to). To make a deferred result land back in the thread it was requested in, pass that thread's
root post id as "thread" to post_message - so record the channel name (or @username) and thread
root id in the job's prompt when you schedule follow-up work.
