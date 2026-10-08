# API Skeleton (frozen baseline)

Mirrors `docs/api-spec.md`. When an endpoint changes here, `docs/api-spec.md`
and the controller change in the same MR (see contract/README.md change
control).

## Health

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness probe for containers and the preview engine |

## Items (sample domain)

| Method | Path | Purpose | Status codes |
|---|---|---|---|
| GET | `/api/v1/items` | List items (newest first) | 200 |
| GET | `/api/v1/items/{id}` | Fetch one item | 200, 404 |
| POST | `/api/v1/items` | Create item (`title` required, ≤100 chars) | 201, 400 |
| DELETE | `/api/v1/items/{id}` | Delete item | 204, 404 |

## Adding a domain

Copy the table format, list every path with its status codes, then implement
controller + service + tests against exactly this list. New endpoints must
appear here **before** parallel implementation branches branch off.
