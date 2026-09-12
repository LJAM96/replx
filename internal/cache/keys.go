// Package cache implements Valkey user scoped response cache keys.
//
// Production 1.0 defaults to user scoped caching. Raw Plex tokens never
// appear in keys. The owner synchronized library index is shared internal
// data and must never be returned directly as a user response.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Key builds replx_edge:{schema}:{server}:{class}:{scope}:{representation}:{hash}.
// params must already exclude X-Plex-Token and other secrets.
func Key(schemaVersion, serverID, class, scope, representation string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
		b.WriteString("&")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("replx_edge:%s:%s:%s:%s:%s:%s",
		schemaVersion, serverID, class, scope, representation, hex.EncodeToString(sum[:])[:16])
}

// UserScope returns user:{identity_uuid}.
func UserScope(identityID string) string {
	return "user:" + identityID
}
