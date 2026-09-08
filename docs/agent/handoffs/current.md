# Current handoff

## Start here

Branch: `feat/chatops-live-card-and-d6`.

P0 for the IM identity center is complete in commit `9650f924`. It groups account-page cards visually by provider and URL origin, but binding and unbinding remain scoped to one `ConnectionID`.

## Non-negotiable boundaries

- A URL is a display hint, not an IM-instance identity or authorization boundary.
- D6 currently reuses identities only when the source and destination bindings have the same `ConnectionID`.
- A Bot-specific Mattermost DM channel cannot be reused by another Bot.
- Project or Agent Worker access is required before exposing a route or generating its bind code.

## Next work

Plan P1 as an additive, verified IM-instance association. Do not implement cross-Bot identity reuse until that association, its administration flow, and negative RBAC tests are defined.

## Evidence

See `../evidence/2026-09-08-im-identity-center-p0.md` and `../decisions/2026-09-08-im-identity-routing-boundary.md`.
