// Package config loads Replx Edge runtime configuration.
//
// Canonical env prefix is REPLX_EDGE_. The legacy REPLEX_ prefix is not
// accepted: it conflated product naming and hid misconfiguration.
package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the minimal Alpha runtime configuration. Secrets, listener
// bindings, ingress mode and database settings require environment change
// and restart in Production 1.0; safe tunables are editable via admin API.
//
// The Cloudflare Tunnel token is deliberately absent: only the cloudflared
// sidecar needs it. Replx Edge never receives nor requires it.
type Config struct {
	ImageOwner           string
	Version              string
	PublicURL            string
	OriginInternalURL    string
	IngressMode          string
	AdminPort            int
	AdminBind            string
	LogLevel             string
	SecretKey            string
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
	// PlexTVBase is the plex.tv API root (override for tests only).
	PlexTVBase string
	// SpikeRouting enables the P0 307 media spike. Default off: media
	// fails closed in tunnel mode until the matrix validates a client.
	SpikeRouting bool
	// ArtworkDir is the filesystem artwork cache. ArtworkMaxGB bounds it;
	// the janitor deletes oldest files first.
	ArtworkDir   string
	ArtworkMaxGB int
	// CacheMaxGB and DiagnosticsMaxGB bound filesystem/valkey budgets.
	// Enforced by janitors; zero disables enforcement for that class.
	CacheMaxGB       int
	DiagnosticsMaxGB int
	// Retention bounds for high-volume records (days). The janitor
	// enforces them daily; zero disables a class.
	TraceRetentionDays    int
	PlaybackRetentionDays int
	AuditRetentionDays    int
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// intEnv reads an integer env with a fallback default. Unparseable values
// fall back silently: retention bounds are safe tunables, not fail-fast
// startup checks.
func intEnv(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
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
		AdminBind:         getenv("REPLX_EDGE_ADMIN_BIND", "127.0.0.1"),
		LogLevel:          getenv("REPLX_EDGE_LOG_LEVEL", "info"),
		SecretKey:         getenv("REPLX_EDGE_SECRET_KEY", ""),
		MediaPublicURL:    getenv("REPLX_EDGE_MEDIA_PUBLIC_URL", ""),
		PostgresHost:      getenv("POSTGRES_HOST", "postgres"),
		PostgresDB:        getenv("POSTGRES_DB", "replx_edge"),
		PostgresUser:      getenv("POSTGRES_USER", "replx_edge"),
		PostgresPass:      getenv("POSTGRES_PASSWORD", ""),
		PostgresURL:       getenv("REPLX_EDGE_POSTGRES_URL", ""),
		ValkeyAddr:        getenv("REPLX_EDGE_VALKEY_ADDR", getenv("VALKEY_ADDR", "valkey:6379")),
		PlexTVBase:        getenv("REPLX_EDGE_PLEXTV_URL", "https://plex.tv"),
		SpikeRouting:      strings.EqualFold(getenv("REPLX_EDGE_SPIKE_ROUTING", "false"), "true"),
		ArtworkDir:        getenv("REPLX_EDGE_ARTWORK_DIR", "/data/artwork"),
	}
	artGB, err := strconv.Atoi(getenv("REPLX_EDGE_ARTWORK_MAX_GB", "50"))
	if err != nil || artGB <= 0 || artGB > 10000 {
		return Config{}, fmt.Errorf("invalid REPLX_EDGE_ARTWORK_MAX_GB")
	}
	cfg.ArtworkMaxGB = artGB
	cfg.CacheMaxGB = intEnv("REPLX_EDGE_CACHE_MAX_GB", 20)
	cfg.DiagnosticsMaxGB = intEnv("REPLX_EDGE_DIAGNOSTICS_MAX_GB", 10)
	if cfg.CacheMaxGB < 0 || cfg.DiagnosticsMaxGB < 0 {
		return Config{}, fmt.Errorf("cache/diagnostics budgets must not be negative")
	}
	cfg.TraceRetentionDays = intEnv("REPLX_EDGE_TRACE_RETENTION_DAYS", 7)
	cfg.PlaybackRetentionDays = intEnv("REPLX_EDGE_PLAYBACK_RETENTION_DAYS", 30)
	cfg.AuditRetentionDays = intEnv("REPLX_EDGE_AUDIT_RETENTION_DAYS", 180)
	if cfg.TraceRetentionDays < 0 || cfg.PlaybackRetentionDays < 0 || cfg.AuditRetentionDays < 0 {
		return Config{}, fmt.Errorf("retention days must not be negative")
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

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces fail-fast startup checks. PMS reachability and owner
// auth are validated asynchronously after admin readiness (see runbook).
func (c Config) Validate() error {
	if err := checkSecretEntropy(c.SecretKey); err != nil {
		return err
	}
	if c.IngressMode != "cloudflare_tunnel" && c.IngressMode != "direct" {
		return fmt.Errorf("REPLX_EDGE_INGRESS_MODE must be cloudflare_tunnel or direct")
	}
	if c.PublicURL == "" {
		return fmt.Errorf("REPLX_EDGE_PUBLIC_URL is required")
	}
	if c.AdminBind == "0.0.0.0" {
		return fmt.Errorf("REPLX_EDGE_ADMIN_BIND must never be 0.0.0.0: bind loopback or a Tailscale IP")
	}
	if c.MediaFallbackEnabled && c.MediaPublicURL == "" {
		return fmt.Errorf("REPLX_EDGE_MEDIA_PUBLIC_URL is required when media fallback is enabled")
	}
	return nil
}

// checkSecretEntropy requires ≥32 characters holding ≥128 Shannon bits.
// Length alone proves nothing: 32 copies of one byte must not pass.
func checkSecretEntropy(secret string) error {
	if len(secret) < 32 {
		return fmt.Errorf("REPLX_EDGE_SECRET_KEY must be at least 32 characters")
	}
	freq := map[byte]int{}
	for i := 0; i < len(secret); i++ {
		freq[secret[i]]++
	}
	var bits float64
	n := float64(len(secret))
	for _, c := range freq {
		p := float64(c) / n
		bits += -p * math.Log2(p) * n
	}
	if bits < 128 {
		return fmt.Errorf("REPLX_EDGE_SECRET_KEY must hold at least 128 bits of entropy (got %.0f): use random bytes, not repetition", bits)
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
