package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/pms"
)

// TVClient is the plex.tv surface onboarding needs (real client or fake).
type TVClient interface {
	AuthURL(pin plextv.PIN) string
	CreatePIN(ctx context.Context) (plextv.PIN, error)
	CreatePINJWT(ctx context.Context, jwk json.RawMessage) (plextv.PIN, error)
	PollPIN(ctx context.Context, id int64) (string, error)
	PollPINJWT(ctx context.Context, id int64, deviceJWT string) (string, error)
	GetUser(ctx context.Context, token string) (plextv.User, error)
	ListServers(ctx context.Context, token string) ([]plextv.Resource, error)
	Nonce(ctx context.Context, token string) (string, error)
	RefreshToken(ctx context.Context, deviceJWT string) (string, error)
}

// Auth modes persisted in app_settings.
const (
	authModeJWT    = "jwt"
	authModeLegacy = "legacy"
)

const (
	setPINID      = "onboarding.pin_id"
	setPINCode    = "onboarding.pin_code"
	setOwnerToken = "onboarding.owner_token"
	setVerified   = "onboarding.verified"
	setAuthMode   = "onboarding.auth_mode"
	setOwnerExp   = "onboarding.owner_expires"
)

// PINIssue is the administrator-facing claim material.
type PINIssue struct {
	AuthURL  string `json:"authUrl"`
	Code     string `json:"code"`
	Stage    string `json:"stage"`
	AuthMode string `json:"authMode"`
}

// IssuePIN creates the installation identity if needed, issues a strong
// PIN bound to the device JWK (JWT flow), and persists the claim window.
// If plex.tv rejects the JWT shape, it falls back to the legacy PIN and
// records the mode so polling matches. The mode is surfaced in Status.
func (s *Service) IssuePIN(ctx context.Context) (PINIssue, error) {
	id, _, jwk, err := s.DeviceCredentials(ctx)
	if err != nil {
		return PINIssue{}, err
	}
	tv := s.NewTV(id)
	mode := authModeJWT
	pin, err := tv.CreatePINJWT(ctx, jwk)
	if err != nil {
		if !plextv.IsClientError(err) {
			return PINIssue{}, fmt.Errorf("onboarding: PIN: %w", err)
		}
		mode = authModeLegacy
		if pin, err = tv.CreatePIN(ctx); err != nil {
			return PINIssue{}, fmt.Errorf("onboarding: PIN: %w", err)
		}
	}
	if err := setSetting(ctx, s.DB, setAuthMode, mode); err != nil {
		return PINIssue{}, err
	}
	// New issuance voids any prior verification.
	delSetting(ctx, s.DB, setVerified)
	if err := setSetting(ctx, s.DB, setPINID, strconv.FormatInt(pin.ID, 10)); err != nil {
		return PINIssue{}, err
	}
	if err := setSetting(ctx, s.DB, setPINCode, pin.Code); err != nil {
		return PINIssue{}, err
	}
	return PINIssue{AuthURL: tv.AuthURL(pin), Code: pin.Code, Stage: StagePINIssued, AuthMode: mode}, nil
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
	clientID, seed, _, err := s.DeviceCredentials(ctx)
	if err != nil {
		return false, err
	}
	tv := s.NewTV(clientID)
	mode, _ := getSetting(ctx, s.DB, setAuthMode)
	token, err := s.pollByMode(ctx, tv, pinID, mode, clientID, seed)
	if err != nil {
		return false, err
	}
	if token == "" {
		return false, nil
	}
	user, err := tv.GetUser(ctx, token)
	if err != nil {
		return false, fmt.Errorf("onboarding: token invalid: %w", err)
	}
	ct, err := crypto.Encrypt(s.Secret, PurposeOwnerJWT, []byte(token))
	if err != nil {
		return false, err
	}
	if exp, ok := plextv.ParseExpiry(token); ok {
		_ = setSetting(ctx, s.DB, setOwnerExp, strconv.FormatInt(exp.Unix(), 10))
	} else {
		delSetting(ctx, s.DB, setOwnerExp)
	}
	for i := range seed {
		seed[i] = 0
	}
	token = ""
	if err := setSetting(ctx, s.DB, setOwnerToken, base64.StdEncoding.EncodeToString(ct)); err != nil {
		return false, err
	}
	delSetting(ctx, s.DB, setPINID)
	delSetting(ctx, s.DB, setPINCode)
	delSetting(ctx, s.DB, setVerified) // new authentication voids prior verification
	_ = user
	return true, nil
}

// pollByMode polls for the claim in the issuance mode, falling back from
// JWT to legacy once if plex.tv rejects the device JWT.
func (s *Service) pollByMode(ctx context.Context, tv TVClient, pinID int64, mode, clientID string, seed []byte) (string, error) {
	if mode != authModeLegacy {
		signer, err := deviceSigner(clientID, seed)
		if err != nil {
			return "", err
		}
		deviceJWT, err := signer.Sign("plex.tv", nil)
		if err != nil {
			return "", err
		}
		token, err := tv.PollPINJWT(ctx, pinID, deviceJWT)
		if err == nil {
			return token, nil
		}
		if !plextv.IsClientError(err) {
			return "", fmt.Errorf("onboarding: poll: %w", err)
		}
		_ = setSetting(ctx, s.DB, setAuthMode, authModeLegacy)
	}
	token, err := tv.PollPIN(ctx, pinID)
	if err != nil {
		return "", fmt.Errorf("onboarding: poll: %w", err)
	}
	return token, nil
}

// deviceSigner builds the EdDSA signer. The kid matches the registered
// JWK (first 8 client ID hex chars, as stored at identity creation).
func deviceSigner(clientID string, seed []byte) (plextv.DeviceSigner, error) {
	if len(seed) != 32 {
		return plextv.DeviceSigner{}, fmt.Errorf("onboarding: bad device seed")
	}
	return plextv.DeviceSigner{Seed: append([]byte(nil), seed...), Kid: kidFor(clientID), ClientID: clientID}, nil
}

func kidFor(clientID string) string {
	if len(clientID) >= 8 {
		return clientID[:8]
	}
	return clientID
}

// SubmitToken is the backup onboarding path: the operator pastes an
// existing Plex token (e.g. when the PIN flow is unreachable). The token
// is validated with GetUser before storage and kept encrypted exactly
// like a PIN-claimed one, but it is always treated as legacy: refresh
// signs with this installation's device key, which a token minted for
// another client will not renew. Re-onboarding is required on 401.
func (s *Service) SubmitToken(ctx context.Context, rawToken string) (string, error) {
	token := strings.TrimSpace(rawToken)
	if len(token) < 8 {
		return "", fmt.Errorf("onboarding: token too short to be valid")
	}
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return "", err
	}
	user, err := s.NewTV(id.ClientID).GetUser(ctx, token)
	if err != nil {
		return "", fmt.Errorf("onboarding: token invalid: %w", err)
	}
	ct, err := crypto.Encrypt(s.Secret, PurposeOwnerJWT, []byte(token))
	if err != nil {
		return "", err
	}
	if exp, ok := plextv.ParseExpiry(token); ok {
		_ = setSetting(ctx, s.DB, setOwnerExp, strconv.FormatInt(exp.Unix(), 10))
	} else {
		delSetting(ctx, s.DB, setOwnerExp)
	}
	tb := []byte(token)
	for i := range tb {
		tb[i] = 0
	}
	token = ""
	if err := setSetting(ctx, s.DB, setOwnerToken, base64.StdEncoding.EncodeToString(ct)); err != nil {
		return "", err
	}
	// A pasted credential replaces any in-flight PIN and voids prior
	// verification, exactly like a fresh claim.
	delSetting(ctx, s.DB, setPINID)
	delSetting(ctx, s.DB, setPINCode)
	delSetting(ctx, s.DB, setVerified)
	_ = setSetting(ctx, s.DB, setAuthMode, authModeLegacy)
	return user.Username, nil
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
// Connection URIs carry no credentials, so their hosts are shown to help
// diagnose unpublished Custom Server Access URLs.
type ServerSummary struct {
	ClientIdentifier string   `json:"clientIdentifier"`
	Name             string   `json:"name"`
	Connections      int      `json:"connections"`
	HTTPSDirect      bool     `json:"httpsDirect"`
	ConnectionHosts  []string `json:"connectionHosts"`
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
			ConnectionHosts:  connectionHosts(r.Connections),
		})
	}
	return out, nil
}

// connectionHosts lists distinct URI hostnames for diagnostics.
func connectionHosts(conns []plextv.Connection) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range conns {
		u, err := url.Parse(c.URI)
		if err != nil || u.Host == "" || seen[u.Host] {
			continue
		}
		seen[u.Host] = true
		out = append(out, u.Host)
	}
	return out
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
	// A new selection voids any prior verification: verified state is
	// always tied to the currently enabled server (see Status).
	delSetting(ctx, s.DB, setVerified)
	return SelectReport{MachineIdentifier: originID.MachineIdentifier, MediaOrigin: mediaOrigin, Stage: StageSelected}, nil
}

// SelectReport summarizes a successful resource binding (no secrets).
type SelectReport struct {
	MachineIdentifier string `json:"machineIdentifier"`
	MediaOrigin       string `json:"mediaOrigin"`
	Stage             string `json:"stage"`
}

// Status reports the current onboarding stage for the admin panel.
// Verification is tied to the enabled server: verified is reported only
// when the stored verification machine matches it, so reselecting a
// server can never inherit a stale verified state.
func (s *Service) Status(ctx context.Context) map[string]any {
	out := map[string]any{"stage": StagePending}
	if mode, ok := getSetting(ctx, s.DB, setAuthMode); ok {
		out["authMode"] = mode
	}
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
	if v, ok := getSetting(ctx, s.DB, setVerified); ok && v == machineID && machineID != "" {
		out["stage"] = StageVerified
	}
	return out
}

// RefreshOwnerJWT refreshes a JWT-mode owner credential when it expires
// within 24h (nonce flow). Legacy tokens have no refresh: they are
// validated on use and require re-onboarding on 401. Returns true when a
// new token was stored. Failures degrade (status auth_degraded) without
// destroying the existing credential.
func (s *Service) RefreshOwnerJWT(ctx context.Context) (bool, error) {
	mode, _ := getSetting(ctx, s.DB, setAuthMode)
	if mode == authModeLegacy {
		return false, nil
	}
	rawExp, ok := getSetting(ctx, s.DB, setOwnerExp)
	if !ok {
		return false, nil
	}
	expUnix, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil || time.Until(time.Unix(expUnix, 0)) > 24*time.Hour {
		return false, nil
	}
	var serverID string
	var ownerCipher []byte
	if err := s.DB.QueryRow(ctx, `SELECT server_id, owner_token_ciphertext FROM plex_owner_credentials
		JOIN plex_servers ON plex_servers.id=server_id WHERE plex_servers.enabled
		ORDER BY plex_servers.created_at DESC LIMIT 1`).Scan(&serverID, &ownerCipher); err != nil {
		return false, nil
	}
	owner, err := crypto.Decrypt(s.Secret, PurposeOwnerJWT, ownerCipher)
	if err != nil {
		return false, err
	}
	defer zeroBytes(owner)
	clientID, seed, _, err := s.DeviceCredentials(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	signer, err := deviceSigner(clientID, seed)
	if err != nil {
		return false, err
	}
	tv := s.NewTV(clientID)
	nonce, err := tv.Nonce(ctx, string(owner))
	if err != nil {
		return s.degraded(serverID, err)
	}
	deviceJWT, err := signer.Sign("plex.tv", map[string]any{"nonce": nonce})
	if err != nil {
		return false, err
	}
	fresh, err := tv.RefreshToken(ctx, deviceJWT)
	if err != nil {
		return s.degraded(serverID, err)
	}
	freshCipher, err := crypto.Encrypt(s.Secret, PurposeOwnerJWT, []byte(fresh))
	if err != nil {
		return false, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE plex_owner_credentials SET owner_token_ciphertext=$1,
		last_refreshed_at=now(), status='verified', last_error=NULL WHERE server_id=$2`, freshCipher, serverID); err != nil {
		return false, err
	}
	if exp, ok := plextv.ParseExpiry(fresh); ok {
		_ = setSetting(ctx, s.DB, setOwnerExp, strconv.FormatInt(exp.Unix(), 10))
	}
	fb := []byte(fresh)
	for i := range fb {
		fb[i] = 0
	}
	return true, nil
}

func (s *Service) degraded(serverID string, err error) (bool, error) {
	_, _ = s.DB.Exec(context.Background(), `UPDATE plex_owner_credentials SET status='auth_degraded',
		last_error=$1 WHERE server_id=$2`, err.Error(), serverID)
	return false, fmt.Errorf("onboarding: refresh degraded: %w", err)
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
