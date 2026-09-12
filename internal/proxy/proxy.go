// Package proxy is the Alpha transparent Plex reverse proxy.
//
// Non-media routes stream through untouched. Bulk media routes fail closed
// with MEDIA_ROUTE_UNAVAILABLE in cloudflare_tunnel ingress mode unless a
// SpikeResolver is configured (P0 spike): resolution upgrades the response
// to an ADR 001 307 redirect, resolution failure still fails closed. Replx
// Edge never silently streams video through the Cloudflare control hostname.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/requestid"
	"github.com/LJAM96/replx/internal/routing"
)

// RequestIDHeader is returned on every control response. Plex clients
// ignore it; operators use it to join logs and traces.
const RequestIDHeader = "X-Replx-Edge-Request-ID"

// MediaRouteUnavailable is the stable diagnostic reason for fail-closed media.
const MediaRouteUnavailable = "MEDIA_ROUTE_UNAVAILABLE"

// hopByHop headers are stripped in both directions per the security model.
// WebSocket Upgrade is denied in Alpha: no route intentionally supports it.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
}

// SpikeResolver maps a media request to an ADR 001 redirect target.
// Nil disables spike routing and keeps fail-closed behaviour.
type SpikeResolver interface {
	Resolve(r *http.Request, requestID string) (location string, ok bool)
}

// Options configures the proxy handler.
type Options struct {
	// OriginBase is the server-to-server PMS URL, e.g. https://origin:32400.
	OriginBase string
	// IngressMode is cloudflare_tunnel or direct.
	IngressMode string
	Logger      *logging.Logger
	// Client overrides the origin HTTP client (tests). Nil uses a default
	// client with a 30s response-header timeout.
	Client *http.Client
	// Spike, when non-nil, upgrades fail-closed media to 307 redirects
	// where it resolves. Resolution failures still fail closed.
	Spike SpikeResolver
}

// Handler proxies Plex requests to the origin PMS.
type Handler struct {
	origin *url.URL
	mode   string
	spike  SpikeResolver
	log    *logging.Logger
	client *http.Client
}

// New validates options and returns a Handler.
func New(opts Options) (*Handler, error) {
	if opts.OriginBase == "" {
		return nil, fmt.Errorf("proxy: origin base is required")
	}
	base, err := url.Parse(opts.OriginBase)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("proxy: invalid origin base")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("proxy: origin base must be http or https")
	}
	if opts.IngressMode != "cloudflare_tunnel" && opts.IngressMode != "direct" {
		return nil, fmt.Errorf("proxy: ingress mode must be cloudflare_tunnel or direct")
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Handler{origin: base, mode: opts.IngressMode, log: opts.Logger, client: client, spike: opts.Spike}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := requestid.New()
	w.Header().Set(RequestIDHeader, id)

	routeClass := "control"
	if gateway.IsBulkMediaRoute(r.URL.Path) {
		routeClass = "media"
	}

	if routeClass == "media" && h.mode == "cloudflare_tunnel" {
		if h.spike != nil {
			if loc, ok := h.spike.Resolve(r, id); ok {
				h.writeMediaRedirect(w, r, id, start, loc)
				return
			}
		}
		h.writeMediaUnavailable(w, r, id, start)
		return
	}
	if h.serveStreaming(w, r, id, start) {
		return
	}
	h.proxy(w, r, id, routeClass, start)
}

func (h *Handler) writeMediaRedirect(w http.ResponseWriter, r *http.Request, id string, start time.Time, location string) {
	routing.WriteMediaRedirect(w, location)
	h.emit(r, id, "media", http.StatusTemporaryRedirect, start, map[string]any{
		"decision": "redirected",
		"location": routing.RedactedLocation(location),
		"range":    r.Header.Get("Range") != "",
	})
}

func (h *Handler) writeMediaUnavailable(w http.ResponseWriter, r *http.Request, id string, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      MediaRouteUnavailable,
			"message":   "bulk media must not traverse the Cloudflare control hostname; use direct origin routing or the media gateway",
			"requestId": id,
		},
	})
	h.emit(r, id, "media", http.StatusForbidden, start, map[string]any{"decision": "unavailable"})
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, id, routeClass string, start time.Time) {
	target := *h.origin
	target.Path = singleJoin(h.origin.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	// ForceRequestURI etc. must not leak; ForceQuery is dropped by design.

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		h.writeBadGateway(w, r, id, routeClass, start)
		return
	}
	copyHeaders(out.Header, r.Header)
	out.Header.Set(RequestIDHeader, id)
	// The origin needs the real client IP for its own logs; X-Forwarded-For
	// is informational only and never trusted for auth.
	if host := clientIP(r); host != "" {
		prior := out.Header.Get("X-Forwarded-For")
		if prior != "" {
			host = prior + ", " + host
		}
		out.Header.Set("X-Forwarded-For", host)
	}
	out.Host = h.origin.Host

	resp, err := h.client.Do(out) //nolint:gosec // target is admin-configured origin only
	if err != nil {
		h.writeBadGateway(w, r, id, routeClass, start)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set(RequestIDHeader, id)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	h.emit(r, id, routeClass, resp.StatusCode, start, nil)
}

func (h *Handler) writeBadGateway(w http.ResponseWriter, r *http.Request, id, routeClass string, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      "ORIGIN_UNAVAILABLE",
			"message":   "PMS origin unreachable; retry or inspect origin health",
			"requestId": id,
		},
	})
	h.emit(r, id, routeClass, http.StatusBadGateway, start, nil)
}

func (h *Handler) emit(r *http.Request, id, routeClass string, status int, start time.Time, fields map[string]any) {
	if h.log == nil {
		return
	}
	h.log.Log(logging.Entry{
		Level:     "info",
		Component: "gateway",
		RequestID: id,
		Method:    r.Method,
		Path:      logging.RedactURLString(r.URL.RequestURI()),
		Status:    status,
		Duration:  time.Since(start).Milliseconds(),
		Route:     routeClass,
		Cache:     "bypass",
		Fields:    fields,
	})
}

// copyHeaders copies src to dst minus hop-by-hop headers and any header
// named in the Connection token list.
func copyHeaders(dst, src http.Header) {
	connected := map[string]bool{}
	for _, token := range strings.Split(src.Get("Connection"), ",") {
		if t := strings.TrimSpace(token); t != "" {
			connected[strings.ToLower(t)] = true
		}
	}
	for k, vv := range src {
		if hopByHop[textprotoCanonical(k)] {
			continue
		}
		if connected[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func textprotoCanonical(k string) string {
	for hop := range hopByHop {
		if strings.EqualFold(k, hop) {
			return hop
		}
	}
	return k
}

func singleJoin(a, b string) string {
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

func clientIP(r *http.Request) string {
	if r.RemoteAddr == "" {
		return ""
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
