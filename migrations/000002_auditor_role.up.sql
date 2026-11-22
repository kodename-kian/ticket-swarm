-- dev-only service password
CREATE ROLE auditor LOGIN PASSWORD 'auditor_dev';
-- column-level grants: DB enforces that auditor never reads tiers.available
GRANT SELECT (id, total_stock)  ON tiers TO auditor;
GRANT SELECT (tier_id, state)   ON holds TO auditor;
