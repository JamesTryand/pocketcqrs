package outbox

import (
	"context"
	"encoding/json"
	"time"
)

// Outcome is what a Handler made of an entry: Done, or due again at RetryAt
// (with Payload as its new state, if not nil, and Err as why). A handler
// that gives up dead-letters the work itself and reports Done.
type Outcome struct {
	Done    bool
	RetryAt time.Time
	Payload json.RawMessage
	Err     error
}

// Handler does one due entry's work.
type Handler func(ctx context.Context, e Entry) Outcome

// Worker takes due entries and hands each to its consumer's Handler. An
// entry whose consumer has no handler (not registered in this process yet)
// waits for one.
type Worker struct {
	Store    *Store
	Handlers map[string]Handler
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Logger receives operational lines; nil is a no-op.
	Logger func(msg string, args ...any)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) log(msg string, args ...any) {
	if w.Logger != nil {
		w.Logger(msg, args...)
	}
}

// Nudge wakes Run to look for due work now (Enqueue does it).
func (w *Worker) Nudge() { w.Store.nudge() }

// RunOnce handles every entry due now, and reports how many it handled.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	handled := 0
	for {
		due, err := w.Store.Due(ctx, w.now(), 100)
		if err != nil {
			return handled, err
		}
		progressed := false
		for _, e := range due {
			h, ok := w.Handlers[e.Consumer]
			if !ok {
				continue
			}
			progressed = true
			handled++
			out := h(ctx, e)
			if out.Done {
				if err := w.Store.Done(ctx, e.ID); err != nil {
					return handled, err
				}
				continue
			}
			msg := ""
			if out.Err != nil {
				msg = out.Err.Error()
			}
			w.log("outbox entry due again", "consumer", e.Consumer, "id", e.ID,
				"attempt", e.Attempts+1, "due", out.RetryAt, "error", msg)
			if err := w.Store.Reschedule(ctx, e.ID, out.Payload, e.Attempts+1, out.RetryAt, msg); err != nil {
				return handled, err
			}
		}
		if !progressed || len(due) < 100 {
			return handled, nil
		}
	}
}

// Run handles due work until ctx is done: each pass takes what is due, then
// waits until the next entry is due or a nudge says there is new work.
func (w *Worker) Run(ctx context.Context) {
	for {
		if _, err := w.RunOnce(ctx); err != nil {
			w.log("outbox pass failed", "error", err)
		}
		// the next entry this worker can take; one whose consumer has no
		// handler here waits for one and never wakes it
		var timer *time.Timer
		var due <-chan time.Time
		if next, ok, err := w.Store.NextDue(ctx, w.consumers()...); err == nil && ok {
			timer = time.NewTimer(max(next.Sub(w.now()), 0))
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-w.Store.wake:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (w *Worker) consumers() []string {
	out := make([]string, 0, len(w.Handlers))
	for c := range w.Handlers {
		out = append(out, c)
	}
	return out
}
