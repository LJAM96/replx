-- Replx Edge migration 0005: persist the complete negotiated selection on
-- playback_sessions independently of index foreign keys.
--
-- Live (non-indexed) negotiation supplies no media_variants/media_parts
-- UUIDs, so sessions previously reloaded empty part identifiers and media
-- index -1 through the variant/part LEFT JOINs. The part boundary then
-- treated the missing selection as permission and allowed arbitrary parts.
--
-- These columns record the negotiated selection directly. The boundary
-- fails closed when they cannot be reconstructed. Plain DDL only.
ALTER TABLE playback_sessions ADD COLUMN IF NOT EXISTS selected_media_index integer NOT NULL DEFAULT -1;
ALTER TABLE playback_sessions ADD COLUMN IF NOT EXISTS selected_part_plex_id text NOT NULL DEFAULT '';
ALTER TABLE playback_sessions ADD COLUMN IF NOT EXISTS selected_part_key text NOT NULL DEFAULT '';
