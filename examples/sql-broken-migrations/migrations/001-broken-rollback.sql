-- Rollback partner for 001-broken-apply.sql. The apply file is intentionally
-- broken and never succeeds, so this rollback is never executed; it exists
-- only to satisfy the db-migrations apply/rollback file-pair contract.
SELECT 1;
