# Handoff — Task Snapshot

> One file per task, named `t-<task-id>.md` (e.g. `t-20261004-fu91es.md`).
> A handoff is a **snapshot, not a plan**: it records what was true when the
> task ended so the next session (human or agent) can resume without
> re-deriving everything.

## When to write one

- Every workflow step completion, and always before a session ends.
- Update an existing handoff **only while its task is still open**.

## The archiving rule (non-negotiable)

Once a task reaches a terminal state (done / cancelled / rejected), its
handoff file is **frozen forever**. Never edit a closed handoff — new state
goes into the new task's own handoff. History stays honest and mistakes don't
propagate backwards.

## Never put in a handoff

- Code dumps (link files and line numbers instead).
- Credentials, tokens, endpoints with secrets, internal network topology.
- Predictions or promises ("will refactor later") — that belongs to the task
  tracker, not a snapshot.

---

## Template (copy everything below)

```markdown
# Handoff: t-<task-id> — <one-line title>

Date: YYYY-MM-DD
Status: done | in-progress | blocked
Branch: <branch name>

## What was done
- <bullet per deliverable, with file paths>

## Verification evidence
- <command> -> <result> (e.g. `make test` -> all green, 42 tests)
- <manual/behavioral check if applicable>

## Not done (priority order)
1. <item> — <why / what unblocks it>

## Risks & pitfalls hit
- <non-obvious traps the next session must know>
```
