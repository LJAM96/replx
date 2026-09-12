// Command replx-edge is the Replx Edge service entrypoint.
//
// Subcommands (CLI contract referenced by deploy/compose.yml):
//
//	serve (default)              run control (32400) + admin (8080) listeners
//	media-gateway                run media fallback listener (32402) only
//	healthcheck                  probe admin /health/live from inside the container
//	media-gateway-healthcheck    probe media /health/live from inside the container
//	version                      print build version
//
// No subcommand may stream bulk media through the Cloudflare control
// hostname when REPLX_EDGE_INGRESS_MODE=cloudflare_tunnel. See ADR 001.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/LJAM96/replx-edge/internal/config"
	"github.com/LJAM96/replx-edge/internal/health"
	"github.com/LJAM96/replx-edge/internal/logging"
	"github.com/LJAM96/replx-edge/internal/proxy"
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
		os.Exit(runProbe(adminHealthURL(), 2*time.Second))
	case "media-gateway-healthcheck":
		os.Exit(runProbe(mediaHealthURL(), 2*time.Second))
	case "version", "--version", "-v":
		fmt.Println("replx-edge", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve|media-gateway|healthcheck|media-gateway-healthcheck|version)\n", cmd)
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
	proxyHandler, err := proxy.New(proxy.Options{
		OriginBase:  cfg.OriginInternalURL,
		IngressMode: cfg.IngressMode,
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	adminMux := health.AdminMux(health.Checks{})
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
	return <-errCh
}

func runMediaGateway() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.MediaFallbackEnabled {
		return fmt.Errorf("media gateway disabled (REPLX_EDGE_MEDIA_FALLBACK_ENABLED=false)")
	}
	mux := health.MediaMux()
	srv := &http.Server{
		Addr:              ":32402",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stdout, "replx-edge %s media-gateway starting\n", version)
	return srv.ListenAndServe()
}

func adminHealthURL() string {
	port := os.Getenv("REPLX_EDGE_ADMIN_PORT")
	if port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port + "/health/live"
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
