package playback

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/trace"
)

// PartIDFromPath extracts the origin part ID from /library/parts/<id>/....
func PartIDFromPath(path string) string {
	parts := strings.Split(strings.ToLower(path), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "library" && parts[i+1] == "parts" {
			id := strings.SplitN(parts[i+2], "?", 1)[0]
			if id != "" {
				return id
			}
		}
	}
	return ""
}

// EnforcePart implements the raw-part boundary for the spike redirector.
// It returns a substitute part key when the request targets a prohibited
// part while the active session holds an allowed selection (Jodie rule),
// deny=true is reserved for known-prohibited parts once identity resolution
// lands (Gamma phase), and empty results mean allow: no session context
// preserves Alpha redirect behaviour exactly.
//
// Manifest requests (empty partID) take the manifest consistency check:
// a direct-play session followed by an explicit transcode manifest fails
// closed when the session policy denies transcoding.
func (e *Engine) EnforcePart(r *http.Request, partID, sessionID string) (substituteKey string, deny bool, reason string) {
	if e == nil || e.Store == nil || sessionID == "" {
		return "", false, ""
	}
	if partID == "" {
		return e.enforceManifest(r, sessionID)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sess, ok, err := e.Store.FindActive(ctx, sessionID)
	if err != nil || !ok {
		return "", false, ""
	}
	if sess.SelectedPartPlexID != "" && partID == sess.SelectedPartPlexID {
		return "", false, ""
	}
	if sess.SelectedPartPlexID != "" && e.sameVariant(ctx, sess, partID) {
		// Same variant, later part file (multi-part media): not a policy
		// violation, just sequential playback.
		return "", false, ""
	}
	if sess.SelectedPartKey != "" {
		return sess.SelectedPartKey, false, "substituted"
	}
	return "", false, ""
}

// enforceManifest guards the manifest boundary against clients that retry
// around the negotiated decision: a session the engine direct-played,
// followed by a manifest explicitly requesting transcode
// (directPlay=0&directStream=0), fails closed when the session's stored
// policy denies transcoding. Anything ambiguous allows: manifests also
// serve legitimate Direct Stream upgrades the decision phase approved.
func (e *Engine) enforceManifest(r *http.Request, sessionID string) (string, bool, string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sess, ok, err := e.Store.FindActive(ctx, sessionID)
	if err != nil || !ok || sess.PlaybackMode != "directPlay" {
		return "", false, ""
	}
	q := r.URL.Query()
	if q.Get("directPlay") != "0" || q.Get("directStream") != "0" {
		return "", false, ""
	}
	var stored struct {
		AllowTranscode string `json:"allowTranscode"`
	}
	if len(sess.EffectivePolicy) > 0 {
		_ = json.Unmarshal(sess.EffectivePolicy, &stored)
	}
	if !strings.EqualFold(stored.AllowTranscode, "deny") {
		return "", false, ""
	}
	return "", true, "POLICY_TRANSCODE_FORBIDDEN"
}

// sameVariant reports whether a requested part belongs to the session's
// selected variant (multi-part playback), via the owner index. Unknown
// parts read as different: substitution, not silent permission.
func (e *Engine) sameVariant(ctx context.Context, sess Session, partPlexID string) bool {
	if e.DB == nil || partPlexID == "" || sess.SelectedVariantID == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var variantID string
	err := e.DB.QueryRow(cctx, `SELECT v.id::text FROM media_parts p
		JOIN media_variants v ON v.id = p.media_variant_id
		JOIN library_items li ON li.id = v.library_item_id
		JOIN plex_servers s ON s.id = li.server_id
		WHERE s.enabled AND p.plex_part_id = $1 AND li.rating_key = $2`,
		partPlexID, sess.RatingKey).Scan(&variantID)
	if err != nil {
		return false
	}
	return variantID == sess.SelectedVariantID
}

// EndSession closes open playback sessions for a Plex session ID.
// Best-effort: stop verbs must never break the proxied control flow.
func (e *Engine) EndSession(r *http.Request) {
	if e == nil || e.Store == nil {
		return
	}
	sessionID := trace.ExtractSession(r)
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_ = e.Store.EndSession(ctx, sessionID)
}
