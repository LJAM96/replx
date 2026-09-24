// Package identity resolves request fingerprints to Plex identities.
//
// Resolution order per request: process-local cache, token-link table,
// plex.tv account lookup (cold fingerprints only), fingerprint fallback.
// plex.tv is never on the steady-state hot path: only the first sighting
// of a token pays the lookup, and only definitive 401/403 responses earn
// the one-hour negative cache. Transport and 5xx failures degrade without
// persisting anything: a previous link still resolves from the table, and
// a cold token simply retries next time.
//
// Two caches stay separate on purpose: account identity is keyed by token
// fingerprint, but the client instance is keyed by fingerprint PLUS client
// identifier, so one device's client UUID (and therefore its device-level
// policy) can never leak into another device sharing the same user token.
//
// Scope strings key the response cache: "acct:<accountID>" when resolved
// (all devices of one user share entries), "tok:<fingerprint>" otherwise.
// Validated user tokens may be encrypted for user-scoped cache refreshes.
// Invalid tokens are never retained.
//
// Client instances and identity bindings are recorded on resolution for
// administration and capability precedence; they never gate traffic.
package identity

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/trace"
)

// revalidateAfter bounds plex.tv traffic for definitively bad tokens.
const revalidateAfter = time.Hour

// credentialValidityPeriod bounds how long a linked credential authorizes
// locally served responses without fresh proof. Identity association (who
// this fingerprint belonged to) is kept indefinitely for scoping and
// policy, but local serving (long lived artwork) requires validity proven
// within this window. plex.tv is consulted at most once per memTTL per
// fingerprint, never on the steady-state hot path.
const credentialValidityPeriod = 24 * time.Hour

// memTTL bounds the process-local resolution caches.
const memTTL = 5 * time.Minute

const memMax = 4096

// Account is the minimal plex.tv account surface the resolver needs.
type Account interface {
	GetUser(ctx context.Context, token string) (id int64, username string, err error)
}

// TokenValidator checks whether the configured PMS accepts a token. A Plex
// Home managed-user token can be valid for PMS while plex.tv's account API
// rejects it. Such tokens are cached only in their own fingerprint scope.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (bool, error)
}

// Client is the Plex client identity for instance recording.
type Client struct {
	Identifier string
	Product    string
	Version    string
	Platform   string
	Device     string
	Model      string
}

// FromTrace converts trace client identity.
func FromTrace(c trace.Client) Client {
	return Client{Identifier: c.ID, Product: c.Product, Version: c.Version,
		Platform: c.Platform, Device: c.Device, Model: c.Model}
}

// Resolved is one fingerprint's identity outcome. Identity association,
// credential validity and library authorization are separate concepts:
//   - Known means the fingerprint links to a Plex account (scoping, policy).
//   - Fresh means the credential was proven valid within
//     credentialValidityPeriod (authorizes locally served responses).
//   - Invalid means plex.tv definitively rejected the credential recently:
//     callers must bypass local serving and let PMS answer (usually 401).
type Resolved struct {
	Scope      string // cache scope: acct:<id> or tok:<fingerprint>
	IdentityID string // plex_identities UUID, "" when unresolved
	ClientID   string // client_instances UUID, "" when unrecorded
	AccountID  int64
	Known      bool // account proven via link table or plex.tv
	Fresh      bool // credential proven within the validity window
	Invalid    bool // definitively rejected recently; do not serve locally
	Degraded   bool // known links keep working; cold tokens retry soon
}

// Resolver links fingerprints to identities.
type Resolver struct {
	DB     database.DBTX
	TV     Account
	PMS    TokenValidator
	Secret string // enables encrypted, validated token retention for warming
	mu     sync.Mutex
	acct   map[string]acctEntry
	cli    map[string]cliEntry
	server string
	srvAt  time.Time
}

type acctEntry struct {
	scope      string
	identityID string
	accountID  int64
	known      bool
	fresh      bool
	invalid    bool
	at         time.Time
}

type cliEntry struct {
	clientID string
	at       time.Time
}

// New builds a Resolver. Nil DB resolves nothing (all fingerprints stay
// token-scoped); nil TV skips live lookup (links only).
func New(db database.DBTX, tv Account) *Resolver {
	return &Resolver{DB: db, TV: tv, acct: map[string]acctEntry{}, cli: map[string]cliEntry{}}
}

// Resolve maps fingerprint+token+client to identity. Token is used for
// cold-start plex.tv lookup only and is never stored.
func (r *Resolver) Resolve(ctx context.Context, fingerprint, token string, client Client) Resolved {
	if r == nil || fingerprint == "" {
		return Resolved{Scope: "tok:" + fingerprint}
	}
	now := time.Now()
	r.mu.Lock()
	acct, acctOK := r.acct[fingerprint]
	cliKey := fingerprint + "\x00" + client.Identifier
	cli, cliOK := r.cli[cliKey]
	r.mu.Unlock()
	if acctOK && now.Sub(acct.at) < memTTL {
		out := Resolved{Scope: acct.scope, IdentityID: acct.identityID, AccountID: acct.accountID,
			Known: acct.known, Fresh: acct.fresh, Invalid: acct.invalid}
		if client.Identifier != "" {
			if cliOK && now.Sub(cli.at) < memTTL {
				out.ClientID = cli.clientID
				return out
			}
			// Same account, new device (or expired device entry):
			// resolve the client independently, never inheriting a
			// sibling device's instance.
			out.ClientID = r.resolveClient(ctx, acct.identityID, client)
			if out.ClientID != "" {
				r.mu.Lock()
				r.cli[cliKey] = cliEntry{clientID: out.ClientID, at: now}
				r.mu.Unlock()
			}
		}
		return out
	}
	res, store := r.resolveCold(ctx, fingerprint, token, client)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store {
		if len(r.acct) >= memMax {
			r.evictAcctLocked()
		}
		r.acct[fingerprint] = acctEntry{scope: res.Scope, identityID: res.IdentityID,
			accountID: res.AccountID, known: res.Known, fresh: res.Fresh,
			invalid: res.Invalid, at: now}
	}
	if client.Identifier == "" {
		return res
	}
	if len(r.cli) >= memMax {
		r.evictCliLocked()
	}
	// Never cache an empty client result: a failed recording would pin
	// the device to no-instance for five minutes. Retry next request.
	if res.ClientID != "" {
		r.cli[cliKey] = cliEntry{clientID: res.ClientID, at: now}
	}
	return res
}

// resolveCold performs the slow path: link table, then plex.tv. The bool
// reports whether the outcome may be cached: degraded failures must retry
// soon, never sit in cache for five minutes.
func (r *Resolver) resolveCold(ctx context.Context, fingerprint, token string, client Client) (Resolved, bool) {
	fallback := Resolved{Scope: "tok:" + fingerprint}
	if r.DB == nil {
		return fallback, true
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	serverID := r.enabledServer(cctx)
	if serverID == "" {
		return fallback, true
	}
	// Known link? Identity association and credential validity are read
	// together but interpreted separately below.
	var identityID *string
	var accountID *int64
	var status string
	var validatedAt *time.Time
	err := r.DB.QueryRow(cctx, `SELECT t.identity_id::text, i.plex_account_id, t.token_status,
		t.last_validated_at
		FROM plex_token_identities t LEFT JOIN plex_identities i ON i.id = t.identity_id
		WHERE t.server_id=$1 AND t.token_fingerprint=$2`, serverID, fingerprint).
		Scan(&identityID, &accountID, &status, &validatedAt)
	linked := err == nil && identityID != nil && *identityID != "" && accountID != nil
	if status == "pms_valid" && validatedAt != nil && time.Since(*validatedAt) < memTTL {
		r.persistToken(cctx, serverID, fingerprint, token)
		return Resolved{Scope: fallback.Scope, Fresh: true}, true
	}
	if status == "invalid" {
		// Definitive rejection on record: never known. Revalidate at
		// most hourly so a revoked credential cannot hammer plex.tv
		// back into validity through request volume.
		if validatedAt != nil && time.Since(*validatedAt) < revalidateAfter {
			return r.resolvePMS(cctx, serverID, fingerprint, token)
		}
	} else if linked {
		fresh := validatedAt != nil && time.Since(*validatedAt) < credentialValidityPeriod
		if fresh || r.TV == nil || token == "" {
			// Proven within the validity window, or nothing available
			// to revalidate with: the association holds and freshness
			// reflects the last proof.
			res := Resolved{Scope: "acct:" + strconv.FormatInt(*accountID, 10),
				IdentityID: *identityID, AccountID: *accountID, Known: true, Fresh: fresh}
			res.ClientID = r.recordClient(cctx, serverID, *identityID, client)
			r.touchLink(cctx, serverID, fingerprint)
			if fresh {
				r.persistToken(cctx, serverID, fingerprint, token)
			}
			return res, true
		}
		// Stale proof: revalidate the credential now.
		id, username, verr := r.TV.GetUser(cctx, token)
		if verr == nil && id != 0 {
			iid := r.upsertIdentity(cctx, serverID, id, username)
			r.upsertLink(cctx, serverID, fingerprint, iid)
			r.persistToken(cctx, serverID, fingerprint, token)
			if iid == "" {
				return fallback, true
			}
			return Resolved{Scope: "acct:" + strconv.FormatInt(id, 10), AccountID: id,
				IdentityID: iid, Known: true, Fresh: true,
				ClientID: r.recordClient(cctx, serverID, iid, client)}, true
		}
		if isAuthFailure(verr) {
			// Definitively rejected: break the association as well as
			// the validity so the stale identity cannot linger.
			r.markInvalid(cctx, serverID, fingerprint)
			return r.resolvePMS(cctx, serverID, fingerprint, token)
		}
		// Transport/5xx: keep the association for scoping and policy
		// but mark it unfresh so long lived local responses fall
		// through to PMS; retry validation soon.
		res := Resolved{Scope: "acct:" + strconv.FormatInt(*accountID, 10),
			IdentityID: *identityID, AccountID: *accountID, Known: true,
			Degraded: true}
		res.ClientID = r.recordClient(cctx, serverID, *identityID, client)
		return res, false
	}
	if r.TV == nil || token == "" {
		return fallback, true
	}
	id, username, err := r.TV.GetUser(cctx, token)
	if err != nil {
		if isAuthFailure(err) {
			r.markInvalid(cctx, serverID, fingerprint)
			return r.resolvePMS(cctx, serverID, fingerprint, token)
		}
		// Transport/5xx/decode: degrade without persisting anything.
		// Established links above keep resolving; cold tokens retry.
		return Resolved{Scope: fallback.Scope, Degraded: true}, false
	}
	if id == 0 {
		return fallback, true
	}
	iid := r.upsertIdentity(cctx, serverID, id, username)
	r.upsertLink(cctx, serverID, fingerprint, iid)
	r.persistToken(cctx, serverID, fingerprint, token)
	if iid == "" {
		// Account proven by plex.tv but not persistable: stay
		// token-scoped rather than claim an identity we cannot bind.
		return fallback, true
	}
	return Resolved{Scope: "acct:" + strconv.FormatInt(id, 10), AccountID: id,
		IdentityID: iid, Known: true, Fresh: true,
		ClientID: r.recordClient(cctx, serverID, iid, client)}, true
}

// resolveClient records (or reuses) the client instance for one device.
// It never consults another device's cached instance.
func (r *Resolver) resolveClient(ctx context.Context, identityID string, client Client) string {
	if r.DB == nil || client.Identifier == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	serverID := r.enabledServer(cctx)
	if serverID == "" {
		return ""
	}
	return r.recordClient(cctx, serverID, identityID, client)
}

// isAuthFailure reports definitive credential rejection (401/403): the
// only failures that earn the negative cache. Rate limits, server errors
// and transport failures must retry soon instead.
func isAuthFailure(err error) bool {
	var se *plextv.StatusError
	if errors.As(err, &se) {
		return se.StatusCode == 401 || se.StatusCode == 403
	}
	return false
}

func (r *Resolver) resolvePMS(ctx context.Context, serverID, fingerprint, token string) (Resolved, bool) {
	fallback := Resolved{Scope: "tok:" + fingerprint, Invalid: true}
	if r.PMS == nil || token == "" {
		return fallback, true
	}
	valid, err := r.PMS.ValidateToken(ctx, token)
	if err != nil {
		// Uncertain validation never authorizes a cached response. Retry
		// after the process-local negative cache expires.
		fallback.Degraded = true
		return fallback, true
	}
	if !valid {
		r.clearStoredToken(ctx, serverID, fingerprint)
		return fallback, true
	}
	_, _ = r.DB.Exec(ctx, `INSERT INTO plex_token_identities(server_id, token_fingerprint, token_status, last_seen_at, last_validated_at)
		VALUES($1,$2,'pms_valid',now(),now())
		ON CONFLICT (server_id, token_fingerprint) DO UPDATE SET
			identity_id=NULL, token_status='pms_valid', last_seen_at=now(), last_validated_at=now()`, serverID, fingerprint)
	r.persistToken(ctx, serverID, fingerprint, token)
	return Resolved{Scope: "tok:" + fingerprint, Fresh: true}, true
}

const PurposeUserToken = "user-token-warming"

func (r *Resolver) persistToken(ctx context.Context, serverID, fingerprint, token string) {
	if r.Secret == "" || token == "" || r.DB == nil || crypto.Fingerprint(r.Secret, token) != fingerprint {
		return
	}
	ciphertext, err := crypto.Encrypt(r.Secret, PurposeUserToken, []byte(token))
	if err != nil {
		return
	}
	_, _ = r.DB.Exec(ctx, `UPDATE plex_token_identities SET token_ciphertext=$3
		WHERE server_id=$1 AND token_fingerprint=$2 AND token_ciphertext IS NULL
		AND token_status IN ('valid','pms_valid')`, serverID, fingerprint, ciphertext)
}

func (r *Resolver) clearStoredToken(ctx context.Context, serverID, fingerprint string) {
	if r.DB != nil {
		_, _ = r.DB.Exec(ctx, `UPDATE plex_token_identities SET token_ciphertext=NULL
			WHERE server_id=$1 AND token_fingerprint=$2`, serverID, fingerprint)
	}
}

func (r *Resolver) enabledServer(ctx context.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server != "" && time.Since(r.srvAt) < time.Minute {
		return r.server
	}
	// DB is always non-nil here (callers check), but stay total anyway.
	if r.DB == nil {
		return ""
	}
	var id string
	if err := r.DB.QueryRow(ctx, `SELECT id FROM plex_servers
		WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		return ""
	}
	r.server, r.srvAt = id, time.Now()
	return id
}

func (r *Resolver) evictAcctLocked() {
	oldest := ""
	var oldestAt time.Time
	first := true
	for k, e := range r.acct {
		if first || e.at.Before(oldestAt) {
			oldest, oldestAt, first = k, e.at, false
		}
	}
	delete(r.acct, oldest)
}

func (r *Resolver) evictCliLocked() {
	oldest := ""
	var oldestAt time.Time
	first := true
	for k, e := range r.cli {
		if first || e.at.Before(oldestAt) {
			oldest, oldestAt, first = k, e.at, false
		}
	}
	delete(r.cli, oldest)
}

func (r *Resolver) upsertIdentity(ctx context.Context, serverID string, accountID int64, username string) string {
	// No ON CONFLICT arbiter and no fallback-after-error: a failed
	// statement inside a transaction aborts it, so a fallback SELECT
	// after an error can never work. INSERT ... DO NOTHING needs no
	// inference, then SELECT reads what won (ours or a concurrent row).
	if _, err := r.DB.Exec(ctx, `INSERT INTO plex_identities(server_id, plex_account_id, username, identity_type, updated_at)
		VALUES($1,$2,$3,'user',now())
		ON CONFLICT DO NOTHING`, serverID, accountID, username); err != nil {
		return ""
	}
	var id string
	if err := r.DB.QueryRow(ctx, `SELECT id FROM plex_identities WHERE server_id=$1 AND plex_account_id=$2`,
		serverID, accountID).Scan(&id); err != nil {
		return ""
	}
	_, _ = r.DB.Exec(ctx, `UPDATE plex_identities SET username=$2, updated_at=now() WHERE id=$1`, id, username)
	return id
}

func (r *Resolver) upsertLink(ctx context.Context, serverID, fingerprint, identityID string) {
	_, _ = r.DB.Exec(ctx, `INSERT INTO plex_token_identities(server_id, identity_id, token_fingerprint, token_status, last_seen_at, last_validated_at)
		VALUES($1,$2,$3,'valid',now(),now())
		ON CONFLICT (server_id, token_fingerprint) DO UPDATE SET
			identity_id=EXCLUDED.identity_id, token_status='valid', last_seen_at=now(), last_validated_at=now()`,
		serverID, nullIfEmpty(identityID), fingerprint)
}

func (r *Resolver) markInvalid(ctx context.Context, serverID, fingerprint string) {
	// Definitive rejection breaks the identity association as well as
	// validity: the stale account link must not linger as Known.
	_, _ = r.DB.Exec(ctx, `INSERT INTO plex_token_identities(server_id, token_fingerprint, token_status, last_seen_at, last_validated_at)
		VALUES($1,$2,'invalid',now(),now())
		ON CONFLICT (server_id, token_fingerprint) DO UPDATE SET
			identity_id=NULL, token_ciphertext=NULL, token_status='invalid', last_seen_at=now(), last_validated_at=now()`,
		serverID, fingerprint)
}

func (r *Resolver) touchLink(ctx context.Context, serverID, fingerprint string) {
	_, _ = r.DB.Exec(ctx, `UPDATE plex_token_identities SET last_seen_at=now()
		WHERE server_id=$1 AND token_fingerprint=$2
		AND last_seen_at < now() - make_interval(hours => 1)`, serverID, fingerprint)
}

// recordClient upserts the client instance and identity binding. Failures
// degrade silently: administration must never gate traffic.
func (r *Resolver) recordClient(ctx context.Context, serverID, identityID string, client Client) string {
	if client.Identifier == "" {
		return ""
	}
	var clientID string
	err := r.DB.QueryRow(ctx, `INSERT INTO client_instances(server_id, plex_client_identifier, product, product_version, platform, device, model, last_seen_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (server_id, plex_client_identifier) DO UPDATE SET
			product=EXCLUDED.product, product_version=EXCLUDED.product_version,
			platform=EXCLUDED.platform, device=EXCLUDED.device, model=EXCLUDED.model,
			last_seen_at=now() RETURNING id`,
		serverID, client.Identifier, nullIfEmpty(client.Product), nullIfEmpty(client.Version),
		nullIfEmpty(client.Platform), nullIfEmpty(client.Device), nullIfEmpty(client.Model)).Scan(&clientID)
	if err != nil || clientID == "" || identityID == "" {
		return clientID
	}
	_, _ = r.DB.Exec(ctx, `INSERT INTO identity_client_bindings(identity_id, client_instance_id, last_seen_at)
		VALUES($1,$2,now())
		ON CONFLICT (identity_id, client_instance_id) DO UPDATE SET last_seen_at=now()`,
		identityID, clientID)
	return clientID
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
