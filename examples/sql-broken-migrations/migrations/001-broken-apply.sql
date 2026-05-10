-- Deliberately broken migration used by the failure-path e2e fixture.
-- References a table that does not exist; the db-operator migrations runner
-- must fail this migration, causing the wp-operator to hold the Application
-- at Ready=False, reason=MigrationFailed.
SELECT * FROM nonexistent;
