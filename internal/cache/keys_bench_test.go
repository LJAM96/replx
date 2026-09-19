package cache

import (
	"net/url"
	"testing"
)

func BenchmarkResponseKey(b *testing.B) {
	q, _ := url.ParseQuery("type=2&contentDirectoryID=22&includeMeta=1&X-Plex-Token=secret&b=1")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ResponseKey("acct:23007893", "GET", "/hubs/home/recentlyAdded", q)
	}
}
