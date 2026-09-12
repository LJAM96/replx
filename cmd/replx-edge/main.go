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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LJAM96/replx/internal/admin"
	"github.com/LJAM96/replx/internal/config"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/pms"
	"github.com/LJAM96/replx/internal/proxy"
	"github.com/LJAM96/replx/internal/spike"
	"github.com/LJAM96/replx/internal/valkey"
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
	proxyHandler, err := proxy.New(proxy.Options{
		OriginBase:  cfg.OriginInternalURL,
		IngressMode: cfg.IngressMode,
		Logger:      logger,
		Spike:       spikeOpt,
	})
	if err != nil {
		return err
	}

	onboard := &onboarding.Service{
		DB:          db.Raw(),
		NewTV:       func(clientID string) onboarding.TVClient { return tvClientFor(cfg, clientID) },
		ControlBase: "http://127.0.0.1:32400",
		Secret:      cfg.SecretKey,
		PublicURL:   cfg.PublicURL,
		InternalURL: cfg.OriginInternalURL,
	}
	setupToken, err := admin.NewSetupToken()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "onboarding setup token: %s\n", setupToken)

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
	fmt.Fprintf(os.Stdout, "replx-edge onboarding panel: http://127.0.0.1:%d/admin/onboarding | spike matrix: http://127.0.0.1:%d/admin/spike\n",
		cfg.AdminPort, cfg.AdminPort)
	adminSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.AdminPort),
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
