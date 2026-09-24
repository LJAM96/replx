package cache

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// CollectionWindowTTL keeps a successfully fetched user-scoped collection
// window available while the warmer tries to refresh it. Authorization is
// checked again before the proxy serves it.
const CollectionWindowTTL = 2 * time.Hour

// CollectionWindowKeyGen shares one cached collection response across its
// pagination requests while retaining content-shaping fields, the user
// scope, representation and invalidation generations.
func CollectionWindowKeyGen(scope, class, method, path string, query url.Values, accept string, scopeGen, globalGen uint64) string {
	return ResponseKeyGen(scope, class, method, path, CollectionWindowQuery(query), accept, scopeGen, globalGen) + ":window"
}

// CollectionWindowQuery removes page coordinates and two Plex Web context
// hints that do not select collection children. Both vary with the browser
// window or pinned sidebar, even while the collection itself stays the same.
func CollectionWindowQuery(query url.Values) url.Values {
	q := url.Values{}
	for k, values := range query {
		lower := strings.ToLower(k)
		if lower == "x-plex-container-start" || lower == "x-plex-container-size" ||
			lower == "x-plex-device-screen-resolution" || lower == "pinnedcontentdirectoryid" {
			continue
		}
		q[k] = append([]string(nil), values...)
	}
	return q
}

// CollectionWindowPage slices a JSON collection response fetched from offset
// zero into the exact range Plex Web requested. Incomplete windows are never
// used for ranges they do not contain.
func CollectionWindowPage(body []byte, start, size int) ([]byte, bool) {
	if start < 0 || size <= 0 || size > 500 || start > 1000000 {
		return nil, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, false
	}
	var container map[string]json.RawMessage
	if json.Unmarshal(root["MediaContainer"], &container) != nil {
		return nil, false
	}
	var offset, pageSize, total int
	if json.Unmarshal(container["offset"], &offset) != nil || offset != 0 ||
		json.Unmarshal(container["size"], &pageSize) != nil ||
		json.Unmarshal(container["totalSize"], &total) != nil || total < 0 {
		return nil, false
	}
	var items []json.RawMessage
	if raw := container["Metadata"]; len(raw) > 0 && json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	if pageSize != len(items) || total < len(items) || start > total {
		return nil, false
	}
	end := start + size
	if end > total {
		end = total
	}
	if end > len(items) {
		return nil, false
	}
	part := items[start:end]
	if part == nil {
		part = []json.RawMessage{}
	}
	container["offset"], _ = json.Marshal(start)
	container["size"], _ = json.Marshal(len(part))
	container["Metadata"], _ = json.Marshal(part)
	root["MediaContainer"], _ = json.Marshal(container)
	out, err := json.Marshal(root)
	return out, err == nil
}
