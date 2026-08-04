---
name: memory-hygiene
description: >-
  Procedure for the daily memory review - judging stale facts, compacting related ones,
  archiving outdated ones. Load when the memory-review job fires or when cleaning up memory.
---
Memory must stay small, current, and trustworthy: the index goes into every turn, so every stale
fact taxes every future decision, and a wrong one can steer it. This review runs daily via the
built-in `memory-review` job.

1. Run the memory tool's `audit` action. It lists every active fact with its age, when it was
   last read and how often, plus the archived fact names. If there are no facts, stop.

2. Run `schedule list` and note every memory fact a job prompt refers to. Those facts are
   PROTECTED: a job prompt may use a fact as its whole specification, so never archive, merge,
   or delete one. If a protected fact is wrong, revise it in place.

3. Judge each unprotected fact by its content first and its metadata second - age and a low use
   count are hints, not verdicts; an old, rarely-read fact can still be true and valuable. Use
   `inspect`, never `read`, to see a fact's content during a review: `read` counts as a use and
   would make every reviewed fact look freshly used at the next audit. Then:
   - partly stale -> revise in place (save under the same name with corrected content);
   - several related, rarely-used facts -> merge into one digest fact, then archive the
     originals;
   - wholly outdated or superseded -> archive it;
   - false or harmful if believed -> delete it.

4. Archiving is reversible: `restore` brings a fact back, archived names stay visible in
   `audit`, and `inspect` reads archived content. When in doubt between archive and delete,
   archive.

5. Keep exactly one fact named `memory-review-log`, revised in place each review, holding the
   review date and a short list of what changed, or "no changes". Do not create per-day log
   facts - that is the clutter this review exists to remove.

6. Never post a message during a review; work only through the memory and schedule tools.
