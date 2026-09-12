-- Link playback sessions to their diagnostic traces. Trace expiry must
-- never delete playback history: ON DELETE SET NULL.

ALTER TABLE playback_sessions
    ADD CONSTRAINT playback_sessions_trace_id_fkey
    FOREIGN KEY (trace_id) REFERENCES diagnostic_traces(id)
    ON DELETE SET NULL;
