-- Shared artwork bytes are content addressed. Variant references are storage
-- metadata, never evidence that a Plex user is allowed to see the resource.
-- Authorization remains a bounded memory-only caller/resource lease.
CREATE TABLE artwork_stores (
    store_id text PRIMARY KEY CHECK (store_id ~ '^[0-9a-f]{64}$'),
    untracked_bytes bigint NOT NULL DEFAULT 0 CHECK (untracked_bytes >= 0),
    recovered_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artwork_blobs (
    store_id text NOT NULL REFERENCES artwork_stores(store_id) ON DELETE CASCADE,
    digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    byte_size bigint NOT NULL CHECK (byte_size > 0 AND byte_size <= 8388608),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, digest)
);

CREATE TABLE artwork_variants (
    store_id text NOT NULL,
    variant_key text NOT NULL CHECK (variant_key ~ '^[0-9a-f]{64}$'),
    blob_digest text NOT NULL,
    content_type text NOT NULL CHECK (length(content_type) <= 255),
    priority smallint NOT NULL CHECK (priority BETWEEN 0 AND 3),
    expires_at timestamptz NOT NULL,
    last_accessed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, variant_key),
    FOREIGN KEY (store_id, blob_digest) REFERENCES artwork_blobs(store_id, digest) ON DELETE CASCADE
);
CREATE INDEX artwork_variant_blobs ON artwork_variants(store_id, blob_digest);
CREATE INDEX artwork_variant_eviction ON artwork_variants(store_id, priority, last_accessed_at);

CREATE TABLE artwork_reservations (
    store_id text NOT NULL REFERENCES artwork_stores(store_id) ON DELETE CASCADE,
    reservation_id text NOT NULL CHECK (reservation_id ~ '^[0-9a-f]{32}$'),
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes > 0),
    priority smallint NOT NULL CHECK (priority BETWEEN 0 AND 3),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (store_id, reservation_id)
);
CREATE INDEX artwork_reservation_expiry ON artwork_reservations(store_id, expires_at);
