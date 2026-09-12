// Package config loads Replx Edge runtime configuration.
//
// Canonical env prefix is REPLX_EDGE_. The legacy REPLEX_ prefix is not
// accepted: it conflated product naming and hid misconfiguration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the minimal Alpha runtime configuration. Secrets, listener
// bindings, ingress mode and database settings require environment change
// and restart in Production 1.0; safe tunables are editable via admin API.
type Config struct {
	ImageOwner           string
	Version              string
	PublicURL            string
	OriginInternalURL    string
	IngressMode          string
	AdminPort            int
	LogLevel             string
	SecretKey            string
	TunnelToken          string
	MediaFallbackEnabled bool
	MediaPublicURL       string
	MediaPort            int
	// Postgres connection. POSTGRES_HOST defaults to the Compose service
	// name; REPLX_EDGE_POSTGRES_URL overrides all components when set
	// (used by tests and non-Compose deployments).
	PostgresHost string
	PostgresPort int
	PostgresDB   string
	PostgresUser string
	PostgresPass string
	PostgresURL  string
	// Valkey address as host:port. Cache is best-effort: Valkey down
	// degrades to PMS fall-through, never to failed readiness.
	ValkeyAddr string
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// Load reads configuration from the environment. TUNNEL_TOKEN may be
// supplied directly or via TUNNEL_TOKEN_FILE (Docker secret compatible).
func Load() (Config, error) {
	cfg := Config{
		ImageOwner:        getenv("REPLX_EDGE_IMAGE_OWNER", ""),
		Version:           getenv("REPLX_EDGE_VERSION", "dev"),
		PublicURL:         getenv("REPLX_EDGE_PUBLIC_URL", ""),
		OriginInternalURL: getenv("REPLX_EDGE_ORIGIN_INTERNAL_URL", ""),
		IngressMode:       getenv("REPLX_EDGE_INGRESS_MODE", "cloudflare_tunnel"),
		LogLevel:          getenv("REPLX_EDGE_LOG_LEVEL", "info"),
		SecretKey:         getenv("REPLX_EDGE_SECRET_KEY", ""),
		TunnelToken:       getenv("TUNNEL_TOKEN", ""),
		MediaPublicURL:    getenv("REPLX_EDGE_MEDIA_PUBLIC_URL", ""),
		PostgresHost:      getenv("POSTGRES_HOST", "postgres"),
		PostgresDB:        getenv("POSTGRES_DB", "replx_edge"),
		PostgresUser:      getenv("POSTGRES_USER", "replx_edge"),
		PostgresPass:      getenv("POSTGRES_PASSWORD", ""),
		PostgresURL:       getenv("REPLX_EDGE_POSTGRES_URL", ""),
		ValkeyAddr:        getenv("REPLX_EDGE_VALKEY_ADDR", getenv("VALKEY_ADDR", "valkey:6379")),
	}
	adminPort, err := strconv.Atoi(getenv("REPLX_EDGE_ADMIN_PORT", "8080"))
	if err != nil || adminPort <= 0 || adminPort > 65535 {
		return Config{}, fmt.Errorf("invalid REPLX_EDGE_ADMIN_PORT")
	}
	cfg.AdminPort = adminPort
	mediaPort, err := strconv.Atoi(getenv("REPLX_EDGE_MEDIA_PORT", "443"))
	if err != nil || mediaPort <= 0 || mediaPort > 65535 {
		return Config{}, fmt.Errorf("invalid REPLX_EDGE_MEDIA_PORT")
	}
	cfg.MediaPort = mediaPort
	pgPort, err := strconv.Atoi(getenv("POSTGRES_PORT", "5432"))
	if err != nil || pgPort <= 0 || pgPort > 65535 {
		return Config{}, fmt.Errorf("invalid POSTGRES_PORT")
	}
	cfg.PostgresPort = pgPort
	cfg.MediaFallbackEnabled = strings.EqualFold(getenv("REPLX_EDGE_MEDIA_FALLBACK_ENABLED", "false"), "true")

	if file := os.Getenv("TUNNEL_TOKEN_FILE"); file != "" && cfg.TunnelToken == "" {
		b, err := os.ReadFile(file) //nolint:gosec // operator-configured secret path
		if err != nil {
			return Config{}, fmt.Errorf("read TUNNEL_TOKEN_FILE: %w", err)
		}
		cfg.TunnelToken = strings.TrimSpace(string(b))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces fail-fast startup checks. PMS reachability and owner
// auth are validated asynchronously after admin readiness (see runbook).
func (c Config) Validate() error {
	if len(c.SecretKey) < 32 {
		return fmt.Errorf("REPLX_EDGE_SECRET_KEY must contain at least 32 characters of entropy")
	}
	if c.IngressMode != "cloudflare_tunnel" && c.IngressMode != "direct" {
		return fmt.Errorf("REPLX_EDGE_INGRESS_MODE must be cloudflare_tunnel or direct")
	}
	if c.PublicURL == "" {
		return fmt.Errorf("REPLX_EDGE_PUBLIC_URL is required")
	}
	if c.IngressMode == "cloudflare_tunnel" && c.TunnelToken == "" {
		return fmt.Errorf("TUNNEL_TOKEN (or TUNNEL_TOKEN_FILE) is required in cloudflare_tunnel mode")
	}
	if c.MediaFallbackEnabled && c.MediaPublicURL == "" {
		return fmt.Errorf("REPLX_EDGE_MEDIA_PUBLIC_URL is required when media fallback is enabled")
	}
	return nil
}

// DatabaseURL returns the Postgres connection URL, honouring an explicit
// REPLX_EDGE_POSTGRES_URL override. The password is URL-escaped.
func (c Config) DatabaseURL() string {
	if c.PostgresURL != "" {
		return c.PostgresURL
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.PostgresUser, c.PostgresPass),
		Host:   fmt.Sprintf("%s:%d", c.PostgresHost, c.PostgresPort),
		Path:   "/" + c.PostgresDB,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}
