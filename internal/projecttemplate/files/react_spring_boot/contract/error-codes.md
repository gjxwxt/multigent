# Error Code Catalog (frozen baseline)

Single source of truth with `GlobalExceptionHandler`. Adding a code here
requires updating that handler in the same MR (and vice versa).

| Code | HTTP | Thrown by | Meaning |
|---|---|---|---|
| `RESOURCE_NOT_FOUND` | 404 | `ResourceNotFoundException` | Requested entity id does not exist |
| `VALIDATION_FAILED` | 400 | `MethodArgumentNotValidException` | Request body failed JSR-380 validation; `details[]` lists per-field messages |
| `BAD_REQUEST` | 400 | `IllegalArgumentException` | Malformed request semantics |
| `INTERNAL_SERVER_ERROR` | 500 | fallback handler | Unexpected server failure (details never leak the stack trace) |

Response envelope (see `docs/api-spec.md` §4):

```json
{
  "code": "RESOURCE_NOT_FOUND",
  "message": "Item with id 'item-999' was not found",
  "details": [],
  "timestamp": "2026-09-09T14:30:00Z"
}
```

Rules for new codes:

1. Codes are `UPPER_SNAKE_CASE`, domain-prefixed when specific
   (`ITEM_STATUS_INVALID` not `INVALID`).
2. One code per failure mode; never reuse a code for a different meaning.
3. Frontend `types/index.ts` `ApiError` consumers should branch on `code`,
   not on `message` text (messages are not stable across locales).
