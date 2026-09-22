package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Plex JSON shapes for section items. Pointers tolerate absent fields;
// missing data stores NULL rather than inventing values.
type guidJSON struct {
	ID string `json:"id"`
}

// Plex uses both an array of Guid objects and a legacy scalar guid string.
// encoding/json matches those keys case-insensitively, so one field must
// accept both shapes or a scalar guid aborts the entire section sync.
type guidList []guidJSON

func (g *guidList) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*g = nil
		return nil
	}
	if raw[0] == '"' {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return err
		}
		*g = nil
		if id != "" {
			*g = guidList{{ID: id}}
		}
		return nil
	}
	var list []guidJSON
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	*g = list
	return nil
}

type streamJSON struct {
	ID           *int64 `json:"id"`
	StreamType   int    `json:"streamType"`
	Default      bool   `json:"default"`
	Codec        string `json:"codec"`
	Profile      string `json:"profile"`
	Language     string `json:"language"`
	LanguageCode string `json:"languageCode"`
	Channels     *int   `json:"channels"`
	Bitrate      *int   `json:"bitrate"`
	Width        *int   `json:"width"`
	Height       *int   `json:"height"`
	Selected     bool   `json:"selected"`
	Forced       bool   `json:"forced"`
}

type partJSON struct {
	ID        *int64       `json:"id"`
	Key       string       `json:"key"`
	Container string       `json:"container"`
	Size      *int64       `json:"size"`
	Duration  *int64       `json:"duration"`
	Stream    []streamJSON `json:"Stream"`
}

type mediaJSON struct {
	ID              *int64     `json:"id"`
	Container       string     `json:"container"`
	VideoCodec      string     `json:"videoCodec"`
	VideoProfile    string     `json:"videoProfile"`
	Width           *int       `json:"width"`
	Height          *int       `json:"height"`
	Bitrate         *int       `json:"bitrate"`
	VideoResolution string     `json:"videoResolution"`
	DynamicRange    string     `json:"dynamicRange"`
	AudioCodec      string     `json:"audioCodec"`
	AudioChannels   *int       `json:"audioChannels"`
	Duration        *int64     `json:"duration"`
	Part            []partJSON `json:"Part"`
}

type itemJSON struct {
	RatingKey            string      `json:"ratingKey"`
	Key                  string      `json:"key"`
	Type                 string      `json:"type"`
	Title                string      `json:"title"`
	TitleSort            string      `json:"titleSort"`
	OriginalTitle        string      `json:"originalTitle"`
	Year                 *int        `json:"year"`
	ParentRatingKey      string      `json:"parentRatingKey"`
	GrandparentRatingKey string      `json:"grandparentRatingKey"`
	Duration             *int64      `json:"duration"`
	Thumb                string      `json:"thumb"`
	Art                  string      `json:"art"`
	AddedAt              *int64      `json:"addedAt"`
	UpdatedAt            *int64      `json:"updatedAt"`
	Guid                 guidList    `json:"Guid"`
	Media                []mediaJSON `json:"Media"`
}

type itemsPage struct {
	MediaContainer struct {
		// Plex semantics: size is the number of items in THIS response,
		// totalSize the total collection size. Older builds omit
		// totalSize, in which case short-page termination applies.
		Size      *int       `json:"size"`
		TotalSize *int       `json:"totalSize"`
		Metadata  []itemJSON `json:"Metadata"`
	} `json:"MediaContainer"`
}

// parseItemsPage decodes one section page. total is the origin-reported
// totalSize. There is deliberately NO fallback to size: Plex defines size
// as the number of items in THIS response, so a 100-item first page of a
// 10,000-item collection would otherwise terminate the sweep. Builds that
// omit totalSize terminate by short page instead (total -1).
func parseItemsPage(raw []byte) ([]itemJSON, int, error) {
	var parsed itemsPage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, -1, fmt.Errorf("sync: items parse: %w", err)
	}
	total := -1
	if parsed.MediaContainer.TotalSize != nil {
		total = *parsed.MediaContainer.TotalSize
	}
	return parsed.MediaContainer.Metadata, total, nil
}

// upsertItem writes one item plus GUIDs, variants, parts and streams.
// gen stamps full-sweep rows (nil on light passes, leaving the stamp for
// the next full sweep to judge). Variants and parts upsert in place by
// their natural keys and only genuinely absent rows are deleted, so
// playback history referencing them survives via ON DELETE SET NULL
// instead of violating foreign keys.
func (w *Worker) upsertItem(ctx context.Context, serverID, libraryID string, it itemJSON, gen *int64) error {
	if it.RatingKey == "" {
		return fmt.Errorf("sync: item without ratingKey")
	}
	var genParam any
	if gen != nil {
		genParam = *gen
	}
	var itemID string
	err := w.DB.QueryRow(ctx, `INSERT INTO library_items(server_id, library_id, rating_key, item_key, item_type, title, sort_title, original_title, year, parent_rating_key, grandparent_rating_key, duration_ms, thumb, art, added_at, updated_at_origin, raw_metadata, sweep_gen, synced_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,to_timestamp($15),to_timestamp($16),$17,$18,now())
		ON CONFLICT (server_id, rating_key) DO UPDATE SET
			library_id=EXCLUDED.library_id, item_key=EXCLUDED.item_key, item_type=EXCLUDED.item_type,
			title=EXCLUDED.title, sort_title=EXCLUDED.sort_title, original_title=EXCLUDED.original_title,
			year=EXCLUDED.year, parent_rating_key=EXCLUDED.parent_rating_key,
			grandparent_rating_key=EXCLUDED.grandparent_rating_key, duration_ms=EXCLUDED.duration_ms,
			thumb=EXCLUDED.thumb, art=EXCLUDED.art, added_at=EXCLUDED.added_at,
			updated_at_origin=EXCLUDED.updated_at_origin, raw_metadata=EXCLUDED.raw_metadata,
			sweep_gen=COALESCE($18, library_items.sweep_gen), synced_at=now()
		RETURNING id`,
		serverID, libraryID, it.RatingKey, nullIfEmpty(it.Key), it.Type, nullIfEmpty(it.Title),
		nullIfEmpty(it.TitleSort), nullIfEmpty(it.OriginalTitle), intOrNil(it.Year),
		nullIfEmpty(it.ParentRatingKey), nullIfEmpty(it.GrandparentRatingKey), int64OrNil(it.Duration),
		nullIfEmpty(it.Thumb), nullIfEmpty(it.Art), epochOrNil(it.AddedAt), epochOrNil(it.UpdatedAt),
		nil, genParam).Scan(&itemID)
	if err != nil {
		return fmt.Errorf("sync: upsert item %s: %w", it.RatingKey, err)
	}
	for _, g := range it.Guid {
		provider, providerID := splitGUID(g.ID)
		if _, err := w.DB.Exec(ctx, `INSERT INTO item_guids(item_id, guid, provider, provider_id)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			itemID, g.ID, nullIfEmpty(provider), nullIfEmpty(providerID)); err != nil {
			return fmt.Errorf("sync: guid %s: %w", it.RatingKey, err)
		}
	}
	if _, err := w.DB.Exec(ctx, `DELETE FROM item_guids WHERE item_id=$1 AND NOT (guid = ANY($2))`, itemID, guidIDs(it)); err != nil {
		return fmt.Errorf("sync: prune guids %s: %w", it.RatingKey, err)
	}
	seenIdx := make([]int32, 0, len(it.Media))
	for idx, m := range it.Media {
		var variantID string
		err := w.DB.QueryRow(ctx, `INSERT INTO media_variants(library_item_id, plex_media_id, media_index, container, video_codec, video_profile, width, height, bitrate_kbps, video_resolution, normalized_dynamic_range, audio_codec, audio_channels, duration_ms, synced_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now())
			ON CONFLICT (library_item_id, media_index) DO UPDATE SET
				plex_media_id=EXCLUDED.plex_media_id, container=EXCLUDED.container,
				video_codec=EXCLUDED.video_codec, video_profile=EXCLUDED.video_profile,
				width=EXCLUDED.width, height=EXCLUDED.height, bitrate_kbps=EXCLUDED.bitrate_kbps,
				video_resolution=EXCLUDED.video_resolution,
				normalized_dynamic_range=EXCLUDED.normalized_dynamic_range,
				audio_codec=EXCLUDED.audio_codec, audio_channels=EXCLUDED.audio_channels,
				duration_ms=EXCLUDED.duration_ms, synced_at=now()
			RETURNING id`,
			itemID, mediaIDString(m.ID), idx, nullIfEmpty(m.Container), nullIfEmpty(m.VideoCodec),
			nullIfEmpty(m.VideoProfile), intOrNil(m.Width), intOrNil(m.Height), intOrNil(m.Bitrate),
			nullIfEmpty(m.VideoResolution),
			NormalizeDynamicRange(m.DynamicRange, "", m.VideoCodec, m.VideoProfile),
			nullIfEmpty(m.AudioCodec), intOrNil(m.AudioChannels), int64OrNil(m.Duration)).Scan(&variantID)
		if err != nil {
			return fmt.Errorf("sync: variant %s/%d: %w", it.RatingKey, idx, err)
		}
		seenIdx = append(seenIdx, int32(idx))
		seenPart := make([]int32, 0, len(m.Part))
		for pidx, p := range m.Part {
			if p.ID == nil {
				// No origin part ID: the row cannot satisfy the unique
				// plex_part_id contract, so it is skipped rather than
				// stored unidentifiable.
				continue
			}
			var partID string
			err := w.DB.QueryRow(ctx, `INSERT INTO media_parts(media_variant_id, plex_part_id, part_index, plex_key, container, size_bytes, duration_ms)
				VALUES($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (media_variant_id, part_index) DO UPDATE SET
					plex_part_id=EXCLUDED.plex_part_id, plex_key=EXCLUDED.plex_key,
					container=EXCLUDED.container, size_bytes=EXCLUDED.size_bytes,
					duration_ms=EXCLUDED.duration_ms
				RETURNING id`,
				variantID, partIDString(p.ID), pidx, nullIfEmpty(p.Key), nullIfEmpty(p.Container),
				int64OrNil(p.Size), int64OrNil(p.Duration)).Scan(&partID)
			if err != nil {
				return fmt.Errorf("sync: part %s/%d: %w", it.RatingKey, idx, err)
			}
			seenPart = append(seenPart, int32(pidx))
			// Streams carry no natural key: replace per part. They are
			// never referenced by history, so this is safe.
			if _, err := w.DB.Exec(ctx, `DELETE FROM media_streams WHERE media_part_id=$1`, partID); err != nil {
				return fmt.Errorf("sync: clear streams %s: %w", it.RatingKey, err)
			}
			for _, st := range p.Stream {
				if _, err := w.DB.Exec(ctx, `INSERT INTO media_streams(media_part_id, plex_stream_id, stream_type, codec, profile, language, language_code, channels, bitrate, width, height, selected, forced, default_stream)
					VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
					partID, streamIDString(st.ID), st.StreamType, nullIfEmpty(st.Codec),
					nullIfEmpty(st.Profile), nullIfEmpty(st.Language), nullIfEmpty(st.LanguageCode),
					intOrNil(st.Channels), intOrNil(st.Bitrate), intOrNil(st.Width), intOrNil(st.Height),
					st.Selected, st.Forced, st.Default); err != nil {
					return fmt.Errorf("sync: stream %s: %w", it.RatingKey, err)
				}
			}
		}
		if _, err := w.DB.Exec(ctx, `DELETE FROM media_parts WHERE media_variant_id=$1 AND NOT (part_index = ANY($2))`,
			variantID, seenPart); err != nil {
			return fmt.Errorf("sync: prune parts %s/%d: %w", it.RatingKey, idx, err)
		}
	}
	// Absent variants/parts are deleted; playback history referencing
	// them survives via ON DELETE SET NULL instead of violating.
	if _, err := w.DB.Exec(ctx, `DELETE FROM media_variants WHERE library_item_id=$1 AND NOT (media_index = ANY($2))`,
		itemID, seenIdx); err != nil {
		return fmt.Errorf("sync: prune variants %s: %w", it.RatingKey, err)
	}
	return nil
}

// guidIDs collects origin GUID strings for absent-row pruning.
func guidIDs(it itemJSON) []string {
	out := make([]string, 0, len(it.Guid))
	for _, g := range it.Guid {
		out = append(out, g.ID)
	}
	return out
}

// splitGUID splits "provider://id" into provider and id. Bare values yield
// an empty provider rather than an invented one.
func splitGUID(g string) (string, string) {
	if i := strings.Index(g, "://"); i >= 0 {
		return g[:i], g[i+3:]
	}
	return "", g
}

func mediaIDString(id *int64) any {
	if id == nil {
		return nil
	}
	return fmt.Sprint(*id)
}

func partIDString(id *int64) any {
	if id == nil {
		// Parts without origin IDs cannot satisfy the unique plex_part_id
		// column contract; callers must skip them. Empty signals skip.
		return nil
	}
	return fmt.Sprint(*id)
}

func streamIDString(id *int64) any {
	if id == nil {
		return nil
	}
	return fmt.Sprint(*id)
}
