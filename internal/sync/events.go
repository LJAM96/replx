package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/origin"
)

// errNoCredentials pauses the event stream until onboarding completes.
var errNoCredentials = errors.New("sync: no owner credential for event stream")

// sectionKey extracts a plex section ID from an activity context key such
// as /library/sections/22/refresh.
var sectionKey = regexp.MustCompile(`/library/sections/(\d+)`)

// eventEnvelope is the PMS NotificationContainer wrapper on the SSE stream.
type eventEnvelope struct {
	NotificationContainer struct {
		Type          string         `json:"type"`
		Activity      *eventActivity `json:"Activity"`
		TimelineEntry []any          `json:"TimelineEntry"`
	} `json:"NotificationContainer"`
}

type eventActivity struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	Progress *int   `json:"progress"`
	Context  *struct {
		Key string `json:"key"`
	} `json:"Context"`
}

// handleEventData processes one SSE data payload. Library refresh activity
// flags the affected section dirty; timeline/watch-state events are left to
// the short response-cache TTLs (documented bound, not losslessness: the
// 15-minute light pass reconciles anything missed).
func (w *Worker) handleEventData(payload string) {
	var env eventEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return
	}
	nc := env.NotificationContainer
	if nc.Type != "activity" || nc.Activity == nil {
		return
	}
	a := nc.Activity
	if !strings.Contains(strings.ToLower(a.Subtype), "library.refresh.items") {
		return
	}
	done := false
	if a.Progress != nil && *a.Progress >= 100 {
		done = true
	}
	if !done {
		return
	}
	var key string
	if a.Context != nil {
		key = a.Context.Key
	}
	if m := sectionKey.FindStringSubmatch(key); m != nil {
		w.MarkDirty(m[1])
	}
}

// Subscribe consumes the PMS event stream until ctx ends, reconnecting with
// backoff. It runs beside Run; a dead stream degrades to periodic
// reconciliation, never to failed sync.
func (w *Worker) Subscribe(ctx context.Context) {
	if w == nil || w.DB == nil {
		return
	}
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.subscribeOnce(ctx); err != nil {
			w.log("events_reconnect", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}

func (w *Worker) subscribeOnce(ctx context.Context) error {
	token, ok := w.OwnerToken(ctx)
	if !ok {
		return errNoCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(w.Origin, "/")+"/:/eventsource/notifications", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Plex-Token", token)
	client, err := origin.StreamClient(w.Origin)
	if err != nil {
		return err
	}
	resp, err := client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sync: event stream status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			w.handleEventData(strings.TrimSpace(payload))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return scanner.Err()
}
