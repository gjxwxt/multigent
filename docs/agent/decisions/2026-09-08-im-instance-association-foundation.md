# IM instance association foundation

## Decision

Use a workspace-admin-created `im_instances` record and attach eligible Agent IM connections to it. Mark the provenance `admin_attested`; do not derive trust from a normalized URL.

## Rationale

Mattermost's public integration surface used here authenticates a Bot and exposes user/channel IDs, but this project has no reliable protocol-level server fingerprint available to distinguish every deployment and URL alias. A URL comparison would make a visual hint into an authorization boundary. An explicit administrator action is auditable and is the only trust claim this release makes.

## Consequence of reverting

Removing the association would return the account page to URL-only visual grouping and leave no durable place to attach later instance-scoped identity/routing rules. Enabling D6 cross-connection fallback without this boundary would remain unsafe.
