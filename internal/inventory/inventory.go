// Package inventory provides a bounded, read-only inventory of a Plex Media Server.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config bounds both individual requests and the number of concurrent requests.
type Config struct {
	BaseURL        string
	Token          string
	PageSize       int
	Workers        int
	RequestTimeout time.Duration
	MaxPages       int
}

type Count struct {
	Value  int64  `json:"value"`
	Status string `json:"status"`          // measured, unknown, or error
	Error  string `json:"error,omitempty"` // sanitized stage/status; never an arbitrary server body
}

func (r Report) HasIncompleteCounts() bool {
	counts := []Count{r.Sections, r.Movies, r.Shows, r.Seasons, r.Episodes, r.Collections}
	for _, s := range r.Libraries {
		counts = append(counts, s.Movies, s.Shows, s.Seasons, s.Episodes, s.Collections)
	}
	for _, c := range counts {
		if c.Status != "measured" {
			return true
		}
	}
	return false
}

type Section struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Type        string `json:"type,omitempty"`
	Movies      Count  `json:"movies"`
	Shows       Count  `json:"shows"`
	Seasons     Count  `json:"seasons"`
	Episodes    Count  `json:"episodes"`
	Collections Count  `json:"collections"`
}

type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Sections    Count     `json:"sections"`
	Movies      Count     `json:"movies"`
	Shows       Count     `json:"shows"`
	Seasons     Count     `json:"seasons"`
	Episodes    Count     `json:"episodes"`
	Collections Count     `json:"collections"`
	Libraries   []Section `json:"libraries,omitempty"`
	Assumptions []string  `json:"assumptions"`
}

type Client struct {
	base     *url.URL
	token    string
	pageSize int
	workers  int
	maxPages int
	timeout  time.Duration
	http     *http.Client
	sem      chan struct{}
}

// New validates configuration. Plain HTTP is accepted only for loopback PMS URLs
// to support local fixtures; production URLs must use HTTPS.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("inventory: PMS token is required")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" {
		return nil, errors.New("inventory: invalid PMS URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("inventory: PMS URL must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("inventory: PMS URL must use HTTPS (HTTP allowed only for loopback)")
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = 200
	}
	if cfg.Workers == 0 {
		cfg.Workers = 4
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 10000
	}
	if cfg.PageSize < 1 || cfg.PageSize > 1000 {
		return nil, errors.New("inventory: page size must be 1..1000")
	}
	if cfg.Workers < 1 || cfg.Workers > 16 {
		return nil, errors.New("inventory: workers must be 1..16")
	}
	if cfg.RequestTimeout < time.Second || cfg.RequestTimeout > 2*time.Minute {
		return nil, errors.New("inventory: request timeout must be 1s..2m")
	}
	if cfg.MaxPages < 1 || cfg.MaxPages > 100000 {
		return nil, errors.New("inventory: max pages must be 1..100000")
	}
	return &Client{base: u, token: cfg.Token, pageSize: cfg.PageSize, workers: cfg.Workers, maxPages: cfg.MaxPages, timeout: cfg.RequestTimeout,
		http: &http.Client{Timeout: cfg.RequestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, sem: make(chan struct{}, cfg.Workers)}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Inventory queries only read endpoints. The returned report includes partial
// results when individual sections or nested requests fail.
func (c *Client) Inventory(ctx context.Context) Report {
	r := Report{GeneratedAt: time.Now().UTC(), Assumptions: []string{
		"Sections use GET /library/sections and MediaContainer.Directory.",
		"Movie, show, season, and episode counts use GET /library/sections/{id}/all?type=1|2|3|4 with start/size pagination, for movie/show sections as applicable.",
		"Collections use GET /library/sections/{id}/collections with start/size pagination.",
		"Counts are API inventory counts only; asset byte sizes and representative image sizes are not measured.",
	}}
	sections, err := c.list(ctx, "/library/sections", nil)
	if err != nil {
		r.Sections = Count{Status: "error", Error: safeError(err)}
		setAllError(&r, safeError(err))
		return r
	}
	r.Sections = Count{Value: int64(len(sections)), Status: "measured"}
	for _, raw := range sections {
		id := raw.Key
		if id == "" {
			failed := Count{Status: "error", Error: "section entry has no key"}
			r.Libraries = append(r.Libraries, Section{Title: raw.Title, Type: raw.Type, Movies: failed, Shows: failed, Seasons: failed, Episodes: failed, Collections: failed})
			continue
		}
		s := Section{ID: id, Title: raw.Title, Type: raw.Type}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.Collections = c.countItems(ctx, "/library/sections/"+url.PathEscape(id)+"/collections", nil)
		}()
		go func() {
			defer wg.Done()
			if raw.Type == "movie" {
				s.Movies = c.countItems(ctx, "/library/sections/"+url.PathEscape(id)+"/all", url.Values{"type": {"1"}})
				s.Shows, s.Seasons, s.Episodes = emptyCount(), emptyCount(), emptyCount()
			} else if raw.Type == "show" {
				s.Movies = emptyCount()
				s.Shows = c.countItems(ctx, "/library/sections/"+url.PathEscape(id)+"/all", url.Values{"type": {"2"}})
				s.Seasons = c.countItems(ctx, "/library/sections/"+url.PathEscape(id)+"/all", url.Values{"type": {"3"}})
				s.Episodes = c.countItems(ctx, "/library/sections/"+url.PathEscape(id)+"/all", url.Values{"type": {"4"}})
			} else {
				s.Movies, s.Shows, s.Seasons, s.Episodes = unknownCount(), unknownCount(), unknownCount(), unknownCount()
			}
		}()
		wg.Wait()
		r.Libraries = append(r.Libraries, s)
	}
	r.Movies = sumCounts(r.Libraries, func(s Section) Count { return s.Movies })
	r.Shows = sumCounts(r.Libraries, func(s Section) Count { return s.Shows })
	r.Seasons = sumCounts(r.Libraries, func(s Section) Count { return s.Seasons })
	r.Episodes = sumCounts(r.Libraries, func(s Section) Count { return s.Episodes })
	r.Collections = sumCounts(r.Libraries, func(s Section) Count { return s.Collections })
	return r
}

type item struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	RatingKey string `json:"ratingKey"`
}
type container struct {
	MediaContainer struct {
		TotalSize *int   `json:"totalSize"`
		Offset    *int   `json:"offset"`
		Directory []item `json:"Directory"`
		Metadata  []item `json:"Metadata"`
	} `json:"MediaContainer"`
}

func (c *Client) list(ctx context.Context, path string, query url.Values) ([]item, error) {
	var all []item
	previousPage := ""
	for pageNo := 0; ; pageNo++ {
		if pageNo >= c.maxPages {
			return all, errors.New("pagination page limit reached")
		}
		offset := pageNo * c.pageSize
		payload, page, err := c.fetchPage(ctx, path, query, offset, c.pageSize)
		if err != nil {
			return all, err
		}
		if payload.MediaContainer.Offset != nil && *payload.MediaContainer.Offset != offset {
			return all, errors.New("pagination offset mismatch")
		}
		if payload.MediaContainer.TotalSize != nil && offset+len(page) < *payload.MediaContainer.TotalSize && len(page) < c.pageSize {
			return all, errors.New("pagination ended before reported total")
		}
		if len(page) >= c.pageSize {
			encoded, _ := json.Marshal(page)
			fingerprint := string(encoded)
			if fingerprint == previousPage {
				return all, errors.New("pagination did not advance")
			}
			previousPage = fingerprint
		}
		all = append(all, page...)
		if len(page) < c.pageSize || (payload.MediaContainer.TotalSize != nil && offset+len(page) >= *payload.MediaContainer.TotalSize) {
			return all, nil
		}
		if len(page) == 0 {
			return all, nil
		}
	}
}

func (c *Client) countItems(ctx context.Context, path string, q url.Values) Count {
	// Plex normally reports totalSize. Ask for one entry first so large libraries
	// can be counted without downloading or retaining their metadata.
	first, firstItems, err := c.fetchPage(ctx, path, q, 0, 1)
	if err != nil {
		return Count{Status: "error", Error: safeError(err)}
	}
	if first.MediaContainer.Offset != nil && *first.MediaContainer.Offset != 0 {
		return Count{Status: "error", Error: "pagination offset mismatch"}
	}
	if first.MediaContainer.TotalSize != nil {
		total := *first.MediaContainer.TotalSize
		if total < 0 || (total > 0 && len(firstItems) == 0) || (total == 0 && len(firstItems) > 0) {
			return Count{Status: "error", Error: "response contradicts reported total"}
		}
		return Count{Value: int64(total), Status: "measured"}
	}
	// Older or nonstandard PMS builds may omit totalSize. Fall back to bounded
	// page-by-page counting without retaining metadata rows.
	return c.countPages(ctx, path, q)
}

func (c *Client) countPages(ctx context.Context, path string, q url.Values) Count {
	var total int64
	previousPage := ""
	for pageNo := 0; pageNo < c.maxPages; pageNo++ {
		offset := pageNo * c.pageSize
		payload, items, err := c.fetchPage(ctx, path, q, offset, c.pageSize)
		if err != nil {
			return Count{Value: total, Status: "error", Error: safeError(err)}
		}
		if payload.MediaContainer.Offset != nil && *payload.MediaContainer.Offset != offset {
			return Count{Value: total, Status: "error", Error: "pagination offset mismatch"}
		}
		if payload.MediaContainer.TotalSize != nil && offset+len(items) < *payload.MediaContainer.TotalSize && len(items) < c.pageSize {
			return Count{Value: total, Status: "error", Error: "pagination ended before reported total"}
		}
		if len(items) >= c.pageSize {
			b, _ := json.Marshal(items)
			fp := string(b)
			if fp == previousPage {
				return Count{Value: total, Status: "error", Error: "pagination did not advance"}
			}
			previousPage = fp
		}
		total += int64(len(items))
		if len(items) < c.pageSize || (payload.MediaContainer.TotalSize != nil && offset+len(items) >= *payload.MediaContainer.TotalSize) {
			return Count{Value: total, Status: "measured"}
		}
	}
	return Count{Value: total, Status: "error", Error: "pagination page limit reached"}
}

func (c *Client) fetchPage(ctx context.Context, path string, query url.Values, offset, size int) (container, []item, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = append([]string(nil), v...)
	}
	q.Set("X-Plex-Container-Start", strconv.Itoa(offset))
	q.Set("X-Plex-Container-Size", strconv.Itoa(size))
	var payload container
	if err := c.getJSON(ctx, path, q, &payload); err != nil {
		return payload, nil, err
	}
	items := payload.MediaContainer.Metadata
	if items == nil {
		items = payload.MediaContainer.Directory
	}
	return payload, items, nil
}

func unknownCount() Count {
	return Count{Status: "unknown", Error: "section type is not supported for this count"}
}
func emptyCount() Count { return Count{Status: "measured"} }

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return errors.New("context canceled")
	}
	defer func() { <-c.sem }()
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("request construction failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("X-Plex-Client-Identifier", "replx-inventory")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("request failed (network, TLS, or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid or oversized JSON response")
	}
	return nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.HasPrefix(s, "HTTP status ") {
		return s
	}
	if s == "context canceled" {
		return s
	}
	return "request failed"
}
func sumCounts(sections []Section, f func(Section) Count) Count {
	var total int64
	for _, s := range sections {
		c := f(s)
		if c.Status != "measured" {
			return Count{Status: "unknown", Error: "one or more libraries could not be counted"}
		}
		total += c.Value
	}
	return Count{Value: total, Status: "measured"}
}
func setAllError(r *Report, msg string) {
	c := Count{Status: "error", Error: msg}
	r.Movies = c
	r.Shows = c
	r.Seasons = c
	r.Episodes = c
	r.Collections = c
}
