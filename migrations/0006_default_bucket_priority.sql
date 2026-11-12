-- Prefer the environment-configured bucket before newly added buckets (100).
-- Upgrade the former default value only; preserve other administrator values.
UPDATE storage_buckets SET priority = 0, updated_at = unixepoch()
WHERE id = 'bkt_default' AND priority = 1000;
