# Contract Baseline (契约基线)

This directory holds the **frozen cross-task contracts** that multiple agents
(human or AI) coordinate against. Contracts exist so parallel work never
diverges: every agent codes against the same API shape, error codes, and table
definitions instead of guessing from whatever code they happened to read.

> Single-task projects may skip this directory and record decisions directly
> in AGENTS.md. The moment a second workstream touches the same endpoints or
> tables, contracts become mandatory.

## Contents

| File | Freezes | When to fill it |
|---|---|---|
| `api-skeleton.md` | The API surface (paths, methods, request/response shapes) | Before parallel implementation starts |
| `error-codes.md` | The error code catalog (must mirror `GlobalExceptionHandler`) | With the API skeleton |
| `ddl.md` | Table definitions once a database profile is enabled | When `db` profile / real DB lands |

## Change control (the rule that makes contracts work)

1. **Contract changes require a dedicated MR** whose title starts with
   `contract:` — reviewers know to check blast radius across all consumers.
2. **`docs/api-spec.md` and controllers must change in the same MR** as the
   contract. A contract change without a spec change (or vice versa) is an
   incomplete MR and must be rejected.
3. **Never edit a frozen contract casually.** If a consumer already depends on
   it, the change is additive (new endpoint, new optional field, new error
   code) or it is a breaking change that requires announcing the migration in
   the task description.
4. Downstream agents may **read** contracts at any time and must treat them as
   ground truth even when they disagree with older code.
