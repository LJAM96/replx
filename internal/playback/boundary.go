package playback

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/policy"
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
// part while the active session holds an allowed selection (Jodie rule).
// Without session state it reconstructs policy from user, client and part
// (stateless path below) instead of allowing: a database outage or a
// decision-skipping client must never promote a restricted part.
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sess, ok, err := e.Store.FindActive(ctx, sessionID)
	if err != nil || !ok {
		return e.enforceStateless(r, partID)
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

// Stateless decision code: the boundary cannot consult negotiation state,
// so the client must negotiate (or re-negotiate) before media flows.
const DecisionRequired = "POLICY_DECISION_REQUIRED"

// enforceStateless reconstructs policy for a sessionless part request
// from user, client and part. Eligible under the resolved effective
// policy it passes with a stateless-allow trace; anything else fails
// closed with POLICY_DECISION_REQUIRED. Database errors fail closed:
// an outage must not promote Jodie's 4K part.
func (e *Engine) enforceStateless(r *http.Request, partID string) (string, bool, string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	token := trace.ExtractToken(r)
	if token == "" {
		return "", true, DecisionRequired
	}
	variant, ratingKey, ok := e.lookupPart(ctx, partID)
	if !ok {
		return "", true, DecisionRequired
	}
	var identityID, clientUUID string
	if e.Identity != nil {
		res := e.Identity.Resolve(ctx, trace.Fingerprint(e.Secret, token), token,
			identity.FromTrace(trace.ExtractClient(r)))
		identityID, clientUUID = res.IdentityID, res.ClientID
	}
	load := e.LoadPolicy
	if load == nil {
		load = DefaultPolicyLoader(e.DB)
	}
	pol, scope, err := load(ctx, strOrNil(identityID), strOrNil(clientUUID))
	if err != nil {
		e.logDecision("", ratingKey, "", "part_stateless_deny", map[string]any{
			"reason": "policy unavailable",
		})
		return "", true, DecisionRequired
	}
	if _, err := policy.Evaluate(pol, scope, []policy.Variant{variant.toPolicy()}); err != nil {
		e.logDecision("", ratingKey, "", "part_stateless_deny", map[string]any{
			"reason": "stateless ineligible",
		})
		return "", true, DecisionRequired
	}
	e.logDecision("", ratingKey, "", "part_stateless_allow", map[string]any{
		"index": variant.MediaIndex,
	})
	return "", false, ""
}

// lookupPart resolves a part to its variant plus item rating key from the
// owner index. Absent rows read as unknown, never as permitted.
func (e *Engine) lookupPart(ctx context.Context, partPlexID string) (variantSource, string, bool) {
	var v variantSource
	var ratingKey string
	if e.DB == nil || partPlexID == "" {
		return v, "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := e.DB.QueryRow(cctx, `SELECT v.media_index, COALESCE(v.plex_media_id,''), v.width, v.height,
		v.bitrate_kbps, v.normalized_dynamic_range, COALESCE(v.video_codec,''), v.audio_channels,
		v.id::text, p.id::text, COALESCE(p.plex_part_id,''), COALESCE(p.plex_key,''), li.rating_key
		FROM media_parts p
		JOIN media_variants v ON v.id = p.media_variant_id
		JOIN library_items li ON li.id = v.library_item_id
		JOIN plex_servers s ON s.id = li.server_id
		WHERE s.enabled AND p.plex_part_id = $1`,
		partPlexID).Scan(&v.MediaIndex, &v.PlexMediaID, &v.Width, &v.Height, &v.BitrateKbps,
		&v.DynamicRange, &v.VideoCodec, &v.AudioChannels, &v.VariantUUID, &v.PartUUID,
		&v.PartPlexID, &v.PartKey, &ratingKey)
	if err != nil {
		return variantSource{}, "", false
	}
	v.PartAvailable = v.PartKey != ""
	return v, ratingKey, true
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
	if err != nil || !ok {
		// No negotiable session: manifests only exist after a decision
		// the engine would have tracked, so PMS itself would fail this
		// too. Deny explicitly rather than allow an untracked transcode.
		return "", true, DecisionRequired
	}
	if sess.PlaybackMode != "directPlay" {
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
