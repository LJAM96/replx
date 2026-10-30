-- Durable namespace invalidation counters. Scope identifiers are SHA-256
-- digests; raw tokens and user scopes are never persisted here. Empty scope
-- and class identify the server counter; empty class identifies a scope counter.
CREATE TABLE cache_generations (
    server_id text NOT NULL,
    scope_hash text NOT NULL,
    class text NOT NULL,
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    PRIMARY KEY (server_id, scope_hash, class),
    CHECK ((scope_hash = '' AND class = '') OR scope_hash ~ '^[0-9a-f]{64}$')
);

-- Retire default generation-zero namespaces from earlier releases. The cache
-- schema version also changes when adopting durable generations.
INSERT INTO cache_generations (server_id, scope_hash, class, generation)
VALUES ('default', '', '', 1);
