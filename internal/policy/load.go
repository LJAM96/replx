package policy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadEffective reads enabled policies for a server and folds them to one
// effective policy with rejection provenance. identityID and clientID are
// the plex_identities/client_instances UUIDs (nil when unresolved, in
// which case those levels contribute nothing). Unknown levels never
// weaken: absent rows merge as inherit.
func LoadEffective(ctx context.Context, db *pgxpool.Pool, serverID string, identityID, clientID *string) (Policy, string) {
	eff := Defaults()
	scope := "GLOBAL"
	levels := []struct {
		scopeType string
		scopeID   *string
		name      string
	}{
		{"global", nil, "GLOBAL"},
		{"user", identityID, "USER"},
		{"device", clientID, "DEVICE"},
	}
	for _, l := range levels {
		p, ok := loadLevel(ctx, db, serverID, l.scopeType, l.scopeID)
		if !ok {
			continue
		}
		eff = Merge(eff, p)
		scope = l.name
	}
	return eff, scope
}

func loadLevel(ctx context.Context, db *pgxpool.Pool, serverID, scopeType string, scopeID *string) (Policy, bool) {
	if db == nil {
		return Policy{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var raw []byte
	var err error
	if scopeID == nil {
		err = db.QueryRow(cctx, `SELECT config FROM policies
			WHERE server_id=$1 AND scope_type=$2 AND enabled ORDER BY updated_at DESC LIMIT 1`,
			serverID, scopeType).Scan(&raw)
	} else {
		err = db.QueryRow(cctx, `SELECT config FROM policies
			WHERE server_id=$1 AND scope_type=$2 AND scope_id=$3 AND enabled ORDER BY updated_at DESC LIMIT 1`,
			serverID, scopeType, *scopeID).Scan(&raw)
	}
	if err != nil || len(raw) == 0 {
		return Policy{}, false
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, false
	}
	return p, true
}
