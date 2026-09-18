// Package proxy is the Alpha transparent Plex reverse proxy.
//
// Non-media routes stream through untouched. Bulk media routes fail closed
// with MEDIA_ROUTE_UNAVAILABLE in cloudflare_tunnel ingress mode unless a
// SpikeResolver is configured (P0 spike): resolution upgrades the response
// to an ADR 001 307 redirect, resolution failure still fails closed. Replx
// Edge never silently streams video through the Cloudflare control hostname.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/requestid"
	"github.com/LJAM96/replx/internal/routing"
	"github.com/LJAM96/replx/internal/trace"
)

// RequestIDHeader is returned on every control response. Plex clients
// ignore it; operators use it to join logs and traces.
const RequestIDHeader = "X-Replx-Edge-Request-ID"

// CacheHeader reports the user-scoped browse cache outcome on control
// responses: hit, miss or bypass. Media redirects never carry it.
const CacheHeader = "X-Replx-Edge-Cache"

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
	// Secret keys user fingerprinting and playback trace IDs
	// (HMAC-SHA256). Empty disables both; raw tokens are never logged.
	Secret string
	// Metrics receives per-request observations. Nil disables.
	Metrics *metrics.Registry
	// Capture arms targeted protocol capture. Nil disables.
	Capture *capture.Store
	// Cache is the user-scoped browse response store (Zeta). Nil
	// disables caching: every control response falls through to origin.
	Cache cache.Store
	// Client overrides the origin HTTP client (tests). Nil uses a default
	// client with a 30s response-header timeout.
	Client *http.Client
	// Spike, when non-nil, upgrades fail-closed media to 307 redirects
	// where it resolves. Resolution failures still fail closed.
	Spike SpikeResolver
}

// Handler proxies Plex requests to the origin PMS.
type Handler struct {
	origin  *url.URL
	mode    string
	spike   SpikeResolver
	log     *logging.Logger
	secret  string
	metrics *metrics.Registry
	capture *capture.Store
	cache   cache.Store
	client  *http.Client
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
	return &Handler{origin: base, mode: opts.IngressMode, log: opts.Logger, secret: opts.Secret,
		metrics: opts.Metrics, capture: opts.Capture, cache: opts.Cache, client: client, spike: opts.Spike}, nil
}

// obs is the per-request Beta observability identity: fingerprinted user,
// client, session/rating correlation and capture match. The raw token is
// never stored here.
type obs struct {
	fingerprint string
	client      trace.Client
	session     string
	ratingKey   string
	playback    string
	captured    bool
	// Cache lookup outcome for control routes: key/ttl when the route is
	// cacheable and the request carries a user fingerprint, plus the
	// served state (hit, miss or bypass) for logs and headers.
	cacheKey   string
	cacheTTL   time.Duration
	cacheable  bool
	cacheState string
}

func (h *Handler) observe(r *http.Request) obs {
	o := obs{
		client:     trace.ExtractClient(r),
		session:    trace.ExtractSession(r),
		ratingKey:  trace.ExtractRatingKey(r),
		cacheState: "bypass",
	}
	if token := trace.ExtractToken(r); token != "" {
		o.fingerprint = trace.Fingerprint(h.secret, token)
	}
	if trace.IsPlaybackRoute(r.URL.Path) {
		o.playback = trace.PlaybackTraceID(h.secret, o.fingerprint, o.client.ID, o.session, o.ratingKey)
	}
	if h.capture != nil && h.capture.Match(r) {
		o.captured = true
	}
	// User-scoped browse cache: anonymous requests and non-cacheable
	// routes stay bypass so no response is ever shared across users.
	if h.cache != nil && o.fingerprint != "" {
		if ttl, ok := cache.Cacheable(r.Method, r.URL.Path); ok {
			o.cacheable = true
			o.cacheTTL = ttl
			o.cacheKey = cache.ResponseKey(o.fingerprint, r.Method, r.URL.Path, r.URL.Query())
		}
	}
	return o
}

// serveCache attempts a cached control response. It reports whether it
// wrote the response. Misses flip the obs state to miss for logging,
// headers and metrics; store errors read as misses (best-effort cache).
func (h *Handler) serveCache(w http.ResponseWriter, r *http.Request, id string, o *obs, start time.Time) bool {
	if !o.cacheable {
		return false
	}
	entry, ok, err := h.cache.Get(r.Context(), o.cacheKey)
	if err != nil || !ok {
		o.cacheState = "miss"
		if h.metrics != nil {
			h.metrics.IncCacheMiss()
		}
		return false
	}
	o.cacheState = "hit"
	if h.metrics != nil {
		h.metrics.IncCacheHit()
		h.metrics.ObserveHTTP("control", entry.Status, time.Since(start))
	}
	w.Header().Set(RequestIDHeader, id)
	w.Header().Set(CacheHeader, "hit")
	if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	}
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	h.emit(r, id, *o, "control", entry.Status, start, map[string]any{"bodyBytes": len(entry.Body)})
	return true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := requestid.New()
	w.Header().Set(RequestIDHeader, id)
	o := h.observe(r)
	if o.playback != "" {
		w.Header().Set(trace.PlaybackTraceHeader, o.playback)
	}

	routeClass := "control"
	action := gateway.Classify(r.URL.Path)
	if action == gateway.ActionMediaRedirect {
		routeClass = "media"
	}

	if action == gateway.ActionMediaRedirect && h.mode == "cloudflare_tunnel" {
		if h.spike != nil {
			if loc, ok := h.spike.Resolve(r, id); ok {
				h.writeMediaRedirect(w, r, id, o, start, loc)
				return
			}
		}
		h.writeMediaUnavailable(w, r, id, o, start, "unavailable")
		return
	}
	if action == gateway.ActionDenyUnknownMedia && h.mode == "cloudflare_tunnel" {
		// Unknown semantics under a media namespace: deny in tunnel mode
		// (no Cloudflare bytes), pass through in direct mode where no
		// tunnel invariant is at risk.
		h.writeMediaUnavailable(w, r, id, o, start, "denied-unknown-transcode")
		return
	}
	if h.serveStreaming(w, r, id, o, start) {
		return
	}
	if routeClass == "control" && h.serveCache(w, r, id, &o, start) {
		return
	}
	h.proxy(w, r, id, o, routeClass, start)
}

func (h *Handler) writeMediaRedirect(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time, location string) {
	routing.WriteMediaRedirect(w, location)
	if h.metrics != nil {
		h.metrics.ObserveHTTP("media", http.StatusTemporaryRedirect, time.Since(start))
		h.metrics.IncMediaRedirect()
	}
	h.emit(r, id, o, "media", http.StatusTemporaryRedirect, start, map[string]any{
		"decision": "redirected",
		"location": routing.RedactedLocation(location),
		"range":    r.Header.Get("Range") != "",
	})
}

func (h *Handler) writeMediaUnavailable(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time, decision string) {
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
	if h.metrics != nil {
		h.metrics.ObserveHTTP("media", http.StatusForbidden, time.Since(start))
		h.metrics.IncMediaFailure(decision)
	}
	h.emit(r, id, o, "media", http.StatusForbidden, start, map[string]any{"decision": decision})
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, id string, o obs, routeClass string, start time.Time) {
	target := *h.origin
	target.Path = singleJoin(h.origin.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	// ForceRequestURI etc. must not leak; ForceQuery is dropped by design.

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		h.writeBadGateway(w, r, id, o, routeClass, start)
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

	originStart := time.Now()
	resp, err := h.client.Do(out) //nolint:gosec // target is admin-configured origin only
	if h.metrics != nil {
		h.metrics.ObserveOrigin(time.Since(originStart), err != nil)
	}
	if err != nil {
		h.writeBadGateway(w, r, id, o, routeClass, start)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set(RequestIDHeader, id)
	if routeClass == "control" {
		w.Header().Set(CacheHeader, o.cacheState)
	}
	w.WriteHeader(resp.StatusCode)
	n := h.copyBody(w, r, o, resp)
	if h.metrics != nil {
		h.metrics.ObserveHTTP(routeClass, resp.StatusCode, time.Since(start))
	}
	h.emit(r, id, o, routeClass, resp.StatusCode, start, map[string]any{"bodyBytes": n})
}

// copyBody streams the origin body to the client while tee-storing
// cacheable 200s. Bodies over MaxEntryBytes stream fully but skip the
// store; the client never sees a truncated response either way.
func (h *Handler) copyBody(w http.ResponseWriter, r *http.Request, o obs, resp *http.Response) int64 {
	if !o.cacheable || resp.StatusCode != http.StatusOK {
		n, _ := io.Copy(w, resp.Body)
		return n
	}
	var buf bytes.Buffer
	n, _ := io.CopyN(io.MultiWriter(w, &buf), resp.Body, cache.MaxEntryBytes+1)
	if n > cache.MaxEntryBytes {
		rest, _ := io.Copy(w, resp.Body)
		return n + rest
	}
	body := buf.Bytes()
	_ = h.cache.Set(r.Context(), o.cacheKey, cache.Entry{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, o.cacheTTL)
	return n
}

func (h *Handler) writeBadGateway(w http.ResponseWriter, r *http.Request, id string, o obs, routeClass string, start time.Time) {
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
	if h.metrics != nil {
		h.metrics.ObserveHTTP(routeClass, http.StatusBadGateway, time.Since(start))
	}
	h.emit(r, id, o, routeClass, http.StatusBadGateway, start, nil)
}

func (h *Handler) emit(r *http.Request, id string, o obs, routeClass string, status int, start time.Time, fields map[string]any) {
	if h.log == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	// Beta identity: fingerprinted user, client and playback correlation.
	// Raw tokens never reach fields; see trace.Fingerprint.
	if o.fingerprint != "" {
		fields["userFingerprint"] = o.fingerprint
	}
	if o.client.ID != "" {
		fields["client"] = o.client.ID
	}
	if o.client.Product != "" {
		fields["clientProduct"] = o.client.Product
	}
	if o.client.Platform != "" {
		fields["clientPlatform"] = o.client.Platform
	}
	if o.client.Version != "" {
		fields["clientVersion"] = o.client.Version
	}
	if o.playback != "" {
		fields["playbackTraceId"] = o.playback
	}
	if o.ratingKey != "" {
		fields["ratingKey"] = o.ratingKey
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
		Cache:     o.cacheState,
		Fields:    fields,
	})
	if o.captured {
		h.capture.Record(capture.Event{
			RequestID:       id,
			PlaybackTraceID: o.playback,
			Method:          r.Method,
			Path:            logging.RedactURLString(r.URL.RequestURI()),
			Client:          o.client.ID,
			ClientProduct:   o.client.Product,
			RatingKey:       o.ratingKey,
		})
		h.log.Log(logging.Entry{
			Level:     "debug",
			Component: "diagnostics.protocol",
			RequestID: id,
			Method:    r.Method,
			Path:      logging.RedactURLString(r.URL.RequestURI()),
			Status:    status,
			Duration:  time.Since(start).Milliseconds(),
			Route:     routeClass,
			Fields: map[string]any{
				"capture":         true,
				"playbackTraceId": o.playback,
				"client":          o.client.ID,
				"clientProduct":   o.client.Product,
				"clientPlatform":  o.client.Platform,
				"clientVersion":   o.client.Version,
				"ratingKey":       o.ratingKey,
				"session":         o.session != "",
				"range":           r.Header.Get("Range") != "",
			},
		})
	}
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
