# sql-broken-migrations

An Application whose `spec.sql.migrations` references a deliberately broken migration (`SELECT * FROM nonexistent;`). Used as an e2e fixture for the migration failure path.

The db-operator migrations runner fails the migration; the wp-operator surfaces `Ready: False, reason: MigrationFailed` on the Application status and never pushes function config — so the declared HTTP route is never wired up at the gateway.

The accompanying e2e test (`TestSQLBrokenMigrationsFailurePath`) asserts both behaviours.
