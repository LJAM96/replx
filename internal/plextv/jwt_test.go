package plextv

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeviceSignerRoundtrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	seed := priv.Seed()
	_ = pub
	signer := DeviceSigner{Seed: seed, Kid: "kid12345", ClientID: "client-1"}
	tok, err := signer.Sign("plex.tv", map[string]any{"nonce": "n-1"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("compact segments: %s", tok)
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if !strings.Contains(string(header), "EdDSA") || !strings.Contains(string(header), "kid12345") {
		t.Fatalf("header: %s", header)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	for _, want := range []string{`"iss":"client-1"`, `"aud":"plex.tv"`, `"nonce":"n-1"`} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("payload missing %s: %s", want, payload)
		}
	}
	// Signature verifies under the public key.
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("signature does not verify")
	}
}

func craftJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "sub": "42"})
	return enc.EncodeToString([]byte(`{"alg":"EdDSA"}`)) + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte("sig"))
}

func TestParseExpiry(t *testing.T) {
	exp, ok := ParseExpiry(craftJWT(t, time.Now().Add(7*24*time.Hour)))
	if !ok || time.Until(exp) < 6*24*time.Hour {
		t.Fatalf("exp: %v %v", exp, ok)
	}
	if _, ok := ParseExpiry("opaque-legacy-token"); ok {
		t.Fatal("legacy token must not parse")
	}
}

// jwtFake speaks the documented JWT shapes.
func jwtFake(t *testing.T, pub, seed []byte, pinCode string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/pins", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			JWK map[string]string `json:"jwk"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.JWK["x"] == "" {
			w.WriteHeader(http.StatusBadRequest) // legacy callers use the plain endpoint shape
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "code": pinCode})
	})
	mux.HandleFunc("/api/v2/pins/11", func(w http.ResponseWriter, r *http.Request) {
		djwt := r.URL.Query().Get("deviceJWT")
		if djwt == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parts := strings.Split(djwt, ".")
		if len(parts) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if !ed25519.Verify(ed25519.PublicKey(pub), []byte(parts[0]+"."+parts[1]), sig) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "authToken": craftJWT(t, time.Now().Add(7*24*time.Hour))})
	})
	mux.HandleFunc("/api/v2/auth/nonce", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nonce": "nonce-9"})
	})
	mux.HandleFunc("/api/v2/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"authToken": craftJWT(t, time.Now().Add(7*24*time.Hour))})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestJWTPINFlow(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	seed := priv.Seed()
	srv := jwtFake(t, pub, seed, "JJJJ")
	c := &Client{BaseURL: srv.URL, ClientIdentifier: "cid-1"}
	jwk, _ := json.Marshal(map[string]string{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub), "kid": "kid"})
	pin, err := c.CreatePINJWT(t.Context(), jwk)
	if err != nil || pin.Code != "JJJJ" {
		t.Fatalf("pin: %+v %v", pin, err)
	}
	signer := DeviceSigner{Seed: seed, Kid: "kid", ClientID: "cid-1"}
	djwt, err := signer.Sign("plex.tv", nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.PollPINJWT(t.Context(), 11, djwt)
	if err != nil || tok == "" {
		t.Fatalf("poll: %q %v", tok, err)
	}
	if exp, ok := ParseExpiry(tok); !ok || time.Until(exp) < 6*24*time.Hour {
		t.Fatal("expected 7-day JWT")
	}
	nonce, err := c.Nonce(t.Context(), tok)
	if err != nil || nonce != "nonce-9" {
		t.Fatalf("nonce: %q %v", nonce, err)
	}
	djwt2, _ := signer.Sign("plex.tv", map[string]any{"nonce": nonce})
	fresh, err := c.RefreshToken(t.Context(), djwt2)
	if err != nil || fresh == "" {
		t.Fatalf("refresh: %q %v", fresh, err)
	}
}
