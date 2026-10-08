# DDL (frozen once the `db` profile lands)

Empty by design: the default template uses an in-memory repository with no
schema. The first table definition gets frozen here when the `db` profile
(`application-db.yml`, seeded `V1__init.sql`) is activated or a real database
is introduced.

Format: one fenced `sql` block per table, final column order, with a one-line
owner note ("owned by task X") and the migration file that introduced it
(`db/migration/V2__*.sql` or later). See `V1__init.sql` for the baseline
`items` table once the db profile is on.
