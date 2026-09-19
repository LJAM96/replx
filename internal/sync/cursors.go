package sync

import (
	"context"
	"encoding/json"
	"time"
)

// loadCursor returns the persisted page start for a section (0 fresh).
func (w *Worker) loadCursor(ctx context.Context, serverID, libraryID string) int {
	var raw []byte
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := w.DB.QueryRow(cctx, `SELECT cursor FROM sync_cursors
		WHERE server_id=$1 AND sync_type='section' AND library_id=$2`,
		serverID, libraryID).Scan(&raw); err != nil || len(raw) == 0 {
		return 0
	}
	var c struct {
		Start int `json:"start"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Start < 0 {
		return 0
	}
	return c.Start
}

// saveCursor persists the page start for a resumable section pass.
func (w *Worker) saveCursor(ctx context.Context, serverID, libraryID string, start int) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cur, _ := json.Marshal(map[string]int{"start": start})
	_, err := w.DB.Exec(cctx, `INSERT INTO sync_cursors(server_id, sync_type, library_id, cursor, status)
		VALUES($1,'section',$2,$3,'running')
		ON CONFLICT (server_id, sync_type, library_id) DO UPDATE SET
			cursor=EXCLUDED.cursor, status='running', last_started_at=now(), last_error=''`,
		serverID, libraryID, cur)
	return err
}

// setCursor records pass-level status for libraries or one section.
// The UNIQUE(server_id, sync_type, library_id) constraint does not dedupe
// NULL library IDs, so library-level rows go through DELETE+INSERT while
// section rows (always keyed) use the upsert.
func (w *Worker) setCursor(ctx context.Context, serverID, syncType string, libraryID *string, status, lastErr string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if libraryID == nil {
		if _, err := w.DB.Exec(cctx, `DELETE FROM sync_cursors
			WHERE server_id=$1 AND sync_type=$2 AND library_id IS NULL`, serverID, syncType); err != nil {
			return err
		}
		_, err := w.DB.Exec(cctx, `INSERT INTO sync_cursors(server_id, sync_type, status, last_error, last_started_at, last_completed_at)
			VALUES($1,$2,$3,$4,now(), CASE WHEN $3='complete' THEN now() ELSE NULL END)`,
			serverID, syncType, status, lastErr)
		return err
	}
	_, err := w.DB.Exec(cctx, `INSERT INTO sync_cursors(server_id, sync_type, library_id, status, last_error, last_started_at, last_completed_at)
		VALUES($1,$2,$3,$4,$5,now(), CASE WHEN $4='complete' THEN now() ELSE NULL END)
		ON CONFLICT (server_id, sync_type, library_id) DO UPDATE SET
			status=EXCLUDED.status, last_error=EXCLUDED.last_error, last_started_at=now(),
			last_completed_at=CASE WHEN EXCLUDED.status='complete' THEN now() ELSE sync_cursors.last_completed_at END`,
		serverID, syncType, *libraryID, status, lastErr)
	return err
}

// CursorStatus is one sync_cursors row for the admin status endpoint.
type CursorStatus struct {
	SyncType    string  `json:"syncType"`
	LibraryID   *string `json:"libraryId"`
	Status      string  `json:"status"`
	LastStarted *string `json:"lastStartedAt"`
	Completed   *string `json:"lastCompletedAt"`
	LastError   string  `json:"lastError"`
}

// Status lists cursor rows newest first.
func (w *Worker) Status(ctx context.Context) ([]CursorStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := w.DB.Query(cctx, `SELECT sync_type, library_id::text, status,
		to_char(last_started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		to_char(last_completed_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		COALESCE(last_error,'') FROM sync_cursors ORDER BY last_started_at DESC NULLS LAST`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CursorStatus
	for rows.Next() {
		var c CursorStatus
		if err := rows.Scan(&c.SyncType, &c.LibraryID, &c.Status, &c.LastStarted, &c.Completed, &c.LastError); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
