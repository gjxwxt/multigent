# Current handoff

## Start here

Branch: `feat/chatops-live-card-and-d6`.
Latest commit: `d15e7fd` (Harden binding conflict defense and ChatOps approval identity resolution).

All 4 mandates from the architecture review have been completed, verified with automated tests, and deployed to the VM:
1. `UserChannelIdentity` is the sole source of truth for Mattermost (stopped writing to global `external_identities`).
2. `ActionTokenPayload` and `DialogTokenPayload` strictly enforce `ConnectionID`; missing or tampered connection fails closed.
3. `ClaimMattermostIdentityInScope` enforces atomic check-and-insert in an SQLite transaction lock.
4. Unified trusted boundary helpers in `internal/api/im_boundary_helper.go` shared across D6, `/mg bind`, and ChatOps callbacks.

## Non-negotiable boundaries

- A URL is a display hint, not an IM-instance identity or authorization boundary.
- `admin_attested` is an administrator's explicit control-plane assertion, not a Mattermost protocol fingerprint.
- D6 and ChatOps approvals may cross `ConnectionID` only when both connections are active, share one `admin_attested` instance, and the recipient has live access to the destination route.
- A cross-Bot Mattermost delivery must discard the source DM `ChatID`; the driver creates a destination-Bot DM from `ExternalUserID`.
- Card and dialog callbacks must load secrets directly from the token's verified `ConnectionID`; never fall back to "first binding".
- Ambiguous identity in a trusted scope must fail closed and record a security audit alert.

## Next work

IM Slash Command instance gateway convergence:
Moving to a single unified Slash Command gateway (`/mg`) per Mattermost Team/Instance so multiple Bot endpoints share one slash command webhook, routing subcommands safely to the appropriate project/agent.

## Evidence

- `internal/db/user_channel_identities_test.go`: `TestClaimMattermostIdentityInScope`, `TestClaimMattermostIdentity_ConcurrentMutualExclusion`.
- `internal/api/chatops_identity_hardening_test.go`: All 5 acceptance tests covering cross-instance isolation, same-instance conflict defense, fail-closed connection checks, ambiguous identity defense, and same-instance cross-Bot approval with detachment revocation.
- Live VM deployment at `192.168.139.231:27892`.
