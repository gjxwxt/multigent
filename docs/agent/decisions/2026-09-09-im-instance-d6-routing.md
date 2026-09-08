# IM-instance D6 routing

## Decision

Permit D6 fallback across different Agent connections only when their active connections share the same `admin_attested` IM instance and the named recipient currently has access to the destination binding. If the Bots differ, pass the external user ID but clear the stored source `ChatID`.

## Rationale

A Mattermost DM is created for a particular Bot/user pair. Reusing Bot A's channel ID with Bot B can post to the wrong conversation or fail. The existing Mattermost driver already opens a direct channel when it receives a user ID with no `ChatID`, so the routing layer owns the boundary check and channel-ID discard.

## Consequence of reverting

Keeping D6 connection-local forces repeat user binding for every Bot. Passing the source channel ID across Bots risks invalid or misrouted delivery. Removing live destination RBAC lets historical bindings receive notifications after access revocation.
