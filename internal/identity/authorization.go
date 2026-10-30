package identity

import (
	"crypto/sha256"
	"sync"
	"time"
)

// Authorization checks whether the caller recently received this exact
// resource from PMS. Identity resolution and plex.tv credential freshness
// do not satisfy this contract.
type Authorization interface {
	Authorized(fingerprint, resourceKey string) bool
}

// ResourceAuthorizationTTL bounds permission staleness when reusing a PMS
// response. Cache hits never extend this interval. Revocation at PMS can
// therefore take up to 30 seconds to stop local reuse.
const ResourceAuthorizationTTL = 30 * time.Second

const resourceGrantMax = 4096

type resourceGrantKey struct {
	fingerprint string
	resource    [sha256.Size]byte
}

// ResourceGrants holds bounded, process-local authorization leases. No raw
// user token, response body, or authorization snapshot is persisted. A
// restart requires fresh caller-authorized PMS responses.
//
// resourceKey must identify the exact server, user response scope, path,
// query, representation and request variations, such as the full versioned
// response-cache key. A section ID, account ID, blob hash or URL without its
// variations is insufficient. Fingerprints additionally isolate credentials
// belonging to the same account.
//
// A lease permits reuse of that exact response within the bounded stale
// interval; it is not a library ACL or proof that an arbitrary indexed item
// is visible to a managed/restricted user. Never use it to authorize another
// response, a refreshed owner-token response, or an unrelated reference to
// a shared artwork blob. A blob's existence never supplies a lease.
type ResourceGrants struct {
	mu     sync.Mutex
	grants map[resourceGrantKey]time.Time
	now    func() time.Time
}

func NewResourceGrants() *ResourceGrants {
	return &ResourceGrants{grants: make(map[resourceGrantKey]time.Time), now: time.Now}
}

// Grant records proof only after a complete successful PMS response fetched
// under this caller's token. Callers must not grant after cache hits, failed
// or partial responses, owner-token requests, or identity lookup. Unknown
// response variation or visibility must bypass local reuse instead.
func (g *ResourceGrants) Grant(fingerprint, resourceKey string) {
	if g == nil || fingerprint == "" || resourceKey == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.grants == nil {
		g.grants = make(map[resourceGrantKey]time.Time)
	}
	key := grantKey(fingerprint, resourceKey)
	if _, exists := g.grants[key]; !exists && len(g.grants) >= resourceGrantMax {
		g.evict(now)
	}
	g.grants[key] = now.Add(ResourceAuthorizationTTL)
}

// Authorized fails closed for unknown, expired or empty inputs. Checking a
// lease never refreshes it; only a new successful caller PMS response can.
func (g *ResourceGrants) Authorized(fingerprint, resourceKey string) bool {
	if g == nil || fingerprint == "" || resourceKey == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := grantKey(fingerprint, resourceKey)
	expires, exists := g.grants[key]
	if !exists {
		return false
	}
	if !g.clock().Before(expires) {
		delete(g.grants, key)
		return false
	}
	return true
}

// Revoke removes all leases for a definitively rejected caller credential.
// Future successful PMS responses may establish fresh proof.
func (g *ResourceGrants) Revoke(fingerprint string) {
	if g == nil || fingerprint == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range g.grants {
		if key.fingerprint == fingerprint {
			delete(g.grants, key)
		}
	}
}

func grantKey(fingerprint, resourceKey string) resourceGrantKey {
	return resourceGrantKey{fingerprint: fingerprint, resource: sha256.Sum256([]byte(resourceKey))}
}

func (g *ResourceGrants) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// evict first removes expired leases, then the soonest-expiring live lease.
// Every eviction is a safe miss; the memory limit never extends a grant.
func (g *ResourceGrants) evict(now time.Time) {
	var oldest resourceGrantKey
	var oldestExpiry time.Time
	for key, expiry := range g.grants {
		if !now.Before(expiry) {
			delete(g.grants, key)
			continue
		}
		if oldestExpiry.IsZero() || expiry.Before(oldestExpiry) {
			oldest, oldestExpiry = key, expiry
		}
	}
	if len(g.grants) >= resourceGrantMax {
		delete(g.grants, oldest)
	}
}
