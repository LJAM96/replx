// Package retention enforces age-based data budgets for high-volume
// records: playback sessions/decisions (30d), diagnostic traces (expiry),
// audit events (180d). Without it a long soak grows Postgres unbounded,
// violating the production invariant that all high-volume records have
// retention. Active sessions (ended_at NULL) are never removed.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
)

// Policy configures age bounds. Zero disables that class.
type Policy struct {
	PlaybackDays int
	AuditDays    int
}

// Result counts one purge pass.
type Result struct {
	Sessions  int64 `json:"sessions"`
	Decisions int64 `json:"decisions"`
	Traces    int64 `json:"traces"`
	Audits    int64 `json:"audits"`
}

// PurgeOnce deletes expired rows. Sessions go first with decisions
// cascading; orphan decisions (session already gone) are swept directly.
// The first database failure aborts the pass and is returned: a silent
// purge would hide a filling disk behind a "purged" log line. Statements
// before the failure already committed.
func PurgeOnce(ctx context.Context, db database.DBTX, p Policy) (Result, error) {
	var out Result
	if db == nil {
		return out, fmt.Errorf("retention: no database")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	exec := func(q string, args ...any) (int64, error) {
		var n int64
		if err := db.QueryRow(cctx, q, args...).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	var err error
	if p.PlaybackDays > 0 {
		if out.Decisions, err = exec(`WITH gone AS (
			DELETE FROM playback_decisions
			WHERE created_at < now() - make_interval(days => $1) RETURNING 1
		) SELECT count(*) FROM gone`, p.PlaybackDays); err != nil {
			return out, fmt.Errorf("retention: decisions: %w", err)
		}
		if out.Sessions, err = exec(`WITH gone AS (
			DELETE FROM playback_sessions
			WHERE ended_at IS NOT NULL AND ended_at < now() - make_interval(days => $1)
			RETURNING 1
		) SELECT count(*) FROM gone`, p.PlaybackDays); err != nil {
			return out, fmt.Errorf("retention: sessions: %w", err)
		}
	}
	// Trace expiry is per-row (expires_at), independent of the policy.
	if out.Traces, err = exec(`WITH gone AS (
		DELETE FROM diagnostic_traces WHERE expires_at < now() RETURNING 1
	) SELECT count(*) FROM gone`); err != nil {
		return out, fmt.Errorf("retention: traces: %w", err)
	}
	if p.AuditDays > 0 {
		if out.Audits, err = exec(`WITH gone AS (
			DELETE FROM audit_events
			WHERE created_at < now() - make_interval(days => $1) RETURNING 1
		) SELECT count(*) FROM gone`, p.AuditDays); err != nil {
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
	res, err := PurgeOnce(ctx, db, p)
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
