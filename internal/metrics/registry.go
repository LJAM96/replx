// Package metrics: Registry holds live counters backing the canonical
// Prometheus names declared in metrics.go. Stdlib only (sync/atomic via
// mutex-guarded maps): no third-party client dependency.
//
// Cache counters stay zero until the Zeta user-scoped cache lands; they
// are exposed now so dashboards and alerts can reference stable names.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Registry is a concurrency-safe live counter set. Zero value is usable.
type Registry struct {
	mu            sync.Mutex
	reqTotal      map[string]int64
	reqDurSum     map[string]float64
	reqDurCount   map[string]int64
	originTotal   int64
	originErr     int64
	originDurSum  float64
	originDurCnt  int64
	mediaRedirect int64
	mediaFailure  map[string]int64
	cacheHits     int64
	cacheMisses   int64
}

// ObserveHTTP records one control-plane response by route class and status.
func (r *Registry) ObserveHTTP(route string, status int, d time.Duration) {
	if route == "" {
		route = "control"
	}
	key := route + "|" + strconv.Itoa(status)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reqTotal == nil {
		r.reqTotal = map[string]int64{}
		r.reqDurSum = map[string]float64{}
		r.reqDurCount = map[string]int64{}
	}
	r.reqTotal[key]++
	r.reqDurSum[route] += d.Seconds()
	r.reqDurCount[route]++
}

// ObserveOrigin records one server-to-server PMS round trip.
func (r *Registry) ObserveOrigin(d time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.originTotal++
	r.originDurSum += d.Seconds()
	r.originDurCnt++
	if failed {
		r.originErr++
	}
}

// IncMediaRedirect counts one ADR 001 307 direct-origin redirect.
func (r *Registry) IncMediaRedirect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mediaRedirect++
}

// IncMediaFailure counts one fail-closed media response by decision
// (e.g. "unavailable", "denied-unknown-transcode").
func (r *Registry) IncMediaFailure(decision string) {
	if decision == "" {
		decision = "unavailable"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mediaFailure == nil {
		r.mediaFailure = map[string]int64{}
	}
	r.mediaFailure[decision]++
}

// IncCacheHit/IncCacheMiss are Zeta hooks. No callers until the
// user-scoped cache lands; exposed so the names stay stable.
func (r *Registry) IncCacheHit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheHits++
}

// IncCacheMiss is a Zeta hook; see IncCacheHit.
func (r *Registry) IncCacheMiss() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheMisses++
}

// Snapshot returns a point-in-time copy for tests and diagnostics.
type Snapshot struct {
	ReqTotal      map[string]int64
	ReqDurCount   map[string]int64
	OriginTotal   int64
	OriginErrors  int64
	MediaRedirect int64
	MediaFailure  map[string]int64
	CacheHits     int64
	CacheMisses   int64
}

// Snapshot copies current counters.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Snapshot{
		ReqTotal:      map[string]int64{},
		ReqDurCount:   map[string]int64{},
		MediaFailure:  map[string]int64{},
		OriginTotal:   r.originTotal,
		OriginErrors:  r.originErr,
		MediaRedirect: r.mediaRedirect,
		CacheHits:     r.cacheHits,
		CacheMisses:   r.cacheMisses,
	}
	for k, v := range r.reqTotal {
		out.ReqTotal[k] = v
	}
	for k, v := range r.reqDurCount {
		out.ReqDurCount[k] = v
	}
	for k, v := range r.mediaFailure {
		out.MediaFailure[k] = v
	}
	return out
}

// WritePrometheus renders the canonical names in Prometheus text exposition
// format. Histograms are exposed as sum+count pairs (buckets land with
// load-testing in Kappa if cardinality proves safe).
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	write := func(name, help, typ string, samples string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s", name, help, name, typ, samples)
	}
	// Per route+status request totals, sorted for stable output.
	keys := make([]string, 0, len(r.reqTotal))
	for k := range r.reqTotal {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var reqSamples string
	for _, k := range keys {
		route, status, _ := splitRouteStatus(k)
		reqSamples += fmt.Sprintf("%s{route=%q,status=%q} %d\n", HTTPRequestsTotal, route, status, r.reqTotal[k])
	}
	write(HTTPRequestsTotal, "Control-plane HTTP responses by route class and status.", "counter", reqSamples)

	var durSamples string
	routes := make([]string, 0, len(r.reqDurCount))
	for route := range r.reqDurCount {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	for _, route := range routes {
		durSamples += fmt.Sprintf("%s_sum{route=%q} %f\n", HTTPRequestDurationSeconds, route, r.reqDurSum[route])
		durSamples += fmt.Sprintf("%s_count{route=%q} %d\n", HTTPRequestDurationSeconds, route, r.reqDurCount[route])
	}
	write(HTTPRequestDurationSeconds, "Control-plane response latency seconds by route class.", "summary", durSamples)

	fmt.Fprintf(w, "# HELP %s PMS origin round trips.\n# TYPE %s counter\n%s %d\n",
		OriginRequestsTotal, OriginRequestsTotal, OriginRequestsTotal, r.originTotal)
	fmt.Fprintf(w, "# HELP %s PMS origin round-trip latency seconds.\n# TYPE %s summary\n%s_sum %f\n%s_count %d\n",
		OriginRequestDurationSeconds, OriginRequestDurationSeconds, OriginRequestDurationSeconds, r.originDurSum, OriginRequestDurationSeconds, r.originDurCnt)
	fmt.Fprintf(w, "# HELP %s PMS origin failures.\n# TYPE %s counter\n%s %d\n",
		OriginErrorsTotal, OriginErrorsTotal, OriginErrorsTotal, r.originErr)
	fmt.Fprintf(w, "# HELP %s ADR 001 direct-origin 307 redirects.\n# TYPE %s counter\n%s %d\n",
		MediaOriginRedirectsTotal, MediaOriginRedirectsTotal, MediaOriginRedirectsTotal, r.mediaRedirect)

	fkeys := make([]string, 0, len(r.mediaFailure))
	for k := range r.mediaFailure {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	var failSamples string
	for _, k := range fkeys {
		failSamples += fmt.Sprintf("%s{decision=%q} %d\n", MediaRouteFailuresTotal, k, r.mediaFailure[k])
	}
	write(MediaRouteFailuresTotal, "Fail-closed media responses by decision.", "counter", failSamples)

	fmt.Fprintf(w, "# HELP %s User-scoped cache hits (Zeta; zero until the cache lands).\n# TYPE %s counter\n%s %d\n",
		CacheHitsTotal, CacheHitsTotal, CacheHitsTotal, r.cacheHits)
	fmt.Fprintf(w, "# HELP %s User-scoped cache misses (Zeta; zero until the cache lands).\n# TYPE %s counter\n%s %d\n",
		CacheMissesTotal, CacheMissesTotal, CacheMissesTotal, r.cacheMisses)
}

func splitRouteStatus(k string) (route, status string, ok bool) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '|' {
			return k[:i], k[i+1:], true
		}
	}
	return k, "", false
}
