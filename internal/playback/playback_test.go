package playback

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/policy"
)

func intp(n int) *int { return &n }

const decisionItem = `{"MediaContainer":{"Metadata":[{
	"ratingKey":"999","key":"/library/metadata/999","type":"movie","title":"Decision Film",
	"Media":[
		{"id":31,"container":"mkv","videoCodec":"hevc","width":3840,"height":2160,"bitrate":60000,
		 "videoResolution":"2160","dynamicRange":"HDR10","audioCodec":"truehd","audioChannels":8,
		 "Part":[{"id":301,"key":"/library/parts/301/file.mkv"}]},
		{"id":32,"container":"mp4","videoCodec":"h264","width":1920,"height":1080,"bitrate":15000,
		 "videoResolution":"1080","dynamicRange":"SDR","audioCodec":"aac","audioChannels":2,
		 "Part":[{"id":302,"key":"/library/parts/302/file.mp4"}]}
	]}]}}`

// fakeOrigin serves metadata and a decision endpoint honouring mediaIndex.
func fakeOrigin(t *testing.T, transcode bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			_, _ = fmt.Fprint(w, decisionItem)
		case strings.Contains(r.URL.Path, "/transcode/universal/decision"):
			mi := r.URL.Query().Get("mediaIndex")
			mode := `"directPlay":true,"directStream":false,"transcode":false,"text":"Direct Play","code":2000`
			if transcode {
				mode = `"directPlay":false,"directStream":false,"transcode":true,"text":"Transcode","code":2002`
			}
			_, _ = fmt.Fprintf(w, `{"MediaContainer":{"decision":{%s},"mediaIndex":%s}}`, mode, miOr(mi))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func miOr(mi string) string {
	if mi == "" {
		return "0"
	}
	return mi
}

func jodiePolicy(context.Context, *string, *string) (policy.Policy, string, error) {
	return policy.Effective(policy.Policy{}, policy.Policy{
		MaxSourceWidth: intp(1920), MaxSourceHeight: intp(1080),
	}, policy.Policy{}), "USER", nil
}

func TestHandleDecisionSelects1080p(t *testing.T) {
	origin := fakeOrigin(t, false)
	defer origin.Close()
	var reg metrics.Registry
	e := &Engine{Origin: origin.URL, Metrics: &reg, Store: NewMemoryStore(), LoadPolicy: jodiePolicy}
	req := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F999&mediaIndex=0&session=sess-1", nil)
	req.Header.Set("X-Plex-Token", "user-tok")
	rec := httptest.NewRecorder()
	if !e.HandleDecision(rec, req, "req-1", "fp-1", "sess-1", "", "") {
		t.Fatal("decision must be handled")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mediaIndex":1`) {
		t.Fatalf("PMS must see rewritten index 1: %d %s", rec.Code, rec.Body.String())
	}
	sess, ok, err := e.Store.FindActive(context.Background(), "sess-1")
	if err != nil || !ok || sess.SelectedPartPlexID != "302" || sess.SelectedPartKey != "/library/parts/302/file.mp4" {
		t.Fatalf("session: %+v %v %v", sess, ok, err)
	}
	if reg.Snapshot().PlaybackDecisions != 1 {
		t.Fatal("decision counter must increment")
	}
}

func TestHandleDecisionDeniesTranscode(t *testing.T) {
	origin := fakeOrigin(t, true) // PMS insists on transcoding
	defer origin.Close()
	e := &Engine{Origin: origin.URL, Store: NewMemoryStore(), LoadPolicy: func(context.Context, *string, *string) (policy.Policy, string, error) {
		p, s, _ := jodiePolicy(context.Background(), nil, nil)
		p.AllowTranscode = policy.Deny
		return p, s, nil
	}}
	req := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F999&mediaIndex=1&session=s2", nil)
	req.Header.Set("X-Plex-Token", "user-tok")
	rec := httptest.NewRecorder()
	if !e.HandleDecision(rec, req, "req-2", "fp-1", "s2", "", "") {
		t.Fatal("must handle")
	}
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), policy.TranscodeForbidden) {
		t.Fatalf("want POLICY_TRANSCODE_FORBIDDEN: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDecisionNoVariant(t *testing.T) {
	origin := fakeOrigin(t, false)
	defer origin.Close()
	e := &Engine{Origin: origin.URL, Store: NewMemoryStore(), LoadPolicy: func(context.Context, *string, *string) (policy.Policy, string, error) {
		return policy.Effective(policy.Policy{}, policy.Policy{MaxSourceHeight: intp(100)}, policy.Policy{}), "USER", nil
	}}
	req := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F999&session=s3", nil)
	req.Header.Set("X-Plex-Token", "user-tok")
	rec := httptest.NewRecorder()
	if !e.HandleDecision(rec, req, "req-3", "fp-1", "s3", "", "") {
		t.Fatal("must handle")
	}
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), policy.NoAllowedVariant) {
		t.Fatalf("want NO_ALLOWED_MEDIA_VARIANT: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDecisionPassthrough(t *testing.T) {
	e := &Engine{Store: NewMemoryStore()}
	// No rating key: not a negotiation the engine understands.
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?session=s", nil)
	if e.HandleDecision(httptest.NewRecorder(), req, "id", "fp", "s", "", "") {
		t.Fatal("must pass through without rating key")
	}
	// No token: cannot forward under user context.
	req2 := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F999", nil)
	if e.HandleDecision(httptest.NewRecorder(), req2, "id", "fp", "s", "", "") {
		t.Fatal("must pass through without token")
	}
}

// TestJodieBoundary is the acceptance rule: a direct request for the
// prohibited 4K part is substituted with the selected allowed part.
func TestJodieBoundary(t *testing.T) {
	e := &Engine{Store: NewMemoryStore()}
	ctx := context.Background()
	_, err := e.Store.Create(ctx, Session{
		PlexSessionID: "sess-j", RatingKey: "999", SelectedMediaIndex: 1,
		SelectedPartPlexID: "302", SelectedPartKey: "/library/parts/302/file.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/library/parts/301/file.mkv", nil)
	sub, deny, reason := e.EnforcePart(r, "301", "sess-j")
	if deny || sub != "/library/parts/302/file.mp4" || reason != "substituted" {
		t.Fatalf("4K part must substitute: %q %v %q", sub, deny, reason)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/library/parts/302/file.mp4", nil)
	if sub, deny, _ := e.EnforcePart(r2, "302", "sess-j"); deny || sub != "" {
		t.Fatal("selected part must pass untouched")
	}
	r3 := httptest.NewRequest(http.MethodGet, "/library/parts/301/file.mkv", nil)
	if sub, deny, _ := e.EnforcePart(r3, "301", "no-such-session"); deny || sub != "" {
		t.Fatal("absent session preserves Alpha allow")
	}
}

func TestPartIDFromPath(t *testing.T) {
	if got := PartIDFromPath("/library/parts/301/file.mkv"); got != "301" {
		t.Fatalf("got %q", got)
	}
	if got := PartIDFromPath("/library/sections"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEndSession(t *testing.T) {
	e := &Engine{Store: NewMemoryStore()}
	ctx := context.Background()
	_, _ = e.Store.Create(ctx, Session{PlexSessionID: "s-end", RatingKey: "1"})
	r := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/stop?session=s-end", nil)
	e.EndSession(r)
	if _, ok, _ := e.Store.FindActive(ctx, "s-end"); ok {
		t.Fatal("session must close on stop")
	}
}

func TestRewriteQuery(t *testing.T) {
	mk := func() url.Values {
		return url.Values{"mediaIndex": {"0"}, "maxVideoBitrate": {"3000"}, "session": {"a"}}
	}
	cap6000 := 6000
	// Policy cap above the client cap: client wins (minimum).
	if got := rewriteQuery(mk(), 1, &cap6000); got.Get("mediaIndex") != "1" || got.Get("maxVideoBitrate") != "3000" {
		t.Fatalf("client tighter cap must survive: %v", got)
	}
	// Policy cap below: policy wins.
	cap1000 := 1000
	if got := rewriteQuery(mk(), 1, &cap1000); got.Get("maxVideoBitrate") != "1000" {
		t.Fatalf("policy cap must apply: %v", got)
	}
	// No cap: index rewritten, bitrate untouched.
	if got := rewriteQuery(mk(), 2, nil); got.Get("mediaIndex") != "2" || got.Get("maxVideoBitrate") != "3000" {
		t.Fatalf("no-cap rewrite: %v", got)
	}
}

func TestManifestBoundary(t *testing.T) {
	newEngine := func() *Engine {
		e := &Engine{Store: NewMemoryStore()}
		_, _ = e.Store.Create(context.Background(), Session{
			PlexSessionID: "sess-m", RatingKey: "999", SelectedMediaIndex: 1,
			SelectedPartPlexID: "302", SelectedPartKey: "/library/parts/302/file.mp4",
			PlaybackMode:    "directPlay",
			EffectivePolicy: []byte(`{"allowTranscode":"deny"}`),
		})
		return e
	}
	// Explicit transcode manifest after a direct-play decision: deny.
	e := newEngine()
	r := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/start.mpd?session=sess-m&directPlay=0&directStream=0", nil)
	if _, deny, reason := e.EnforcePart(r, "", "sess-m"); !deny || reason != policy.TranscodeForbidden {
		t.Fatalf("transcode retry must fail closed: %v %q", deny, reason)
	}
	// Direct-stream manifest on the same session: legitimate upgrade path.
	e2 := newEngine()
	r2 := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/start.mpd?session=sess-m&directPlay=0&directStream=1", nil)
	if _, deny, _ := e2.EnforcePart(r2, "", "sess-m"); deny {
		t.Fatal("direct-stream manifest must pass")
	}
	// No session: Alpha allow-through.
	e3 := &Engine{Store: NewMemoryStore()}
	r3 := httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/start.mpd?session=ghost&directPlay=0&directStream=0", nil)
	if _, deny, _ := e3.EnforcePart(r3, "", "ghost"); deny {
		t.Fatal("absent session must allow")
	}
}
