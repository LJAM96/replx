// Command replx-edge is the Replx Edge service entrypoint.
//
// Subcommands (CLI contract referenced by deploy/compose.yml):
//
//	serve (default)              run control (32400) + admin (8080) listeners
//	media-gateway                run media fallback listener (32402) only
//	healthcheck                  probe admin /health/live (liveness)
//	readycheck                   probe admin /health/ready (readiness; gates cloudflared)
//	media-gateway-healthcheck    probe media /health/live from inside the container
//	version                      print build version
//
// No subcommand may stream bulk media through the Cloudflare control
// hostname when REPLX_EDGE_INGRESS_MODE=cloudflare_tunnel. See ADR 001.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LJAM96/replx/internal/admin"
	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/config"
	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/playback"
	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/pms"
	"github.com/LJAM96/replx/internal/proxy"
	"github.com/LJAM96/replx/internal/retention"
	"github.com/LJAM96/replx/internal/spike"
	syncpkg "github.com/LJAM96/replx/internal/sync"
	"github.com/LJAM96/replx/internal/valkey"
	"github.com/LJAM96/replx/internal/warmer"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := runServe(); err != nil {
			fmt.Fprintln(os.Stderr, "replx-edge serve:", err)
			os.Exit(1)
		}
	case "media-gateway":
		if err := runMediaGateway(); err != nil {
			fmt.Fprintln(os.Stderr, "replx-edge media-gateway:", err)
			os.Exit(1)
		}
	case "healthcheck":
		os.Exit(runProbe(adminHealthURL("/health/live"), 2*time.Second))
	case "readycheck":
		os.Exit(runProbe(adminHealthURL("/health/ready"), 5*time.Second))
	case "media-gateway-healthcheck":
		os.Exit(runProbe(mediaHealthURL(), 2*time.Second))
	case "version", "--version", "-v":
		fmt.Println("replx-edge", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve|media-gateway|healthcheck|readycheck|media-gateway-healthcheck|version)\n", cmd)
		os.Exit(2)
	}
}

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stderr)

	// The media gateway profile is a documented placeholder in this
	// build (health endpoint only, no session/policy/streaming): enabling
	// it must be a deliberate, loudly logged choice, never mistaken for
	// a functional media plane. Policy routingMode values that would
	// select it are rejected at the admin API for the same reason.
	if cfg.MediaFallbackEnabled {
		logger.Log(logging.Entry{Level: "warn", Component: "gateway",
			Fields: map[string]any{"event": "media_fallback_placeholder",
				"msg": "REPLX_EDGE_MEDIA_FALLBACK_ENABLED=true but the media gateway serves health checks only; direct-origin routing remains the media plane"}})
	}

	if cfg.OriginInternalURL == "" {
		return fmt.Errorf("REPLX_EDGE_ORIGIN_INTERNAL_URL is required for serve")
	}

	// Postgres pool + advisory-locked migrations. Failure degrades
	// readiness but never takes down the admin plane: operators need
	// the admin UI to inspect and recover.
	ctx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	db, err := database.Open(ctx, cfg.DatabaseURL())
	if err != nil {
		return err
	}
	defer db.Close()
	migrate := func() {
		mctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := db.Migrate(mctx); err != nil {
			logger.Log(logging.Entry{Level: "error", Component: "database",
				Fields: map[string]any{"event": "migrate_failed", "error": err.Error()}})
			return
		}
		logger.Log(logging.Entry{Level: "info", Component: "database",
			Fields: map[string]any{"event": "migrate_complete"}})
	}
	go func() {
		migrate()
		if db.MigrationsComplete() {
			return
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			migrate()
			if db.MigrationsComplete() {
				return
			}
		}
	}()

	// PMS status refreshes in the background so readiness probes stay fast.
	var pmsStatus atomic.Value
	pmsStatus.Store(pms.StatusUnknown)
	go func() {
		refresh := func() { pmsStatus.Store(pms.Check(cfg.OriginInternalURL)) }
		refresh()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			refresh()
		}
	}()
	valkeyOK := func() bool { return valkey.Ping(cfg.ValkeyAddr, 2*time.Second) }

	// P0 spike: 307 media redirects with trace capture. Off by default;
	// media fails closed until the matrix validates a client.
	var spikeStore *spike.Store
	var spikeOpt proxy.SpikeResolver
	if cfg.SpikeRouting {
		spikeStore = spike.NewPostgresStore(db.Raw(), publicHostOf(cfg.PublicURL), logger)
		spikeOpt = spikeStore
		logger.Log(logging.Entry{Level: "warn", Component: "routing.spike",
			Fields: map[string]any{"event": "spike_enabled"}})
	}
	// Beta observability: live Prometheus counters plus targeted,
	// time-limited protocol capture. Both are process-local and reset on
	// restart; the secret keys fingerprinting and trace IDs.
	registry := &metrics.Registry{}
	captureStore := capture.New()
	// Zeta browse cache: best-effort Valkey store with a short op timeout
	// so a down Valkey degrades to origin fall-through instead of stalling
	// requests. Store errors read as misses; see internal/cache.
	cacheClient := valkey.NewClient(cfg.ValkeyAddr, 300*time.Millisecond)
	if valkey.Ping(cfg.ValkeyAddr, 2*time.Second) {
		logger.Log(logging.Entry{Level: "info", Component: "cache",
			Fields: map[string]any{"event": "cache_enabled"}})
	} else {
		logger.Log(logging.Entry{Level: "warn", Component: "cache",
			Fields: map[string]any{"event": "cache_degraded", "reason": "valkey unreachable; browse falls through to origin"}})
	}
	cacheStore := cache.NewValkeyStore(cacheClient)
	// Identity pipeline: fingerprint to Plex account resolution with
	// account-scoped cache sharing across a user's devices. plex.tv is
	// consulted on cold fingerprints only; validated user tokens are
	// encrypted for per-user cache refreshes.
	idResolver := identity.New(db.Raw(), tvAccount{client: &plextv.Client{
		BaseURL: cfg.PlexTVBase, ClientIdentifier: "replx-edge-identity",
		Product: "Replx Edge", Version: version, Platform: "Linux",
	}})
	pmsValidator, err := identity.NewPMSValidator(cfg.OriginInternalURL)
	if err != nil {
		return err
	}
	idResolver.PMS = pmsValidator
	idResolver.Secret = cfg.SecretKey
	ownerAccount := func(ctx context.Context) (int64, bool) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var id *int64
		if err := db.Raw().QueryRow(cctx, `SELECT c.plex_account_id FROM plex_owner_credentials c
			JOIN plex_servers s ON s.id = c.server_id
			WHERE s.enabled ORDER BY s.created_at DESC LIMIT 1`).Scan(&id); err != nil || id == nil {
			return 0, false
		}
		return *id, true
	}
	ownerPMSToken := func(ctx context.Context) (string, bool) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var serverID string
		if err := db.Raw().QueryRow(cctx, `SELECT id FROM plex_servers
			WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID); err != nil {
			return "", false
		}
		var ct []byte
		if err := db.Raw().QueryRow(cctx, `SELECT pms_access_token_ciphertext
			FROM plex_owner_credentials WHERE server_id=$1`, serverID).Scan(&ct); err != nil {
			return "", false
		}
		pt, err := crypto.Decrypt(cfg.SecretKey, onboarding.PurposePMSToken, ct)
		if err != nil {
			return "", false
		}
		return string(pt), true
	}
	warm := warmer.New(cacheStore, cfg.OriginInternalURL, cfg.SecretKey, ownerPMSToken, logger, registry)
	warm.OwnerAccount = ownerAccount
	go warm.Run(ctx, 2*time.Second)
	// Eta artwork: shared filesystem transcode cache with oldest-first
	// janitor. Directory failure degrades to uncached artwork, never to
	// failed startup.
	var artworkStore *artwork.Store
	if art, err := artwork.New(cfg.ArtworkDir, cfg.ArtworkMaxGB); err != nil {
		logger.Log(logging.Entry{Level: "warn", Component: "cache",
			Fields: map[string]any{"event": "artwork_degraded", "error": err.Error()}})
	} else {
		artworkStore = art
		go art.Run(ctx, 30*time.Minute)
	}
	// Gamma index: owner library sync plus the PMS event consumer. Both
	// run behind the single-replica worker loops; absence of credentials
	// (pre-onboarding) idles them without failing startup.
	syncWorker := syncpkg.New(db.Raw(), cfg.OriginInternalURL, ownerPMSToken, logger, registry)
	go syncWorker.Run(ctx)
	go syncWorker.Subscribe(ctx)
	// Epsilon enforcement: negotiation interception plus the raw-part
	// boundary behind the spike redirector. Sessions persist in Postgres;
	// absent sessions preserve Alpha allow-through exactly.
	playbackEngine := &playback.Engine{
		DB: db.Raw(), Origin: cfg.OriginInternalURL, Secret: cfg.SecretKey, Logger: logger,
		Metrics: registry, Store: &playback.PGStore{DB: db.Raw()}, Identity: idResolver,
		LoadPolicy: playback.DefaultPolicyLoader(db.Raw()),
	}
	if spikeStore != nil {
		spikeStore.PartPolicy = playbackEngine.EnforcePart
	}
	proxyHandler, err := proxy.New(proxy.Options{
		OriginBase:  cfg.OriginInternalURL,
		IngressMode: cfg.IngressMode,
		Logger:      logger,
		Secret:      cfg.SecretKey,
		Metrics:     registry,
		Capture:     captureStore,
		Cache:       cacheStore,
		Warmer:      warm,
		Playback:    playbackEngine,
		Artwork:     artworkStore,
		Identity:    idResolver,
		Spike:       spikeOpt,
		// Media authorization precedes transport selection in every
		// ingress mode; see Options.PartPolicy.
		PartPolicy: playbackEngine.EnforcePart,
		MediaFallbackURL: func() string {
			if cfg.MediaFallbackEnabled {
				return cfg.MediaPublicURL
			}
			return ""
		}(),
		SearchDB: db.Raw(),
	})
	if err != nil {
		return err
	}
	// Warmer uses the canonical owner scope (identity UUID) and
	// generation-aware keys so refreshes land in the live namespace.
	warm.DB = db.Raw()
	warm.KeyFunc = func(s warmer.Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		class := s.Class
		if class == "" {
			class = cache.ClassOf(s.Path)
		}
		sg, gg := proxyHandler.Generations().Get(s.Scope, class)
		return cache.ResponseKeyGen(s.Scope, class, s.Method, s.Path, q, s.Accept, sg, gg)
	}
	warm.Artwork = artworkStore
	warm.PreloadHubQuery = os.Getenv("REPLX_EDGE_PRELOAD_HUB_QUERY")
	warm.PreloadCollectionQuery = os.Getenv("REPLX_EDGE_PRELOAD_COLLECTION_QUERY")
	warm.PreloadSections = func(ctx context.Context) ([]string, error) {
		rows, err := db.Raw().Query(ctx, `SELECT plex_section_id FROM libraries
			WHERE server_id=(SELECT id FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1)
			ORDER BY plex_section_id LIMIT 8`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var sections []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			sections = append(sections, id)
		}
		return sections, rows.Err()
	}
	warm.PreloadArtworkPaths = func(ctx context.Context) ([]string, error) {
		rows, err := db.Raw().Query(ctx, `SELECT thumb FROM library_items
			WHERE server_id=(SELECT id FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1)
				AND thumb LIKE '/library/metadata/%'
			ORDER BY added_at DESC NULLS LAST LIMIT 96`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var thumbs []string
		for rows.Next() {
			var thumb string
			if err := rows.Scan(&thumb); err != nil {
				return nil, err
			}
			thumbs = append(thumbs, thumb)
		}
		return thumbs, rows.Err()
	}
	go warm.RunPreload(ctx, 5*time.Minute)

	onboard := &onboarding.Service{
		DB:          db.Raw(),
		NewTV:       func(clientID string) onboarding.TVClient { return tvClientFor(cfg, clientID) },
		ControlBase: "http://127.0.0.1:32400",
		Secret:      cfg.SecretKey,
		PublicURL:   cfg.PublicURL,
		InternalURL: cfg.OriginInternalURL,
		Log: func(level, component, msg string, fields map[string]any) {
			logger.Log(logging.Entry{Level: level, Component: component, Fields: appendField(fields, "msg", msg)})
		},
	}
	setupToken, err := admin.NewSetupToken()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "onboarding setup token (valid 15m, single-use setup via POST /api/v1/setup): %s\n", setupToken)

	// Owner JWT refresh (JWT mode only): hourly check, refresh within 24h
	// of expiry. Failures degrade credentials without destroying them.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if _, err := onboard.RefreshOwnerJWT(ctx); err != nil {
				logger.Log(logging.Entry{Level: "warn", Component: "onboarding",
					Fields: map[string]any{"event": "owner_refresh", "error": err.Error()}})
			}
		}
	}()

	spikeObs := spike.Observations{DB: db.Raw()}
	adminMux := admin.NewMux(health.Checks{
		MigrationsComplete: db.MigrationsComplete,
		PostgresOK:         func() bool { return db.Ping(ctx) },
		ValkeyOK:           valkeyOK,
		PMSStatus:          func() string { return pmsStatus.Load().(string) },
	}, onboard, setupToken, true, spikeStore, &spikeObs)
	adminMux.SetMetrics(registry)
	adminMux.SetCapture(captureStore)
	adminMux.SetWarmer(warm.Stats)
	adminMux.SetSync(syncWorker)
	adminMux.SetCacheInvalidator(proxyHandler.InvalidateCache)
	// Diagnostics gauge: active targeted captures.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				registry.SetDiagnosticsActive(int64(len(captureStore.Targets())))
			}
		}
	}()
	// Retention janitor: daily purge of playback, trace and audit history.
	// Bounded batches avoid long locks. First pass runs minutes after
	// startup to reclaim long-down backlog.
	go retention.Run(ctx, db.Raw(), retention.Policy{
		PlaybackDays: cfg.PlaybackRetentionDays, AuditDays: cfg.AuditRetentionDays,
		BatchSize: 1000,
	}, logger, 24*time.Hour)
	fmt.Fprintf(os.Stdout, "replx-edge onboarding panel: http://127.0.0.1:%d/admin/onboarding | spike matrix: http://127.0.0.1:%d/admin/spike\n",
		cfg.AdminPort, cfg.AdminPort)
	adminSrv := &http.Server{
		Addr:              net.JoinHostPort(cfg.EffectiveAdminListen(), strconv.Itoa(cfg.AdminPort)),
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	controlSrv := &http.Server{
		Addr:              ":32400",
		Handler:           proxyHandler,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	fmt.Fprintf(os.Stdout, "replx-edge %s starting (ingress=%s public=%s origin=%s)\n",
		version, cfg.IngressMode, cfg.PublicURL, logging.RedactURLString(cfg.OriginInternalURL))

	// Graceful shutdown: SIGINT/SIGTERM stops listeners and background
	// loops instead of dying at Docker's kill timeout.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 2)
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin listener: %w", err)
		}
	}()
	go func() {
		if err := controlSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("control listener: %w", err)
		}
	}()
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		fmt.Fprintf(os.Stdout, "replx-edge received %s, shutting down\n", sig)
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = adminSrv.Shutdown(shutdown)
		_ = controlSrv.Shutdown(shutdown)
		stopWorkers()
		return nil
	}
}

func runMediaGateway() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.MediaFallbackEnabled {
		return fmt.Errorf("media gateway disabled (REPLX_EDGE_MEDIA_FALLBACK_ENABLED=false)")
	}
	// Alpha placeholder: health endpoint only. Capability validation,
	// session lookup, policy enforcement and media streaming land with
	// the media gateway phase; enabling the profile today only opens
	// the listener, it does not serve media.
	fmt.Fprintf(os.Stdout, "replx-edge %s media-gateway starting (PLACEHOLDER: health only, no media yet)\n", version)
	mux := health.MediaMux()
	srv := &http.Server{
		Addr:              ":32402",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

// publicHostOf returns the lowercase hostname of the public URL.
func publicHostOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func appendField(fields map[string]any, k string, v any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields[k] = v
	return fields
}

// tvClientFor binds a plex.tv client to the installation client ID.
func tvClientFor(cfg config.Config, clientID string) *plextv.Client {
	return &plextv.Client{
		BaseURL:          cfg.PlexTVBase,
		ClientIdentifier: clientID,
		Product:          "Replx Edge",
		Version:          version,
		Platform:         "Linux",
	}
}

// tvAccount adapts plextv.Client to the identity resolver's account
// surface (ID plus username only, never tokens).
type tvAccount struct {
	client *plextv.Client
}

func (a tvAccount) GetUser(ctx context.Context, token string) (int64, string, error) {
	u, err := a.client.GetUser(ctx, token)
	if err != nil {
		return 0, "", err
	}
	return u.ID, u.Username, nil
}

func adminHealthURL(path string) string {
	port := os.Getenv("REPLX_EDGE_ADMIN_PORT")
	if port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port + path
}

func mediaHealthURL() string {
	return "http://127.0.0.1:32402/health/live"
}

func runProbe(url string, timeout time.Duration) int {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url) //nolint:noctx,gosec // localhost probe inside container
	if err != nil {
		fmt.Fprintln(os.Stderr, "health probe failed:", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health probe status:", resp.Status)
		return 1
	}
	return 0
}
