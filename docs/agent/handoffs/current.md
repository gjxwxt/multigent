# Current handoff

## Start here

Branch: `feat/chatops-live-card-and-d6`.

P0 for the IM identity center is complete in commit `9650f924`. The current branch adds administrator-attested IM instances and safe D6 notification reuse: a user bound through one Bot can receive a direct message from another Bot only inside the same attested instance and only while they retain destination-route access. Account-page cards show that as same-instance reuse; bind/unbind storage remains scoped to one `ConnectionID`.

## Non-negotiable boundaries

- A URL is a display hint, not an IM-instance identity or authorization boundary.
- `admin_attested` is an administrator's explicit control-plane assertion, not a Mattermost protocol fingerprint.
- D6 may cross `ConnectionID` only when both connections are active, share one `admin_attested` instance, and the recipient has live access to the destination route.
- A cross-Bot Mattermost delivery must discard the source DM `ChatID`; the driver creates a destination-Bot DM from `ExternalUserID`.
- Project or Agent Worker access is required before exposing a route or generating its bind code.

## Next work

Harden approval and binding identity resolution. Mattermost conflict detection in `agent_channel_mattermost_handlers.go` and callback user resolution in `chatops_handlers.go` still consult provider-global `external_identities`; they must be source-binding or instance-scoped before this feature can safely promise cross-Bot Slash Command/approval continuity. Do not weaken the D6 fail-closed checks while doing so.

## Evidence

See `../evidence/2026-09-08-im-identity-center-p0.md`, `../evidence/2026-09-08-im-instance-association-foundation.md`, `../evidence/2026-09-09-im-instance-d6-routing.md`, and the paired decision records.
