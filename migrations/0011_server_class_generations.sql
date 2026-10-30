-- Permit a server-wide counter for one response class (for example collection
-- catalog changes) without invalidating unrelated user scopes/classes.
ALTER TABLE cache_generations DROP CONSTRAINT cache_generations_check;
ALTER TABLE cache_generations ADD CONSTRAINT cache_generations_scope_class_check CHECK (
    (scope_hash = '' AND class = '') OR
    (scope_hash = '' AND class ~ '^[a-z][a-z0-9_-]{0,31}$') OR
    (scope_hash ~ '^[0-9a-f]{64}$' AND (class = '' OR class ~ '^[a-z][a-z0-9_-]{0,31}$'))
);
