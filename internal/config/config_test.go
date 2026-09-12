package config

import (
	"os"
	"testing"
)

func TestValidateRejectsShortSecret(t *testing.T) {
	c := Config{SecretKey: "short", PublicURL: "https://plex.example.com", IngressMode: "direct"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected secret key error")
	}
}

func TestValidateRejectsLowEntropySecret(t *testing.T) {
	c := Config{SecretKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PublicURL: "https://plex.example.com", IngressMode: "direct"}
	if err := c.Validate(); err == nil {
		t.Fatal("32 identical bytes must not pass entropy validation")
	}
}

func TestValidateAcceptsRandomHexSecret(t *testing.T) {
	c := Config{SecretKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PublicURL: "https://plex.example.com", IngressMode: "direct"}
	if err := c.Validate(); err != nil {
		t.Fatalf("random hex must pass: %v", err)
	}
}

func TestValidateTunnelNeedsNoToken(t *testing.T) {
	// The Tunnel token belongs to cloudflared only; the app must not
	// require or read it.
	c := Config{SecretKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PublicURL: "https://plex.example.com", IngressMode: "cloudflare_tunnel"}
	if err := c.Validate(); err != nil {
		t.Fatalf("tunnel mode must validate without token: %v", err)
	}
}

func TestLoadIgnoresTunnelToken(t *testing.T) {
	t.Setenv("REPLX_EDGE_SECRET_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("REPLX_EDGE_PUBLIC_URL", "https://plex.example.com")
	t.Setenv("REPLX_EDGE_INGRESS_MODE", "cloudflare_tunnel")
	t.Setenv("REPLX_EDGE_ADMIN_PORT", "8080")
	t.Setenv("REPLX_EDGE_MEDIA_PORT", "32402")
	t.Setenv("TUNNEL_TOKEN", "should-be-ignored")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IngressMode != "cloudflare_tunnel" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadReadsEnv(t *testing.T) {
	t.Setenv("REPLX_EDGE_SECRET_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("REPLX_EDGE_PUBLIC_URL", "https://plex.example.com")
	t.Setenv("REPLX_EDGE_INGRESS_MODE", "direct")
	t.Setenv("REPLX_EDGE_ADMIN_PORT", "8080")
	t.Setenv("REPLX_EDGE_MEDIA_PORT", "32402")
	os.Unsetenv("TUNNEL_TOKEN")
	os.Unsetenv("TUNNEL_TOKEN_FILE")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminPort != 8080 || cfg.IngressMode != "direct" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}
