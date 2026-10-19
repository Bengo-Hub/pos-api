-- Per-service tenant backups were removed: tenants back up from the SSO client area (auth-api
-- collects /api/v1/s2s/tenant-export) and the platform pg_dumpall covers disaster recovery.
DROP TABLE IF EXISTS "backups";
DROP TABLE IF EXISTS "backup_settings";
