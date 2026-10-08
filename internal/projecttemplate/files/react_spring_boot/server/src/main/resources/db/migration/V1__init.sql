-- Baseline schema for the optional `db` profile (application-db.yml).
-- Item table mirrors the in-memory ItemRepository contract so switching
-- repository implementations is a drop-in change (see docs/architecture.md §4).
CREATE TABLE IF NOT EXISTS items (
    id          VARCHAR(64)  PRIMARY KEY,
    title       VARCHAR(100) NOT NULL,
    description VARCHAR(500),
    status      VARCHAR(32)  NOT NULL DEFAULT 'PENDING',
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
