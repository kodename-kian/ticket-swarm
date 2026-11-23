-- dev-only service password
CREATE ROLE monitor LOGIN PASSWORD 'monitor_dev';
GRANT pg_monitor TO monitor;
