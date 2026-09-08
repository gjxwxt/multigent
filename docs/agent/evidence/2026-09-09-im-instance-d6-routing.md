# IM-instance D6 routing — verification

## Covered behavior

- Same-connection fallback still succeeds and retains its direct-message channel ID.
- A different Bot in the same administrator-attested Mattermost instance receives the external user ID with an empty `ChatID`, requiring a fresh direct channel for that Bot.
- An unassociated connection does not cross the D6 boundary.
- A user whose current destination-route access was removed cannot receive the notification despite a historical bind.
- The account API labels the second Bot as `sharedBinding`; the UI renders it as same-instance reuse rather than another bind action.

## Automated checks

Run locally on 2026-09-09:

| Command | Result |
| --- | --- |
| `go test ./internal/api ./internal/imbridge -run TestD6OutboundIdentityFallback\|TestRuntimeNotify\|TestRuntimeChannels\|TestMattermost` | Pass |
| `make test` | Pass |
| `make web` | Pass |

## Deferred boundary

Mattermost Slash Command bind-conflict checks and interactive-card callback resolution still have provider-global `external_identities` fallbacks. This delivery change does not use those fallbacks; they need instance-scoped migration before approval identity can span Bots.
