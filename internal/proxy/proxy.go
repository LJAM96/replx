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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/origin"
	"github.com/LJAM96/replx/internal/playback"
	"github.com/LJAM96/replx/internal/policy"
	"github.com/LJAM96/replx/internal/requestid"
	"github.com/LJAM96/replx/internal/routing"
	"github.com/LJAM96/replx/internal/spike"
	"github.com/LJAM96/replx/internal/trace"
	"github.com/LJAM96/replx/internal/warmer"
)

// RequestIDHeader is returned on every control response. Plex clients
// ignore it; operators use it to join logs and traces.
const RequestIDHeader = "X-Replx-Edge-Request-ID"

// CacheHeader reports the user-scoped browse cache outcome on control
// responses: hit, miss or bypass. Media redirects never carry it.
const CacheHeader = "X-Replx-Edge-Cache"

// Limit simultaneous uncached browse fetches so a Plex Web collection burst
// cannot open dozens of expensive library queries against one PMS at once.
const browseOriginConcurrency = 8

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
	Resolve(r *http.Request, ctx spike.ResolveContext) (location string, ok bool)
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
	// PublicBase is the advertised HTTPS connection for browser redirects.
	PublicBase string
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
	// PartPolicy enforces the playback part/manifest boundary. It runs
	// before transport selection in every ingress mode: tunnel mode
	// consults it inside the spike resolver, and direct mode consults it
	// here so a negotiation-skipping client cannot pull a prohibited part
	// through Replx itself. Nil preserves transparent proxy behaviour.
	PartPolicy func(r *http.Request, partID, sessionID string) (substituteKey string, deny bool, reason string)
	// MediaFallbackURL is the optional DNS-only media gateway base URL
	// (e.g. https://media.example.com). When set and spike resolution
	// fails in tunnel mode, media 307-redirects there instead of 403.
	// Empty disables the fallback: requests fail with MEDIA_ROUTE_UNAVAILABLE.
	MediaFallbackURL string
	// SearchDB enables local title search (/hubs/search) with PMS fallback.
	// Nil disables local search: requests fall through to origin.
	SearchDB database.DBTX
}

// Handler proxies Plex requests to the origin PMS.
type Handler struct {
	origin            *url.URL
	public            *url.URL
	mode              string
	spike             SpikeResolver
	log               *logging.Logger
	secret            string
	metrics           *metrics.Registry
	capture           *capture.Store
	cache             cache.Store
	warmer            *warmer.Warmer
	playback          PlaybackEngine
	artwork           *artwork.Store
	identity          *identity.Resolver
	client            *http.Client
	browseSlots       chan struct{}
	staleRefreshSlots chan struct{}
	mediaFallback     *url.URL
	searchDB          database.DBTX
	// partPolicy is the playback boundary hook; see Options.PartPolicy.
	partPolicy func(r *http.Request, partID, sessionID string) (string, bool, string)
	// gens implements namespace cache invalidation; see cache.Generations.
	gens *cache.Generations
	// flightMu guards in-flight cacheable origin fetches for stampede
	// control: one request refreshes an expired object while concurrent
	// requests for the same key wait bounded for the cache to populate.
	flightMu            sync.Mutex
	flight              map[string]struct{}
	artworkWriteLogOnce sync.Once
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
	var public *url.URL
	if opts.PublicBase != "" {
		public, err = url.Parse(opts.PublicBase)
		if err != nil || public.Scheme != "https" || public.Host == "" {
			return nil, fmt.Errorf("proxy: invalid public base")
		}
	}
	client := opts.Client
	if client == nil {
		// Transparent by design: origin 3xx responses pass through to
		// the Plex client untouched instead of being followed (and
		// re-credentialed) by Replx. See internal/origin.
		client = origin.TransparentClient(60 * time.Second)
	}
	var fallback *url.URL
	if opts.MediaFallbackURL != "" {
		fb, err := url.Parse(opts.MediaFallbackURL)
		if err != nil || fb.Scheme == "" || fb.Host == "" {
			return nil, fmt.Errorf("proxy: invalid media fallback URL")
		}
		if fb.Scheme != "https" {
			return nil, fmt.Errorf("proxy: media fallback URL must be https")
		}
		fallback = fb
	}
	return &Handler{origin: base, public: public, mode: opts.IngressMode, log: opts.Logger, secret: opts.Secret,
		metrics: opts.Metrics, capture: opts.Capture, cache: opts.Cache, warmer: opts.Warmer,
		playback: opts.Playback, artwork: opts.Artwork, identity: opts.Identity,
		client: client, browseSlots: make(chan struct{}, browseOriginConcurrency),
		staleRefreshSlots: make(chan struct{}, 2),
		spike:             opts.Spike, mediaFallback: fallback, searchDB: opts.SearchDB,
		partPolicy: opts.PartPolicy, gens: cache.NewGenerations()}, nil
}

// obs is the per-request Beta observability identity: fingerprinted user,
// resolved identity scope, client, session/rating correlation and capture
// match. Raw tokens are never stored here.
type obs struct {
	fingerprint string
	scope       string
	identityID  string
	clientUUID  string
	// fresh authorizes long lived locally served responses (artwork):
	// the credential was proven within the validity window. invalid
	// marks definitively rejected credentials: bypass all local serving.
	fresh     bool
	invalid   bool
	client    trace.Client
	session   string
	ratingKey string
	playback  string
	captured  bool
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
			if res.IdentityID != "" {
				o.scope = cache.UserScope(res.IdentityID)
			} else if res.Scope != "" {
				o.scope = res.Scope
			}
			o.identityID, o.clientUUID = res.IdentityID, res.ClientID
			o.fresh, o.invalid = res.Fresh, res.Invalid
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
			class := cache.ClassOf(r.URL.Path)
			sg, gg := h.gens.Get(o.scope, class)
			o.cacheKey = cache.ResponseKeyGen(o.scope, class, r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Accept"), sg, gg)
		}
	}
	return o
}

// serveCache attempts a cached control response. It reports whether it
// wrote the response. Misses flip the obs state to miss for logging,
// headers and metrics; store errors read as misses (best-effort cache).
func (h *Handler) serveCache(w http.ResponseWriter, r *http.Request, id string, o *obs, start time.Time) bool {
	if !o.cacheable || o.invalid {
		// Definitively rejected credentials never read local state:
		// PMS answers live (usually 401) instead.
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
	// Cloudflare edge cache stays disabled for Plex API routes in 1.0:
	// Replx Edge owns cache correctness (user-scoped TTLs above).
	w.Header().Set("CDN-Cache-Control", "no-store")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
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

// serveCollectionWindow answers a paged collection request from a complete
// user-scoped JSON window. The exact user, non-pagination query, response
// format and invalidation generations must match. PMS permissions are
// revalidated by the identity resolver before any local response is used.
func (h *Handler) serveCollectionWindow(w http.ResponseWriter, r *http.Request, id string, o *obs, start time.Time) bool {
	if h.cache == nil || !o.cacheable || o.invalid || !o.fresh || r.Method != http.MethodGet ||
		!strings.HasPrefix(r.URL.Path, "/library/collections/") || !strings.HasSuffix(r.URL.Path, "/children") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Accept")), "json") {
		return false
	}
	var offset, count int
	var haveOffset, haveCount bool
	for k, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(k) {
		case "x-plex-container-start":
			offset, haveOffset = parseWindowNumber(values[0])
		case "x-plex-container-size":
			count, haveCount = parseWindowNumber(values[0])
		}
	}
	if !haveOffset || !haveCount {
		return false
	}
	class := cache.ClassOf(r.URL.Path)
	sg, gg := h.gens.Get(o.scope, class)
	key := cache.CollectionWindowKeyGen(o.scope, class, r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Accept"), sg, gg)
	entry, ok, err := h.cache.Get(r.Context(), key)
	if err != nil || !ok || entry.Status != http.StatusOK || !strings.Contains(strings.ToLower(entry.ContentType), "json") {
		return false
	}
	body, ok := cache.CollectionWindowPage(entry.Body, offset, count)
	if !ok {
		return false
	}
	o.cacheState = "window"
	if h.metrics != nil {
		h.metrics.IncCacheHit()
		h.metrics.ObserveHTTP("control", http.StatusOK, time.Since(start))
	}
	w.Header().Set(RequestIDHeader, id)
	w.Header().Set(CacheHeader, "window")
	w.Header().Set("CDN-Cache-Control", "no-store")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	w.Header().Set("Content-Type", entry.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	h.emit(r, id, *o, "control", http.StatusOK, start, map[string]any{"bodyBytes": len(body)})
	return true
}

func parseWindowNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// serveStale keeps an already visited structural page responsive while Plex
// is slow. A validated credential and the exact user/query cache key are
// required; Continue Watching and playback state never use this path.
func (h *Handler) serveStale(w http.ResponseWriter, r *http.Request, id string, o *obs, start time.Time) bool {
	if _, allowed := cache.FallbackTTL(r.URL.Path); !o.cacheable || !allowed || o.invalid ||
		(h.identity != nil && !o.fresh) {
		return false
	}
	entry, ok, err := h.cache.Get(r.Context(), cache.StaleKey(o.cacheKey))
	if err != nil || !ok || entry.Status != http.StatusOK {
		return false
	}
	o.cacheState = "stale"
	if h.metrics != nil {
		h.metrics.IncCacheStale()
		h.metrics.ObserveHTTP("control", entry.Status, time.Since(start))
	}
	w.Header().Set(RequestIDHeader, id)
	w.Header().Set(CacheHeader, "stale")
	w.Header().Set("CDN-Cache-Control", "no-store")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	}
	for k, v := range entry.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	h.emit(r, id, *o, "control", entry.Status, start, map[string]any{"bodyBytes": len(entry.Body)})
	h.refreshStale(r, *o)
	return true
}

func (h *Handler) refreshStale(r *http.Request, o obs) {
	if !h.tryBeginFlight(o.cacheKey) {
		return
	}
	select {
	case h.staleRefreshSlots <- struct{}{}:
	default:
		h.endFlight(o.cacheKey)
		return
	}
	go func() {
		defer h.endFlight(o.cacheKey)
		defer func() { <-h.staleRefreshSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()
		request := r.Clone(ctx)
		request.Body = nil // stale refreshes are GET/HEAD only
		refresh := o
		refresh.cacheState = "miss"
		h.proxy(&discardResponseWriter{header: make(http.Header)}, request, requestid.New(), refresh, "control", time.Now())
	}()
}

type discardResponseWriter struct{ header http.Header }

func (w *discardResponseWriter) Header() http.Header         { return w.header }
func (w *discardResponseWriter) WriteHeader(int)             {}
func (w *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := requestid.New()
	w.Header().Set(RequestIDHeader, id)
	o := h.observe(r)
	if o.playback != "" {
		w.Header().Set(trace.PlaybackTraceHeader, o.playback)
	}

	// A request target carrying its own authority (absolute URL or
	// scheme-relative //host/...) must never become an origin fetch or a
	// redirect target. Reject before classification.
	if r.URL.Host != "" || strings.HasPrefix(r.URL.Path, "//") {
		h.writeInvalidPath(w, r, id, o, start)
		return
	}

	routeClass := "control"
	action := gateway.Classify(r.URL.Path)
	switch action {
	case gateway.ActionMediaRedirect:
		routeClass = "media"
	case gateway.ActionDenySessions:
		h.writeSessionsDeny(w, r, id, o, start)
		return
	case gateway.ActionPlayQueueObserve:
		// Observe for correlation; enforcement stays at the decision and
		// part boundaries. Never rewrite queue items in 1.0.
		h.emit(r, id, o, "control", 0, start, map[string]any{
			"playQueue": "observed",
			"uri":       logging.RedactURLString(r.URL.RequestURI()),
		})
		// Fall through to control proxy below.
	case gateway.ActionDenyUnknownMedia:
		if h.mode == "cloudflare_tunnel" {
			// Unknown semantics under a media namespace: deny in tunnel mode
			// (no Cloudflare bytes), pass through in direct mode where no
			// tunnel invariant is at risk.
			h.writeMediaUnavailable(w, r, id, o, start, "denied-unknown-transcode")
			return
		}
	}

	if action == gateway.ActionMediaRedirect && h.mode == "cloudflare_tunnel" {
		if h.spike != nil {
			ctx := spike.ResolveContext{RequestID: id, SessionID: o.session, PlaybackTraceID: o.playback}
			if loc, ok := h.spike.Resolve(r, ctx); ok {
				h.writeMediaRedirect(w, r, id, o, start, loc)
				return
			}
		}
		if h.mediaFallback != nil {
			h.writeMediaGatewayRedirect(w, r, id, o, start)
			return
		}
		h.writeMediaUnavailable(w, r, id, o, start, "unavailable")
		return
	}
	// Media authorization precedes transport selection in direct mode as
	// well: a client that skips negotiation must not pull a prohibited
	// part or trigger a forbidden transcode through Replx itself.
	// Segments stay proxied: their PMS transcode keys only exist after a
	// policy-checked negotiation, and they carry no stable session binding.
	if action == gateway.ActionMediaRedirect && h.mode == "direct" {
		if pol := h.boundaryPolicy(); pol != nil {
			partID := playback.PartIDFromPath(r.URL.Path)
			if partID != "" || gateway.IsManifest(r.URL.Path) {
				sub, deny, reason := pol(r, partID, o.session)
				if deny {
					h.writePartDeny(w, r, id, o, start, reason)
					return
				}
				if sub != "" {
					r = substitutePartRequest(r, sub)
				}
			}
		}
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
	if routeClass == "control" && h.serveSearch(w, r, id, o, start) {
		return
	}
	if routeClass == "control" && h.serveCache(w, r, id, &o, start) {
		return
	}
	if routeClass == "control" && h.serveCollectionWindow(w, r, id, &o, start) {
		return
	}
	if routeClass == "control" && h.serveStale(w, r, id, &o, start) {
		return
	}
	// Watch-state invalidation runs for every state mutation, whether or
	// not the route itself is cacheable: timeline/scrobble POSTs retire
	// the writer's Continue Watching namespace via generations.
	if isStateWrite(r.URL.Path) {
		h.bumpStateGeneration(o.scope)
	}
	// Stampede control: one request refreshes an expired object while
	// concurrent requests for the same key wait bounded for the cache to
	// populate, then serve the fresh entry instead of churning origin.
	// Acquisition is atomic: exactly one request per key becomes the
	// refresher, the rest wait and re-check.
	if o.cacheable && h.cache != nil {
		if h.waitForFlight(r.Context(), o.cacheKey) {
			if h.serveCache(w, r, id, &o, start) {
				return
			}
		}
		if h.tryBeginFlight(o.cacheKey) {
			defer h.endFlight(o.cacheKey)
		} else {
			// Lost the race after waiting: re-check once, then fall
			// through to origin rather than serialize behind the winner.
			if h.serveCache(w, r, id, &o, start) {
				return
			}
		}
	}
	h.proxy(w, r, id, o, routeClass, start)
}

// isStateWrite reports watch-state mutations that retire Continue
// Watching: timeline progress plus scrobble transitions.
func isStateWrite(path string) bool {
	p := strings.ToLower(path)
	return strings.Contains(p, "/:/timeline") ||
		strings.Contains(p, "/:/scrobble") ||
		strings.Contains(p, "/:/unscrobble")
}

// bumpStateGeneration retires the writer's Continue Watching namespace in
// constant time. TTLs remain the correctness backstop.
func (h *Handler) bumpStateGeneration(scope string) {
	if h.gens == nil || scope == "" {
		return
	}
	h.gens.Bump(scope, "cw")
}

// Generations exposes the cache invalidation registry for admin wiring.
func (h *Handler) Generations() *cache.Generations {
	if h == nil {
		return nil
	}
	return h.gens
}

// InvalidateCache retires cache namespaces for the admin invalidation API:
// scope "all" retires everything, empty class retires the whole scope,
// otherwise one (scope, class) namespace.
func (h *Handler) InvalidateCache(scope, class string) {
	if h.gens == nil {
		return
	}
	switch {
	case scope == "" || scope == "all":
		h.gens.BumpAll()
	case class == "":
		h.gens.BumpScope(scope)
	default:
		h.gens.Bump(scope, class)
	}
}

// waitForFlight reports whether another request is already refreshing key.
// When true the caller should re-check the cache after a bounded wait.
func (h *Handler) waitForFlight(ctx context.Context, key string) bool {
	h.flightMu.Lock()
	if h.flight == nil {
		h.flight = map[string]struct{}{}
	}
	_, inflight := h.flight[key]
	h.flightMu.Unlock()
	if !inflight {
		return false
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
		if h.cache == nil {
			return false
		}
		if _, ok, _ := h.cache.Get(ctx, key); ok {
			return true
		}
		h.flightMu.Lock()
		_, still := h.flight[key]
		h.flightMu.Unlock()
		if !still {
			return true
		}
	}
	return false
}

func (h *Handler) tryBeginFlight(key string) bool {
	h.flightMu.Lock()
	defer h.flightMu.Unlock()
	if h.flight == nil {
		h.flight = map[string]struct{}{}
	}
	if _, ok := h.flight[key]; ok {
		return false
	}
	h.flight[key] = struct{}{}
	return true
}

func (h *Handler) endFlight(key string) {
	h.flightMu.Lock()
	defer h.flightMu.Unlock()
	delete(h.flight, key)
}

// serveArtwork serves account-scoped filesystem transcodes. The scope in
// the key means one account's poster can never satisfy another account's
// request; anonymous requests bypass (authorization to reference required).
// A definitively rejected or stale credential (resolver present, not
// fresh) falls through to PMS, which re-authorizes live: the 7-day cache
// must not extend a dead credential. Without a resolver (tests) the gate
// cannot evaluate and artwork caching stays permissive.
func (h *Handler) serveArtwork(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) bool {
	if h.artwork == nil || o.fingerprint == "" || o.scope == "" || !artwork.Match(r.URL.Path) {
		return false
	}
	if o.invalid || (h.identity != nil && !o.fresh) {
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
	if fwd := forwardedFor(r); fwd != "" {
		out.Header.Set("X-Forwarded-For", fwd)
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
		if err := h.artwork.Set(key, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), body); err != nil && h.log != nil {
			h.artworkWriteLogOnce.Do(func() {
				h.log.Log(logging.Entry{Level: "warn", Component: "cache",
					Fields: map[string]any{"event": "artwork_write_failed"}})
			})
		}
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

// rewriteWebRedirect keeps Plex Web on the public Replx connection when PMS
// redirects /web to its own plex.direct address. Other redirect targets pass
// through unchanged, including media routes.
func (h *Handler) rewriteWebRedirect(raw string) string {
	if h.public == nil || raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Path, "/web/") {
		return ""
	}
	if u.IsAbs() && !strings.EqualFold(u.Host, h.origin.Host) {
		return ""
	}
	if u.IsAbs() && u.Scheme != h.origin.Scheme {
		return ""
	}
	target := *h.public
	target.Path = u.Path
	target.RawQuery = u.RawQuery
	target.Fragment = u.Fragment
	return target.String()
}

// serveSearch is intentionally a PMS passthrough in Production 1.0: the
// owner index carries no per-user library grants, so serving its
// candidates would bypass Plex visibility controls (any token string,
// valid or not, could read owner-only titles). /hubs/search always falls
// through to PMS until per-user grants are synchronized or candidates are
// PMS-authorized. The search package remains for candidate generation
// behind a future authorized path.
func (h *Handler) serveSearch(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) bool {
	_ = w
	_ = r
	_ = id
	_ = o
	_ = start
	return false
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

func (h *Handler) writeMediaGatewayRedirect(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) {
	target := *h.mediaFallback
	target.Path = singleJoin(h.mediaFallback.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	routing.WriteMediaRedirect(w, target.String())
	if h.metrics != nil {
		h.metrics.ObserveHTTP("media", http.StatusTemporaryRedirect, time.Since(start))
		h.metrics.IncMediaGatewayRoute()
	}
	h.emit(r, id, o, "media", http.StatusTemporaryRedirect, start, map[string]any{
		"decision": "media-gateway",
		"location": routing.RedactedLocation(target.String()),
		"range":    r.Header.Get("Range") != "",
	})
}

// writeSessionsDeny refuses public /status/sessions with an explainable
// 403. Companion control across Replx Edge is unsupported in 1.0;
// operators use GET /api/v1/sessions on the private admin listener.
func (h *Handler) writeSessionsDeny(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      "SESSIONS_OWNER_ADMIN_ONLY",
			"message":   "GET /status/sessions is owner-admin only via the private admin API (GET /api/v1/sessions); companion control across Replx Edge is unsupported in 1.0",
			"requestId": id,
		},
	})
	if h.metrics != nil {
		h.metrics.ObserveHTTP("control", http.StatusForbidden, time.Since(start))
	}
	h.emit(r, id, o, "control", http.StatusForbidden, start, map[string]any{"decision": "sessions-deny"})
}

// writeInvalidPath rejects request targets carrying their own authority.
// Such references must never become origin fetches or redirect targets.
func (h *Handler) writeInvalidPath(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      "INVALID_PATH",
			"message":   "request target must be an origin-relative path",
			"requestId": id,
		},
	})
	if h.metrics != nil {
		h.metrics.ObserveHTTP("control", http.StatusBadRequest, time.Since(start))
	}
	h.emit(r, id, o, "control", http.StatusBadRequest, start, map[string]any{"decision": "invalid-path"})
}

// writePartDeny renders a playback-boundary refusal in direct mode: the
// negotiated selection forbids this part or manifest.
func (h *Handler) writePartDeny(w http.ResponseWriter, r *http.Request, id string, o obs, start time.Time, reason string) {
	code := reason
	if code == "" {
		code = playback.DecisionRequired
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   "playback policy forbids this media request",
			"requestId": id,
		},
	})
	if h.metrics != nil {
		h.metrics.ObserveHTTP("media", http.StatusForbidden, time.Since(start))
		h.metrics.IncPolicyRejection()
	}
	h.emit(r, id, o, "media", http.StatusForbidden, start, map[string]any{"decision": "part-deny", "code": code})
}

// boundaryPolicy returns the playback boundary hook: the explicitly wired
// PartPolicy first, else the spike store's hook (tunnel wiring) so media
// authorization runs before transport selection in every ingress mode.
func (h *Handler) boundaryPolicy() func(r *http.Request, partID, sessionID string) (string, bool, string) {
	if h.partPolicy != nil {
		return h.partPolicy
	}
	if bp, ok := h.spike.(interface {
		BoundaryPolicy() func(*http.Request, string, string) (string, bool, string)
	}); ok && bp != nil {
		return bp.BoundaryPolicy()
	}
	return nil
}

// substitutePartRequest rewrites a prohibited part request to the session's
// selected allowed part. The substitute is a part key path; the original
// query is preserved unless the substitute carries its own.
func substitutePartRequest(r *http.Request, substitute string) *http.Request {
	path, query := substitute, r.URL.RawQuery
	if i := strings.Index(substitute, "?"); i >= 0 {
		path, query = substitute[:i], substitute[i+1:]
	}
	if !strings.HasPrefix(path, "/") {
		return r
	}
	r2 := r.Clone(r.Context())
	u2 := *r2.URL
	u2.Path, u2.RawPath, u2.RawQuery = path, "", query
	r2.URL = &u2
	r2.RequestURI = ""
	return r2
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
		h.metrics.IncPolicyRejection()
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
	// Cacheable origin requests are normalized to the identity encoding:
	// Go's transport then fetches (and transparently decodes) gzip
	// itself, so stored bytes are always servable regardless of what
	// Accept-Encoding the client sent. Non-cacheable traffic keeps the
	// client's encoding untouched.
	if o.cacheable {
		out.Header.Del("Accept-Encoding")
	}
	// The origin gets an informational client IP for its own logs, never
	// for auth; see forwardedFor for the trust model.
	if fwd := forwardedFor(r); fwd != "" {
		out.Header.Set("X-Forwarded-For", fwd)
	}
	out.Host = h.origin.Host

	originQueueMs := int64(0)
	if o.cacheable && h.browseSlots != nil {
		queueStart := time.Now()
		select {
		case h.browseSlots <- struct{}{}:
			defer func() { <-h.browseSlots }()
		case <-r.Context().Done():
			return
		}
		originQueueMs = time.Since(queueStart).Milliseconds()
	}
	preOriginMs := time.Since(start).Milliseconds()
	originStart := time.Now()
	resp, err := h.client.Do(out) //nolint:gosec // target is admin-configured origin only
	originHeaderMs := time.Since(originStart).Milliseconds()
	if h.metrics != nil {
		h.metrics.ObserveOrigin(time.Since(originStart), err != nil)
	}
	if err != nil {
		h.logOriginFailure(id, err, preOriginMs, originQueueMs, originHeaderMs)
		h.writeBadGateway(w, r, id, o, routeClass, start)
		return
	}
	defer resp.Body.Close()

	// Response-side media guard: in tunnel mode a control-classified route
	// that returns bulk media bytes must fail closed BEFORE streaming.
	// This covers unknown large-body media the path classifier could not
	// predict. Artwork (image/*) and small XML/JSON stay control.
	if h.mode == "cloudflare_tunnel" && routeClass == "control" && isBulkMediaResponse(resp.Header) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		h.writeMediaUnavailable(w, r, id, o, start, "response-media-guard")
		return
	}

	copyHeaders(w.Header(), resp.Header)
	if routeClass == "control" && strings.HasPrefix(r.URL.Path, "/web") && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if location := h.rewriteWebRedirect(resp.Header.Get("Location")); location != "" {
			w.Header().Set("Location", location)
		}
	}
	w.Header().Set(RequestIDHeader, id)
	if routeClass == "control" {
		w.Header().Set(CacheHeader, o.cacheState)
		// Disable Cloudflare edge caching for Plex API routes.
		w.Header().Set("CDN-Cache-Control", "no-store")
		w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	}
	w.WriteHeader(resp.StatusCode)
	n := h.copyBody(w, r, o, resp)
	if h.metrics != nil {
		h.metrics.ObserveHTTP(routeClass, resp.StatusCode, time.Since(start))
	}
	h.emit(r, id, o, routeClass, resp.StatusCode, start, map[string]any{
		"bodyBytes": n, "preOriginMs": preOriginMs, "originQueueMs": originQueueMs,
		"originHeaderMs": originHeaderMs})
}

// logOriginFailure records a bounded error category without logging the
// origin URL, Plex token, or request headers. Those may be embedded in the
// error text returned by net/http.
func (h *Handler) logOriginFailure(requestID string, err error, preOriginMs, originQueueMs, originHeaderMs int64) {
	if h.log == nil {
		return
	}
	h.log.Log(logging.Entry{Level: "warn", Component: "gateway.origin", RequestID: requestID,
		Fields: map[string]any{"errorClass": originErrorClass(err),
			"preOriginMs": preOriginMs, "originQueueMs": originQueueMs,
			"originHeaderMs": originHeaderMs}})
}

func originErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "request_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network_timeout"
	}
	return "transport_other"
}

// copyBody streams the origin body to the client while tee-storing
// cacheable 200s. Only cleanly completed bodies within the cap are
// stored: a mid-stream origin failure must never persist a truncated
// response for replay, and oversize bodies stream through uncached. The
// client always receives whatever the origin produced either way.
func (h *Handler) copyBody(w http.ResponseWriter, r *http.Request, o obs, resp *http.Response) int64 {
	if !o.cacheable || resp.StatusCode != http.StatusOK || !storableResponse(resp.Header) {
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
	entry := cache.Entry{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Headers:     cache.SafeHeaders(resp.Header),
		Body:        body,
	}
	_ = h.cache.Set(r.Context(), o.cacheKey, entry, o.cacheTTL)
	if ttl, ok := cache.FallbackTTL(r.URL.Path); ok {
		_ = h.cache.Set(r.Context(), cache.StaleKey(o.cacheKey), entry, ttl)
	}
	if h.warmer != nil {
		h.warmer.Track(o.cacheKey, warmer.Snapshot{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Accept:   r.Header.Get("Accept"),
			Scope:    o.scope,
			Class:    cache.ClassOf(r.URL.Path),
			TTL:      o.cacheTTL,
		})
	}
	return n
}

// storableResponse reports whether an origin response is safe to persist:
// identity encoding only (stored bytes must be servable regardless of the
// requester's Accept-Encoding) and no wildcard Vary (the key does not
// model arbitrary request variation).
func storableResponse(hdr http.Header) bool {
	if ce := strings.TrimSpace(strings.ToLower(hdr.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		return false
	}
	for _, v := range strings.Split(hdr.Get("Vary"), ",") {
		if strings.TrimSpace(strings.ToLower(v)) == "*" {
			return false
		}
	}
	return true
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

// isBulkMediaResponse reports origin responses carrying bulk media bytes:
// video/*, audio/* (artwork image/* stays control), HLS/DASH manifests.
// Checked BEFORE streaming in tunnel mode so unknown large-body media
// fails closed with MEDIA_ROUTE_UNAVAILABLE instead of traversing Cloudflare.
func isBulkMediaResponse(h http.Header) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(h.Get("Content-Type"), ";", 2)[0]))
	switch {
	case strings.HasPrefix(ct, "video/"):
		return true
	case strings.HasPrefix(ct, "audio/"):
		return true
	case ct == "application/vnd.apple.mpegurl" || ct == "application/x-mpegurl":
		return true
	case ct == "application/dash+xml":
		return true
	default:
		return false
	}
}

func singleJoin(a, b string) string {
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

// tailscaleRange mirrors the admin bind policy: the CGNAT overlay is an
// explicitly permitted private route. See internal/config.
var tailscaleRange = netip.MustParsePrefix("100.64.0.0/10")

// forwardedFor builds the X-Forwarded-For value for origin requests under
// a defined trusted-peer model. The immediate peer address is parsed with
// net.SplitHostPort (never a naive colon split) and always recorded.
// Inbound history is preserved only when the immediate peer is
// infrastructure Replx terminates behind (loopback, private, Tailscale
// CGNAT, link-local): any other peer could have forged the leftmost
// entries, so the value is replaced with just the observed address.
// Informational for origin logs only, never authentication.
func forwardedFor(r *http.Request) string {
	observed := peerIP(r)
	if observed == "" {
		return ""
	}
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" && trustedPeer(observed) {
		return prior + ", " + observed
	}
	return observed
}

// peerIP parses the immediate peer address from RemoteAddr.
func peerIP(r *http.Request) string {
	if r == nil || r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return strings.TrimSpace(host)
	}
	// No port present: accept a bare IP literally, reject anything else.
	if ip, err := netip.ParseAddr(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return ip.String()
	}
	return ""
}

// trustedPeer reports whether forwarding history arriving from ip may be
// preserved: only infrastructure peers, never arbitrary clients.
func trustedPeer(ip string) bool {
	parsed, err := netip.ParseAddr(ip)
	if err != nil || parsed.Zone() != "" {
		return false
	}
	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() ||
		tailscaleRange.Contains(parsed)
}
