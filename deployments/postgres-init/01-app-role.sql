-- Local Compose only. The password is a development default, not a production
-- secret. Production injects AKF_DATABASE_URL from a secret manager and does
-- not mount this file.
DO $$
BEGIN
    CREATE ROLE akf_app LOGIN PASSWORD 'akf' NOSUPERUSER NOBYPASSRLS;
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

GRANT CONNECT ON DATABASE akf TO akf_app;
GRANT USAGE ON SCHEMA public TO akf_app;
