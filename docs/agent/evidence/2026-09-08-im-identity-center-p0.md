# IM identity center P0 verification

## Scope

Commit `9650f924` adds display-only grouping for IM connections on the account page and response filtering for visible Bot routes.

## Confirmed behavior

- Project access and Agent Worker access independently permit a route to be returned.
- A historical binding remains visible without route access so its owner can unbind it.
- An unbound connection with no visible route is omitted from the response.
- Every bind and unbind action still targets a single connection.

## Verification

On 2026-09-08, the following commands passed locally:

```text
go test -v ./internal/api -run TestUserIMIdentities_VisibilityRules_ABC
make test
make build
```

The public VM health endpoint returned `{\"ok\":true,\"version\":\"dev\"}`. That proves reachability only, not which source commit is deployed.

## Deliberate boundary

P0 does not create an IM-instance identity, share bindings across connections, or change D6 routing.
