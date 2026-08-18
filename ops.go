package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/events"
)

// opsStoreErr maps a store-write failure on an ops route. On a
// --cqrsRole=secondary the store is read-only and every write fails with
// events.ErrReadOnly — answered as a clean 503 naming the actual problem,
// the same mapping gateway.go gives commands, instead of the generic 400
// that used to leak the raw "events: store is read-only" string.
func opsStoreErr(re *core.RequestEvent, err error) error {
	if errors.Is(err, events.ErrReadOnly) {
		return re.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "this node is a read-only replica; admin writes must go to the master",
		})
	}
	return apis.NewBadRequestError(err.Error(), err)
}

// registerOpsRoutes binds the superuser-only operational API:
//
//	GET  /api/cqrs/events?after=&before=&limit=&aggregate=&aggregateId=&type=
//	GET  /api/cqrs/streams?aggregate=
//	GET  /api/cqrs/deadletters?all=
//	POST /api/cqrs/deadletters/{id}/retry
//	POST /api/cqrs/deadletters/{id}/dismiss
//	POST /api/cqrs/deadletters/retry            (every pending dead letter)
//	GET  /api/cqrs/admin/mode
//	POST /api/cqrs/admin/mode   body: {"mode":"running"|"maintenance"}
//
// The events feed is the platform's public log interface: dashboards and
// out-of-process read-model consumers tail the log through it instead of
// touching events.db (preserving the single-process model). All reads see
// events at their latest schema version (store-level upcasting).
func registerOpsRoutes(e *core.ServeEvent, c *components) {
	// the log feed, in position order; limit defaults to 100, capped at 1000
	e.Router.GET("/api/cqrs/events", func(re *core.RequestEvent) error {
		qv := re.Request.URL.Query()
		after, _ := strconv.ParseInt(qv.Get("after"), 10, 64)
		before, _ := strconv.ParseInt(qv.Get("before"), 10, 64)
		limit, _ := strconv.Atoi(qv.Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		if limit > 1000 {
			limit = 1000
		}
		evs, err := c.store.QueryEvents(re.Request.Context(), events.EventQuery{
			After:       after,
			Before:      before,
			Limit:       limit,
			Aggregate:   qv.Get("aggregate"),
			AggregateID: qv.Get("aggregateId"),
			Type:        qv.Get("type"),
		})
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"events": evs})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// one row per stream, optionally restricted to one aggregate
	e.Router.GET("/api/cqrs/streams", func(re *core.RequestEvent) error {
		streams, err := c.store.ListStreamInfos(re.Request.Context(),
			re.Request.URL.Query().Get("aggregate"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"streams": streams})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// failed function deliveries; pending only unless ?all=1
	e.Router.GET("/api/cqrs/deadletters", func(re *core.RequestEvent) error {
		includeResolved := re.Request.URL.Query().Get("all") == "1"
		letters, err := c.store.DeadLetters(re.Request.Context(), includeResolved)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"deadLetters": letters})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// re-deliver one dead letter through the CURRENT function code.
	//
	// The route is more specific than the gateway's
	// POST /api/cqrs/{aggregate}/{id}/{command}, so http.ServeMux (which
	// PocketBase's router wraps) matches this one first.
	e.Router.POST("/api/cqrs/deadletters/{id}/retry", func(re *core.RequestEvent) error {
		id, err := strconv.ParseInt(re.Request.PathValue("id"), 10, 64)
		if err != nil {
			return apis.NewBadRequestError("invalid dead letter id", err)
		}
		letters, err := c.pendingDeadLetters(re.Request.Context(), id)
		if err != nil {
			return err
		}
		results, err := c.retryDeadLetters(re.Request.Context(), letters)
		if err != nil {
			return opsStoreErr(re, err)
		}
		return re.JSON(http.StatusOK, results[0])
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// re-deliver every pending dead letter, oldest first
	e.Router.POST("/api/cqrs/deadletters/retry", func(re *core.RequestEvent) error {
		letters, err := c.store.DeadLetters(re.Request.Context(), false)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		results, err := c.retryDeadLetters(re.Request.Context(), letters)
		if err != nil {
			return opsStoreErr(re, err)
		}
		return re.JSON(http.StatusOK, map[string]any{"results": results})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// resolve a dead letter without re-delivering it
	e.Router.POST("/api/cqrs/deadletters/{id}/dismiss", func(re *core.RequestEvent) error {
		id, err := strconv.ParseInt(re.Request.PathValue("id"), 10, 64)
		if err != nil {
			return apis.NewBadRequestError("invalid dead letter id", err)
		}
		if err := c.store.ResolveDeadLetter(re.Request.Context(), id); err != nil {
			if errors.Is(err, events.ErrReadOnly) {
				return opsStoreErr(re, err)
			}
			return apis.NewNotFoundError(fmt.Sprintf("no dead letter with id %d", id), err)
		}
		return re.JSON(http.StatusOK, deadLetterResult{ID: id, Resolved: true})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	// the system mode barrier (running|maintenance); POST wraps SetMode,
	// so an invalid value is a 400 and the current mode never changes
	e.Router.GET("/api/cqrs/admin/mode", func(re *core.RequestEvent) error {
		mode, err := c.store.Mode(re.Request.Context())
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]string{"mode": mode})
	}).Bind(authverify.RequireSuperuser(c.verifier))

	e.Router.POST("/api/cqrs/admin/mode", func(re *core.RequestEvent) error {
		payload, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return apis.NewBadRequestError("invalid JSON body: "+err.Error(), err)
		}
		if err := c.store.SetMode(re.Request.Context(), body.Mode); err != nil {
			return opsStoreErr(re, err)
		}
		return re.JSON(http.StatusOK, map[string]string{"mode": body.Mode})
	}).Bind(authverify.RequireSuperuser(c.verifier))
}

// deadLetterResult reports the outcome of one retry or dismissal.
//
// A retry that fails again is NOT an HTTP error: a poison event staying
// poison is the ordinary case, and the caller needs to tell it apart from
// "the endpoint is broken". So every adjudicated delivery answers 200 with
// Resolved=false and the failure text; 4xx is reserved for a malformed or
// unknown id.
type deadLetterResult struct {
	ID       int64  `json:"id"`
	Consumer string `json:"consumer,omitempty"`
	Resolved bool   `json:"resolved"`
	Attempts int64  `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
}

// pendingDeadLetters returns the single pending dead letter with the given
// id, or a 404 when there is none.
func (c *components) pendingDeadLetters(ctx context.Context, id int64) ([]events.DeadLetter, error) {
	letters, err := c.store.DeadLetters(ctx, false)
	if err != nil {
		return nil, apis.NewBadRequestError(err.Error(), err)
	}
	letters = filterLetters(letters, id)
	if len(letters) == 0 {
		return nil, apis.NewNotFoundError(fmt.Sprintf("no pending dead letter with id %d", id), nil)
	}
	return letters, nil
}

// retryDeadLetters re-delivers each letter through the current function
// code, resolving the ones that now succeed and recording an attempt on the
// ones that do not — the same adjudication as `pocketcqrs deadletter retry`,
// shared so the CLI and the HTTP API can never drift apart.
//
// It holds reloadMu because a hot reload swaps c.fnRuntime wholesale: a
// retry must run entirely against one runtime, not half of each.
func (c *components) retryDeadLetters(ctx context.Context, letters []events.DeadLetter) ([]deadLetterResult, error) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	if c.fnRuntime == nil {
		return nil, errors.New("function runtime not initialized")
	}

	results := make([]deadLetterResult, 0, len(letters))
	for _, dl := range letters {
		res := deadLetterResult{ID: dl.ID, Consumer: dl.Consumer, Attempts: dl.Attempts}
		// dead letters record the consumer name ("fn:<name>"); the runtime
		// knows the function by its bare name
		name := strings.TrimPrefix(dl.Consumer, "fn:")
		if err := c.fnRuntime.RetryEventFunction(name, dl.Event); err != nil {
			res.Error = err.Error()
			res.Attempts = dl.Attempts + 1
			if serr := c.store.FailDeadLetterRetry(ctx, dl.ID, err); serr != nil {
				return nil, serr
			}
		} else {
			res.Resolved = true
			if serr := c.store.ResolveDeadLetter(ctx, dl.ID); serr != nil {
				return nil, serr
			}
		}
		results = append(results, res)
	}
	return results, nil
}
