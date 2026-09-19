-- Replx Edge migration 0002: crash-safe sweep generations and
-- history-preserving variant eviction.
--
-- library_items.sweep_gen stamps the full-sweep generation that last saw
-- each row, so a crashed sweep cannot delete rows it never visited: the
-- next sweep uses a fresh generation and only removes rows outside it.
--
-- Playback history FKs become ON DELETE SET NULL so evicting a stale
-- variant or part preserves the audit trail instead of violating it.
-- Plain DDL only (statement splitter has no plpgsql parser).
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS sweep_gen BIGINT;
CREATE INDEX IF NOT EXISTS library_items_sweep_gen_idx ON library_items(library_id, sweep_gen);
ALTER TABLE playback_sessions DROP CONSTRAINT IF EXISTS playback_sessions_selected_media_variant_id_fkey;
ALTER TABLE playback_sessions ADD CONSTRAINT playback_sessions_selected_media_variant_id_fkey FOREIGN KEY (selected_media_variant_id) REFERENCES media_variants(id) ON DELETE SET NULL;
ALTER TABLE playback_sessions DROP CONSTRAINT IF EXISTS playback_sessions_selected_media_part_id_fkey;
ALTER TABLE playback_sessions ADD CONSTRAINT playback_sessions_selected_media_part_id_fkey FOREIGN KEY (selected_media_part_id) REFERENCES media_parts(id) ON DELETE SET NULL;
