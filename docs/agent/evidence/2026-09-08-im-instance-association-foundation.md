# IM instance association foundation — verification

## Scope

- `connections.im_instance_id` and `im_instances` persist an administrator-attested association.
- Admin-only APIs create, attach, detach, list, and delete empty associations.
- Agent channel details provide the admin control; the account page groups confirmed associations before URL-origin display grouping.
- No cross-Bot notification, slash-command, or approval-routing behavior changed.

## Automated checks

Run on 2026-09-08 in the local repository:

| Command | Result |
| --- | --- |
| `go test ./internal/db ./internal/api -run TestIMInstance` | Pass |
| `make test` | Pass |
| `make web` | Pass |
| `make build` | Pass |

The Vite build retained its pre-existing large-chunk advisory; it is not a build failure.

## Deliberately unverified / deferred

- No VM deployment was performed for this change.
- The system has not enabled cross-Bot D6 reuse; its end-to-end behavior must be verified only after instance-scoped identity and RBAC hardening lands.
