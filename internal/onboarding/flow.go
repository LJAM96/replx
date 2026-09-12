package onboarding

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/LJAM96/replx-edge/internal/crypto"
	"github.com/LJAM96/replx-edge/internal/plextv"
	"github.com/LJAM96/replx-edge/internal/pms"
)

// TVClient is the plex.tv surface onboarding needs (real client or fake).
type TVClient interface {
	AuthURL(pin plextv.PIN) string
	CreatePIN(ctx context.Context) (plextv.PIN, error)
	PollPIN(ctx context.Context, id int64) (string, error)
	GetUser(ctx context.Context, token string) (plextv.User, error)
	ListServers(ctx context.Context, token string) ([]plextv.Resource, error)
}

const (
	setPINID      = "onboarding.pin_id"
	setPINCode    = "onboarding.pin_code"
	setOwnerToken = "onboarding.owner_token"
	setVerified   = "onboarding.verified"
)

// PINIssue is the administrator-facing claim material.
type PINIssue struct {
	AuthURL string `json:"authUrl"`
	Code    string `json:"code"`
	Stage   string `json:"stage"`
}

// IssuePIN creates the installation identity if needed, issues a strong
// PIN at plex.tv and persists the claim window so a restart never orphans it.
func (s *Service) IssuePIN(ctx context.Context) (PINIssue, error) {
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return PINIssue{}, err
	}
	tv := s.NewTV(id.ClientID)
	pin, err := tv.CreatePIN(ctx)
	if err != nil {
		return PINIssue{}, fmt.Errorf("onboarding: PIN: %w", err)
	}
	if err := setSetting(ctx, s.DB, setPINID, strconv.FormatInt(pin.ID, 10)); err != nil {
		return PINIssue{}, err
	}
	if err := setSetting(ctx, s.DB, setPINCode, pin.Code); err != nil {
		return PINIssue{}, err
	}
	return PINIssue{AuthURL: tv.AuthURL(pin), Code: pin.Code, Stage: StagePINIssued}, nil
}

// PollClaim checks whether the administrator claimed the PIN. On first
// success it validates the token, stores it encrypted as pending owner
// state and advances to owner_authenticated. The owner token is never
// returned to the caller.
func (s *Service) PollClaim(ctx context.Context) (bool, error) {
	rawID, ok := getSetting(ctx, s.DB, setPINID)
	if !ok {
		return false, fmt.Errorf("onboarding: no PIN issued")
	}
	pinID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return false, fmt.Errorf("onboarding: bad PIN state")
	}
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return false, err
	}
	token, err := s.NewTV(id.ClientID).PollPIN(ctx, pinID)
	if err != nil {
		return false, fmt.Errorf("onboarding: poll: %w", err)
	}
	if token == "" {
		return false, nil
	}
	user, err := s.NewTV(id.ClientID).GetUser(ctx, token)
	if err != nil {
		return false, fmt.Errorf("onboarding: token invalid: %w", err)
	}
	ct, err := crypto.Encrypt(s.Secret, PurposeOwnerJWT, []byte(token))
	if err != nil {
		return false, err
	}
	token = ""
	if err := setSetting(ctx, s.DB, setOwnerToken, base64.StdEncoding.EncodeToString(ct)); err != nil {
		return false, err
	}
	delSetting(ctx, s.DB, setPINID)
	delSetting(ctx, s.DB, setPINCode)
	_ = user
	return true, nil
}

// pendingOwnerToken decrypts the pre-selection owner token, if any.
func (s *Service) pendingOwnerToken(ctx context.Context) (string, bool) {
	raw, ok := getSetting(ctx, s.DB, setOwnerToken)
	if !ok {
		return "", false
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	pt, err := crypto.Decrypt(s.Secret, PurposeOwnerJWT, ct)
	if err != nil {
		return "", false
	}
	return string(pt), true
}

// ServerSummary is a browser-safe resource listing (no tokens).
type ServerSummary struct {
	ClientIdentifier string `json:"clientIdentifier"`
	Name             string `json:"name"`
	Connections      int    `json:"connections"`
	HTTPSDirect      bool   `json:"httpsDirect"`
}

// ListServers returns the selectable PMS resources for the owner token.
func (s *Service) ListServers(ctx context.Context) ([]ServerSummary, error) {
	token, ok := s.ownerTokenForAdmin(ctx)
	if !ok {
		return nil, fmt.Errorf("onboarding: authenticate first")
	}
	defer zero(&token)
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return nil, err
	}
	resources, err := s.NewTV(id.ClientID).ListServers(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("onboarding: resources: %w", err)
	}
	out := make([]ServerSummary, 0, len(resources))
	for _, r := range resources {
		out = append(out, ServerSummary{
			ClientIdentifier: r.ClientIdentifier,
			Name:             r.Name,
			Connections:      len(r.Connections),
			HTTPSDirect:      hasHTTPSDirect(r.Connections),
		})
	}
	return out, nil
}

// ownerTokenForAdmin resolves the pending token, else the stored credential.
func (s *Service) ownerTokenForAdmin(ctx context.Context) (string, bool) {
	if tok, ok := s.pendingOwnerToken(ctx); ok {
		return tok, true
	}
	var ct []byte
	if err := s.DB.QueryRow(ctx, `SELECT c.owner_token_ciphertext FROM plex_owner_credentials c
		JOIN plex_servers sv ON sv.id=c.server_id WHERE sv.enabled ORDER BY sv.created_at DESC LIMIT 1`).Scan(&ct); err != nil || ct == nil {
		return "", false
	}
	pt, err := crypto.Decrypt(s.Secret, PurposeOwnerJWT, ct)
	if err != nil {
		return "", false
	}
	return string(pt), true
}

// SelectResource binds exactly one PMS resource: it verifies the internal
// origin reports the same machineIdentifier, picks the client reachable
// media origin, then creates the single enabled server row and its owner
// credential. Production 1.0 rejects a second active origin.
func (s *Service) SelectResource(ctx context.Context, clientIdentifier string) (SelectReport, error) {
	token, ok := s.ownerTokenForAdmin(ctx)
	if !ok {
		return SelectReport{}, fmt.Errorf("onboarding: authenticate first")
	}
	defer zero(&token)
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return SelectReport{}, err
	}
	tv := s.NewTV(id.ClientID)
	user, err := tv.GetUser(ctx, token)
	if err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: owner token invalid: %w", err)
	}
	resources, err := tv.ListServers(ctx, token)
	if err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: resources: %w", err)
	}
	var picked *plextv.Resource
	for i, r := range resources {
		if r.ClientIdentifier == clientIdentifier {
			picked = &resources[i]
			break
		}
	}
	if picked == nil {
		return SelectReport{}, fmt.Errorf("onboarding: unknown resource")
	}
	if picked.AccessToken == "" {
		return SelectReport{}, fmt.Errorf("onboarding: resource has no access token")
	}
	originID, err := pms.FetchIdentity(s.InternalURL, picked.AccessToken)
	if err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: origin unreachable: %w", err)
	}
	if originID.MachineIdentifier != picked.ClientIdentifier {
		return SelectReport{}, fmt.Errorf("onboarding: origin machineIdentifier %q != resource %q: refusing to bind the wrong server",
			originID.MachineIdentifier, picked.ClientIdentifier)
	}
	mediaOrigin, err := SelectMediaOrigin(picked.Connections, publicHost(s.PublicURL))
	if err != nil {
		return SelectReport{}, err
	}
	pmsCipher, err := crypto.Encrypt(s.Secret, PurposePMSToken, []byte(picked.AccessToken))
	if err != nil {
		return SelectReport{}, err
	}
	ownerCipher, err := crypto.Encrypt(s.Secret, PurposeOwnerJWT, []byte(token))
	if err != nil {
		return SelectReport{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return SelectReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize concurrent selections and satisfy the single-enabled
	// partial unique index: disable first, then insert.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('replx_edge_onboarding'))"); err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE plex_servers SET enabled=false WHERE enabled"); err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: single-origin guard: %w", err)
	}
	var serverID string
	err = tx.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, client_media_origin_url, machine_identifier, friendly_name, plex_version, connection_status)
		VALUES($1, $2, $3, $4, $5, $6, 'unknown') RETURNING id`,
		picked.Name, s.InternalURL, mediaOrigin, originID.MachineIdentifier, originID.FriendlyName, originID.Version).Scan(&serverID)
	if err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: save server: %w", err)
	}
	var jwkPublic string
	var jwkPriv []byte
	if err := tx.QueryRow(ctx, "SELECT jwk_public, jwk_private_ciphertext FROM app_identity WHERE id='singleton'").Scan(&jwkPublic, &jwkPriv); err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO plex_owner_credentials(server_id, plex_account_id, replx_edge_client_identifier, jwk_public, jwk_private_ciphertext, owner_token_ciphertext, pms_access_token_ciphertext, selected_resource_id, last_refreshed_at, status)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, now(), 'selected')
		ON CONFLICT (server_id) DO UPDATE SET plex_account_id=EXCLUDED.plex_account_id, owner_token_ciphertext=EXCLUDED.owner_token_ciphertext,
		pms_access_token_ciphertext=EXCLUDED.pms_access_token_ciphertext, selected_resource_id=EXCLUDED.selected_resource_id,
		last_refreshed_at=now(), status='selected'`,
		serverID, user.ID, id.ClientID, jwkPublic, jwkPriv, ownerCipher, pmsCipher, picked.ClientIdentifier); err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: save credentials: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SelectReport{}, fmt.Errorf("onboarding: commit: %w", err)
	}
	delSetting(ctx, s.DB, setOwnerToken)
	return SelectReport{MachineIdentifier: originID.MachineIdentifier, MediaOrigin: mediaOrigin, Stage: StageSelected}, nil
}

// SelectReport summarizes a successful resource binding (no secrets).
type SelectReport struct {
	MachineIdentifier string `json:"machineIdentifier"`
	MediaOrigin       string `json:"mediaOrigin"`
	Stage             string `json:"stage"`
}

// Status reports the current onboarding stage for the admin panel.
func (s *Service) Status(ctx context.Context) map[string]any {
	out := map[string]any{"stage": StagePending}
	if _, ok := getSetting(ctx, s.DB, setPINID); ok {
		out["stage"] = StagePINIssued
		if code, ok := getSetting(ctx, s.DB, setPINCode); ok {
			out["pinCode"] = code
		}
	}
	if _, ok := s.pendingOwnerToken(ctx); ok {
		out["stage"] = StageOwnerAuthenticated
	}
	var machineID string
	if err := s.DB.QueryRow(ctx, "SELECT machine_identifier FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1").Scan(&machineID); err == nil {
		out["stage"] = StageSelected
		out["machineIdentifier"] = machineID
	}
	if v, ok := getSetting(ctx, s.DB, setVerified); ok && v == "true" {
		out["stage"] = StageVerified
	}
	return out
}

func publicHost(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func hasHTTPSDirect(conns []plextv.Connection) bool {
	for _, c := range conns {
		if strings.EqualFold(c.Protocol, "https") && !c.Relay {
			return true
		}
	}
	return false
}

func zero(s *string) {
	if s == nil {
		return
	}
	b := []byte(*s)
	for i := range b {
		b[i] = 0
	}
	*s = ""
}
