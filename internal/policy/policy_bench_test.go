package policy

import "testing"

func benchVariants(n int) []Variant {
	out := make([]Variant, n)
	for i := range out {
		w, h := 1920, 1080
		if i%3 == 0 {
			w, h = 3840, 2160
		}
		dr := "SDR"
		if i%4 == 0 {
			dr = "HDR10"
		}
		out[i] = Variant{MediaIndex: i, Width: intp(w), Height: intp(h),
			BitrateKbps: intp(8000 + i*500), DynamicRange: dr,
			VideoCodec: "h264", Playability: Playability(i % 4), PartAvailable: true}
	}
	return out
}

func BenchmarkEvaluate(b *testing.B) {
	p := Effective(Policy{}, Policy{MaxSourceHeight: intp(1080)}, Policy{})
	variants := benchVariants(8)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Evaluate(p, "USER", variants); err != nil {
			b.Fatal(err)
		}
	}
}
