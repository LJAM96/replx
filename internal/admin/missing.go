package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/pms"
)

// registerMissing wires Production 1.0 admin endpoints absent from the Alpha
// mux: status, server, users, devices, library, cache invalidate, storage,
// sessions alias, playback detail, diagnostics traces, logs, settings and
// first-run setup. All collections use cursor pagination {data,meta:{nextCursor}}.
func (m *Mux) registerMissing(checks health.Checks) {
	m.checks = checks
	m.mux.HandleFunc("/api/v1/status", m.auth(m.handleOverallStatus))
	m.mux.HandleFunc("/api/v1/server", m.auth(m.handleServer))
	m.mux.HandleFunc("/api/v1/server/test", m.auth(m.handleServerTest))
	m.mux.HandleFunc("/api/v1/server/sync", m.auth(m.handleServerSync))
	m.mux.HandleFunc("/api/v1/users", m.auth(m.handleUsers))
	m.mux.HandleFunc("/api/v1/users/", m.auth(m.handleUserDetail))
	m.mux.HandleFunc("/api/v1/devices", m.auth(m.handleDevices))
	m.mux.HandleFunc("/api/v1/devices/", m.auth(m.handleDeviceDetail))
	m.mux.HandleFunc("/api/v1/library/items", m.auth(m.handleLibraryItems))
	m.mux.HandleFunc("/api/v1/library/items/", m.auth(m.handleLibraryItem))
	m.mux.HandleFunc("/api/v1/cache", m.auth(m.handleCacheStats))
	m.mux.HandleFunc("/api/v1/cache/invalidate", m.auth(m.handleCacheInvalidate))
	m.mux.HandleFunc("/api/v1/storage", m.auth(m.handleStorage))
	m.mux.HandleFunc("/api/v1/sessions", m.auth(m.handleSessionsAlias))
	m.mux.HandleFunc("/api/v1/playback/", m.auth(m.handlePlaybackDetail))
	m.mux.HandleFunc("/api/v1/diagnostics/traces", m.auth(m.handleTraces))
	m.mux.HandleFunc("/api/v1/diagnostics/traces/", m.auth(m.handleTraceDetail))
	m.mux.HandleFunc("/api/v1/logs", m.auth(m.handleLogs))
	m.mux.HandleFunc("/api/v1/settings", m.auth(m.handleSettings))
	m.mux.HandleFunc("/api/v1/setup", m.handleSetup)
}

// --- pagination ---

func parseCursor(r *http.Request, def, max int) (limit, offset int) {
	limit = parseLimit(r, def, max)
	offset = 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(c); err == nil {
			if n, err := strconv.Atoi(string(raw)); err == nil && n >= 0 {
				offset = n
			}
		}
	}
	return limit, offset
}

func encodeCursor(offset int) *string {
	if offset <= 0 {
		return nil
	}
	s := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
	return &s
}

func writePage(w http.ResponseWriter, data any, nextOffset *int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	var next *string
	if nextOffset != nil {
		next = encodeCursor(*nextOffset)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"nextCursor": next}})
}

// --- status / server ---

func (m *Mux) handleOverallStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	out := map[string]any{"ok": true}
	if m.svc != nil {
		out["onboarding"] = m.svc.Status(r.Context())
	}
	writeData(w, http.StatusOK, out)
}

func (m *Mux) handleServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SERVER_DISABLED", "no database wired")
		return
	}
	var id, name, internalURL, mediaURL, machine, friendly, version, status, lastErr string
	_ = id
	err := m.svc.DB.QueryRow(r.Context(), `SELECT id::text, name, internal_origin_url,
		COALESCE(client_media_origin_url,''), machine_identifier, COALESCE(friendly_name,''),
		COALESCE(plex_version,''), connection_status, COALESCE(last_error,'')
		FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(
		&id, &name, &internalURL, &mediaURL, &machine, &friendly, &version, &status, &lastErr)
	if err != nil {
		writeData(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"configured": true, "name": name, "machineIdentifier": machine,
		"friendlyName": friendly, "plexVersion": version, "connectionStatus": status,
		"lastError": lastErr, "mediaOriginConfigured": mediaURL != "",
	})
}

func (m *Mux) handleServerTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if !m.checkRate("server-test:"+subjectOf(r), time.Minute, 5) {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "POST /server/test at most 5 per minute")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SERVER_DISABLED", "no database wired")
		return
	}
	var internalURL, machine string
	if err := m.svc.DB.QueryRow(r.Context(), `SELECT internal_origin_url, machine_identifier FROM plex_servers
		WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&internalURL, &machine); err != nil {
		writeError(w, http.StatusConflict, "NO_SERVER", "no enabled origin")
		return
	}
	status := pms.Check(internalURL)
	id, err := pms.FetchIdentity(internalURL, "")
	match := err == nil && id.MachineIdentifier != "" && id.MachineIdentifier == machine
	out := map[string]any{"reachable": status != "down" && status != "unknown", "pmsStatus": status, "machineMatch": match}
	if err != nil {
		out["identityError"] = err.Error()
	} else {
		out["originMachine"] = id.MachineIdentifier
	}
	m.auditEvent(r.Context(), subjectOf(r), "server.test", "server", "origin", out)
	writeData(w, http.StatusOK, out)
}

func (m *Mux) handleServerSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	// Alias to single-flight full sync with job-ID semantics.
	m.handleSyncFull(w, r)
}

// --- users / devices ---

func (m *Mux) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "USERS_DISABLED", "no database wired")
		return
	}
	// The local identity table is populated as users visit Replx. Refresh
	// Plex's sharing list so people with library access appear before they
	// visit; retain the last good table if plex.tv is temporarily unavailable.
	m.usersMu.Lock()
	if time.Since(m.lastUsersSync) >= 5*time.Minute {
		m.lastUsersSync = time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		_ = m.svc.SyncSharedUsers(ctx)
		cancel()
	}
	m.usersMu.Unlock()
	limit, offset := parseCursor(r, 50, 200)
	rows, err := m.svc.DB.Query(r.Context(), `SELECT i.id::text, COALESCE(i.plex_account_id,0), COALESCE(i.username,''),
		COALESCE(i.friendly_name,''), i.identity_type, COALESCE(i.restricted,false),
		COALESCE(i.plex_account_id=c.plex_account_id,false)
		FROM plex_identities i JOIN plex_servers s ON s.id=i.server_id
		LEFT JOIN plex_owner_credentials c ON c.server_id=s.id
		WHERE s.enabled ORDER BY i.created_at DESC LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, "USERS_FAILED", err.Error())
		return
	}
	defer rows.Close()
	type u struct {
		ID         string `json:"id"`
		Account    int64  `json:"accountId,omitempty"`
		Username   string `json:"username,omitempty"`
		Name       string `json:"friendlyName,omitempty"`
		Type       string `json:"type"`
		Restricted bool   `json:"restricted,omitempty"`
		Owner      bool   `json:"owner"`
	}
	var out []u
	for rows.Next() {
		var rr u
		if err := rows.Scan(&rr.ID, &rr.Account, &rr.Username, &rr.Name, &rr.Type, &rr.Restricted, &rr.Owner); err != nil {
			writeError(w, http.StatusBadGateway, "USERS_FAILED", err.Error())
			return
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "USERS_FAILED", err.Error())
		return
	}
	var next *int
	if len(out) > limit {
		out = out[:limit]
		n := offset + limit
		next = &n
	}
	if out == nil {
		out = []u{}
	}
	writePage(w, map[string]any{"users": out}, next)
}

func (m *Mux) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "user id required")
		return
	}
	uid := parts[0]
	isPolicy := len(parts) == 2 && parts[1] == "policy"
	switch {
	case isPolicy && r.Method == http.MethodGet:
		m.handleUserPolicyGet(w, r, uid)
	case isPolicy && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		m.handleUserPolicyPut(w, r, uid)
	case !isPolicy && r.Method == http.MethodGet:
		if m.svc == nil {
			writeError(w, http.StatusConflict, "USERS_DISABLED", "no database wired")
			return
		}
		var id, username, friendly, itype string
		var account *int64
		var restricted *bool
		err := m.svc.DB.QueryRow(r.Context(), `SELECT id::text, username, friendly_name, identity_type, plex_account_id, restricted
			FROM plex_identities WHERE id=$1`, uid).Scan(&id, &username, &friendly, &itype, &account, &restricted)
		if err != nil {
			writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "unknown user")
			return
		}
		writeData(w, http.StatusOK, map[string]any{"id": id, "username": username, "friendlyName": friendly, "type": itype, "accountId": account, "restricted": restricted})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or PATCH")
	}
}

func (m *Mux) handleUserPolicyGet(w http.ResponseWriter, r *http.Request, uid string) {
	if m.svc == nil {
		writeError(w, http.StatusConflict, "POLICY_DISABLED", "no database wired")
		return
	}
	var raw json.RawMessage
	err := m.svc.DB.QueryRow(r.Context(), `SELECT config FROM policies WHERE scope_type='user' AND scope_id=$1 AND enabled LIMIT 1`, uid).Scan(&raw)
	if err != nil {
		writeData(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeData(w, http.StatusOK, map[string]any{"configured": true, "config": raw})
}

func (m *Mux) handleUserPolicyPut(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || len(body.Config) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "policy {name, config} required")
		return
	}
	if body.Name == "" {
		body.Name = "user-policy"
	}
	var before json.RawMessage
	if m.svc != nil {
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT config FROM policies WHERE scope_type='user' AND scope_id=$1 LIMIT 1`, uid).Scan(&before)
	}
	if err := m.storePolicy(r.Context(), "user", uid, body.Name, body.Config, true); err != nil {
		writeError(w, http.StatusBadRequest, "POLICY_STORE_FAILED", err.Error())
		return
	}
	m.auditEventWithBefore(r.Context(), subjectOf(r), "policy.put", "policy", "user/"+uid, before, map[string]any{"scopeType": "user", "scopeId": uid})
	writeData(w, http.StatusOK, map[string]any{"stored": true})
}

func (m *Mux) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "DEVICES_DISABLED", "no database wired")
		return
	}
	limit, offset := parseCursor(r, 50, 200)
	rows, err := m.svc.DB.Query(r.Context(), `SELECT id::text, plex_client_identifier, COALESCE(friendly_name,''),
		COALESCE(product,''), COALESCE(product_version,''), COALESCE(platform,'')
		FROM client_instances ORDER BY last_seen_at DESC LIMIT $1 OFFSET $2`, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DEVICES_FAILED", err.Error())
		return
	}
	defer rows.Close()
	type d struct {
		ID       string `json:"id"`
		Client   string `json:"clientIdentifier"`
		Name     string `json:"friendlyName,omitempty"`
		Product  string `json:"product,omitempty"`
		Version  string `json:"productVersion,omitempty"`
		Platform string `json:"platform,omitempty"`
	}
	var out []d
	for rows.Next() {
		var rr d
		if err := rows.Scan(&rr.ID, &rr.Client, &rr.Name, &rr.Product, &rr.Version, &rr.Platform); err != nil {
			writeError(w, http.StatusBadGateway, "DEVICES_FAILED", err.Error())
			return
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "DEVICES_FAILED", err.Error())
		return
	}
	var next *int
	if len(out) > limit {
		out = out[:limit]
		n := offset + limit
		next = &n
	}
	if out == nil {
		out = []d{}
	}
	writePage(w, map[string]any{"devices": out}, next)
}

func (m *Mux) handleDeviceDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/devices/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "device id required")
		return
	}
	did := parts[0]
	isPolicy := len(parts) == 2 && parts[1] == "policy"
	switch {
	case isPolicy && r.Method == http.MethodGet:
		m.handleDevicePolicyGet(w, r, did)
	case isPolicy && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		m.handleDevicePolicyPut(w, r, did)
	case !isPolicy && r.Method == http.MethodGet:
		if m.svc == nil {
			writeError(w, http.StatusConflict, "DEVICES_DISABLED", "no database wired")
			return
		}
		var id, client, name, product, version, platform string
		err := m.svc.DB.QueryRow(r.Context(), `SELECT id::text, plex_client_identifier, COALESCE(friendly_name,''),
			COALESCE(product,''), COALESCE(product_version,''), COALESCE(platform,'')
			FROM client_instances WHERE id=$1`, did).Scan(&id, &client, &name, &product, &version, &platform)
		if err != nil {
			writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "unknown device")
			return
		}
		writeData(w, http.StatusOK, map[string]any{"id": id, "clientIdentifier": client, "friendlyName": name, "product": product, "productVersion": version, "platform": platform})
	case !isPolicy && r.Method == http.MethodPatch:
		var body struct {
			FriendlyName *string `json:"friendlyName"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "JSON required")
			return
		}
		if m.svc == nil {
			writeError(w, http.StatusConflict, "DEVICES_DISABLED", "no database wired")
			return
		}
		var before string
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT COALESCE(friendly_name,'') FROM client_instances WHERE id=$1`, did).Scan(&before)
		if body.FriendlyName != nil {
			if _, err := m.svc.DB.Exec(r.Context(), `UPDATE client_instances SET friendly_name=$1 WHERE id=$2`, *body.FriendlyName, did); err != nil {
				writeError(w, http.StatusBadGateway, "DEVICE_UPDATE_FAILED", err.Error())
				return
			}
		}
		m.auditEventWithBefore(r.Context(), subjectOf(r), "device.override", "device", did, before, body)
		writeData(w, http.StatusOK, map[string]any{"updated": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET, PATCH or policy")
	}
}

func (m *Mux) handleDevicePolicyGet(w http.ResponseWriter, r *http.Request, did string) {
	if m.svc == nil {
		writeError(w, http.StatusConflict, "POLICY_DISABLED", "no database wired")
		return
	}
	var raw json.RawMessage
	err := m.svc.DB.QueryRow(r.Context(), `SELECT config FROM policies WHERE scope_type='device' AND scope_id=$1 AND enabled LIMIT 1`, did).Scan(&raw)
	if err != nil {
		writeData(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeData(w, http.StatusOK, map[string]any{"configured": true, "config": raw})
}

func (m *Mux) handleDevicePolicyPut(w http.ResponseWriter, r *http.Request, did string) {
	var body struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || len(body.Config) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "policy {name, config} required")
		return
	}
	if body.Name == "" {
		body.Name = "device-policy"
	}
	var before json.RawMessage
	if m.svc != nil {
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT config FROM policies WHERE scope_type='device' AND scope_id=$1 LIMIT 1`, did).Scan(&before)
	}
	if err := m.storePolicy(r.Context(), "device", did, body.Name, body.Config, true); err != nil {
		writeError(w, http.StatusBadRequest, "POLICY_STORE_FAILED", err.Error())
		return
	}
	m.auditEventWithBefore(r.Context(), subjectOf(r), "policy.put", "policy", "device/"+did, before, map[string]any{"scopeType": "device", "scopeId": did})
	writeData(w, http.StatusOK, map[string]any{"stored": true})
}

// --- library ---

func (m *Mux) handleLibraryItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "LIBRARY_DISABLED", "no database wired")
		return
	}
	limit, offset := parseCursor(r, 50, 200)
	q := r.URL.Query().Get("q")
	var rowsUrls string
	_ = rowsUrls
	rows, err := m.svc.DB.Query(r.Context(), `SELECT id::text, rating_key, COALESCE(title,''), item_type, COALESCE(year,0)
		FROM library_items WHERE ($1='' OR title ILIKE '%'||$1||'%') ORDER BY title LIMIT $2 OFFSET $3`, q, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, "LIBRARY_FAILED", err.Error())
		return
	}
	defer rows.Close()
	type it struct {
		ID        string `json:"id"`
		RatingKey string `json:"ratingKey"`
		Title     string `json:"title"`
		Type      string `json:"type"`
		Year      int    `json:"year,omitempty"`
	}
	var out []it
	for rows.Next() {
		var rr it
		if err := rows.Scan(&rr.ID, &rr.RatingKey, &rr.Title, &rr.Type, &rr.Year); err != nil {
			writeError(w, http.StatusBadGateway, "LIBRARY_FAILED", err.Error())
			return
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "LIBRARY_FAILED", err.Error())
		return
	}
	var next *int
	if len(out) > limit {
		out = out[:limit]
		n := offset + limit
		next = &n
	}
	if out == nil {
		out = []it{}
	}
	writePage(w, map[string]any{"items": out}, next)
}

func (m *Mux) handleLibraryItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "LIBRARY_DISABLED", "no database wired")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/library/items/")
	if id == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "item id required")
		return
	}
	var itemID, ratingKey, title, itype string
	err := m.svc.DB.QueryRow(r.Context(), `SELECT id::text, rating_key, COALESCE(title,''), item_type FROM library_items WHERE id=$1`, id).Scan(&itemID, &ratingKey, &title, &itype)
	if err != nil {
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "unknown item")
		return
	}
	vrows, err := m.svc.DB.Query(r.Context(), `SELECT media_index, COALESCE(container,''), COALESCE(video_codec,''), width, height, bitrate_kbps, normalized_dynamic_range
		FROM media_variants WHERE library_item_id=$1 ORDER BY media_index`, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VARIANTS_FAILED", err.Error())
		return
	}
	defer vrows.Close()
	var variants []map[string]any
	for vrows.Next() {
		var idx int
		var container, vcodec, dr string
		var width, height, bitrate *int
		if err := vrows.Scan(&idx, &container, &vcodec, &width, &height, &bitrate, &dr); err != nil {
			writeError(w, http.StatusBadGateway, "VARIANTS_FAILED", err.Error())
			return
		}
		variants = append(variants, map[string]any{"mediaIndex": idx, "container": container, "videoCodec": vcodec, "width": width, "height": height, "bitrateKbps": bitrate, "dynamicRange": dr})
	}
	if err := vrows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "VARIANTS_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]any{"id": itemID, "ratingKey": ratingKey, "title": title, "type": itype, "variants": variants})
}

// --- cache / storage ---

func (m *Mux) handleCacheInvalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if !m.checkRate("cache-invalidate:"+subjectOf(r), time.Minute, 10) {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "cache invalidate at most 10 per minute")
		return
	}
	var body struct {
		Scope string `json:"scope"`
		Class string `json:"class"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	if body.Scope == "" {
		body.Scope = "all"
	}
	// Namespace invalidation retires matching keys in constant time via
	// generations; TTLs remain the backstop if no invalidator is wired.
	if m.invalidator != nil {
		m.invalidator(body.Scope, body.Class)
	}
	m.auditEvent(r.Context(), subjectOf(r), "cache.invalidate", "cache", body.Scope+"/"+body.Class,
		map[string]any{"scope": body.Scope, "class": body.Class})
	writeData(w, http.StatusOK, map[string]any{"invalidated": body.Scope, "class": body.Class})
}

func (m *Mux) handleStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	out := map[string]any{"cache": "valkey-best-effort", "artwork": "filesystem", "diagnostics": "filesystem"}
	if m.storageUsage != nil {
		for k, v := range m.storageUsage() {
			out[k] = v
		}
	}
	if m.svc != nil {
		var items, sessions, decisions, traces, audits int64
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT count(*) FROM library_items`).Scan(&items)
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT count(*) FROM playback_sessions WHERE ended_at IS NULL`).Scan(&sessions)
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT count(*) FROM playback_decisions`).Scan(&decisions)
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT count(*) FROM diagnostic_traces`).Scan(&traces)
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT count(*) FROM audit_events`).Scan(&audits)
		out["libraryItems"] = items
		out["activeSessions"] = sessions
		out["decisions"] = decisions
		out["traces"] = traces
		out["audits"] = audits
	}
	writeData(w, http.StatusOK, out)
}

// --- sessions / playback ---

func (m *Mux) handleSessionsAlias(w http.ResponseWriter, r *http.Request) {
	m.handleSessions(w, r)
}

func (m *Mux) handlePlaybackDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SESSIONS_DISABLED", "no database wired")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/playback/")
	if id == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "playback id required")
		return
	}
	var sid, ratingKey, mode, routing, started string
	var ended, finalStatus *string
	var policy json.RawMessage
	err := m.svc.DB.QueryRow(r.Context(), `SELECT id::text, rating_key, playback_mode, routing_mode,
		to_char(started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		to_char(ended_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), final_status, effective_policy
		FROM playback_sessions WHERE id=$1`, id).Scan(&sid, &ratingKey, &mode, &routing, &started, &ended, &finalStatus, &policy)
	if err != nil {
		writeError(w, http.StatusNotFound, "PLAYBACK_NOT_FOUND", "unknown playback")
		return
	}
	rows, err := m.svc.DB.Query(r.Context(), `SELECT requested_media_index, selected_media_index, decision, decision_reason,
		plex_decision_code, details FROM playback_decisions WHERE playback_session_id=$1 ORDER BY created_at`, sid)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DECISIONS_FAILED", err.Error())
		return
	}
	defer rows.Close()
	var decisions []map[string]any
	for rows.Next() {
		var req, sel, code int
		var dec, reason string
		var details json.RawMessage
		if err := rows.Scan(&req, &sel, &dec, &reason, &code, &details); err != nil {
			writeError(w, http.StatusBadGateway, "DECISIONS_FAILED", err.Error())
			return
		}
		decisions = append(decisions, map[string]any{"requestedIndex": req, "selectedIndex": sel, "decision": dec, "reason": reason, "plexCode": code, "details": details})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "DECISIONS_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"id": sid, "ratingKey": ratingKey, "mode": mode, "routing": routing,
		"startedAt": started, "endedAt": ended, "finalStatus": finalStatus,
		"effectivePolicy": policy, "decisions": decisions,
		"cloudflareMediaRisk": false,
	})
}

// --- diagnostics traces ---

func (m *Mux) handleTraces(w http.ResponseWriter, r *http.Request) {
	if m.svc == nil {
		writeError(w, http.StatusConflict, "TRACES_DISABLED", "no database wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, offset := parseCursor(r, 50, 200)
		rows, err := m.svc.DB.Query(r.Context(), `SELECT id::text, trace_type, status,
			to_char(started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), to_char(expires_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
			FROM diagnostic_traces ORDER BY started_at DESC LIMIT $1 OFFSET $2`, limit+1, offset)
		if err != nil {
			writeError(w, http.StatusBadGateway, "TRACES_FAILED", err.Error())
			return
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var id, typ, status, started, expires string
			if err := rows.Scan(&id, &typ, &status, &started, &expires); err != nil {
				writeError(w, http.StatusBadGateway, "TRACES_FAILED", err.Error())
				return
			}
			out = append(out, map[string]any{"id": id, "type": typ, "status": status, "startedAt": started, "expiresAt": expires})
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusBadGateway, "TRACES_FAILED", err.Error())
			return
		}
		var next *int
		if len(out) > limit {
			out = out[:limit]
			n := offset + limit
			next = &n
		}
		if out == nil {
			out = []map[string]any{}
		}
		writePage(w, map[string]any{"traces": out}, next)
	case http.MethodPost:
		var body struct {
			TraceType string `json:"traceType"`
			Target    string `json:"target"`
			Minutes   int    `json:"minutes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "trace JSON required")
			return
		}
		if body.TraceType == "" {
			body.TraceType = "protocol"
		}
		ttl := body.Minutes
		if ttl <= 0 || ttl > 8*60 {
			ttl = 30
		}
		var serverID string
		if err := m.svc.DB.QueryRow(r.Context(), `SELECT id FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID); err != nil {
			writeError(w, http.StatusConflict, "NO_SERVER", "no enabled origin")
			return
		}
		var id string
		err := m.svc.DB.QueryRow(r.Context(), `INSERT INTO diagnostic_traces(server_id, trace_type, status, expires_at, summary)
			VALUES($1,$2,'active', now() + make_interval(mins => $3), $4) RETURNING id::text`,
			serverID, body.TraceType, ttl, map[string]any{"target": body.Target}).Scan(&id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "TRACE_CREATE_FAILED", err.Error())
			return
		}
		m.auditEvent(r.Context(), subjectOf(r), "trace.create", "trace", id, map[string]any{"type": body.TraceType})
		writeData(w, http.StatusCreated, map[string]any{"id": id, "expiresMinutes": ttl})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

func (m *Mux) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "TRACES_DISABLED", "no database wired")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/diagnostics/traces/")
	var typ, status, started, expires string
	var summary json.RawMessage
	err := m.svc.DB.QueryRow(r.Context(), `SELECT trace_type, status,
		to_char(started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		to_char(expires_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), summary
		FROM diagnostic_traces WHERE id=$1`, id).Scan(&typ, &status, &started, &expires, &summary)
	if err != nil {
		writeError(w, http.StatusNotFound, "TRACE_NOT_FOUND", "unknown trace")
		return
	}
	writeData(w, http.StatusOK, map[string]any{"id": id, "type": typ, "status": status, "startedAt": started, "expiresAt": expires, "summary": summary})
}

func (m *Mux) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	// Not implemented as a finished capability: structured logs stream
	// to container stderr (docker compose logs replx-edge). 501 instead
	// of an empty 200 so operators never mistake the stub for coverage.
	writeError(w, http.StatusNotImplemented, "LOGS_NOT_IMPLEMENTED",
		"live log streaming is not implemented; inspect container stderr (docker compose logs replx-edge)")
}

func (m *Mux) handleSettings(w http.ResponseWriter, r *http.Request) {
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SETTINGS_DISABLED", "no database wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := m.svc.DB.Query(r.Context(), `SELECT key, value FROM app_settings WHERE key = ANY($1) ORDER BY key`, runtimeSettingKeys())
		if err != nil {
			writeError(w, http.StatusBadGateway, "SETTINGS_FAILED", err.Error())
			return
		}
		defer rows.Close()
		out := map[string]any{}
		for rows.Next() {
			var k string
			var v json.RawMessage
			if err := rows.Scan(&k, &v); err != nil {
				writeError(w, http.StatusBadGateway, "SETTINGS_FAILED", err.Error())
				return
			}
			out[k] = v
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusBadGateway, "SETTINGS_FAILED", err.Error())
			return
		}
		writeData(w, http.StatusOK, map[string]any{"settings": out, "spec": runtimeSettingSpec()})
	case http.MethodPatch:
		var body map[string]json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "settings JSON required")
			return
		}
		for k, v := range body {
			n, err := validatedSetting(k, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "SETTING_INVALID", err.Error())
				return
			}
			var before json.RawMessage
			_ = m.svc.DB.QueryRow(r.Context(), `SELECT value FROM app_settings WHERE key=$1`, k).Scan(&before)
			raw, _ := json.Marshal(n)
			if _, err := m.svc.DB.Exec(r.Context(), `INSERT INTO app_settings(key,value,updated_at) VALUES($1,$2,now())
				ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, k, string(raw)); err != nil {
				writeError(w, http.StatusBadGateway, "SETTING_STORE_FAILED", err.Error())
				return
			}
			m.auditEventWithBefore(r.Context(), subjectOf(r), "retention.setting.change", "setting", k, before, raw)
		}
		writeData(w, http.StatusOK, map[string]any{"updated": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or PATCH")
	}
}

// settingBounds defines the explicit allowlist of runtime-mutable
// settings: name, description, range in days, and default. Anything else
// is rejected: secrets, listener bindings, ingress mode, database wiring
// and onboarding state require environment changes and restart, and
// arbitrary keys would leave the API contract undefined.
var settingBounds = map[string]struct {
	desc     string
	min, max int
	def      int
}{
	"replx.playback_retention_days": {"playback decisions and completed sessions retention (days); live, reloaded each purge", 1, 3650, 30},
	"replx.audit_retention_days":    {"audit events retention (days); live, reloaded each purge", 1, 3650, 180},
}

func runtimeSettingKeys() []string {
	out := make([]string, 0, len(settingBounds))
	for k := range settingBounds {
		out = append(out, k)
	}
	return out
}

func runtimeSettingSpec() map[string]any {
	out := map[string]any{}
	for k, b := range settingBounds {
		out[k] = map[string]any{"description": b.desc, "minDays": b.min, "maxDays": b.max, "defaultDays": b.def, "restartRequired": false}
	}
	return out
}

// validatedSetting rejects unknown keys and out-of-range values,
// returning the normalized integer days.
func validatedSetting(key string, raw json.RawMessage) (int, error) {
	b, ok := settingBounds[key]
	if !ok {
		return 0, fmt.Errorf("unknown setting %q: mutable settings are exactly %v; secrets, bindings and onboarding state require restart/re-onboarding", key, runtimeSettingKeys())
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("setting %q must be an integer number of days", key)
	}
	if n < b.min || n > b.max {
		return 0, fmt.Errorf("setting %q must be %d..%d days", key, b.min, b.max)
	}
	return n, nil
}

// --- setup / rate limiting ---

func (m *Mux) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SETUP_DISABLED", "no database wired")
		return
	}
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		SetupToken string `json:"setupToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "JSON {username, password, setupToken} required")
		return
	}
	// First-run setup requires the bootstrap capability: bearer token or
	// explicit field. An empty admin table alone never authorizes claims.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = body.SetupToken
	}
	if !m.setupTokenValid(token) {
		writeError(w, http.StatusUnauthorized, "SETUP_TOKEN_REQUIRED", "valid setup token required (15m from startup, single use)")
		return
	}
	hash, err := hashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
		return
	}
	// Atomic singleton creation: advisory lock serializes concurrent
	// setups, the count re-checks inside the lock, and the unique index
	// (0006) rejects any residual race at the database level.
	cctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tx, err := m.svc.DB.Begin(cctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SETUP_FAILED", err.Error())
		return
	}
	defer func() { _ = tx.Rollback(cctx) }()
	if _, err := tx.Exec(cctx, `SELECT pg_advisory_xact_lock(hashtext('replx_edge_admin_setup'))`); err != nil {
		writeError(w, http.StatusBadGateway, "SETUP_FAILED", err.Error())
		return
	}
	var count int
	if err := tx.QueryRow(cctx, `SELECT count(*) FROM admin_users`).Scan(&count); err != nil {
		writeError(w, http.StatusBadGateway, "SETUP_FAILED", err.Error())
		return
	}
	if count > 0 {
		writeError(w, http.StatusConflict, "SETUP_DISABLED", "admin already exists")
		return
	}
	if _, err := tx.Exec(cctx, `INSERT INTO admin_users(username, password_hash) VALUES($1,$2)`, body.Username, hash); err != nil {
		// Concurrent winner: unique singleton index rejects the loser.
		writeError(w, http.StatusConflict, "SETUP_DISABLED", "admin already exists")
		return
	}
	if err := tx.Commit(cctx); err != nil {
		writeError(w, http.StatusBadGateway, "SETUP_FAILED", err.Error())
		return
	}
	// Creation consumes the bootstrap capability in the same step:
	// bearer auth retires and every setup-minted session dies now.
	m.consumeSetup()
	m.auditEvent(r.Context(), "setup", "admin.credential.change", "admin", body.Username, map[string]any{"created": true})
	writeData(w, http.StatusCreated, map[string]any{"created": true})
}

func (m *Mux) checkRate(key string, window time.Duration, max int) bool {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	now := time.Now()
	if m.rate == nil {
		m.rate = map[string][]time.Time{}
	}
	hits := m.rate[key]
	fresh := hits[:0]
	for _, t := range hits {
		if now.Sub(t) < window {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= max {
		m.rate[key] = fresh
		return false
	}
	m.rate[key] = append(fresh, now)
	return true
}

var _ = context.Background
var _ = health.Checks{}
