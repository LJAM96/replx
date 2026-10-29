package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Collection snapshots are internal owner data. Serving client responses still
// requires that caller's authorization proof through PMS.
type collectionJSON struct {
	RatingKey string          `json:"ratingKey"`
	Key       string          `json:"key"`
	Title     string          `json:"title"`
	TitleSort string          `json:"titleSort"`
	Summary   string          `json:"summary"`
	Thumb     string          `json:"thumb"`
	Art       string          `json:"art"`
	Smart     json.RawMessage `json:"smart"`
}

type collectionMetadata struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	TitleSort string `json:"titleSort"`
	Summary   string `json:"summary"`
	Thumb     string `json:"thumb"`
	Art       string `json:"art"`
	Smart     bool   `json:"smart"`
}

type collectionPageContainer struct {
	TotalSize *int             `json:"totalSize"`
	Size      *int             `json:"size"`
	Offset    *int             `json:"offset"`
	Metadata  []collectionJSON `json:"Metadata"`
}

type collectionMember struct {
	RatingKey string `json:"ratingKey"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Thumb     string `json:"thumb"`
	Art       string `json:"art"`
}

// syncLibraryCollections stages every catalog entry and every ordered child
// list before publishing one library generation. A page error, changing total,
// repeated item, or ambiguous empty response leaves the old generation live.
func (w *Worker) syncLibraryCollections(ctx context.Context, serverID, token string, s sectionInfo) (retErr error) {
	if _, err := w.DB.Exec(ctx, `INSERT INTO library_collection_syncs(library_id) VALUES($1) ON CONFLICT DO NOTHING`, s.LibraryID); err != nil {
		return err
	}
	defer func() {
		if retErr != nil && ctx.Err() == nil {
			_, _ = w.DB.Exec(ctx, `UPDATE library_collection_syncs SET last_error=$2 WHERE library_id=$1`, s.LibraryID, retErr.Error())
		}
	}()
	var gen int64
	if err := w.DB.QueryRow(ctx, `SELECT nextval('collection_snapshot_generation_seq')`).Scan(&gen); err != nil {
		return err
	}
	pageSize := w.collectionPageSize()
	start, expectedTotal := 0, -1
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 100000 {
			return fmt.Errorf("sync: collection catalog exceeds page bound")
		}
		raw, err := w.Client.GetJSON(ctx, w.collectionURL("/library/sections/"+url.PathEscape(s.SectionID)+"/collections", start, pageSize), token, 8<<20)
		if err != nil {
			return err
		}
		var response struct {
			MediaContainer *collectionPageContainer `json:"MediaContainer"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.MediaContainer == nil {
			return fmt.Errorf("sync: invalid collection catalog response")
		}
		p := response.MediaContainer
		done, err := collectionPageComplete(start, pageSize, len(p.Metadata), p.Size, p.Offset, p.TotalSize, &expectedTotal)
		if err != nil {
			return err
		}
		for _, c := range p.Metadata {
			if c.RatingKey == "" || seen[c.RatingKey] {
				return fmt.Errorf("sync: missing or repeated collection ratingKey")
			}
			seen[c.RatingKey] = true
			if err := w.stageCollection(ctx, serverID, s.LibraryID, token, c, gen); err != nil {
				return err
			}
		}
		start += len(p.Metadata)
		if done {
			break
		}
	}
	if err := w.publishCollections(ctx, s.LibraryID, gen); err != nil {
		return err
	}
	// Incomplete generations are disposable staging data. Retain recent failed
	// attempts for diagnosis; successful publication prunes this library below.
	_, err := w.DB.Exec(ctx, `DELETE FROM collection_snapshots cs USING library_collections c WHERE cs.collection_id=c.id AND cs.started_at < now()-interval '7 days' AND cs.generation IS DISTINCT FROM c.published_generation`)
	return err
}

func (w *Worker) collectionPageSize() int {
	if w.PageSize <= 0 {
		return 100
	}
	if w.PageSize > 1000 {
		return 1000
	}
	return w.PageSize
}

func (w *Worker) collectionURL(path string, start, size int) string {
	return fmt.Sprintf("%s%s?X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", strings.TrimSuffix(w.Origin, "/"), path, start, size)
}

// Missing totalSize is supported by short-page termination, except that an
// empty first page must explicitly prove totalSize=0 before erasing old data.
func collectionPageComplete(start, requested, count int, size, offset, total *int, expectedTotal *int) (bool, error) {
	if count > requested || (size != nil && *size != count) || (offset != nil && *offset != start) {
		return false, fmt.Errorf("sync: collection pagination size or offset mismatch")
	}
	if total != nil {
		if *total < 0 || (*expectedTotal >= 0 && *expectedTotal != *total) || count > *total-start {
			return false, fmt.Errorf("sync: collection pagination total changed or invalid")
		}
		*expectedTotal = *total
	} else if *expectedTotal >= 0 {
		return false, fmt.Errorf("sync: collection pagination total disappeared")
	}
	if start == 0 && count == 0 && (total == nil || *total != 0) {
		return false, fmt.Errorf("sync: ambiguous empty collection response")
	}
	if *expectedTotal >= 0 {
		if start+count == *expectedTotal {
			return true, nil
		}
		if count == 0 {
			return false, fmt.Errorf("sync: collection page ended before advertised total")
		}
		return false, nil
	}
	return count < requested, nil
}

func collectionDisplayMetadata(c collectionJSON) (collectionMetadata, error) {
	m := collectionMetadata{Key: c.Key, Title: c.Title, TitleSort: c.TitleSort, Summary: c.Summary, Thumb: c.Thumb, Art: c.Art}
	if len(c.Smart) == 0 || string(c.Smart) == "null" {
		return m, nil
	}
	if err := json.Unmarshal(c.Smart, &m.Smart); err == nil {
		return m, nil
	}
	var n int
	if err := json.Unmarshal(c.Smart, &n); err != nil || (n != 0 && n != 1) {
		return m, fmt.Errorf("sync: invalid smart collection flag")
	}
	m.Smart = n == 1
	return m, nil
}

func (w *Worker) stageCollection(ctx context.Context, serverID, libraryID, token string, c collectionJSON, gen int64) error {
	metadata, err := collectionDisplayMetadata(c)
	if err != nil {
		return err
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	// New catalog entries have no published generation. Existing display data is
	// changed only in publishCollections after every scan has completed.
	if _, err := w.DB.Exec(ctx, `INSERT INTO library_collections(server_id,library_id,rating_key,title) VALUES($1,$2,$3,$4) ON CONFLICT(library_id,rating_key) DO NOTHING`, serverID, libraryID, c.RatingKey, c.Title); err != nil {
		return err
	}
	var id string
	if err := w.DB.QueryRow(ctx, `SELECT id FROM library_collections WHERE library_id=$1 AND rating_key=$2`, libraryID, c.RatingKey).Scan(&id); err != nil {
		return err
	}
	if _, err := w.DB.Exec(ctx, `INSERT INTO collection_snapshots(collection_id,generation,metadata) VALUES($1,$2,$3)`, id, gen, metadataJSON); err != nil {
		return err
	}
	h := sha256.New()
	_, _ = h.Write(metadataJSON)
	_, _ = h.Write([]byte{'\n'})
	pageSize := w.collectionPageSize()
	start, expectedTotal := 0, -1
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 100000 {
			return fmt.Errorf("sync: collection member list exceeds page bound")
		}
		raw, err := w.Client.GetJSON(ctx, w.collectionURL("/library/collections/"+url.PathEscape(c.RatingKey)+"/children", start, pageSize), token, 8<<20)
		if err != nil {
			return err
		}
		var response struct {
			MediaContainer *struct {
				Size      *int               `json:"size"`
				Offset    *int               `json:"offset"`
				TotalSize *int               `json:"totalSize"`
				Metadata  []collectionMember `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.MediaContainer == nil {
			return fmt.Errorf("sync: invalid collection children response")
		}
		p := response.MediaContainer
		done, err := collectionPageComplete(start, pageSize, len(p.Metadata), p.Size, p.Offset, p.TotalSize, &expectedTotal)
		if err != nil {
			return err
		}
		for i, m := range p.Metadata {
			if m.RatingKey == "" || seen[m.RatingKey] {
				return fmt.Errorf("sync: missing or repeated collection member ratingKey")
			}
			seen[m.RatingKey] = true
			encoded, _ := json.Marshal(m)
			_, _ = h.Write(encoded)
			_, _ = h.Write([]byte{'\n'})
			if _, err := w.DB.Exec(ctx, `INSERT INTO collection_members(collection_id,generation,position,rating_key,item_type,title,thumb,art) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, gen, start+i, m.RatingKey, m.Type, nullIfEmpty(m.Title), nullIfEmpty(m.Thumb), nullIfEmpty(m.Art)); err != nil {
				return err
			}
		}
		start += len(p.Metadata)
		if done {
			break
		}
	}
	_, err = w.DB.Exec(ctx, `UPDATE collection_snapshots SET completed_at=now(),item_count=$3,content_hash=$4 WHERE collection_id=$1 AND generation=$2`, id, gen, start, hex.EncodeToString(h.Sum(nil)))
	return err
}

func (w *Worker) publishCollections(ctx context.Context, libraryID string, gen int64) error {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A pending invalidation survives a crash between DB publication and cache
	// invalidation and is retried even if the next origin scan is unchanged.
	var changed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM library_collections c LEFT JOIN collection_snapshots staged ON staged.collection_id=c.id AND staged.generation=$2 LEFT JOIN collection_snapshots old ON old.collection_id=c.id AND old.generation=c.published_generation WHERE c.library_id=$1 AND (staged.generation IS NULL OR staged.content_hash IS DISTINCT FROM old.content_hash))`, libraryID, gen).Scan(&changed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_collections c SET collection_key=NULLIF(s.metadata->>'key',''),title=s.metadata->>'title',sort_title=NULLIF(s.metadata->>'titleSort',''),summary=NULLIF(s.metadata->>'summary',''),thumb=NULLIF(s.metadata->>'thumb',''),art=NULLIF(s.metadata->>'art',''),smart=(s.metadata->>'smart')::boolean,catalog_generation=$2,published_generation=CASE WHEN s.content_hash IS DISTINCT FROM old.content_hash THEN $2 ELSE c.published_generation END,synced_at=now() FROM collection_snapshots s LEFT JOIN collection_snapshots old ON old.collection_id=s.collection_id AND old.generation=(SELECT previous.published_generation FROM library_collections previous WHERE previous.id=s.collection_id) WHERE c.id=s.collection_id AND c.library_id=$1 AND s.generation=$2 AND s.completed_at IS NOT NULL`, libraryID, gen); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM library_collections WHERE library_id=$1 AND catalog_generation<>$2`, libraryID, gen); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_collection_syncs SET catalog_generation=$2,last_completed_at=now(),last_error=NULL,invalidation_pending=invalidation_pending OR $3 WHERE library_id=$1`, libraryID, gen, changed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM collection_snapshots s USING library_collections c WHERE s.collection_id=c.id AND c.library_id=$1 AND s.generation IS DISTINCT FROM c.published_generation`, libraryID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	var pending bool
	if err := w.DB.QueryRow(ctx, `SELECT invalidation_pending FROM library_collection_syncs WHERE library_id=$1`, libraryID).Scan(&pending); err != nil {
		return err
	}
	if pending && w.InvalidateCollections != nil {
		if err := w.InvalidateCollections(ctx); err != nil {
			return err
		}
		_, err = w.DB.Exec(ctx, `UPDATE library_collection_syncs SET invalidation_pending=false WHERE library_id=$1`, libraryID)
		return err
	}
	return nil
}

// SyncCollectionOnce refreshes one existing collection's child snapshot. It
// never reconciles the catalog or changes sibling collections.
func (w *Worker) SyncCollectionOnce(ctx context.Context, ratingKey string) error {
	if ratingKey == "" {
		return fmt.Errorf("sync: collection ratingKey required")
	}
	w.mu.Lock()
	if w.inFlight {
		w.mu.Unlock()
		return ErrInFlight
	}
	w.inFlight = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.inFlight = false; w.mu.Unlock() }()
	token, ok := w.OwnerToken(ctx)
	if !ok {
		return fmt.Errorf("sync: no owner credential (onboarding incomplete)")
	}
	serverID, err := enabledServer(ctx, w.DB)
	if err != nil {
		return err
	}
	var libraryID, id string
	var c collectionJSON
	var key, sortTitle, summary, thumb, art string
	var smart bool
	err = w.DB.QueryRow(ctx, `SELECT c.id::text,c.library_id::text,c.rating_key,COALESCE(c.collection_key,''),c.title,COALESCE(c.sort_title,''),COALESCE(c.summary,''),COALESCE(c.thumb,''),COALESCE(c.art,''),c.smart
		FROM library_collections c WHERE c.server_id=$1 AND c.rating_key=$2 AND c.published_generation IS NOT NULL`, serverID, ratingKey).Scan(
		&id, &libraryID, &c.RatingKey, &key, &c.Title, &sortTitle, &summary, &thumb, &art, &smart)
	if err != nil {
		return fmt.Errorf("sync: collection %s not found or unpublished: %w", ratingKey, err)
	}
	c.Key, c.TitleSort, c.Summary, c.Thumb, c.Art = key, sortTitle, summary, thumb, art
	c.Smart, _ = json.Marshal(smart)
	var gen int64
	if err := w.DB.QueryRow(ctx, `SELECT nextval('collection_snapshot_generation_seq')`).Scan(&gen); err != nil {
		return err
	}
	if err := w.stageCollection(ctx, serverID, libraryID, token, c, gen); err != nil {
		return err
	}
	return w.publishOneCollection(ctx, id, libraryID, gen)
}

func (w *Worker) publishOneCollection(ctx context.Context, collectionID, libraryID string, gen int64) error {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var changed bool
	if err := tx.QueryRow(ctx, `SELECT staged.content_hash IS DISTINCT FROM old.content_hash
		FROM collection_snapshots staged JOIN library_collections c ON c.id=staged.collection_id
		LEFT JOIN collection_snapshots old ON old.collection_id=c.id AND old.generation=c.published_generation
		WHERE c.id=$1 AND staged.generation=$2 AND staged.completed_at IS NOT NULL`, collectionID, gen).Scan(&changed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_collections c SET collection_key=NULLIF(s.metadata->>'key',''),title=s.metadata->>'title',sort_title=NULLIF(s.metadata->>'titleSort',''),summary=NULLIF(s.metadata->>'summary',''),thumb=NULLIF(s.metadata->>'thumb',''),art=NULLIF(s.metadata->>'art',''),smart=(s.metadata->>'smart')::boolean,published_generation=CASE WHEN s.content_hash IS DISTINCT FROM old.content_hash THEN $2 ELSE c.published_generation END,synced_at=now()
		FROM collection_snapshots s LEFT JOIN collection_snapshots old ON old.collection_id=s.collection_id AND old.generation=(SELECT previous.published_generation FROM library_collections previous WHERE previous.id=s.collection_id)
		WHERE c.id=$1 AND s.collection_id=c.id AND s.generation=$2 AND s.completed_at IS NOT NULL`, collectionID, gen); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_collection_syncs SET invalidation_pending=invalidation_pending OR $2 WHERE library_id=$1`, libraryID, changed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM collection_snapshots WHERE collection_id=$1 AND generation IS DISTINCT FROM (SELECT published_generation FROM library_collections WHERE id=$1)`, collectionID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return w.flushCollectionInvalidation(ctx, libraryID)
}

func (w *Worker) flushCollectionInvalidation(ctx context.Context, libraryID string) error {
	var pending bool
	if err := w.DB.QueryRow(ctx, `SELECT invalidation_pending FROM library_collection_syncs WHERE library_id=$1`, libraryID).Scan(&pending); err != nil {
		return err
	}
	if !pending || w.InvalidateCollections == nil {
		return nil
	}
	if err := w.InvalidateCollections(ctx); err != nil {
		return err
	}
	_, err := w.DB.Exec(ctx, `UPDATE library_collection_syncs SET invalidation_pending=false WHERE library_id=$1`, libraryID)
	return err
}
