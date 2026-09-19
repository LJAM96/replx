// Cache policy, entry codec and Store implementations for the Zeta
// user-scoped browse cache.
//
// Keys never contain raw tokens: the scope is the HMAC user fingerprint and
// secret query params are stripped before hashing. Watch state
// (timeline), negotiation (transcode decisions), media bytes and artwork
// transcodes are never cacheable: only idempotent browse GETs with short
// TTLs. Invalidation is TTL plus the Gamma PMS event consumer (later);
// short TTLs keep operator surprise bounded until then.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/valkey"
)

// SchemaVersion prefixes every key so a codec/policy change can roll keys.
const SchemaVersion = "v1"

// MaxEntryBytes caps a cacheable body. Larger origins stream uncached;
// the client still receives full bytes, only caching is skipped.
const MaxEntryBytes = 2 << 20 // 2 MiB

// secretParams are stripped from keys (case-insensitive).
var secretParams = map[string]bool{
	"x-plex-token": true, "token": true, "authtoken": true,
}

// ttlByPrefix maps cacheable path prefixes to TTLs. Lookup uses longest
// prefix match so /library/sections (list, 5m) and /library/sections/
// (browse pages, 60s) coexist.
//
// Hub TTLs exceed the spec table minimums (home 10s, CW 5s, RA 15s) on
// purpose: the owner warmer renews hot entries at TTL/2, which bounds
// staleness without churning the origin on every poll. There is no stale
// serving: expiry is a hard miss.
var ttlByPrefix = []struct {
	prefix string
	ttl    time.Duration
}{
	{"/library/sections/", 60 * time.Second},
	{"/library/collections/", 2 * time.Minute},
	{"/library/metadata/", 5 * time.Minute},
	{"/library/sections", 5 * time.Minute},
	{"/identity", 5 * time.Minute},
	{"/hubs/", 30 * time.Second},
}

// hubTTL refines /hubs/ by feed: Continue Watching and Recently Added
// change faster than structural hubs.
func hubTTL(path string) (time.Duration, bool) {
	p := strings.ToLower(path)
	if !strings.HasPrefix(p, "/hubs/") {
		return 0, false
	}
	if strings.Contains(p, "continuewatching") {
		return 15 * time.Second, true
	}
	if strings.Contains(p, "recentlyadded") {
		return 30 * time.Second, true
	}
	return 30 * time.Second, true
}

// Cacheable reports whether a method+path pair is safe to cache and its
// TTL. Only idempotent browse GETs qualify; timeline, decisions, media,
// artwork and streams never do (they are absent from the prefix table).
func Cacheable(method, path string) (time.Duration, bool) {
	if method != "GET" && method != "HEAD" {
		return 0, false
	}
	p := strings.ToLower(path)
	if ttl, ok := hubTTL(p); ok {
		return ttl, true
	}
	best := -1
	for i, e := range ttlByPrefix {
		if e.prefix == "/hubs/" {
			continue // handled above with feed granularity
		}
		if strings.HasPrefix(p, e.prefix) && (best < 0 || len(e.prefix) > len(ttlByPrefix[best].prefix)) {
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return ttlByPrefix[best].ttl, true
}

// ResponseKey builds replx_edge:v1:browse:user:<fingerprint>:<method>:<hash>.
// query must be the request's parsed query; secret params are stripped and
// the remainder is sorted, so param order and token rotation never split
// cache entries across the same user.
func ResponseKey(userFingerprint, method, path string, query url.Values) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(0)
	b.WriteString(path)
	b.WriteByte(0)
	keys := make([]string, 0, len(query))
	for k := range query {
		if secretParams[strings.ToLower(k)] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string(nil), query[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(v)
			b.WriteByte('&')
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("replx_edge:%s:browse:user:%s:%s:%s",
		SchemaVersion, userFingerprint, strings.ToUpper(method), hex.EncodeToString(sum[:])[:16])
}

// safeHeaders is the deliberate allowlist of origin response headers
// preserved across a cache hit. Keys are Go-canonicalized ("ETag" arrives
// as "Etag"): HTTP semantics are case-insensitive, so this is cosmetic.
// Everything authentication-related, hop-by-hop, or Replx-managed is
// dropped: Set-Cookie, Authorization, Cookie, X-Plex-Token,
// Content-Length (recomputed) and X-Replx-* never persist, and
// Cache-Control stays ours (TTLs govern, not origin hints).
var safeHeaders = map[string]bool{
	"Etag": true, "Last-Modified": true, "Content-Language": true,
	"Content-Encoding": true,
}

// SafeHeaders extracts the allowlisted subset of an origin header set.
func SafeHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vv := range h {
		canonical := http.CanonicalHeaderKey(k)
		if !safeHeaders[canonical] || len(vv) == 0 {
			continue
		}
		out[canonical] = vv[0]
	}
	return out
}

// Entry is one cached origin response.
type Entry struct {
	Status      int
	ContentType string
	Headers     map[string]string
	Body        []byte
}

// Marshal encodes e as version|status|ctypeLen|ctype|headers|body.
// Version 0x02 carries the header allowlist; 0x01 legacy entries remain
// decodable by Unmarshal for rolling-deploy safety.
func (e Entry) Marshal() ([]byte, error) {
	if len(e.Body) > MaxEntryBytes {
		return nil, fmt.Errorf("cache: body %d exceeds %d", len(e.Body), MaxEntryBytes)
	}
	var hb strings.Builder
	keys := make([]string, 0, len(e.Headers))
	for k := range e.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		hb.WriteString(k)
		hb.WriteByte(0)
		hb.WriteString(e.Headers[k])
		hb.WriteByte(0)
	}
	hraw := hb.String()
	out := make([]byte, 0, 13+len(e.ContentType)+len(hraw)+len(e.Body))
	out = append(out, 0x02)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(e.Status))
	out = append(out, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:], uint32(len(e.ContentType)))
	out = append(out, tmp[:]...)
	out = append(out, e.ContentType...)
	binary.BigEndian.PutUint32(tmp[:], uint32(len(hraw)))
	out = append(out, tmp[:]...)
	out = append(out, hraw...)
	out = append(out, e.Body...)
	return out, nil
}

// Unmarshal decodes Marshal output, accepting legacy 0x01 entries
// (status|ctype|body, no headers).
func Unmarshal(b []byte) (Entry, error) {
	var e Entry
	if len(b) < 1 {
		return e, fmt.Errorf("cache: empty entry")
	}
	switch b[0] {
	case 0x01:
		return unmarshalV1(b)
	case 0x02:
		return unmarshalV2(b)
	default:
		return e, fmt.Errorf("cache: bad version")
	}
}

func unmarshalV1(b []byte) (Entry, error) {
	var e Entry
	if len(b) < 9 {
		return e, fmt.Errorf("cache: truncated v1 entry")
	}
	e.Status = int(binary.BigEndian.Uint32(b[1:5]))
	clen := int(binary.BigEndian.Uint32(b[5:9]))
	if len(b) < 9+clen {
		return e, fmt.Errorf("cache: truncated v1 content type")
	}
	e.ContentType = string(b[9 : 9+clen])
	e.Body = b[9+clen:]
	return e, nil
}

func unmarshalV2(b []byte) (Entry, error) {
	var e Entry
	if len(b) < 13 {
		return e, fmt.Errorf("cache: truncated v2 entry")
	}
	e.Status = int(binary.BigEndian.Uint32(b[1:5]))
	clen := int(binary.BigEndian.Uint32(b[5:9]))
	if len(b) < 13+clen {
		return e, fmt.Errorf("cache: truncated v2 content type")
	}
	e.ContentType = string(b[9 : 9+clen])
	rest := b[9+clen:]
	hlen := int(binary.BigEndian.Uint32(rest[:4]))
	if len(rest) < 4+hlen {
		return e, fmt.Errorf("cache: truncated v2 headers")
	}
	// Pairs are k\x00v\x00 encoded; re-filter on read so entries
	// written by older allowlists cannot smuggle new headers in.
	e.Headers = map[string]string{}
	parts := strings.Split(string(rest[4:4+hlen]), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if safeHeaders[parts[i]] {
			e.Headers[parts[i]] = parts[i+1]
		}
	}
	e.Body = rest[4+hlen:]
	return e, nil
}

// Store persists entries. Implementations fail open: callers fall through
// to the origin on any error.
type Store interface {
	Get(ctx context.Context, key string) (Entry, bool, error)
	Set(ctx context.Context, key string, e Entry, ttl time.Duration) error
}

// Memory is an in-process Store for tests and degraded fallback.
type Memory struct {
	mu    sync.Mutex
	items map[string]memItem
	now   func() time.Time
}

type memItem struct {
	entry Entry
	exp   time.Time
}

// NewMemory returns an empty store using wall-clock time.
func NewMemory() *Memory {
	return &Memory{items: map[string]memItem{}, now: time.Now}
}

func (m *Memory) Get(_ context.Context, key string) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok || m.now().After(it.exp) {
		delete(m.items, key)
		return Entry{}, false, nil
	}
	return it.entry, true, nil
}

func (m *Memory) Set(_ context.Context, key string, e Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cache: non-positive TTL")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = memItem{entry: e, exp: m.now().Add(ttl)}
	return nil
}

// ValkeyStore is the production Store backed by a RESP client.
type ValkeyStore struct {
	c *valkey.Client
}

// NewValkeyStore wraps c. A nil client disables caching (all ops miss).
func NewValkeyStore(c *valkey.Client) *ValkeyStore { return &ValkeyStore{c: c} }

func (s *ValkeyStore) Get(ctx context.Context, key string) (Entry, bool, error) {
	if s == nil || s.c == nil {
		return Entry{}, false, nil
	}
	_ = ctx
	raw, ok, err := s.c.Get(key)
	if err != nil || !ok {
		return Entry{}, false, err
	}
	e, err := Unmarshal(raw)
	if err != nil {
		return Entry{}, false, nil // corrupt entry reads as miss
	}
	return e, true, nil
}

func (s *ValkeyStore) Set(ctx context.Context, key string, e Entry, ttl time.Duration) error {
	if s == nil || s.c == nil {
		return nil
	}
	_ = ctx
	raw, err := e.Marshal()
	if err != nil {
		return err
	}
	return s.c.Set(key, raw, ttl)
}
