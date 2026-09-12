# Database Schema

## Scope

Production 1.0 supports one active PMS origin. Tables retain `server_id` so a future multi server release can evolve without destructive key changes.

The runtime enforces a maximum of one enabled `plex_servers` row.

## Extensions

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
```

## plex_servers

```sql
CREATE TABLE plex_servers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    internal_origin_url text NOT NULL,
    client_media_origin_url text,
    machine_identifier text NOT NULL,
    friendly_name text,
    plex_version text,
    detected_api_version text,
    enabled boolean NOT NULL DEFAULT true,
    connection_status text NOT NULL DEFAULT 'unknown',
    last_connected_at timestamptz,
    last_event_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX plex_servers_one_enabled_idx
ON plex_servers ((enabled))
WHERE enabled = true;
```

## plex_owner_credentials

Ciphertext fields are encrypted at application level with key material derived from `REPLX_EDGE_SECRET_KEY`.

```sql
CREATE TABLE plex_owner_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL UNIQUE REFERENCES plex_servers(id) ON DELETE CASCADE,
    plex_account_id bigint,
    replx_edge_client_identifier text NOT NULL,
    jwk_public jsonb NOT NULL,
    jwk_private_ciphertext bytea NOT NULL,
    owner_token_ciphertext bytea,
    owner_token_expires_at timestamptz,
    pms_access_token_ciphertext bytea,
    selected_resource_id text,
    last_refreshed_at timestamptz,
    status text NOT NULL DEFAULT 'pending',
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

## plex_identities

```sql
CREATE TABLE plex_identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    plex_account_id bigint,
    username text,
    friendly_name text,
    identity_type text NOT NULL,
    restricted boolean,
    unresolved boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX plex_identities_server_idx ON plex_identities(server_id);
CREATE UNIQUE INDEX plex_identities_server_account_idx
ON plex_identities(server_id, plex_account_id)
WHERE plex_account_id IS NOT NULL;
```

## plex_token_identities

```sql
CREATE TABLE plex_token_identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    identity_id uuid REFERENCES plex_identities(id) ON DELETE SET NULL,
    token_fingerprint text NOT NULL,
    token_ciphertext bytea,
    token_status text NOT NULL DEFAULT 'unknown',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    last_validated_at timestamptz,
    UNIQUE(server_id, token_fingerprint)
);
```

## client_instances

```sql
CREATE TABLE client_instances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    plex_client_identifier text NOT NULL,
    friendly_name text,
    product text,
    product_version text,
    platform text,
    platform_version text,
    device text,
    model text,
    vendor text,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(server_id, plex_client_identifier)
);

CREATE INDEX client_instances_last_seen_idx ON client_instances(server_id, last_seen_at DESC);
```

## identity_client_bindings

```sql
CREATE TABLE identity_client_bindings (
    identity_id uuid NOT NULL REFERENCES plex_identities(id) ON DELETE CASCADE,
    client_instance_id uuid NOT NULL REFERENCES client_instances(id) ON DELETE CASCADE,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(identity_id, client_instance_id)
);
```

## capability_observations

```sql
CREATE TABLE capability_observations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_instance_id uuid NOT NULL REFERENCES client_instances(id) ON DELETE CASCADE,
    observation_type text NOT NULL,
    source text NOT NULL,
    product_version text,
    capabilities jsonb NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX capability_observations_client_idx
ON capability_observations(client_instance_id, observed_at DESC);
```

A major client version change reduces or resets learned routing certainty at application level.

## libraries

```sql
CREATE TABLE libraries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    plex_section_id text NOT NULL,
    section_uuid text,
    title text NOT NULL,
    media_type text NOT NULL,
    agent text,
    scanner text,
    updated_at_origin timestamptz,
    synced_at timestamptz,
    UNIQUE(server_id, plex_section_id)
);
```

## library_items

```sql
CREATE TABLE library_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    library_id uuid REFERENCES libraries(id) ON DELETE CASCADE,
    rating_key text NOT NULL,
    item_key text,
    item_type text NOT NULL,
    title text,
    sort_title text,
    original_title text,
    year integer,
    parent_rating_key text,
    grandparent_rating_key text,
    duration_ms bigint,
    thumb text,
    art text,
    added_at timestamptz,
    updated_at_origin timestamptz,
    raw_metadata jsonb,
    search_vector tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', coalesce(title,'') || ' ' || coalesce(original_title,'') || ' ' || coalesce(sort_title,''))
    ) STORED,
    synced_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(server_id, rating_key)
);

CREATE INDEX library_items_library_idx ON library_items(library_id);
CREATE INDEX library_items_fts_idx ON library_items USING gin(search_vector);
CREATE INDEX library_items_title_trgm_idx ON library_items USING gin(title gin_trgm_ops);
CREATE INDEX library_items_sort_title_trgm_idx ON library_items USING gin(sort_title gin_trgm_ops);
CREATE INDEX library_items_original_title_trgm_idx ON library_items USING gin(original_title gin_trgm_ops);
```

## item_guids

```sql
CREATE TABLE item_guids (
    item_id uuid NOT NULL REFERENCES library_items(id) ON DELETE CASCADE,
    guid text NOT NULL,
    provider text,
    provider_id text,
    PRIMARY KEY(item_id, guid)
);

CREATE INDEX item_guids_provider_idx ON item_guids(provider, provider_id);
```

## canonical_works

```sql
CREATE TABLE canonical_works (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_type text NOT NULL,
    canonical_provider text,
    canonical_provider_id text,
    title text,
    year integer,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE canonical_work_items (
    canonical_work_id uuid NOT NULL REFERENCES canonical_works(id) ON DELETE CASCADE,
    library_item_id uuid NOT NULL REFERENCES library_items(id) ON DELETE CASCADE,
    confidence numeric(5,4) NOT NULL DEFAULT 1,
    match_method text NOT NULL,
    PRIMARY KEY(canonical_work_id, library_item_id)
);
```

Production 1.0 groups separate Plex items automatically only when strong provider identifiers agree.

## media_variants

```sql
CREATE TABLE media_variants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    library_item_id uuid NOT NULL REFERENCES library_items(id) ON DELETE CASCADE,
    plex_media_id text,
    media_index integer NOT NULL,
    container text,
    video_codec text,
    video_profile text,
    width integer,
    height integer,
    bitrate_kbps integer,
    video_resolution text,
    normalized_dynamic_range text NOT NULL DEFAULT 'UNKNOWN',
    hdr_format text,
    audio_codec text,
    audio_channels integer,
    duration_ms bigint,
    raw_media jsonb,
    synced_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(library_item_id, media_index)
);

CREATE INDEX media_variants_item_idx ON media_variants(library_item_id, media_index);
```

## media_parts

```sql
CREATE TABLE media_parts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    media_variant_id uuid NOT NULL REFERENCES media_variants(id) ON DELETE CASCADE,
    plex_part_id text NOT NULL,
    part_index integer NOT NULL,
    plex_key text,
    container text,
    size_bytes bigint,
    duration_ms bigint,
    raw_part jsonb,
    UNIQUE(media_variant_id, part_index)
);

CREATE UNIQUE INDEX media_parts_plex_part_id_idx ON media_parts(plex_part_id);
```

## media_streams

```sql
CREATE TABLE media_streams (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    media_part_id uuid NOT NULL REFERENCES media_parts(id) ON DELETE CASCADE,
    plex_stream_id text,
    stream_type integer NOT NULL,
    codec text,
    profile text,
    language text,
    language_code text,
    channels integer,
    bitrate integer,
    width integer,
    height integer,
    selected boolean,
    forced boolean,
    default_stream boolean,
    metadata jsonb
);
```

## policies

```sql
CREATE TABLE policies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    scope_type text NOT NULL CHECK (scope_type IN ('global','user','device')),
    scope_id uuid,
    name text NOT NULL,
    config jsonb NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (scope_type = 'global' AND scope_id IS NULL)
        OR (scope_type IN ('user','device') AND scope_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX policies_one_global_idx
ON policies(server_id)
WHERE scope_type = 'global';

CREATE UNIQUE INDEX policies_scope_idx
ON policies(server_id, scope_type, scope_id)
WHERE scope_type IN ('user','device') AND scope_id IS NOT NULL;
```

Only one enabled global policy row is effective. Additional global rows must remain disabled or the admin API must reject them. User and device scopes allow at most one row per scoped object.

## compatibility_profiles

```sql
CREATE TABLE compatibility_profiles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    platform text,
    product text,
    product_version_pattern text,
    device_model_pattern text,
    playback_type text,
    direct_origin_status text NOT NULL DEFAULT 'UNKNOWN',
    media_gateway_status text NOT NULL DEFAULT 'UNKNOWN',
    confidence numeric(5,4) NOT NULL DEFAULT 0,
    observations integer NOT NULL DEFAULT 0,
    last_observed_product_version text,
    notes text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

## playback_sessions

```sql
CREATE TABLE playback_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id),
    identity_id uuid REFERENCES plex_identities(id),
    client_instance_id uuid REFERENCES client_instances(id),
    plex_session_identifier text,
    rating_key text,
    selected_media_variant_id uuid REFERENCES media_variants(id),
    selected_media_part_id uuid REFERENCES media_parts(id),
    playback_mode text,
    routing_mode text,
    effective_policy jsonb,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    final_status text,
    trace_id uuid
);

CREATE INDEX playback_sessions_server_time_idx ON playback_sessions(server_id, started_at DESC);
CREATE INDEX playback_sessions_identity_time_idx ON playback_sessions(identity_id, started_at DESC);
CREATE INDEX playback_sessions_client_time_idx ON playback_sessions(client_instance_id, started_at DESC);
CREATE INDEX playback_sessions_plex_session_idx ON playback_sessions(plex_session_identifier);
```

`playback_sessions.trace_id` is a loose correlation to `diagnostic_traces.id`, not a hard foreign key. A playback session may exist without a targeted protocol trace, and trace expiry must never cascade-delete playback history.

## playback_decisions

```sql
CREATE TABLE playback_decisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    playback_session_id uuid REFERENCES playback_sessions(id) ON DELETE CASCADE,
    server_id uuid NOT NULL REFERENCES plex_servers(id),
    requested_media_index integer,
    selected_media_index integer,
    requested_resolution text,
    requested_bitrate_kbps integer,
    decision text NOT NULL,
    decision_reason text NOT NULL,
    plex_decision_code integer,
    details jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX playback_decisions_server_time_idx ON playback_decisions(server_id, created_at DESC);
CREATE INDEX playback_decisions_session_time_idx ON playback_decisions(playback_session_id, created_at DESC);
```

Default retention is 30 days. The janitor deletes older rows in bounded batches. If deployment volume later justifies partitioning, migrate this table to monthly range partitions without changing the API contract.

## sync_cursors

```sql
CREATE TABLE sync_cursors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id) ON DELETE CASCADE,
    sync_type text NOT NULL,
    library_id uuid REFERENCES libraries(id) ON DELETE CASCADE,
    cursor jsonb,
    status text NOT NULL,
    last_started_at timestamptz,
    last_completed_at timestamptz,
    last_error text,
    UNIQUE(server_id, sync_type, library_id)
);
```

## diagnostic_traces

```sql
CREATE TABLE diagnostic_traces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id uuid NOT NULL REFERENCES plex_servers(id),
    identity_id uuid REFERENCES plex_identities(id),
    client_instance_id uuid REFERENCES client_instances(id),
    trace_type text NOT NULL,
    status text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    expires_at timestamptz NOT NULL,
    summary jsonb
);

CREATE INDEX diagnostic_traces_expiry_idx ON diagnostic_traces(expires_at);
CREATE INDEX diagnostic_traces_server_time_idx ON diagnostic_traces(server_id, started_at DESC);
```

Full diagnostic events remain compressed NDJSON files, not unbounded database rows.

## audit_events

```sql
CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_subject text NOT NULL,
    action text NOT NULL,
    object_type text,
    object_id text,
    before_state jsonb,
    after_state jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_time_idx ON audit_events(created_at DESC);
```

Default audit retention is 180 days unless local requirements specify longer.

## admin_users

Local administrator account for Production 1.0. Password uses Argon2id with an individual salt. OIDC is future work.

```sql
CREATE TABLE admin_users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

## app_settings

Safe runtime tunables editable via the admin API. Secrets, listener bindings, ingress mode and database connection settings remain environment based and require restart.

```sql
CREATE TABLE app_settings (
    key text PRIMARY KEY,
    value jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

## app_identity

Installation identity for Plex owner onboarding. Exactly one row. The Ed25519 private key is stored encrypted with key material derived from `REPLX_EDGE_SECRET_KEY`; the stable client identifier binds Plex PIN and API calls to this installation.

```sql
CREATE TABLE app_identity (
    id text PRIMARY KEY DEFAULT 'singleton' CHECK (id = 'singleton'),
    client_identifier text NOT NULL,
    jwk_public jsonb NOT NULL,
    jwk_private_ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

## Retention worker

At least daily:

```text
expire diagnostic trace metadata past expires_at
remove trace files past retention
remove playback decisions and completed sessions older than configured retention
remove expired compatibility observations where safe
retain audit events according to audit retention
```

Deletion runs in bounded batches to avoid long database locks.
