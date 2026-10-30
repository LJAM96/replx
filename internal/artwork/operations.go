package artwork

import (
	"context"
	"errors"
)

// EvictionResult reports a bounded maintenance pass. FreedBytes includes
// payload and the conservative metadata allowance released by its references.
type EvictionResult struct {
	Removed    int   `json:"removed"`
	FreedBytes int64 `json:"freedBytes"`
}

// EvictLowPriority removes at most 128 blobs whose every live reference is Low.
// A low-priority alias cannot cause a blob shared by a poster/high tier to be
// removed. Publication and reads use the same store lock.
func (s *Store) EvictLowPriority(ctx context.Context) (EvictionResult, error) {
	var result EvictionResult
	if s == nil || s.shared == nil {
		return result, errors.New("artwork: shared store disabled")
	}
	ss := s.shared
	if err := ss.ensure(ctx); err != nil {
		return result, err
	}
	tx, err := ss.lock(ctx, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	before, err := ss.stats(ctx, tx)
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT b.digest FROM artwork_blobs b LEFT JOIN artwork_variants v ON v.store_id=b.store_id AND v.blob_digest=b.digest AND v.expires_at>now() WHERE b.store_id=$1 GROUP BY b.digest HAVING COALESCE(max(v.priority),0)=0 ORDER BY max(v.last_accessed_at) ASC NULLS FIRST,b.digest LIMIT 128`, ss.id)
	if err != nil {
		return result, err
	}
	var digests []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			rows.Close()
			return result, err
		}
		digests = append(digests, digest)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, digest := range digests {
		if err := ss.removeBlob(ctx, tx, digest); err != nil {
			return result, err
		}
		result.Removed++
	}
	after, err := ss.stats(ctx, tx)
	if err != nil {
		return result, err
	}
	result.FreedBytes = before.TotalBytes - after.TotalBytes
	if err := tx.Commit(ctx); err != nil {
		return EvictionResult{}, err
	}
	return result, nil
}
