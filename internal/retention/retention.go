// Package retention enforces age-based data budgets for high-volume
// records: playback sessions/decisions (30d), diagnostic traces (expiry),
// audit events (180d). Without it a long soak grows Postgres unbounded,
// violating the production invariant that all high-volume records have
// retention. Active sessions (ended_at NULL) are never removed.
package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
)

// Policy configures age bounds. Zero disables that class.
type Policy struct {
	PlaybackDays int
	AuditDays    int
	// BatchSize bounds each DELETE batch to avoid long locks.
	// Zero defaults to 1000.
	BatchSize int
}

// Result counts one purge pass.
type Result struct {
	Sessions  int64 `json:"sessions"`
	Decisions int64 `json:"decisions"`
	Traces    int64 `json:"traces"`
	Audits    int64 `json:"audits"`
}

func batchSize(p Policy) int {
	if p.BatchSize > 0 {
		return p.BatchSize
	}
	return 1000
}

// purgeBatched deletes matching rows in LIMIT-bounded batches until none
// remain or the context expires. days < 0 means the predicate takes no arg.
// Postgres has no DELETE ... LIMIT, so each batch deletes a bounded ctid
// set selected in a subquery: bounded locks without long transactions.
func purgeBatched(ctx context.Context, db database.DBTX, table, where string, days, batch int) (int64, error) {
	var total int64
	lim := strconv.Itoa(batch)
	for {
		var n int64
		var err error
		q := `WITH gone AS (DELETE FROM ` + table + ` WHERE ctid IN (SELECT ctid FROM ` + table + ` WHERE ` + where + ` LIMIT ` + lim + `) RETURNING 1) SELECT count(*) FROM gone`
		if days < 0 {
			err = db.QueryRow(ctx, q).Scan(&n)
		} else {
			err = db.QueryRow(ctx, q, days).Scan(&n)
		}
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// PurgeOnce deletes expired rows in bounded batches. Sessions go first with
// decisions cascading; orphan decisions (session already gone) are swept
// directly. The first database failure aborts the pass and is returned: a
// silent purge would hide a filling disk behind a "purged" log line.
// Statements before the failure already committed.
func PurgeOnce(ctx context.Context, db database.DBTX, p Policy) (Result, error) {
	var out Result
	if db == nil {
		return out, fmt.Errorf("retention: no database")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	batch := batchSize(p)
	var err error
	if p.PlaybackDays > 0 {
		if out.Decisions, err = purgeBatched(cctx, db, `playback_decisions`, `created_at < now() - make_interval(days => $1)`, p.PlaybackDays, batch); err != nil {
			return out, fmt.Errorf("retention: decisions: %w", err)
		}
		if out.Sessions, err = purgeBatched(cctx, db, `playback_sessions`, `ended_at IS NOT NULL AND ended_at < now() - make_interval(days => $1)`, p.PlaybackDays, batch); err != nil {
			return out, fmt.Errorf("retention: sessions: %w", err)
		}
	}
	// Trace expiry is per-row (expires_at), independent of the policy.
	if out.Traces, err = purgeBatched(cctx, db, `diagnostic_traces`, `expires_at < now()`, -1, batch); err != nil {
		return out, fmt.Errorf("retention: traces: %w", err)
	}
	if p.AuditDays > 0 {
		if out.Audits, err = purgeBatched(cctx, db, `audit_events`, `created_at < now() - make_interval(days => $1)`, p.AuditDays, batch); err != nil {
			return out, fmt.Errorf("retention: audits: %w", err)
		}
	}
	return out, nil
}

// Run purges daily until ctx ends.
func Run(ctx context.Context, db database.DBTX, p Policy, logger *logging.Logger, interval time.Duration) {
	if db == nil {
		return
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	// First pass shortly after startup so a long-down instance reclaims
	// before the next daily tick.
	initial := time.NewTimer(5 * time.Minute)
	defer initial.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
			purgeAndLog(ctx, db, p, logger)
		case <-ticker.C:
			purgeAndLog(ctx, db, p, logger)
		}
	}
}

func purgeAndLog(ctx context.Context, db database.DBTX, p Policy, logger *logging.Logger) {
	res, err := PurgeOnce(ctx, db, livePolicy(ctx, db, p))
	if logger == nil {
		return
	}
	if err != nil {
		logger.Log(logging.Entry{Level: "warn", Component: "retention",
			Fields: map[string]any{"event": "purge_failed", "error": err.Error()}})
		return
	}
	logger.Log(logging.Entry{Level: "info", Component: "retention",
		Fields: map[string]any{"event": "purged", "sessions": res.Sessions,
			"decisions": res.Decisions, "traces": res.Traces, "audits": res.Audits}})
}

// livePolicy reloads retention bounds from the runtime settings table so
// PATCH /api/v1/settings takes effect without restart. Unset, corrupt or
// out-of-range values fall back to the startup policy; the admin API only
// ever stores validated integers, so fallback paths cover races and
// hand-edited rows.
func livePolicy(ctx context.Context, db database.DBTX, fallback Policy) Policy {
	if db == nil {
		return fallback
	}
	out := fallback
	if n, ok := settingDays(ctx, db, "replx.playback_retention_days"); ok && n >= 1 && n <= 3650 {
		out.PlaybackDays = n
	}
	if n, ok := settingDays(ctx, db, "replx.audit_retention_days"); ok && n >= 1 && n <= 3650 {
		out.AuditDays = n
	}
	return out
}

func settingDays(ctx context.Context, db database.DBTX, key string) (int, bool) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(cctx, `SELECT value FROM app_settings WHERE key=$1`, key).Scan(&raw); err != nil {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}
