package playback

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/policy"
)

// forwarded is the PMS decision answer ready to relay.
type forwarded struct {
	status      int
	contentType string
	body        []byte
}

// variants loads candidates from the owner index first, falling back to
// user-scoped origin metadata. indexed reports the provenance for the
// audit trail.
func (e *Engine) variants(ctx context.Context, d decisionRequest, token string) ([]variantSource, bool) {
	if out := e.indexVariants(ctx, d.RatingKey); len(out) > 0 {
		return out, true
	}
	rawPath := ""
	if p := d.Query.Get("path"); p != "" {
		if decoded, err := url.QueryUnescape(p); err == nil {
			rawPath = decoded
		}
	}
	if rawPath == "" {
		if k := d.Query.Get("key"); k != "" {
			if decoded, err := url.QueryUnescape(k); err == nil {
				rawPath = decoded
			}
		}
	}
	if rawPath == "" {
		return nil, false
	}
	out, err := e.liveVariants(ctx, rawPath, token)
	if err != nil {
		return nil, false
	}
	return out, false
}

// indexVariants reads candidates from the Gamma owner index.
func (e *Engine) indexVariants(ctx context.Context, ratingKey string) []variantSource {
	if e.DB == nil || ratingKey == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := e.DB.Query(cctx, `SELECT v.media_index, COALESCE(v.plex_media_id,''), v.width, v.height,
		v.bitrate_kbps, v.normalized_dynamic_range, COALESCE(v.video_codec,''), v.audio_channels,
		v.id::text, p.id::text, COALESCE(p.plex_part_id,''), COALESCE(p.plex_key,'')
		FROM media_variants v
		JOIN library_items li ON li.id = v.library_item_id
		JOIN plex_servers s ON s.id = li.server_id
		LEFT JOIN media_parts p ON p.media_variant_id = v.id AND p.part_index = 0
		WHERE s.enabled AND li.rating_key = $1
		ORDER BY v.media_index`, ratingKey)
	if err != nil {
		e.logDecision("", ratingKey, "", "index_unavailable", map[string]any{
			"reason": "index query failed, falling back to live metadata",
		})
		return nil
	}
	defer rows.Close()
	var out []variantSource
	for rows.Next() {
		var s variantSource
		var width, height, bitrate, channels *int
		var dr string
		if err := rows.Scan(&s.MediaIndex, &s.PlexMediaID, &width, &height, &bitrate,
			&dr, &s.VideoCodec, &channels, &s.VariantUUID, &s.PartUUID,
			&s.PartPlexID, &s.PartKey); err != nil {
			e.logDecision("", ratingKey, "", "index_unavailable", map[string]any{
				"reason": "index scan failed, falling back to live metadata",
			})
			return nil
		}
		s.Width, s.Height, s.BitrateKbps, s.AudioChannels = width, height, bitrate, channels
		s.DynamicRange = dr
		s.PartAvailable = s.PartKey != ""
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.logDecision("", ratingKey, "", "index_unavailable", map[string]any{
			"reason": "index iteration failed, falling back to live metadata",
		})
		return nil
	}
	return out
}

// liveVariants fetches user-scoped origin metadata for items the index has
// not synced yet. The user token (never the owner token) authenticates.
func (e *Engine) liveVariants(ctx context.Context, rawPath, token string) ([]variantSource, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target := strings.TrimSuffix(e.Origin, "/") + rawPath
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", token)
	resp, err := e.client().Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("playback: metadata status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	return parseLiveVariants(raw)
}

// forward sends the rewritten negotiation to PMS with the user token and
// returns the answer. Bodies are small (KBs); 1 MiB caps pathologies.
func (e *Engine) forward(ctx context.Context, r *http.Request, d decisionRequest, token string, selected int, cap *int) (forwarded, error) {
	target := strings.TrimSuffix(e.Origin, "/") + r.URL.Path + "?" + rewriteQuery(d.Query, selected, cap).Encode()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, nil)
	if err != nil {
		return forwarded{}, err
	}
	copyDecisionHeaders(req.Header, r.Header)
	req.Header.Set("X-Plex-Token", token)
	resp, err := e.client().Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return forwarded{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return forwarded{}, err
	}
	return forwarded{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}, nil
}

// copyDecisionHeaders forwards client headers minus hop-by-hop and auth
// material the engine sets itself.
func copyDecisionHeaders(dst, src http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "keep-alive", "te", "trailer", "transfer-encoding",
			"upgrade", "proxy-authenticate", "proxy-authorization", "x-plex-token",
			"content-length":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// persistDecision writes the audit row. Session-less denials store a NULL
// session; the server scope is always recorded.
func (e *Engine) persistDecision(ctx context.Context, sessionID string, requested, selected int, decision, reason string, plexCode int, rejected []policy.Rejection, pol policy.Policy) {
	if e.DB == nil {
		return
	}
	details, _ := json.Marshal(map[string]any{"rejected": rejected})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = e.DB.Exec(cctx, `INSERT INTO playback_decisions(playback_session_id, server_id, requested_media_index, selected_media_index, decision, decision_reason, plex_decision_code, details)
		VALUES((SELECT id FROM playback_sessions WHERE id::text=$1),
			(SELECT id FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1),
			$2,$3,$4,$5,$6,$7)`,
		nullSession(sessionID), requested, selected, decision, reason, plexCode, details)
}

func nullSession(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (e *Engine) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

func (e *Engine) writePolicyError(w http.ResponseWriter, code, message string, rejected []policy.Rejection) {
	rendered := make([]string, 0, len(rejected))
	for _, r := range rejected {
		rendered = append(rendered, policy.RenderReason(r))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message, "rejected": rendered},
	})
}

func (e *Engine) logDecision(id, ratingKey string, fp string, decision string, fields map[string]any) {
	if e.Logger == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = "playback_" + decision
	if fp != "" {
		fields["userFingerprint"] = fp
	}
	e.Logger.Log(logging.Entry{
		Level: "info", Component: "playback.policy", RequestID: id,
		Path: "/library/metadata/" + ratingKey, Fields: fields,
	})
}
