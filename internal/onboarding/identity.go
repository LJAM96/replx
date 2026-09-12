// Package onboarding implements Plex owner onboarding: installation
// identity, PIN sign-in, single-PMS selection and the identity
// triple-check that proves Replx Edge is the same server, not a second one.
//
// State machine (persisted in app_settings + plex tables so a restart
// never orphans a claim window):
//
//	pending -> pin_issued -> owner_authenticated -> selected -> verified
//
// The pending owner token lives encrypted in app_settings until a PMS is
// selected; only then are plex_servers + plex_owner_credentials rows
// created (their FKs require a server). Raw tokens never leave process
// memory except as ciphertext, and never reach logs or the browser:
// the admin API only ever returns PIN codes, names and verification reports.
package onboarding

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LJAM96/replx/internal/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stages of the onboarding state machine.
const (
	StagePending            = "pending"
	StagePINIssued          = "pin_issued"
	StageOwnerAuthenticated = "owner_authenticated"
	StageSelected           = "selected"
	StageVerified           = "verified"
)

// Purposes for context-separated encryption.
const (
	PurposeDeviceKey = "device-key"
	PurposeOwnerJWT  = "owner-token"
	PurposePMSToken  = "pms-token"
)

// Service orchestrates onboarding against Postgres, plex.tv and the origin.
type Service struct {
	DB *pgxpool.Pool
	// NewTV builds a plex.tv client bound to the installation client ID.
	// Injected for tests (fake plex.tv); production dials plex.tv.
	NewTV func(clientID string) TVClient
	// ControlBase is the replx-edge control listener used to fetch the
	// proxied root during verify (exercises the real proxy path).
	// Defaults to http://127.0.0.1:32400.
	ControlBase string
	Secret      string
	PublicURL   string
	InternalURL string
}

// AppIdentity is the stable installation identity.
type AppIdentity struct {
	ClientID string
}

// EnsureIdentity returns the installation identity, creating and persisting
// it (client ID + Ed25519 JWK keypair, private half encrypted) on first run.
// Concurrent creators race safely: INSERT ... ON CONFLICT DO NOTHING
// followed by a re-read means losers return the stored winner, never a
// phantom local ID. Genuine database errors surface instead of masquerading
// as "no identity".
func (s *Service) EnsureIdentity(ctx context.Context) (AppIdentity, error) {
	var clientID string
	err := s.DB.QueryRow(ctx, "SELECT client_identifier FROM app_identity WHERE id='singleton'").Scan(&clientID)
	if err == nil {
		return AppIdentity{ClientID: clientID}, nil
	}
	if err != nil && !isNoRows(err) {
		return AppIdentity{}, fmt.Errorf("onboarding: identity lookup: %w", err)
	}
	id, priv, err := generateIdentity()
	if err != nil {
		return AppIdentity{}, err
	}
	seedCipher, err := crypto.Encrypt(s.Secret, PurposeDeviceKey, priv.Seed())
	if err != nil {
		return AppIdentity{}, err
	}
	jwk, err := json.Marshal(map[string]string{
		"kty": "OKP", "crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		"kid": id[:8],
	})
	if err != nil {
		return AppIdentity{}, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO app_identity(id, client_identifier, jwk_public, jwk_private_ciphertext)
		VALUES('singleton', $1, $2, $3) ON CONFLICT (id) DO NOTHING`, id, string(jwk), seedCipher); err != nil {
		return AppIdentity{}, fmt.Errorf("onboarding: save identity: %w", err)
	}
	if err := s.DB.QueryRow(ctx, "SELECT client_identifier FROM app_identity WHERE id='singleton'").Scan(&clientID); err != nil {
		return AppIdentity{}, fmt.Errorf("onboarding: identity re-read: %w", err)
	}
	return AppIdentity{ClientID: clientID}, nil
}

// DeviceCredentials returns the client ID, decrypted Ed25519 seed and
// public JWK for plex.tv device-JWT calls.
func (s *Service) DeviceCredentials(ctx context.Context) (clientID string, seed []byte, jwk json.RawMessage, err error) {
	var jwkRaw string
	var seedCipher []byte
	qerr := s.DB.QueryRow(ctx, `SELECT client_identifier, jwk_public, jwk_private_ciphertext
		FROM app_identity WHERE id='singleton'`).Scan(&clientID, &jwkRaw, &seedCipher)
	if qerr != nil {
		if !isNoRows(qerr) {
			return "", nil, nil, fmt.Errorf("onboarding: identity lookup: %w", qerr)
		}
		if _, cerr := s.EnsureIdentity(ctx); cerr != nil {
			return "", nil, nil, cerr
		}
		if qerr = s.DB.QueryRow(ctx, `SELECT client_identifier, jwk_public, jwk_private_ciphertext
			FROM app_identity WHERE id='singleton'`).Scan(&clientID, &jwkRaw, &seedCipher); qerr != nil {
			return "", nil, nil, fmt.Errorf("onboarding: identity re-read: %w", qerr)
		}
	}
	seed, err = crypto.Decrypt(s.Secret, PurposeDeviceKey, seedCipher)
	if err != nil {
		return "", nil, nil, fmt.Errorf("onboarding: device key: %w", err)
	}
	return clientID, seed, json.RawMessage(jwkRaw), nil
}

func isNoRows(err error) bool {
	return err != nil && (errors.Is(err, pgx.ErrNoRows) || strings.Contains(err.Error(), "no rows"))
}

func generateIdentity() (string, ed25519.PrivateKey, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("onboarding: rand: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("onboarding: ed25519: %w", err)
	}
	return hex.EncodeToString(b), priv, nil
}

// setting helpers persist small onboarding state in app_settings.

func getSetting(ctx context.Context, db *pgxpool.Pool, key string) (string, bool) {
	var raw json.RawMessage
	if err := db.QueryRow(ctx, "SELECT value FROM app_settings WHERE key=$1", key).Scan(&raw); err != nil {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}

func setSetting(ctx context.Context, db *pgxpool.Pool, key, value string) error {
	raw, _ := json.Marshal(value)
	_, err := db.Exec(ctx, `INSERT INTO app_settings(key, value, updated_at) VALUES($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, string(raw))
	return err
}

func delSetting(ctx context.Context, db *pgxpool.Pool, key string) {
	_, _ = db.Exec(ctx, "DELETE FROM app_settings WHERE key=$1", key)
}
