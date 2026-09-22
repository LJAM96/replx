-- Replx Edge migration 0006: database-enforced singleton administrator.
--
-- Production 1.0 supports one local administrator account. Application
-- code serializes setup with an advisory lock, but only a database
-- constraint makes concurrent setup attempts safe: a count followed by an
-- insert is not atomic. A unique index over a constant expression permits
-- at most one row in the table. Plain DDL only.
CREATE UNIQUE INDEX IF NOT EXISTS admin_users_singleton_idx ON admin_users ((1));
