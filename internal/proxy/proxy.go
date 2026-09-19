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

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/playback"
	"github.com/LJAM96/replx/internal/policy"
	"github.com/LJAM96/replx/internal/requestid"
	"github.com/LJAM96/replx/internal/routing"
	"github.com/LJAM96/replx/internal/trace"
	"github.com/LJAM96/replx/internal/warmer"
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

// PlaybackEngine enforces policy at negotiation and session boundaries.
// Nil preserves transparent proxy behaviour.
type PlaybackEngine interface {
	// HandleDecision intercepts one universal negotiation request. It
	// returns (handled, deny): handled means the response is written;
	// deny non-nil means refuse with the coded 403; (false, nil) means
	// transparent proxy (engine disabled path only).
	HandleDecision(w http.ResponseWriter, r *http.Request, id, fingerprint, sessionID, identityID, clientUUID string) (bool, *playback.Deny)
	// EndSession closes playback sessions on stop verbs. Best-effort:
	// it must never break the proxied control flow.
	EndSession(r *http.Request)
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
	// Warmer tracks freshly stored entries for background refresh. Nil
	// disables tracking; caching still works, entries just expire cold.
	Warmer *warmer.Warmer
	// Playback, when non-nil, intercepts universal negotiation for policy
	// enforcement and closes sessions on stop verbs.
	Playback PlaybackEngine
	// Artwork is the shared filesystem transcode cache (Eta). Nil
	// disables it; artwork falls through to origin uncached.
	Artwork *artwork.Store
	// Identity resolves fingerprints to Plex identities for account
	// scoping and policy levels. Nil keeps token-fingerprint scoping.
	Identity *identity.Resolver
	// Client overrides the origin HTTP client (tests). Nil uses a default
	// client with a 30s response-header timeout.
	Client *http.Client
	// Spike, when non-nil, upgrades fail-closed media to 307 redirects
	// where it resolves. Resolution failures still fail closed.
	Spike SpikeResolver
}

// Handler proxies Plex requests to the origin PMS.
type Handler struct {
	origin   *url.URL
	mode     string
	spike    SpikeResolver
	log      *logging.Logger
	secret   string
	metrics  *metrics.Registry
	capture  *capture.Store
	cache    cache.Store
	warmer   *warmer.Warmer
	playback PlaybackEngine
	artwork  *artwork.Store
	identity *identity.Resolver
	client   *http.Client
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
		metrics: opts.Metrics, capture: opts.Capture, cache: opts.Cache, warmer: opts.Warmer,
		playback: opts.Playback, artwork: opts.Artwork, identity: opts.Identity,
		client: client, spike: opts.Spike}, nil
}

// obs is the per-request Beta observability identity: fingerprinted user,
// resolved identity scope, client, session/rating correlation and capture
// match. Raw tokens are never stored here.
type obs struct {
	fingerprint string
	scope       string
	identityID  string
	clientUUID  string
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
	token := trace.ExtractToken(r)
	if token != "" {
		o.fingerprint = trace.Fingerprint(h.secret, token)
		o.scope = "tok:" + o.fingerprint
		if h.identity != nil {
			res := h.identity.Resolve(r.Context(), o.fingerprint, token, identity.FromTrace(o.client))
			if res.Scope != "" {
				o.scope = res.Scope
			}
			o.identityID, o.clientUUID = res.IdentityID, res.ClientID
		}
	}
	if trace.IsPlaybackRoute(r.URL.Path) {
		o.playback = trace.PlaybackTraceID(h.secret, o.fingerprint, o.client.ID, o.session, o.ratingKey)
	}
	if h.capture != nil && h.capture.Match(r) {
		o.captured = true
	}
	// User-scoped browse cache: anonymous requests and non-cacheable
	// routes stay bypass so no response is ever shared across users.
	// Resolved accounts share one scope across all their devices. The
	// fingerprint gate (secret+token required) keeps a missing secret
	// from collapsing everyone into one bucket.
	if h.cache != nil && o.fingerprint != "" && o.scope != "" {
		if ttl, ok := cache.Cacheable(r.Method, r.URL.Path); ok {
			o.cacheable = true
			o.cacheTTL = ttl
			o.cacheKey = cache.ResponseKey(o.scope, r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Accept"))
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
	for k, v := range entry.Headers {
		w.Header().Set(k, v)
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
	if routeClass == "control" && h.playback != nil {
		if gateway.IsSessionStop(r.URL.Path) {
			h.playback.EndSession(r)
		}
		if gateway.IsDecision(r.URL.Path) {
			handled, deny := h.playback.HandleDecision(w, r, id, o.fingerprint, o.session, o.identityID, o.clientUUID)
			if deny != nil {
				h.writePolicyDeny(w, r, id, o, start, deny)
				return
			}
			if handled {
				return
			}
		}
	}
	if routeClass == "control" && h.serveArtwork(w, r, id, o, start) {
		return
	}
	if routeClass == "control" && h.serveCache(w, r, id, &o, start) {
		return
	}
	h.proxy(w, r, id, o, routeClass, start)
}

// serveArtwork serves account-scoped filesystem transcodes. The scope in
// the key means one account's poster can never satisfy another account's
// request; anonymous requests bypass (authorization to reference required).
func (h *Handler) serveArtwork(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) bool {
	if h.artwork == nil || o.fingerprint == "" || o.scope == "" || !artwork.Match(r.URL.Path) {
		return false
	}
	key := artwork.Key(o.scope, r.URL.Path, r.URL.Query())
	if ct, body, ok := h.artwork.Get(key); ok {
		o.cacheState = "hit"
		if h.metrics != nil {
			h.metrics.IncCacheHit()
			h.metrics.ObserveHTTP("control", http.StatusOK, time.Since(start))
		}
		w.Header().Set(RequestIDHeader, id)
		w.Header().Set(CacheHeader, "hit")
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		h.emit(r, id, o, "control", http.StatusOK, start, map[string]any{"bodyBytes": len(body), "artwork": true})
		return true
	}
	target := *h.origin
	target.Path = singleJoin(h.origin.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		return false
	}
	copyHeaders(out.Header, r.Header)
	out.Header.Set(RequestIDHeader, id)
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
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, artwork.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > artwork.MaxBodyBytes {
		// Oversize or unreadable: decline the artwork fast path so the
		// normal proxy streams it uncached. Nothing written yet.
		return false
	}
	if resp.StatusCode == http.StatusOK {
		_ = h.artwork.Set(key, resp.Header.Get("Content-Type"), body)
	}
	o.cacheState = "miss"
	if h.metrics != nil {
		h.metrics.IncCacheMiss()
		h.metrics.ObserveHTTP("control", resp.StatusCode, time.Since(start))
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set(RequestIDHeader, id)
	w.Header().Set(CacheHeader, "miss")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	h.emit(r, id, o, "control", resp.StatusCode, start, map[string]any{"bodyBytes": len(body), "artwork": true})
	return true
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

// writePolicyDeny renders an engine fail-closed refusal: a known playback
// decision endpoint that could not be policy-checked never degrades to
// transparent proxy.
func (h *Handler) writePolicyDeny(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time, deny *playback.Deny) {
	rendered := make([]string, 0, len(deny.Rejected))
	for _, rj := range deny.Rejected {
		rendered = append(rendered, policy.RenderReason(rj))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      deny.Code,
			"message":   deny.Message,
			"rejected":  rendered,
			"requestId": id,
		},
	})
	if h.metrics != nil {
		h.metrics.ObserveHTTP("control", http.StatusForbidden, time.Since(start))
		h.metrics.IncPlaybackDecision()
	}
	h.emit(r, id, o, "control", http.StatusForbidden, start,
		map[string]any{"decision": "deny", "code": deny.Code})
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
// cacheable 200s. Only cleanly completed bodies within the cap are
// stored: a mid-stream origin failure must never persist a truncated
// response for replay, and oversize bodies stream through uncached. The
// client always receives whatever the origin produced either way.
func (h *Handler) copyBody(w http.ResponseWriter, r *http.Request, o obs, resp *http.Response) int64 {
	if !o.cacheable || resp.StatusCode != http.StatusOK {
		n, _ := io.Copy(w, resp.Body)
		return n
	}
	var buf bytes.Buffer
	n, copyErr := io.CopyN(io.MultiWriter(w, &buf), resp.Body, cache.MaxEntryBytes+1)
	// io.CopyN reports EOF when the body ends before the cap: that IS a
	// clean, complete body. Any other error means the origin died
	// mid-stream and the partial bytes must never be cached.
	if copyErr != nil && copyErr != io.EOF {
		return n
	}
	if n > cache.MaxEntryBytes {
		rest, _ := io.Copy(w, resp.Body)
		return n + rest
	}
	body := buf.Bytes()
	_ = h.cache.Set(r.Context(), o.cacheKey, cache.Entry{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Headers:     cache.SafeHeaders(resp.Header),
		Body:        body,
	}, o.cacheTTL)
	if h.warmer != nil {
		h.warmer.Track(o.cacheKey, warmer.Snapshot{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Accept:   r.Header.Get("Accept"),
			Scope:    o.scope,
			TTL:      o.cacheTTL,
		})
	}
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
	if o.identityID != "" {
		fields["identityId"] = o.identityID
	}
	if o.clientUUID != "" {
		fields["clientInstanceId"] = o.clientUUID
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
