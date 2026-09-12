-- App installation identity for Plex owner onboarding.
-- One row per Replx Edge installation. The Ed25519 private key is stored
-- encrypted (see internal/crypto); the stable client identifier binds all
-- Plex PIN and API calls to this installation.

CREATE TABLE app_identity (
    id text PRIMARY KEY DEFAULT 'singleton' CHECK (id = 'singleton'),
    client_identifier text NOT NULL,
    jwk_public jsonb NOT NULL,
    jwk_private_ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
