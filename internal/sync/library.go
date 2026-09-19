package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OriginClient fetches internal PMS API JSON with an owner token.
type OriginClient struct {
	client *http.Client
}

// NewOriginClient builds a client with the given request timeout.
func NewOriginClient(timeout time.Duration) *OriginClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OriginClient{client: &http.Client{Timeout: timeout}}
}

// GetJSON GETs url with the owner token and JSON accept, capped at limit.
func (c *OriginClient) GetJSON(ctx context.Context, url, token string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", token)
	resp, err := c.client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return nil, fmt.Errorf("sync: origin request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("sync: origin status %s", resp.Status)
	}
	// 8 MiB transform ceiling per the route matrix: pages are bounded by
	// PageSize, anything larger bypasses index parsing, never DOM-crashes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > 8<<20 {
		return nil, fmt.Errorf("sync: page exceeds 8MiB transform ceiling")
	}
	return body, nil
}

// sectionInfo is one library section with its sync watermark.
type sectionInfo struct {
	LibraryID   string
	SectionID   string
	Title       string
	MediaType   string
	NeverSynced bool
}

type sectionDir struct {
	Key       string `json:"key"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Agent     string `json:"agent"`
	Scanner   string `json:"scanner"`
	UUID      string `json:"uuid"`
	UpdatedAt *int64 `json:"updatedAt"`
}

type sectionsResponse struct {
	MediaContainer struct {
		Directory []sectionDir `json:"Directory"`
	} `json:"MediaContainer"`
}

// syncSections refreshes the section list and returns sections with their
// watermarks. On full sweeps, sections absent from the origin are removed.
func (w *Worker) syncSections(ctx context.Context, serverID, token string) ([]sectionInfo, error) {
	raw, err := w.Client.GetJSON(ctx, w.Origin+"/library/sections", token, 8<<20)
	if err != nil {
		return nil, err
	}
	var parsed sectionsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("sync: sections parse: %w", err)
	}
	seen := map[string]bool{}
	var out []sectionInfo
	for _, d := range parsed.MediaContainer.Directory {
		if d.Key == "" {
			continue
		}
		seen[d.Key] = true
		var libraryID string
		err := w.DB.QueryRow(ctx, `INSERT INTO libraries(server_id, plex_section_id, section_uuid, title, media_type, agent, scanner, updated_at_origin, synced_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,to_timestamp($8),now())
			ON CONFLICT (server_id, plex_section_id) DO UPDATE SET
				section_uuid=EXCLUDED.section_uuid, title=EXCLUDED.title, media_type=EXCLUDED.media_type,
				agent=EXCLUDED.agent, scanner=EXCLUDED.scanner,
				updated_at_origin=EXCLUDED.updated_at_origin, synced_at=now()
			RETURNING id`, serverID, d.Key, nullIfEmpty(d.UUID), d.Title, d.Type,
			nullIfEmpty(d.Agent), nullIfEmpty(d.Scanner), epochOrNil(d.UpdatedAt)).Scan(&libraryID)
		if err != nil {
			return nil, fmt.Errorf("sync: upsert library %s: %w", d.Key, err)
		}
		// NeverSynced is approximated: sections touched long ago with no
		// items row are picked up by the items pass watermark below.
		var items int
		_ = w.DB.QueryRow(ctx, `SELECT count(*) FROM library_items li JOIN libraries l ON l.id=li.library_id
			WHERE l.server_id=$1 AND l.plex_section_id=$2`, serverID, d.Key).Scan(&items)
		out = append(out, sectionInfo{LibraryID: libraryID, SectionID: d.Key, Title: d.Title, MediaType: d.Type, NeverSynced: items == 0})
	}
	return out, nil
}

// syncSection paginates one section's items with a resumable cursor. The
// cursor carries its sweep generation: a full sweep with a fresh
// generation always restarts at zero, so resuming a cursor from a crashed
// older sweep can never skip pages. On a full sweep, items outside the
// current generation are deleted; light passes stamp nothing.
func (w *Worker) syncSection(ctx context.Context, serverID, token string, s sectionInfo, full bool, gen int64) error {
	start, savedGen := w.loadCursor(ctx, serverID, s.LibraryID)
	trackGen := savedGen
	if full {
		if savedGen != gen {
			start = 0
		}
		trackGen = gen
	} else {
		// Light dirty refreshes always restart at zero: a completed
		// cursor points at the end, and resuming it would revisit
		// nothing. Progress is still saved per page for visibility.
		start = 0
	}
	page := w.PageSize
	if page <= 0 {
		page = 100
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		url := fmt.Sprintf("%s/library/sections/%s/all?X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
			w.Origin, s.SectionID, start, page)
		raw, err := w.Client.GetJSON(ctx, url, token, 8<<20)
		if err != nil {
			return err
		}
		items, total, err := parseItemsPage(raw)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := w.upsertItem(ctx, serverID, s.LibraryID, it, genOrNil(full, gen)); err != nil {
				return err
			}
			if w.Metrics != nil {
				w.Metrics.IncSyncItemsTotal()
			}
		}
		start += len(items)
		_ = w.saveCursor(ctx, serverID, s.LibraryID, start, trackGen)
		if len(items) < page || (total >= 0 && start >= total) {
			break
		}
	}
	if full {
		if _, err := w.DB.Exec(ctx, `DELETE FROM library_items
			WHERE library_id=$1 AND sweep_gen IS DISTINCT FROM $2`,
			s.LibraryID, gen); err != nil {
			return fmt.Errorf("sync: sweep delete: %w", err)
		}
	}
	return nil
}

// genOrNil stamps full-sweep rows with the sweep generation and leaves
// light-pass rows untouched for the next full sweep to judge.
func genOrNil(full bool, gen int64) *int64 {
	if !full {
		return nil
	}
	return &gen
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func epochOrNil(sec *int64) any {
	if sec == nil {
		return nil
	}
	return float64(*sec)
}

func intOrNil(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func int64OrNil(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
