package sync

import "testing"

func BenchmarkNormalizeDynamicRange(b *testing.B) {
	for i := 0; i < b.N; i++ {
		NormalizeDynamicRange("HDR10", "", "hevc", "main 10")
		NormalizeDynamicRange("", "", "hevc", "main 10 dvhe.05.06")
		NormalizeDynamicRange("SDR", "", "h264", "high")
		NormalizeDynamicRange("", "", "av1", "main")
	}
}
