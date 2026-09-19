// Package sync owns the Gamma persistent Plex index: owner-synchronized
// libraries, items, GUIDs, media variants, parts and streams.
//
// The index is shared internal data for variant lookup, canonical identity,
// search candidates and administration. It is NEVER returned directly as a
// user response: user visibility is enforced at serving time (policy and
// search fall back to PMS whenever visibility is uncertain).
//
// Sync is resumable: per-section page cursors persist in sync_cursors, so
// an interrupted pass continues mid-section within the same process run.
// A process restart begins the current section at zero under a fresh
// sweep generation instead of trusting a stale cursor: restarts cost some
// origin requests but can never skip pages. Crash safety beats resume
// precision for 1.0.
package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
)

// ErrInFlight reports a second concurrent SyncOnce; full sync is single
// flight per the admin contract.
var ErrInFlight = errors.New("sync: another sync is already running")

// Worker performs owner library synchronization against one origin.
type Worker struct {
	DB         database.DBTX
	Origin     string
	OwnerToken func(ctx context.Context) (string, bool)
	Logger     *logging.Logger
	Metrics    *metrics.Registry
	Client     HTTPClient

	LightInterval time.Duration
	FullInterval  time.Duration
	PageSize      int

	mu       sync.Mutex
	inFlight bool
	dirty    map[string]bool
}

// HTTPClient is the minimal origin client surface (injectable for tests).
type HTTPClient interface {
	GetJSON(ctx context.Context, url, token string, limit int64) ([]byte, error)
}

// New builds a Worker with Production defaults: light reconciliation every
// 15 minutes, full consistency sweep every 6 hours, 100-item pages.
func New(db database.DBTX, origin string, owner func(ctx context.Context) (string, bool),
	logger *logging.Logger, reg *metrics.Registry) *Worker {
	return &Worker{
		DB: db, Origin: origin, OwnerToken: ownerTokenOrNil(owner),
		Logger: logger, Metrics: reg,
		Client:        NewOriginClient(30 * time.Second),
		LightInterval: 15 * time.Minute, FullInterval: 6 * time.Hour,
		PageSize: 100, dirty: map[string]bool{},
	}
}

func ownerTokenOrNil(owner func(ctx context.Context) (string, bool)) func(ctx context.Context) (string, bool) {
	if owner != nil {
		return owner
	}
	return func(ctx context.Context) (string, bool) { return "", false }
}

// MarkDirty flags a section (plex section ID) for the next light pass.
func (w *Worker) MarkDirty(sectionID string) {
	if w == nil || sectionID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirty[sectionID] = true
}

// Run performs an initial full sync in the background, then light passes
// on LightInterval and full sweeps on FullInterval until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.DB == nil {
		return
	}
	go func() {
		if err := w.SyncOnce(ctx, true); err != nil && !errors.Is(err, ErrInFlight) {
			w.log("sync_initial", err.Error())
		}
	}()
	light := time.NewTicker(w.LightInterval)
	defer light.Stop()
	full := time.NewTicker(w.FullInterval)
	defer full.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-light.C:
			if err := w.SyncOnce(ctx, false); err != nil && !errors.Is(err, ErrInFlight) {
				w.log("sync_light", err.Error())
			}
		case <-full.C:
			if err := w.SyncOnce(ctx, true); err != nil && !errors.Is(err, ErrInFlight) {
				w.log("sync_full", err.Error())
			}
		}
	}
}

// SyncOnce runs one light (full=false) or full pass. Concurrent callers get
// ErrInFlight; the running pass is unaffected.
func (w *Worker) SyncOnce(ctx context.Context, full bool) error {
	w.mu.Lock()
	if w.inFlight {
		w.mu.Unlock()
		return ErrInFlight
	}
	w.inFlight = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight = false
		w.mu.Unlock()
	}()
	token, ok := w.OwnerToken(ctx)
	if !ok {
		return fmt.Errorf("sync: no owner credential (onboarding incomplete)")
	}
	serverID, err := enabledServer(ctx, w.DB)
	if err != nil {
		return err
	}
	if err := w.setCursor(ctx, serverID, "libraries", nil, "running", ""); err != nil {
		return err
	}
	sections, err := w.syncSections(ctx, serverID, token)
	if err != nil {
		w.countErr()
		_ = w.setCursor(ctx, serverID, "libraries", nil, "error", err.Error())
		return err
	}
	_ = w.setCursor(ctx, serverID, "libraries", nil, "complete", "")
	// A full sweep owns a fresh generation: rows it stamps are the only
	// ones the end-of-section delete may spare. A crashed sweep's
	// generation never completes, so its partial stamps cannot delete
	// rows the next sweep has not visited yet.
	var gen int64
	if full {
		gen = time.Now().UnixNano()
	}
	for _, s := range sections {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		refresh := full || w.isDirty(s.SectionID) || s.NeverSynced
		if !refresh {
			continue
		}
		if err := w.syncSection(ctx, serverID, token, s, full, gen); err != nil {
			w.countErr()
			_ = w.setCursor(ctx, serverID, "section", &s.LibraryID, "error", err.Error())
			// One bad section must not abort the sweep.
			continue
		}
		_ = w.setCursor(ctx, serverID, "section", &s.LibraryID, "complete", "")
		w.clearDirty(s.SectionID)
	}
	return nil
}

func (w *Worker) log(event, errText string) {
	if w.Logger == nil {
		return
	}
	fields := map[string]any{"event": event}
	if errText != "" {
		fields["error"] = errText
	}
	w.Logger.Log(logging.Entry{Level: "info", Component: "sync", Fields: fields})
}

func (w *Worker) countErr() {
	if w.Metrics != nil {
		w.Metrics.IncSyncErrorsTotal()
	}
}

func (w *Worker) isDirty(sectionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirty[sectionID]
}

func (w *Worker) clearDirty(sectionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.dirty, sectionID)
}

func enabledServer(ctx context.Context, db database.DBTX) (string, error) {
	var id string
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.QueryRow(cctx, `SELECT id FROM plex_servers
		WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		return "", fmt.Errorf("sync: no enabled server: %w", err)
	}
	return id, nil
}
