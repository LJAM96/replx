// Package search generates local search candidates from the Gamma owner
// index: title, sort_title and original_title only, via FTS with a trigram
// fallback. Cast, director, collection and label queries fall back to PMS.
//
// Visibility rule: local candidates serve only when the index is fresh (a
// completed section sweep within FreshnessWindow). Per-user library grants
// are not synced in Production 1.0, so callers must fall back to PMS
// whenever visibility of a candidate for the requesting user is uncertain.
// Search is an optimization, never an authorization decision.
package search

import (
	"context"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/database"
)

// FreshnessWindow bounds index staleness for serving candidates: twice the
// default full-sweep interval, so one missed sweep still serves.
const FreshnessWindow = 12 * time.Hour

// Candidate is one local search hit.
type Candidate struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	ItemType  string `json:"itemType"`
	Year      *int   `json:"year,omitempty"`
	Thumb     string `json:"thumb,omitempty"`
}

// Candidates returns FTS+trigram matches for query (empty when the query
// is blank, overlong, or the index is stale). Fresh means EVERY synced
// library section has a recent completed cursor: one fresh section must
// never vouch for a stale sibling. Callers fall back to PMS on stale.
func Candidates(ctx context.Context, db database.DBTX, serverID, query string, limit int) ([]Candidate, bool, error) {
	query = truncateQuery(query)
	if db == nil || query == "" {
		return nil, false, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 25
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var total, stale int
	if err := db.QueryRow(cctx, `SELECT count(*),
		count(*) FILTER (WHERE NOT EXISTS(SELECT 1 FROM sync_cursors c WHERE c.server_id=l.server_id AND c.sync_type='section'
			AND c.library_id=l.id AND c.status='complete'
			AND c.last_completed_at > now() - make_interval(hours => 12)))
		FROM libraries l WHERE l.server_id=$1`, serverID).Scan(&total, &stale); err != nil {
		return nil, false, err
	}
	// Zero libraries means nothing has ever synced: not ready, never
	// vacuously fresh.
	if total == 0 || stale != 0 {
		return nil, false, nil
	}
	rows, err := db.Query(cctx, `SELECT rating_key, title, item_type, year, COALESCE(thumb,'') FROM library_items
		WHERE server_id=$1 AND (
			search_vector @@ plainto_tsquery('simple', $2)
			OR title % $2 OR sort_title % $2 OR original_title % $2
		)
		ORDER BY ts_rank(search_vector, plainto_tsquery('simple', $2)) DESC,
			similarity(title, $2) DESC
		LIMIT $3`, serverID, query, limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var year *int
		var thumb string
		if err := rows.Scan(&c.RatingKey, &c.Title, &c.ItemType, &year, &thumb); err != nil {
			return nil, false, err
		}
		c.Year, c.Thumb = year, thumb
		out = append(out, c)
	}
	return out, true, rows.Err()
}

func truncateQuery(q string) string {
	q = strings.TrimSpace(q)
	if len(q) > 120 {
		q = q[:120]
	}
	return q
}
