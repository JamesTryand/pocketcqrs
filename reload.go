package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/JamesTryand/pocketcqrs/events"
	"github.com/JamesTryand/pocketcqrs/functions"
	"github.com/JamesTryand/pocketcqrs/writeguard"
)

// reloadReport summarizes one hot reload (POST /api/cqrs/admin/reload).
type reloadReport struct {
	Mode               string   `json:"mode"`
	EffectsReloaded    []string `json:"effectsReloaded"`
	HTTPReloaded       []string `json:"httpReloaded"`
	CronReloaded       []string `json:"cronReloaded"`
	SchemaTier         string   `json:"schemaTier"` // "reloaded" or "skipped: not in maintenance"
	Projections        []string `json:"projectionsReloaded,omitempty"`
	ProjectionsRemoved []string `json:"projectionsRemoved,omitempty"`
	DecidersReloaded   []string `json:"decidersReloaded,omitempty"`
	DecidersRemoved    []string `json:"decidersRemoved,omitempty"`
	DecidersRefused    []string `json:"decidersRefused,omitempty"`
}

// registerReloadRoute binds the superuser-only hot-reload endpoint:
//
//	POST /api/cqrs/admin/reload
//
// Pure effect functions (event/http/cron) reload in ANY mode — they are
// additive consumers with no schema footprint. Schema-bearing files (JS
// projection schemas, JS deciders) reload only in maintenance mode, behind
// the barrier that rejects domain commands; deciders are re-validated
// (ValidateDeciderSpec) before their swap.
func registerReloadRoute(e *core.ServeEvent, c *components, functionsDir string) {
	e.Router.POST("/api/cqrs/admin/reload", func(re *core.RequestEvent) error {
		report, err := c.reloadFunctions(re.Request.Context(), functionsDir)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, report)
	}).Bind(apis.RequireSuperuserAuth())
}

// reloadFunctions reloads the functions directory into the running system.
// Everything is compiled and validated before any live structure is
// touched: a broken file aborts the reload, leaving the previous code
// serving. Checkpoints carry over by consumer name, so swapped consumers
// resume where the old code left off.
func (c *components) reloadFunctions(ctx context.Context, functionsDir string) (*reloadReport, error) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	mode, err := c.store.Mode(ctx)
	if err != nil {
		return nil, err
	}
	report := &reloadReport{Mode: mode}

	// load into a fresh runtime with the same dependencies; on any error
	// the old runtime and every live registration stay untouched
	fresh := functions.NewGojaRuntime(func(msg string, args ...any) { c.app.Logger().Info(msg, args...) })
	fresh.SetReader(functions.NewAppReader(c.app))
	fresh.SetStore(c.store)
	loaded, err := functions.LoadDir(fresh, c.app, functionsDir)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------
	// effects tier: safe to swap in any mode
	// -----------------------------------------------------------------
	for _, consumer := range c.fnRuntime.Consumers() {
		c.engine.Unregister(consumer.Name())
	}
	for _, consumer := range fresh.Consumers() {
		c.engine.Register(consumer)
		report.EffectsReloaded = append(report.EffectsReloaded, consumer.Name())
	}

	c.httpFns.ReplaceFrom(loaded.HTTP)
	report.HTTPReloaded = loaded.HTTP.Names()
	sort.Strings(report.HTTPReloaded)
	sort.Strings(report.EffectsReloaded)

	for _, id := range c.cronJobs {
		c.app.Cron().Remove(id)
	}
	c.cronJobs = nil
	for _, job := range fresh.CronJobs() {
		id := "fn:" + job.Name
		if err := c.app.Cron().Add(id, job.Schedule, job.Fire); err != nil {
			return nil, fmt.Errorf("reload: cron function %s: %w", job.Name, err)
		}
		c.cronJobs = append(c.cronJobs, id)
		report.CronReloaded = append(report.CronReloaded, job.Name)
	}
	sort.Strings(report.CronReloaded)

	c.fnRuntime = fresh

	// -----------------------------------------------------------------
	// schema-bearing tier: behind the maintenance barrier
	// -----------------------------------------------------------------
	if mode != events.ModeMaintenance {
		report.SchemaTier = "skipped: not in maintenance"
		return report, nil
	}
	report.SchemaTier = "reloaded"

	if err := functions.ReconcileSchemas(c.app, loaded.Projections); err != nil {
		return nil, fmt.Errorf("reload: schema reconcile: %w", err)
	}

	// JS deciders: re-validate before swap; refusals keep the old code
	// serving. Built-in (Go) aggregates can never be displaced.
	newManaged := map[string]*functions.DeciderSpec{}
	var validated []*functions.DeciderSpec
	for _, spec := range loaded.Deciders {
		if c.registry.Has(spec.Aggregate) {
			if _, isJS := c.jsDeciders[spec.Aggregate]; !isJS {
				report.DecidersRefused = append(report.DecidersRefused,
					spec.Aggregate+" (collides with a built-in decider)")
				continue
			}
		}
		if err := functions.ValidateDeciderSpec(c.store, spec); err != nil {
			report.DecidersRefused = append(report.DecidersRefused,
				fmt.Sprintf("%s (%v)", spec.Aggregate, err))
			// keep the previously registered code serving, if any
			if old, ok := c.jsDeciders[spec.Aggregate]; ok {
				newManaged[spec.Aggregate] = old
				validated = append(validated, old)
			}
			continue
		}
		c.registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
		newManaged[spec.Aggregate] = spec
		validated = append(validated, spec)
		report.DecidersReloaded = append(report.DecidersReloaded, spec.Aggregate)
	}
	for agg := range c.jsDeciders {
		if _, ok := newManaged[agg]; !ok {
			c.registry.Unregister(agg)
			report.DecidersRemoved = append(report.DecidersRemoved, agg)
		}
	}
	c.jsDeciders = newManaged
	c.store.SetUpcaster(functions.BuildUpcaster(validated))

	// JS projections: swap consumers by checkpoint name
	for _, p := range c.jsProjs {
		c.engine.Unregister(p.Name())
	}
	c.jsProjs = nil
	for _, spec := range loaded.Projections {
		p := spec.Consumer()
		c.engine.Register(p)
		c.jsProjs = append(c.jsProjs, p)
		report.Projections = append(report.Projections, spec.Name)
	}

	// newly declared collections must be write-guarded too (binding again
	// for an already-guarded collection is harmless: deny is idempotent)
	writeguard.Register(c.app, projectionCollections(c.jsProjs)...)

	sort.Strings(report.Projections)
	sort.Strings(report.DecidersReloaded)
	sort.Strings(report.DecidersRemoved)
	sort.Strings(report.DecidersRefused)
	return report, nil
}

// projectionCollections flattens the owned collections of JS projections.
func projectionCollections(projs []*functions.JSProjection) []string {
	var out []string
	for _, p := range projs {
		out = append(out, p.Collections()...)
	}
	return out
}
