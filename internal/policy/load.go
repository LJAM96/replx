package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/jackc/pgx/v5"
)

// LoadEffective reads enabled policies for a server and folds them to one
// effective policy with rejection provenance. Identity/client UUIDs
// activate user/device levels; nil keeps those levels inherited.
//
// Failure semantics are the point: no row means inherit, but a database
// error or corrupt config on an APPLICABLE level fails the whole load.
// Callers fail playback closed on error, so a Postgres outage while a
// restriction applies can never silently promote global policy.
func LoadEffective(ctx context.Context, db database.DBTX, serverID string, identityID, clientID *string) (Policy, string, error) {
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
		if l.scopeID == nil && l.scopeType != "global" {
			continue // unresolved identity: level inapplicable, not failed
		}
		p, found, err := loadLevel(ctx, db, serverID, l.scopeType, l.scopeID)
		if err != nil {
			return Policy{}, "", fmt.Errorf("policy: %s level unavailable: %w", l.name, err)
		}
		if !found {
			continue
		}
		eff = Merge(eff, p)
		scope = l.name
	}
	return eff, scope, nil
}

// loadLevel reads one scope level: found=false is clean inheritance,
// while transport and corruption errors fail loudly.
func loadLevel(ctx context.Context, db database.DBTX, serverID, scopeType string, scopeID *string) (Policy, bool, error) {
	if db == nil {
		return Policy{}, false, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("policy: %s query: %w", scopeType, err)
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, false, fmt.Errorf("policy: %s config corrupt: %w", scopeType, err)
	}
	return p, true, nil
}
