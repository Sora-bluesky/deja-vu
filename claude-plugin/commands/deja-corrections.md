---
name: deja-corrections
description: Find what the user keeps having to teach their agents, from their own past sessions (deja-vu)
---

The user's past sessions hold the turns where they corrected an agent — the
agent assumed one thing, and they wrote out what is actually true in their
work. Your job is to find those turns, distil what each one taught, and offer
the lessons worth keeping. Nothing is saved unless the user says so.

Scope: `$ARGUMENTS` may name a project or a period such as `30d`. Default to
the last 30 days of every project.

1. Read the candidate turns, newest first, a page at a time:

   ```bash
   deja corrections --candidates --json --since 30d --limit 50 --offset 0
   ```

   Add `--project <name>` when the user named one. Each turn carries
   `agent_before` (the end of the agent's previous reply), `command_before`,
   `turn` (what the user wrote), `agent_after` (the start of the next reply)
   and `acknowledged` (deja saw the agent concede). Keep paging with
   `--offset` until you have read 300 turns or `total`, whichever is smaller.

2. Judge each turn. It is a correction when the user says, or plainly
   implies, that the agent's previous claim, assumption, step or output was
   wrong or missing. It is not one when it is the next instruction, an
   approval, a question that does not assert a mistake, thanks, a new
   request, or text no person wrote (another session halting a worker,
   scheduled checks, automatic approvals). `acknowledged: true` is right
   about four times in five; most corrections are not acknowledged, so read
   every turn.

3. For each correction write the lesson as one line: what the agent assumed,
   and what is true instead — "assumed pnpm; this repository uses bun".
   Mark it `domain` when it is about the work itself (facts, tools, steps,
   quality standards) and `workflow` when it is about how the agent should
   work with the user (asking versus proceeding, reporting, delegating).
   Merge lessons that say the same thing and count the sessions behind each.

4. Show the user a table, most-repeated first: lesson, kind, sessions, last
   seen, project. Do not quote secrets, credentials or personal data from the
   turns. Ask which lessons to keep, and whether to reword any.

5. For each lesson the user approves, and only those:

   ```bash
   deja remember --tag correction --tag <domain|workflow> --project <project> "<lesson>"
   ```

   Saved lessons outrank raw transcripts in every later recall, so the next
   agent starts already knowing them.

If the deja CLI is unavailable, say so and stop rather than guessing from
general knowledge.
