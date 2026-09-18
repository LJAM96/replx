package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestObserveAndSnapshot(t *testing.T) {
	var r Registry
	r.ObserveHTTP("control", 200, 10*time.Millisecond)
	r.ObserveHTTP("media", 307, 5*time.Millisecond)
	r.ObserveHTTP("control", 200, 20*time.Millisecond)
	r.ObserveOrigin(30*time.Millisecond, false)
	r.ObserveOrigin(time.Second, true)
	r.IncMediaRedirect()
	r.IncMediaFailure("denied-unknown-transcode")

	s := r.Snapshot()
	if s.ReqTotal["control|200"] != 2 || s.ReqTotal["media|307"] != 1 {
		t.Fatalf("request totals: %+v", s.ReqTotal)
	}
	if s.OriginTotal != 2 || s.OriginErrors != 1 {
		t.Fatalf("origin: total=%d errors=%d", s.OriginTotal, s.OriginErrors)
	}
	if s.MediaRedirect != 1 || s.MediaFailure["denied-unknown-transcode"] != 1 {
		t.Fatalf("media: redirect=%d failure=%+v", s.MediaRedirect, s.MediaFailure)
	}
	if s.CacheHits != 0 || s.CacheMisses != 0 {
		t.Fatal("cache counters must stay zero until Zeta")
	}
}

func TestPrometheusExposition(t *testing.T) {
	var r Registry
	r.ObserveHTTP("control", 200, time.Millisecond)
	r.IncMediaRedirect()
	var b strings.Builder
	r.WritePrometheus(&b)
	out := b.String()
	for _, name := range []string{
		HTTPRequestsTotal, HTTPRequestDurationSeconds,
		OriginRequestsTotal, OriginRequestDurationSeconds, OriginErrorsTotal,
		MediaOriginRedirectsTotal, MediaRouteFailuresTotal,
		CacheHitsTotal, CacheMissesTotal,
	} {
		if !strings.Contains(out, name) {
			t.Errorf("exposition missing %s", name)
		}
	}
	if !strings.Contains(out, `route="control",status="200"} 1`) {
		t.Errorf("missing labelled sample:\n%s", out)
	}
}
