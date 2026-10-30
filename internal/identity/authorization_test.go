package identity

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestResourceGrantsRequireExactCallerAndResource(t *testing.T) {
	g := NewResourceGrants()
	g.Grant("caller-a", "v3:server-a:acct-1:resource:json:headers-a")
	for _, test := range []struct {
		fingerprint string
		resource    string
		want        bool
	}{
		{"caller-a", "v3:server-a:acct-1:resource:json:headers-a", true},
		{"caller-b", "v3:server-a:acct-1:resource:json:headers-a", false},
		{"caller-a", "v3:server-b:acct-1:resource:json:headers-a", false},
		{"caller-a", "v3:server-a:acct-1:other-resource:json:headers-a", false},
		{"caller-a", "v3:server-a:acct-1:resource:xml:headers-a", false},
		{"caller-a", "v3:server-a:acct-1:resource:json:headers-b", false},
		{"", "v3:server-a:acct-1:resource:json:headers-a", false},
		{"caller-a", "", false},
	} {
		if got := g.Authorized(test.fingerprint, test.resource); got != test.want {
			t.Errorf("Authorized(%q, %q) = %v, want %v", test.fingerprint, test.resource, got, test.want)
		}
	}
}

func TestResourceGrantExpiresWithoutSlidingOnCacheHits(t *testing.T) {
	now := time.Now()
	g := NewResourceGrants()
	g.now = func() time.Time { return now }
	g.Grant("caller", "resource")
	for i := 0; i < 29; i++ {
		now = now.Add(time.Second)
		if !g.Authorized("caller", "resource") {
			t.Fatal("lease expired early")
		}
	}
	now = now.Add(time.Second)
	if g.Authorized("caller", "resource") {
		t.Fatal("lease must expire at exactly 30 seconds, despite cache hits")
	}
	if len(g.grants) != 0 {
		t.Fatal("expired lease was retained")
	}
	g.Grant("caller", "resource") // a new completed caller PMS response
	if !g.Authorized("caller", "resource") {
		t.Fatal("fresh origin proof did not establish a new lease")
	}
}

func TestResourceGrantRevocationIsCredentialSpecific(t *testing.T) {
	g := NewResourceGrants()
	g.Grant("caller-a", "resource-1")
	g.Grant("caller-a", "resource-2")
	g.Grant("caller-b", "resource-1")
	g.Revoke("caller-a")
	if g.Authorized("caller-a", "resource-1") || g.Authorized("caller-a", "resource-2") {
		t.Fatal("revoked caller retained a lease")
	}
	if !g.Authorized("caller-b", "resource-1") {
		t.Fatal("revocation affected another credential")
	}
}

func TestResourceGrantsAreBoundedAndEvictOldest(t *testing.T) {
	now := time.Now()
	g := NewResourceGrants()
	g.now = func() time.Time { return now }
	for i := 0; i <= resourceGrantMax; i++ {
		g.Grant("caller", fmt.Sprint(i))
		now = now.Add(time.Microsecond)
	}
	if len(g.grants) != resourceGrantMax {
		t.Fatalf("retained %d grants, want %d", len(g.grants), resourceGrantMax)
	}
	if g.Authorized("caller", "0") || !g.Authorized("caller", fmt.Sprint(resourceGrantMax)) {
		t.Fatal("capacity eviction must prefer the oldest proof")
	}
	now = now.Add(ResourceAuthorizationTTL)
	g.Grant("caller", "new")
	if len(g.grants) != 1 || !g.Authorized("caller", "new") {
		t.Fatal("expired grants did not release capacity")
	}
}

func TestResourceGrantsMissingStateFailsClosed(t *testing.T) {
	var absent *ResourceGrants
	var zero ResourceGrants
	for _, g := range []*ResourceGrants{absent, &zero, NewResourceGrants()} {
		g.Grant("", "resource")
		g.Grant("caller", "")
		g.Revoke("")
		if g.Authorized("caller", "resource") {
			t.Fatal("missing resource proof must fail closed")
		}
	}
	zero.Grant("caller", "resource")
	if !zero.Authorized("caller", "resource") {
		t.Fatal("zero-value grants should accept explicit origin proof")
	}
}

func TestResourceGrantsConcurrentUse(t *testing.T) {
	g := NewResourceGrants()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fingerprint := fmt.Sprint(i)
			for j := 0; j < 100; j++ {
				g.Grant(fingerprint, "resource")
				if !g.Authorized(fingerprint, "resource") {
					t.Error("independent caller lost its fresh lease")
				}
				g.Revoke(fingerprint)
			}
		}(i)
	}
	wg.Wait()
}
