# Current handoff

## Start here

Branch: `feat/chatops-live-card-and-d6`.

P0 for the IM identity center is complete in commit `9650f924`. The follow-up association foundation is in the current branch head: a workspace administrator can explicitly group active Agent IM connections into an `admin_attested` instance. Account-page cards prefer this grouping, but binding and unbinding remain scoped to one `ConnectionID`.

## Non-negotiable boundaries

- A URL is a display hint, not an IM-instance identity or authorization boundary.
- `admin_attested` is an administrator's explicit control-plane assertion, not a Mattermost protocol fingerprint.
- D6 currently reuses identities only when the source and destination bindings have the same `ConnectionID`.
- A Bot-specific Mattermost DM channel cannot be reused by another Bot.
- Project or Agent Worker access is required before exposing a route or generating its bind code.

## Next work

Implement instance-scoped routing hardening before enabling cross-Bot identity reuse. Specifically: migrate Mattermost conflict/callback identity lookups away from provider-global `external_identities`, enforce live recipient access to the destination Agent route, and test that the destination Bot opens a new DM rather than reusing the source `ChatID`. Unassociated connections must remain fail-closed.

## Evidence

See `../evidence/2026-09-08-im-identity-center-p0.md`, `../evidence/2026-09-08-im-instance-association-foundation.md`, and the paired decision records.
