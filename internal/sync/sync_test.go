package sync

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNormalizeDynamicRange(t *testing.T) {
	cases := map[string][4]string{
		// name: {dynamicRange, hdrFormat, videoCodec, videoProfile} -> want checked below
		"dovi":        {"", "", "hevc", "main 10 dovi"},
		"dvhe":        {"", "", "hevc", "dvhe.05.06"},
		"explicit-dv": {"Dolby Vision", "", "hevc", "main 10"},
		"hdr10plus":   {"HDR10+", "", "hevc", "main 10"},
		"hdr10-plus":  {"HDR10 Plus", "", "hevc", "main 10"},
		"hdr10":       {"HDR10", "", "hevc", "main 10"},
		"pq":          {"PQ", "", "hevc", "main 10"},
		"hlg":         {"HLG", "", "h264", "high"},
		"bare-hdr":    {"HDR", "", "hevc", "main 10"},
		"sdr":         {"SDR", "", "h264", "high"},
		"empty":       {"", "", "h264", "high"},
		"mp4-only":    {"", "", "h264", ""},
	}
	want := map[string]string{
		"dovi": DRDolbyVision, "dvhe": DRDolbyVision, "explicit-dv": DRDolbyVision,
		"hdr10plus": DRHDR10Plus, "hdr10-plus": DRHDR10Plus,
		"hdr10": DRHDR10, "pq": DRHDR10, "hlg": DRHLG, "bare-hdr": DRHDROther,
		"sdr": DRSDR, "empty": DRUnknown, "mp4-only": DRUnknown,
	}
	for name, in := range cases {
		if got := NormalizeDynamicRange(in[0], in[1], in[2], in[3]); got != want[name] {
			t.Errorf("%s: want %s, got %s", name, want[name], got)
		}
	}
	// Dolby Vision beats HDR10 when both markers appear (DV uses PQ).
	if got := NormalizeDynamicRange("HDR10", "dovi", "hevc", "main 10"); got != DRDolbyVision {
		t.Errorf("dv-over-hdr10: want DOLBY_VISION, got %s", got)
	}
	// UNKNOWN is never auto-classified as HDR.
	if got := NormalizeDynamicRange("", "", "av1", "main"); got == DRHDR10 || got == DRHDROther {
		t.Errorf("av1 must not imply HDR, got %s", got)
	}
}

func TestParseItemsPage(t *testing.T) {
	raw := `{"MediaContainer":{"size":2,"Metadata":[
		{"ratingKey":"1001","key":"/library/metadata/1001","type":"movie","title":"Fixture Film","titleSort":"Fixture Film","year":2024,
		 "duration":5400000,"thumb":"/t/1","art":"/a/1","addedAt":1700000000,"updatedAt":1700000100,
		 "Guid":[{"id":"imdb://tt1234567"},{"id":"tmdb://98765"}],
		 "Media":[
			{"id":11,"container":"mkv","videoCodec":"hevc","videoProfile":"main 10","width":3840,"height":2160,"bitrate":61000,
			 "videoResolution":"2160","dynamicRange":"HDR","audioCodec":"truehd","audioChannels":8,"duration":5400000,
			 "Part":[{"id":101,"key":"/library/parts/101/file.mkv","container":"mkv","size":9000000,"duration":5400000,
				"Stream":[{"id":1001,"streamType":1,"codec":"hevc","selected":true},{"id":1002,"streamType":2,"codec":"truehd","channels":8}]}]},
			{"id":12,"container":"mp4","videoCodec":"h264","width":1920,"height":1080,"bitrate":14000,
			 "videoResolution":"1080","dynamicRange":"SDR","audioCodec":"aac","audioChannels":2,
			 "Part":[{"id":102,"key":"/library/parts/102/file.mp4"}]}
		 ]},
		{"ratingKey":"1002","type":"movie","title":"Bare Entry"}
	]}}`
	items, total, err := parseItemsPage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	m := items[0]
	if m.Title != "Fixture Film" || m.Year == nil || *m.Year != 2024 {
		t.Fatalf("item fields: %+v", m)
	}
	if len(m.Guid) != 2 || len(m.Media) != 2 {
		t.Fatalf("guids=%d media=%d", len(m.Guid), len(m.Media))
	}
	v0 := m.Media[0]
	if v0.VideoCodec != "hevc" || len(v0.Part) != 1 || len(v0.Part[0].Stream) != 2 {
		t.Fatalf("variant: %+v", v0)
	}
	if NormalizeDynamicRange(v0.DynamicRange, "", v0.VideoCodec, v0.VideoProfile) != DRHDROther {
		t.Fatal("bare HDR with no other signal must normalize to HDR_OTHER, never assumed HDR10")
	}
	if _, _, err := parseItemsPage([]byte(`{broken`)); err == nil {
		t.Fatal("want parse error for garbage")
	}
	// totalSize is authoritative; size counts only this response.
	totalRaw := `{"MediaContainer":{"size":1,"totalSize":84833,"offset":0,"Metadata":[]}}`
	if _, total, err := parseItemsPage([]byte(totalRaw)); err != nil || total != 84833 {
		t.Fatalf("totalSize must win: total=%d err=%v", total, err)
	}
	legacyRaw := `{"MediaContainer":{"size":2,"Metadata":[]}}`
	if _, total, err := parseItemsPage([]byte(legacyRaw)); err != nil || total != 2 {
		t.Fatalf("legacy size fallback: total=%d err=%v", total, err)
	}
}

func TestSplitGUID(t *testing.T) {
	p, id := splitGUID("imdb://tt1234567")
	if p != "imdb" || id != "tt1234567" {
		t.Fatalf("got %q %q", p, id)
	}
	if p, id := splitGUID("bare"); p != "" || id != "bare" {
		t.Fatalf("bare: got %q %q", p, id)
	}
}

func TestHandleEventData(t *testing.T) {
	w := &Worker{dirty: map[string]bool{}}
	w.handleEventData(`{"NotificationContainer":{"type":"activity","Activity":{"type":"library.refresh.items","subtype":"library.refresh.items","progress":100,"Context":{"key":"/library/sections/22/refresh"}}}}`)
	if !w.isDirty("22") {
		t.Fatal("completed refresh must dirty the section")
	}
	w2 := &Worker{dirty: map[string]bool{}}
	w2.handleEventData(`{"NotificationContainer":{"type":"activity","Activity":{"type":"library.refresh.items","subtype":"library.refresh.items","progress":42,"Context":{"key":"/library/sections/22/refresh"}}}}`)
	if w2.isDirty("22") {
		t.Fatal("incomplete progress must not dirty")
	}
	w3 := &Worker{dirty: map[string]bool{}}
	w3.handleEventData(`{"NotificationContainer":{"type":"timeline","TimelineEntry":[]}}`)
	if len(w3.dirty) != 0 {
		t.Fatal("timeline events are TTL-bound, never sync-dirtying")
	}
	w4 := &Worker{dirty: map[string]bool{}}
	w4.handleEventData(`not json`)
	if len(w4.dirty) != 0 {
		t.Fatal("garbage must be ignored")
	}
}

func TestSyncOnceSingleFlight(t *testing.T) {
	w := &Worker{
		// OwnerToken blocks until the context ends: the first pass holds
		// the flight while the second must observe ErrInFlight.
		OwnerToken: func(ctx context.Context) (string, bool) {
			<-ctx.Done()
			return "", false
		},
		dirty: map[string]bool{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = w.SyncOnce(ctx, false) }()
	time.Sleep(50 * time.Millisecond)
	if err := w.SyncOnce(context.Background(), false); err != ErrInFlight {
		t.Fatalf("want ErrInFlight, got %v", err)
	}
	cancel()
	wg.Wait()
}
