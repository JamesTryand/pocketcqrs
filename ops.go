package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/JamesTryand/pocketcqrs/events"
)

// registerOpsRoutes binds the superuser-only operational API:
//
//	GET  /api/cqrs/events?after=&before=&limit=&aggregate=&aggregateId=&type=
//	GET  /api/cqrs/streams?aggregate=
//	GET  /api/cqrs/deadletters?all=
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
	}).Bind(apis.RequireSuperuserAuth())

	// one row per stream, optionally restricted to one aggregate
	e.Router.GET("/api/cqrs/streams", func(re *core.RequestEvent) error {
		streams, err := c.store.ListStreamInfos(re.Request.Context(),
			re.Request.URL.Query().Get("aggregate"))
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"streams": streams})
	}).Bind(apis.RequireSuperuserAuth())

	// failed function deliveries; pending only unless ?all=1
	e.Router.GET("/api/cqrs/deadletters", func(re *core.RequestEvent) error {
		includeResolved := re.Request.URL.Query().Get("all") == "1"
		letters, err := c.store.DeadLetters(re.Request.Context(), includeResolved)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]any{"deadLetters": letters})
	}).Bind(apis.RequireSuperuserAuth())

	// the system mode barrier (running|maintenance); POST wraps SetMode,
	// so an invalid value is a 400 and the current mode never changes
	e.Router.GET("/api/cqrs/admin/mode", func(re *core.RequestEvent) error {
		mode, err := c.store.Mode(re.Request.Context())
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]string{"mode": mode})
	}).Bind(apis.RequireSuperuserAuth())

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
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, map[string]string{"mode": body.Mode})
	}).Bind(apis.RequireSuperuserAuth())
}
