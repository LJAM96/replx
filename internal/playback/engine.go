package playback

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/origin"
	"github.com/LJAM96/replx/internal/policy"
	"github.com/LJAM96/replx/internal/trace"
)

// variantSource is one candidate with origin provenance attached.
type variantSource struct {
	MediaIndex    int
	PlexMediaID   string
	Width         *int
	Height        *int
	BitrateKbps   *int
	DynamicRange  string
	VideoCodec    string
	AudioChannels *int
	PartAvailable bool
	PartKey       string
	PartPlexID    string
	VariantUUID   string
	PartUUID      string
}

func (v variantSource) toPolicy() policy.Variant {
	return policy.Variant{
		MediaIndex: v.MediaIndex, Width: v.Width, Height: v.Height,
		BitrateKbps: v.BitrateKbps, DynamicRange: v.DynamicRange,
		VideoCodec: v.VideoCodec, AudioChannels: v.AudioChannels,
		Playability: policy.UnknownPlayability, PartAvailable: v.PartAvailable,
	}
}

// Deny is a fail-closed policy refusal the proxy renders as a coded 403.
// It exists so known playback-decision endpoints can never become
// transparent simply because the engine could not parse or contextualize
// them: engine disabled stays transparent, but an undecipherable decision
// request fails closed.
type Deny struct {
	Code     string
	Message  string
	Rejected []policy.Rejection
}

// DecisionUnsupported is returned when a universal decision request
// reaches the engine without a parseable rating key or user context.
const DecisionUnsupported = "POLICY_DECISION_UNSUPPORTED"

// Engine enforces playback policy at negotiation and part boundaries.
type Engine struct {
	DB       database.DBTX
	Origin   string
	Secret   string
	Logger   *logging.Logger
	Metrics  *metrics.Registry
	Client   *http.Client
	Store    SessionStore
	Identity *identity.Resolver
	// LoadPolicy resolves effective policy with rejection provenance.
	// Identity/client UUIDs activate user/device levels; nil keeps
	// global-only resolution.
	LoadPolicy func(ctx context.Context, identityID, clientID *string) (policy.Policy, string, error)
}

func (e *Engine) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	// Server-to-server PMS calls stay within the configured origin:
	// cross-origin redirects are refused, never followed with the user
	// credential. An unparseable base falls back to transparent (no
	// redirect following at all), which is equally credential-safe.
	if c, err := origin.APIClient(e.Origin, 30*time.Second); err == nil {
		return c
	}
	return origin.TransparentClient(30 * time.Second)
}

// DefaultPolicyLoader resolves global-only effective policy. User and
// device levels activate with the identity pipeline; until then absent
// rows merge as inherit and global governs.
func DefaultPolicyLoader(db database.DBTX) func(ctx context.Context, identityID, clientID *string) (policy.Policy, string, error) {
	return func(ctx context.Context, identityID, clientID *string) (policy.Policy, string, error) {
		if db == nil {
			return policy.Defaults(), "GLOBAL", nil
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var serverID string
		if err := db.QueryRow(cctx, `SELECT id FROM plex_servers
			WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID); err != nil {
			return policy.Defaults(), "GLOBAL", err
		}
		p, scope, err := policy.LoadEffective(cctx, db, serverID, identityID, clientID)
		if err != nil {
			return policy.Policy{}, "", err
		}
		return p, scope, nil
	}
}

// HandleDecision intercepts one universal negotiation request. It reports
// whether it wrote the response; false means proxy through untouched
// (unparseable, unauthenticated, or engine not fully wired).
// HandleDecision intercepts one universal negotiation request, returning
// (handled, deny): handled means the engine wrote the response; deny
// non-nil means the proxy must refuse with the coded 403; (false, nil)
// means transparent proxy (engine disabled only).
func (e *Engine) HandleDecision(w http.ResponseWriter, r *http.Request, id, fp, sessionID, identityID, clientUUID string) (bool, *Deny) {
	if e == nil || e.Store == nil {
		return false, nil
	}
	d := parseDecision(r)
	if d.RatingKey == "" {
		return false, &Deny{Code: DecisionUnsupported,
			Message: "playback decision without an identifiable rating key cannot be policy-checked"}
	}
	token := trace.ExtractToken(r)
	if token == "" {
		return false, &Deny{Code: DecisionUnsupported,
			Message: "playback decision without user context cannot be policy-checked"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	load := e.LoadPolicy
	if load == nil {
		load = DefaultPolicyLoader(e.DB)
	}
	pol, scope, err := load(ctx, strOrNil(identityID), strOrNil(clientUUID))
	if err != nil {
		e.writeError(w, http.StatusBadGateway, "POLICY_LOAD_FAILED", "effective policy unavailable")
		return true, nil
	}
	sources, indexed := e.variants(ctx, d, token)
	if len(sources) == 0 {
		e.writeError(w, http.StatusBadGateway, "NO_ORIGIN_VARIANTS", "origin returned no media variants")
		return true, nil
	}
	pvars := make([]policy.Variant, len(sources))
	for i, s := range sources {
		pvars[i] = s.toPolicy()
	}
	dec, err := policy.Evaluate(pol, scope, pvars)
	if perr, ok := err.(*policy.Error); ok && perr.Code == policy.NoAllowedVariant {
		e.persistDecision(ctx, "", d.RequestedIndex, -1, "deny", perr.Code, 0, dec.Rejected, pol)
		e.writePolicyError(w, perr.Code, "no media variant satisfies effective policy", dec.Rejected)
		return true, nil
	}
	if err != nil {
		e.writeError(w, http.StatusInternalServerError, "POLICY_EVAL_FAILED", "policy evaluation failed")
		return true, nil
	}
	spos := indexOf(sources, dec.SelectedIndex)
	if spos < 0 {
		e.writeError(w, http.StatusInternalServerError, "POLICY_EVAL_FAILED", "selected index not in candidates")
		return true, nil
	}
	selected := sources[spos]
	// Forward with the selected index and output cap applied.
	fwd, forwardErr := e.forward(ctx, r, d, token, dec.SelectedIndex, dec.OutputBitrateKbps)
	if forwardErr != nil {
		e.writeError(w, http.StatusBadGateway, "ORIGIN_UNAVAILABLE", "PMS decision unreachable")
		return true, nil
	}
	resp, err := parseDecisionResponse(fwd.body)
	if err != nil {
		e.writeError(w, http.StatusBadGateway, "DECISION_PARSE_FAILED", "PMS decision unreadable")
		return true, nil
	}
	// Only a successful PMS negotiation creates session state: an
	// error status or a body without any decision signal means PMS
	// itself rejected the request, and persisting it would create
	// phantom active sessions with misleading diagnostics.
	if fwd.status < 200 || fwd.status > 299 {
		e.persistDecision(ctx, "", d.RequestedIndex, d.RequestedIndex, "deny", "PMS_DECISION_REJECTED", resp.Code, dec.Rejected, pol)
		e.writeError(w, http.StatusBadGateway, "PMS_DECISION_REJECTED", "PMS rejected the playback negotiation")
		return true, nil
	}
	if resp.Code == 0 && !resp.DirectPlay && !resp.DirectStream && !resp.Transcode {
		e.persistDecision(ctx, "", d.RequestedIndex, d.RequestedIndex, "deny", "PMS_DECISION_REJECTED", 0, dec.Rejected, pol)
		e.writeError(w, http.StatusBadGateway, "PMS_DECISION_REJECTED", "PMS decision carries no usable negotiation outcome")
		return true, nil
	}
	// PMS disagreement: a rejected index back from PMS fails closed.
	if resp.HasIndex && resp.MediaIndex != dec.SelectedIndex {
		for _, rj := range dec.Rejected {
			if rj.MediaIndex == resp.MediaIndex {
				e.persistDecision(ctx, "", d.RequestedIndex, resp.MediaIndex, "deny", policy.OriginMismatch, resp.Code, dec.Rejected, pol)
				e.writePolicyError(w, policy.OriginMismatch, "PMS selected a policy-rejected source", dec.Rejected)
				return true, nil
			}
		}
		// Open policy and PMS picked another eligible index: accept the
		// PMS choice and track it, rather than fight the authority.
		if pos := indexOf(sources, resp.MediaIndex); pos >= 0 {
			selected = sources[pos]
			dec.SelectedIndex = resp.MediaIndex
		}
	}
	if modeOf(resp) == "transcode" && pol.AllowTranscode.Normalize() == policy.Deny {
		e.persistDecision(ctx, "", d.RequestedIndex, dec.SelectedIndex, "deny", policy.TranscodeForbidden, resp.Code, dec.Rejected, pol)
		e.writePolicyError(w, policy.TranscodeForbidden, "transcoding is denied and the eligible source requires it", dec.Rejected)
		return true, nil
	}
	sess, err := e.Store.Create(ctx, Session{
		PlexSessionID: sessionID, RatingKey: d.RatingKey,
		IdentityID: identityID, ClientUUID: clientUUID,
		SelectedMediaIndex: dec.SelectedIndex,
		SelectedPartPlexID: selected.PartPlexID, SelectedPartKey: selected.PartKey,
		SelectedVariantID: selected.VariantUUID, SelectedPartID: selected.PartUUID,
		PlaybackMode: modeOf(resp), RoutingMode: routingMode(pol),
		EffectivePolicy: marshalPolicy(pol),
	})
	if err != nil {
		// Persistence backs the part boundary: without it enforcement
		// cannot be proven later, so fail closed rather than serve an
		// untracked selection.
		e.writeError(w, http.StatusInternalServerError, "SESSION_PERSIST_FAILED", "playback session could not be recorded")
		return true, nil
	}
	e.persistDecision(ctx, sess.ID, d.RequestedIndex, dec.SelectedIndex, "allow", modeOf(resp), resp.Code, dec.Rejected, pol)
	if e.Metrics != nil {
		e.Metrics.IncPlaybackDecision()
	}
	w.Header().Set("Content-Type", fwd.contentType)
	w.WriteHeader(fwd.status)
	_, _ = w.Write(fwd.body)
	e.logDecision(id, d.RatingKey, fp, "decision", map[string]any{
		"selectedIndex": dec.SelectedIndex, "requestedIndex": d.RequestedIndex,
		"mode": modeOf(resp), "indexed": indexed,
		"outputCapKbps": dec.OutputBitrateKbps,
	})
	return true, nil
}
func indexOf(sources []variantSource, idx int) int {
	for i, s := range sources {
		if s.MediaIndex == idx {
			return i
		}
	}
	return -1
}

// strOrNil maps empty identity UUIDs to nil so the policy loader treats
// unresolved callers as global-only.
func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func routingMode(p policy.Policy) string {
	if p.RoutingMode != "" {
		return p.RoutingMode
	}
	return "automatic"
}

func marshalPolicy(p policy.Policy) []byte {
	raw, _ := json.Marshal(p)
	return raw
}
