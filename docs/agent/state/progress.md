# Current progress

## Completed: IM identity center P0

Commit `9650f924` completed the safe visual grouping slice. The account API exposes only routes the current user may access by project or Agent Worker grant. A bound route remains visible solely so its owner can unbind it; an unbound and inaccessible route is omitted.

Verified locally on 2026-09-08:

- `go test -v ./internal/api -run TestUserIMIdentities_VisibilityRules_ABC`
- `make test`
- `make build`

The VM health endpoint responded successfully, but reports version `dev`; it is not provenance evidence for commit `9650f924`.

## Completed: IM instance association foundation

The control plane now stores an `im_instance_id` on each connection and an `im_instances` record with `admin_attested` provenance. Only a workspace administrator can create an association, attach a Bot connection, or detach it. Account cards prefer that association when grouping; the existing URL-origin grouping remains a display-only fallback.

Verified locally on 2026-09-08:

- `go test ./internal/db ./internal/api -run TestIMInstance`
- `make test`
- `make web`
- `make build`

## Completed: IM instance delivery routing

D6 now accepts a user identity from another Agent connection only if both active connections share the same `admin_attested` IM instance and the recipient still has access to the destination Agent route. For a different Mattermost Bot, it clears the original `ChatID`; the existing Mattermost driver then creates the destination Bot's direct channel from the external user ID. The account page labels this as same-instance reuse rather than a second direct bind.

Verified locally on 2026-09-09:

- `go test ./internal/api ./internal/imbridge -run TestD6OutboundIdentityFallback|TestRuntimeNotify|TestRuntimeChannels|TestMattermost`
- `make test`
- `make web`

## Next: IM approval identity hardening

`external_identities` is still keyed by `(workspace, provider, external_user_id)`. Do not use it as an instance-safe Mattermost lookup for binding conflicts or interactive-card approvals. Migrate those paths to source-binding or administrator-attested-instance scope before changing Slash Command registration or allowing a callback to resolve a user from a different Bot route.
