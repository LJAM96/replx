package config

import (
	"os"
	"testing"
)

func TestValidateRejectsShortSecret(t *testing.T) {
	c := Config{SecretKey: "short", PublicURL: "https://plex.example.com", IngressMode: "direct", AdminBind: "127.0.0.1", AdminPublishBind: "127.0.0.1"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected secret key error")
	}
}

func TestValidateRejectsLowEntropySecret(t *testing.T) {
	c := Config{SecretKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PublicURL: "https://plex.example.com", IngressMode: "direct", AdminBind: "127.0.0.1", AdminPublishBind: "127.0.0.1"}
	if err := c.Validate(); err == nil {
		t.Fatal("32 identical bytes must not pass entropy validation")
	}
}

func TestValidateAcceptsRandomHexSecret(t *testing.T) {
	c := Config{SecretKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PublicURL: "https://plex.example.com", IngressMode: "direct", AdminBind: "127.0.0.1", AdminPublishBind: "127.0.0.1"}
	if err := c.Validate(); err != nil {
		t.Fatalf("random hex must pass: %v", err)
	}
}

func TestValidateTunnelNeedsNoToken(t *testing.T) {
	// The Tunnel token belongs to cloudflared only; the app must not
	// require or read it.
	c := Config{SecretKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PublicURL: "https://plex.example.com", IngressMode: "cloudflare_tunnel", AdminBind: "127.0.0.1", AdminPublishBind: "127.0.0.1"}
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

func TestAdminBindClasses(t *testing.T) {
	good := []Config{
		{AdminBind: "127.0.0.1", AdminPublishBind: "127.0.0.1"},
		{AdminBind: "100.116.199.128", AdminPublishBind: "100.116.199.128"}, // Tailscale
		{AdminBind: "192.168.1.10", AdminPublishBind: "192.168.1.10"},
		{AdminListen: "0.0.0.0", AdminPublishBind: "127.0.0.1", InDocker: true},
		{AdminListen: "::", AdminPublishBind: "127.0.0.1", InDocker: true},
	}
	for i, c := range good {
		c.SecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		c.PublicURL = "https://plex.example.com"
		c.IngressMode = "direct"
		if err := c.Validate(); err != nil {
			t.Errorf("case %d must validate: %v", i, err)
		}
	}
	bad := []Config{
		{AdminBind: "0.0.0.0", AdminPublishBind: "127.0.0.1"},          // wildcard outside Docker
		{AdminBind: "::", AdminPublishBind: "127.0.0.1"},               // v6 wildcard outside Docker
		{AdminBind: "", AdminPublishBind: "127.0.0.1"},                 // empty is wildcard
		{AdminBind: "203.0.113.10", AdminPublishBind: "127.0.0.1"},     // public listen
		{AdminBind: "plex.example.com", AdminPublishBind: "127.0.0.1"}, // hostname, not literal
		{AdminBind: "127.0.0.1", AdminPublishBind: "0.0.0.0"},          // wildcard publish
		{AdminBind: "127.0.0.1", AdminPublishBind: ""},                 // empty publish
		{AdminBind: "127.0.0.1", AdminPublishBind: "203.0.113.10"},     // public publish
		{AdminBind: "127.0.0.1", AdminPublishBind: "::"},               // v6 wildcard publish
		{AdminListen: "0.0.0.0", AdminPublishBind: "127.0.0.1"},        // wildcard without marker
	}
	for i, c := range bad {
		c.SecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		c.PublicURL = "https://plex.example.com"
		c.IngressMode = "direct"
		if err := c.Validate(); err == nil {
			t.Errorf("case %d must be rejected: %+v", i, c)
		}
	}
}

func TestLoadReadsBudgets(t *testing.T) {
	t.Setenv("REPLX_EDGE_SECRET_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("REPLX_EDGE_PUBLIC_URL", "https://plex.example.com")
	t.Setenv("REPLX_EDGE_INGRESS_MODE", "direct")
	t.Setenv("REPLX_EDGE_CACHE_MAX_GB", "20")
	t.Setenv("REPLX_EDGE_DIAGNOSTICS_MAX_GB", "10")
	t.Setenv("REPLX_EDGE_ADMIN_BIND", "127.0.0.1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CacheMaxGB != 20 || cfg.DiagnosticsMaxGB != 10 || cfg.AdminBind != "127.0.0.1" {
		t.Fatalf("budgets/bind: %+v", cfg)
	}
}

func TestSecretRequiresGeneratedMaterial(t *testing.T) {
	hexSecret := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := checkSecretEntropy(hexSecret); err != nil {
		t.Fatalf("64 hex chars must pass: %v", err)
	}
	if err := checkSecretEntropy("MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI="); err != nil {
		t.Fatalf("base64 32 bytes must pass: %v", err)
	}
	for _, bad := range []string{
		"",
		"short",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 16 bytes: too short
		"correct horse battery staple, quite long indeed",
		"replace-with-at-least-32-random-bytes",
	} {
		if err := checkSecretEntropy(bad); err == nil {
			t.Errorf("passphrase %q must be rejected", bad)
		}
	}
}
