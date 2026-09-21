package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/policy"
	"github.com/LJAM96/replx/internal/spike"
	syncpkg "github.com/LJAM96/replx/internal/sync"
)

// subjectKey carries the admin subject ("setup-token" or "browser-session")
// for audit rows.
type subjectKey struct{}

func withSubject(r *http.Request, subject string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), subjectKey{}, subject))
}

func subjectOf(r *http.Request) string {
	if s, ok := r.Context().Value(subjectKey{}).(string); ok && s != "" {
		return s
	}
	return "admin"
}

// auditEvent records an admin mutation. Best-effort: audit failure never
// blocks the operation it describes, but it is logged via the error.
func (m *Mux) auditEvent(ctx context.Context, subject, action, objType, objID string, after any) {
	m.auditEventWithBefore(ctx, subject, action, objType, objID, nil, after)
}

// auditEventWithBefore records before/after state where appropriate.
func (m *Mux) auditEventWithBefore(ctx context.Context, subject, action, objType, objID string, before, after any) {
	if m.svc == nil {
		return
	}
	rawAfter, _ := json.Marshal(after)
	var rawBefore []byte
	if before != nil {
		rawBefore, _ = json.Marshal(before)
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = m.svc.DB.Exec(cctx, `INSERT INTO audit_events(admin_subject, action, object_type, object_id, before_state, after_state)
		VALUES($1,$2,$3,$4,$5,$6)`, subject, action, objType, objID, rawBefore, rawAfter)
}

func parseLimit(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// SetSync registers sync status and single-flight full sync behind setup
// auth. Nil worker reports disabled instead of failing.
func (m *Mux) SetSync(w syncpkgWorker) {
	m.syncWorker = w
	m.mux.HandleFunc("/api/v1/sync/status", m.auth(m.handleSyncStatus))
	m.mux.HandleFunc("/api/v1/sync/full", m.auth(m.handleSyncFull))
}

func (m *Mux) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.syncWorker == nil {
		writeData(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	rows, err := m.syncWorker.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "SYNC_STATUS_FAILED", err.Error())
		return
	}
	if rows == nil {
		rows = []syncpkg.CursorStatus{}
	}
	writeData(w, http.StatusOK, map[string]any{"cursors": rows})
}

func (m *Mux) handleSyncFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if m.syncWorker == nil {
		writeError(w, http.StatusConflict, "SYNC_DISABLED", "no sync worker wired")
		return
	}
	m.syncMu.Lock()
	sinceLast := time.Since(m.lastSyncFull)
	if sinceLast < 5*time.Minute {
		jobID := m.lastSyncJob
		m.syncMu.Unlock()
		if jobID == "" {
			jobID = "sync-full-current"
		}
		// Idempotent while queued/running: 429 with current job ID.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "SYNC_RATE_LIMITED", "message": "full sync at most every 5 minutes"}, "data": map[string]any{"jobId": jobID, "status": "in-flight"}})
		return
	}
	jobID := time.Now().UTC().Format("20060102T150405Z")
	m.lastSyncFull = time.Now()
	m.lastSyncJob = jobID
	m.syncMu.Unlock()
	if err := m.syncWorker.SyncOnce(r.Context(), true); err != nil {
		if errors.Is(err, syncpkg.ErrInFlight) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "SYNC_IN_FLIGHT", "message": "another sync is already running"}, "data": map[string]any{"jobId": jobID, "status": "in-flight"}})
			return
		}
		writeError(w, http.StatusBadGateway, "SYNC_FAILED", err.Error())
		return
	}
	m.auditEvent(r.Context(), subjectOf(r), "sync.full", "sync", "full", map[string]any{"result": "complete", "jobId": jobID})
	writeData(w, http.StatusOK, map[string]any{"completed": true, "jobId": jobID})
}

// validatePolicy checks scope rules and config shape before storage.
func validatePolicy(scopeType, scopeID, name string, config json.RawMessage) (policy.Policy, error) {
	switch scopeType {
	case "global":
		if scopeID != "" {
			return policy.Policy{}, errors.New("global scope takes no scope_id")
		}
	case "user", "device":
		if scopeID == "" {
			return policy.Policy{}, errors.New(scopeType + " scope requires scope_id")
		}
	default:
		return policy.Policy{}, errors.New("scope_type must be global, user or device")
	}
	if name == "" {
		return policy.Policy{}, errors.New("name is required")
	}
	var p policy.Policy
	if err := json.Unmarshal(config, &p); err != nil {
		return policy.Policy{}, errors.New("config must be policy JSON")
	}
	for _, t := range []policy.TriState{p.Allow4K, p.AllowHDR, p.AllowDolbyVision, p.AllowTranscode, p.PreferDirectPlay, p.UnknownDRBehavior} {
		switch policy.TriState(strings.ToLower(string(t))) {
		case "", policy.Inherit, policy.Allow, policy.Deny:
		default:
			return policy.Policy{}, errors.New("tristates must be inherit, allow or deny")
		}
	}
	switch p.RoutingMode {
	case "", "inherit", "automatic", "origin_preferred", "media_fallback":
	default:
		return policy.Policy{}, errors.New("routingMode must be inherit, automatic, origin_preferred or media_fallback")
	}
	return p, nil
}

func (m *Mux) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if m.svc == nil {
		writeError(w, http.StatusConflict, "POLICY_DISABLED", "no database wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, offset := parseCursor(r, 50, 200)
		rows, err := m.svc.DB.Query(r.Context(), `SELECT scope_type, COALESCE(scope_id::text,''), name, config, enabled FROM policies
			ORDER BY scope_type, name LIMIT $1 OFFSET $2`, limit+1, offset)
		if err != nil {
			writeError(w, http.StatusBadGateway, "POLICIES_FAILED", err.Error())
			return
		}
		defer rows.Close()
		type row struct {
			ScopeType string          `json:"scopeType"`
			ScopeID   string          `json:"scopeId,omitempty"`
			Name      string          `json:"name"`
			Config    json.RawMessage `json:"config"`
			Enabled   bool            `json:"enabled"`
		}
		var out []row
		for rows.Next() {
			var rr row
			if err := rows.Scan(&rr.ScopeType, &rr.ScopeID, &rr.Name, &rr.Config, &rr.Enabled); err != nil {
				writeError(w, http.StatusBadGateway, "POLICIES_FAILED", err.Error())
				return
			}
			out = append(out, rr)
		}
		var next *int
		if len(out) > limit {
			out = out[:limit]
			n := offset + limit
			next = &n
		}
		if out == nil {
			out = []row{}
		}
		writePage(w, map[string]any{"policies": out}, next)
	case http.MethodPut:
		var body struct {
			ScopeType string          `json:"scopeType"`
			ScopeID   string          `json:"scopeId"`
			Name      string          `json:"name"`
			Config    json.RawMessage `json:"config"`
			Enabled   *bool           `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "policy JSON required")
			return
		}
		if _, err := validatePolicy(body.ScopeType, body.ScopeID, body.Name, body.Config); err != nil {
			writeError(w, http.StatusBadRequest, "POLICY_INVALID", err.Error())
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		var before json.RawMessage
		_ = m.svc.DB.QueryRow(r.Context(), `SELECT config FROM policies WHERE scope_type=$1 AND COALESCE(scope_id::text,'')=$2 LIMIT 1`, body.ScopeType, body.ScopeID).Scan(&before)
		if err := m.storePolicy(r.Context(), body.ScopeType, body.ScopeID, body.Name, body.Config, enabled); err != nil {
			writeError(w, http.StatusBadGateway, "POLICY_STORE_FAILED", err.Error())
			return
		}
		m.auditEventWithBefore(r.Context(), subjectOf(r), "policy.put", "policy", body.ScopeType+"/"+body.Name,
			before, map[string]any{"scopeType": body.ScopeType, "scopeId": body.ScopeID, "enabled": enabled})
		writeData(w, http.StatusOK, map[string]any{"stored": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or PUT")
	}
}

// storePolicy replaces the row for a scope (single row per scope) after
// verifying user/device references exist.
func (m *Mux) storePolicy(ctx context.Context, scopeType, scopeID, name string, config json.RawMessage, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var serverID string
	if err := m.svc.DB.QueryRow(cctx, `SELECT id FROM plex_servers
		WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID); err != nil {
		return errors.New("no enabled server")
	}
	if scopeType == "user" {
		var exists bool
		if err := m.svc.DB.QueryRow(cctx, `SELECT EXISTS(SELECT 1 FROM plex_identities WHERE id=$1)`, scopeID).Scan(&exists); err != nil || !exists {
			return errors.New("unknown user scope_id")
		}
	}
	if scopeType == "device" {
		var exists bool
		if err := m.svc.DB.QueryRow(cctx, `SELECT EXISTS(SELECT 1 FROM client_instances WHERE id=$1)`, scopeID).Scan(&exists); err != nil || !exists {
			return errors.New("unknown device scope_id")
		}
	}
	tx, err := m.svc.DB.Begin(cctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(cctx) }()
	if scopeType == "global" {
		if _, err := tx.Exec(cctx, `DELETE FROM policies WHERE server_id=$1 AND scope_type='global'`, serverID); err != nil {
			return err
		}
		_, err = tx.Exec(cctx, `INSERT INTO policies(server_id, scope_type, name, config, enabled)
			VALUES($1,'global',$2,$3,$4)`, serverID, name, config, enabled)
	} else {
		if _, err := tx.Exec(cctx, `DELETE FROM policies WHERE server_id=$1 AND scope_type=$2 AND scope_id=$3`,
			serverID, scopeType, scopeID); err != nil {
			return err
		}
		_, err = tx.Exec(cctx, `INSERT INTO policies(server_id, scope_type, scope_id, name, config, enabled)
			VALUES($1,$2,$3,$4,$5,$6)`, serverID, scopeType, scopeID, name, config, enabled)
	}
	if err != nil {
		return err
	}
	return tx.Commit(cctx)
}

// handleSessions lists recent playback sessions with their latest
// decision: the explainability surface (what was requested, selected,
// why others were rejected, what PMS decided, where media routed).
func (m *Mux) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if m.svc == nil {
		writeError(w, http.StatusConflict, "SESSIONS_DISABLED", "no database wired")
		return
	}
	limit, offset := parseCursor(r, 50, 200)
	before := r.URL.Query().Get("before")
	rows, err := m.svc.DB.Query(r.Context(), `SELECT ps.id::text, ps.plex_session_identifier, ps.rating_key,
		ps.playback_mode, ps.routing_mode, to_char(ps.started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		COALESCE(to_char(ps.ended_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),''), COALESCE(ps.final_status,''),
		(SELECT row_to_json(d) FROM (SELECT decision, decision_reason, requested_media_index, selected_media_index, plex_decision_code, details
			FROM playback_decisions WHERE playback_session_id=ps.id ORDER BY created_at DESC LIMIT 1) d)
		FROM playback_sessions ps
		WHERE ($1='' OR ps.started_at < $1::timestamptz)
		ORDER BY ps.started_at DESC LIMIT $2 OFFSET $3`, before, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SESSIONS_FAILED", err.Error())
		return
	}
	defer rows.Close()
	type row struct {
		ID             string          `json:"id"`
		Session        string          `json:"plexSessionId"`
		RatingKey      string          `json:"ratingKey"`
		Mode           string          `json:"playbackMode"`
		Routing        string          `json:"routingMode"`
		StartedAt      string          `json:"startedAt"`
		EndedAt        string          `json:"endedAt,omitempty"`
		FinalStatus    string          `json:"finalStatus,omitempty"`
		LatestDecision json.RawMessage `json:"latestDecision,omitempty"`
	}
	var out []row
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.ID, &rr.Session, &rr.RatingKey, &rr.Mode, &rr.Routing,
			&rr.StartedAt, &rr.EndedAt, &rr.FinalStatus, &rr.LatestDecision); err != nil {
			writeError(w, http.StatusBadGateway, "SESSIONS_FAILED", err.Error())
			return
		}
		out = append(out, rr)
	}
	var next *int
	if len(out) > limit {
		out = out[:limit]
		n := offset + limit
		next = &n
	}
	if out == nil {
		out = []row{}
	}
	writePage(w, map[string]any{"sessions": out}, next)
}

// handleCompat exposes the compatibility matrix (spike-backed until
// learned profiles accumulate): per client product and playback type.
func (m *Mux) handleCompat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	rows, err := m.spikeObs.Matrix(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "MATRIX_FAILED", err.Error())
		return
	}
	if rows == nil {
		rows = []spike.ObservationRow{}
	}
	writeData(w, http.StatusOK, map[string]any{"compat": rows})
}
