-- A reservation remains responsible for a published filesystem candidate
-- until the blob/reference transaction commits or cleanup confirms removal.
ALTER TABLE artwork_reservations ADD COLUMN candidate_digest text
    CHECK (candidate_digest IS NULL OR candidate_digest ~ '^[0-9a-f]{64}$');
