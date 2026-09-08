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

## Next: IM instance routing hardening

Do not widen D6 from `ConnectionID` to URL comparison or to `im_instance_id` yet. Mattermost dynamic DM creation already exists in the driver, but the surrounding identity and approval paths still use provider-global `external_identities`. First scope those paths to the administrator-attested instance and add live recipient RBAC checks. A cross-Bot delivery must clear the source Bot's DM channel ID so the destination Bot opens its own DM.
