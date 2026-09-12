// Package plextv JWT support: EdDSA device JWTs for the Plex account API.
//
// Plex account authentication (September 2025 API release) uses a device
// public-key model: the device registers its Ed25519 JWK, then exchanges
// self-signed device JWTs for 7-day Plex JWTs usable exactly like legacy
// tokens in X-Plex-Token. JWS compact serialization needs no dependency:
// header.payload signed with crypto/ed25519, base64url-encoded.
package plextv

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DeviceSigner signs device JWTs with the installation Ed25519 seed.
type DeviceSigner struct {
	Seed     []byte
	Kid      string
	ClientID string
}

// Sign returns a compact JWS for audience aud merged with extra claims.
// Standard claims: iss=clientID, aud, iat, exp=now+5m, jti=random.
func (s DeviceSigner) Sign(aud string, extra map[string]any) (string, error) {
	if len(s.Seed) != ed25519.SeedSize {
		return "", fmt.Errorf("plextv: bad device seed length")
	}
	header := map[string]string{"alg": "EdDSA", "kid": s.Kid, "typ": "JWT"}
	now := time.Now()
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	payload := map[string]any{
		"iss": s.ClientID, "aud": aud,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"jti": hex.EncodeToString(jti),
	}
	for k, v := range extra {
		payload[k] = v
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(s.Seed), []byte(unsigned))
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// ParseExpiry extracts exp from a JWT payload without verifying the
// signature. It is scheduling metadata only, never an auth decision.
func ParseExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
