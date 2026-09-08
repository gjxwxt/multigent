# Mattermost Compose Example

This directory contains a generic Docker Compose example for a persistent
Mattermost Team Edition installation with PostgreSQL.

Set the deployment-specific values in the environment before starting:

- POSTGRES_PASSWORD
- MM_SITE_URL
- MM_ALLOWED_UNTRUSTED_INTERNAL_CONNECTIONS

The callback URL and any internal-network allowlist must use addresses that
are reachable from the Mattermost container in the target deployment. Do not
commit those addresses or credentials.

Example:

    export POSTGRES_PASSWORD='change-me-before-start'
    export MM_SITE_URL='http://localhost:8065'
    export MM_ALLOWED_UNTRUSTED_INTERNAL_CONNECTIONS='127.0.0.1'
    docker compose up -d
