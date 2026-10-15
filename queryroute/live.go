package queryroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// LiveSuffix is appended to a read route's path for its live route:
// GET /api/cqrs/rm/{collection}/live.
const LiveSuffix = "/live"

// hub fans change notices for a collection out to its live viewers. One
// per app, kept in the app's store.
type hub struct {
	mu    sync.Mutex
	subs  map[string]map[chan struct{}]struct{}
	bound map[string]bool
}

const hubKey = "pocketcqrs.queryroute.live"

func hubOf(app core.App) *hub {
	if h, ok := app.Store().Get(hubKey).(*hub); ok {
		return h
	}
	h := &hub{subs: map[string]map[chan struct{}]struct{}{}, bound: map[string]bool{}}
	app.Store().Set(hubKey, h)
	return h
}

// watch makes every committed change to collection a notice, and, for a
// read model holding personal data, every subject newly erased (no row
// changes then, but its values now read redacted). PocketBase's
// ...AfterXSuccess hooks run after the write commits, so a viewer re-reading
// on the notice sees the new rows, never the old ones.
func (h *hub) watch(app core.App, spec Spec, cfg Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bound[spec.Collection] {
		return
	}
	h.bound[spec.Collection] = true
	notice := func(e *core.RecordEvent) error {
		h.notify(spec.Collection)
		return e.Next()
	}
	app.OnRecordAfterCreateSuccess(spec.Collection).BindFunc(notice)
	app.OnRecordAfterUpdateSuccess(spec.Collection).BindFunc(notice)
	app.OnRecordAfterDeleteSuccess(spec.Collection).BindFunc(notice)
	if len(spec.PII) > 0 && cfg.PII != nil && cfg.PII.Cache != nil {
		cfg.PII.Cache.OnErased(func(string) { h.notify(spec.Collection) })
	}
}

// notify tells every viewer of collection that it may have changed. It
// never blocks: a viewer already holding a notice gets no second one, so
// changes landing while it re-reads coalesce into one more read.
func (h *hub) notify(collection string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[collection] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *hub) subscribe(collection string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[collection] == nil {
		h.subs[collection] = map[chan struct{}]struct{}{}
	}
	h.subs[collection][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[collection], ch)
		h.mu.Unlock()
	}
}

// live serves the read route's result as server-sent events: an `event:
// view` with the same body GET returns, for this viewer and under the same
// access rules and reveals, and then another whenever a committed change
// (or an erasure) changes it. It ends when the viewer disconnects, or with
// `event: expired` when their token does. Nothing polls: each push follows
// a notice from the write that caused it.
func live(re *core.RequestEvent, spec Spec, cfg Config) error {
	h := hubOf(re.App)
	notices, unsubscribe := h.subscribe(spec.Collection)
	defer unsubscribe()

	status, body := answer(re, spec, cfg)
	if status != http.StatusOK {
		return re.JSON(status, body) // refused (or failing) before any stream
	}
	flusher, ok := re.Response.(http.Flusher)
	if !ok {
		return re.JSON(http.StatusInternalServerError, map[string]string{"error": "streaming is not supported"})
	}
	header := re.Response.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("Connection", "keep-alive")
	re.Response.WriteHeader(http.StatusOK)

	var last []byte
	send := func(event string, body any) error {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if event == "view" && bytes.Equal(data, last) {
			return nil // unchanged for this viewer: nothing to push
		}
		if event == "view" {
			last = data
		}
		if _, err := fmt.Fprintf(re.Response, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := send("view", body); err != nil {
		return nil
	}

	var expiry <-chan time.Time
	if until := tokenExpiry(re); !until.IsZero() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		select {
		case <-re.Request.Context().Done():
			return nil
		case <-expiry:
			_ = send("expired", map[string]string{"reason": "the token expired; open the view again"})
			return nil
		case <-notices:
			status, body := answer(re, spec, cfg)
			event := "view"
			if status != http.StatusOK {
				event = "error" // e.g. the key service is down: say so, keep the stream
			}
			if err := send(event, body); err != nil {
				return nil
			}
		}
	}
}

// tokenExpiry is when the request's auth token expires (zero when unknown).
// The token was already verified by PocketBase's auth middleware; only its
// exp claim is read here.
func tokenExpiry(re *core.RequestEvent) time.Time {
	token := strings.TrimSpace(strings.TrimPrefix(re.Request.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		return time.Time{}
	}
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		return time.Time{}
	}
	switch exp := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0)
	case json.Number:
		if n, err := exp.Int64(); err == nil {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}
