-- Runs once, when compose's PostgreSQL starts with an empty volume
-- (/docker-entrypoint-initdb.d). The development database "goflow" comes
-- from POSTGRES_DB; the tests get their own, so they can never touch it.
CREATE DATABASE goflow_test;
