-- Owner-only collection catalog and immutable ordered membership snapshots.
-- A collection's published_generation changes only after a complete child scan.
CREATE SEQUENCE collection_snapshot_generation_seq;

CREATE TABLE library_collections (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    library_id uuid NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rating_key text NOT NULL,
    collection_key text,
    title text NOT NULL,
    sort_title text,
    summary text,
    thumb text,
    art text,
    smart boolean NOT NULL DEFAULT false,
    catalog_generation bigint NOT NULL DEFAULT 0,
    published_generation bigint,
    synced_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(library_id, rating_key)
);
CREATE INDEX library_collections_catalog_idx ON library_collections(library_id, catalog_generation);

CREATE TABLE collection_snapshots (
    collection_id uuid NOT NULL REFERENCES library_collections(id) ON DELETE CASCADE,
    generation bigint NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    item_count integer NOT NULL DEFAULT 0,
    content_hash text,
    metadata jsonb NOT NULL,
    PRIMARY KEY(collection_id, generation)
);

CREATE TABLE collection_members (
    collection_id uuid NOT NULL,
    generation bigint NOT NULL,
    position integer NOT NULL CHECK (position >= 0),
    rating_key text NOT NULL,
    item_type text NOT NULL,
    title text,
    thumb text,
    art text,
    PRIMARY KEY(collection_id, generation, position),
    UNIQUE(collection_id, generation, rating_key),
    FOREIGN KEY(collection_id, generation) REFERENCES collection_snapshots(collection_id, generation) ON DELETE CASCADE
);
CREATE INDEX collection_members_rating_key_idx ON collection_members(rating_key);

CREATE TABLE library_collection_syncs (
    library_id uuid PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    catalog_generation bigint NOT NULL DEFAULT 0,
    last_completed_at timestamptz,
    last_error text,
    invalidation_pending boolean NOT NULL DEFAULT false
);
