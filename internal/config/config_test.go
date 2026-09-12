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

func TestValidateTunnelRequiresToken(t *testing.T) {
	c := Config{SecretKey: "0123456789abcdef0123456789abcdef", PublicURL: "https://plex.example.com", IngressMode: "cloudflare_tunnel"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected tunnel token error")
	}
}

func TestLoadReadsEnv(t *testing.T) {
	t.Setenv("REPLX_EDGE_SECRET_KEY", "0123456789abcdef0123456789abcdef")
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
