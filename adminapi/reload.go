package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// ReloadReport summarizes one hot reload (POST /api/cqrs/admin/reload).
type ReloadReport struct {
	Mode               string   `json:"mode"`
	EffectsReloaded    []string `json:"effectsReloaded"`
	HTTPReloaded       []string `json:"httpReloaded"`
	CronReloaded       []string `json:"cronReloaded"`
	ReactorsReloaded   []string `json:"reactorsReloaded,omitempty"`
	ReactorsRefused    []string `json:"reactorsRefused,omitempty"`
	SchemaTier         string   `json:"schemaTier"` // "reloaded" or "skipped: not in maintenance"
	Projections        []string `json:"projectionsReloaded,omitempty"`
	ProjectionsRemoved []string `json:"projectionsRemoved,omitempty"`
	DecidersReloaded   []string `json:"decidersReloaded,omitempty"`
	DecidersRemoved    []string `json:"decidersRemoved,omitempty"`
	DecidersRefused    []string `json:"decidersRefused,omitempty"`
}

// RegisterReloadRoute binds the superuser-only hot-reload endpoint:
//
//	POST /api/cqrs/admin/reload
//
// Pure effect functions (event/http/cron) reload in ANY mode — they are
// additive consumers with no schema footprint. Schema-bearing files (JS
// projection schemas, JS deciders) reload only in maintenance mode, behind
// the barrier that rejects domain commands; deciders are re-validated
// (ValidateDeciderSpec) before their swap.
func RegisterReloadRoute(e *core.ServeEvent, s *State, functionsDir string) {
	e.Router.POST("/api/cqrs/admin/reload", func(re *core.RequestEvent) error {
		report, err := s.reloadFunctions(re.Request.Context(), functionsDir)
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, report)
	}).Bind(authverify.RequireSuperuser(s.Verifier))
}

// reloadFunctions reloads the functions directory into the running system.
// Everything is compiled and validated before any live structure is
// touched: a broken file aborts the reload, leaving the previous code
// serving. Checkpoints carry over by consumer name, so swapped consumers
// resume where the old code left off.
// CarryCapabilities copies every capability the live runtime holds onto a
// freshly built one.
//
// A hot reload swaps the runtime wholesale, so ANY capability that is not
// copied here works until the first reload and then silently vanishes — the
// worst kind of bug, because nothing fails at the point of the mistake.
// $http did exactly this during development. It is a named method rather
// than inline setup so the property can be tested without standing up an
// app, and so a new capability has one obvious place to be added.
//
// The outbound client is guarded rather than passed straight through: a nil
// *outbound.Client stored in an interface is NOT a nil interface, so an
// unconditional call would install the binding on an instance that never
// enabled outbound, and it would panic on first use.
func (s *State) CarryCapabilities(fresh *functions.GojaRuntime) {
	fresh.SetReader(functions.NewAppReader(s.App))
	fresh.SetStore(s.Store)
	fresh.SetWarn(func(msg string, args ...any) { s.App.Logger().Warn(msg, args...) })
	if s.Outbound != nil {
		fresh.SetOutbound(s.Outbound)
	}
}

// ProspectiveCommands answers "which aggregates will accept which commands
// once this reload finishes", which is what the //@dispatches gate has to
// check against — not the live registry.
//
// The answer depends on the mode, because the two tiers reload on different
// schedules:
//
//   - running mode: deciders do NOT reload (the schema tier is skipped), so
//     the live registry already is the post-reload truth.
//   - maintenance mode: deciders swap later in this same pass, so the truth
//     is the live registry with this load's JS deciders overlaid — added or
//     changed ones present, removed ones gone.
//
// Getting this wrong is not a subtle bug: check against the live registry in
// maintenance mode and a decider shipped together with the reactor that
// dispatches to it is refused, which would make the gate worse than the fault
// it fixes.
func (s *State) ProspectiveCommands(mode string, loaded *functions.LoadResult) functions.CommandTarget {
	if mode != events.ModeMaintenance {
		return s.Registry
	}
	p := &ProspectiveSet{live: s.Registry, adds: map[string][]string{}, removes: map[string]bool{}}
	// every JS decider currently live is a candidate for removal; anything
	// this load still carries is put back below
	for aggregate := range s.JSDeciders {
		p.removes[aggregate] = true
	}
	for _, spec := range loaded.Deciders {
		delete(p.removes, spec.Aggregate)
		p.adds[spec.Aggregate] = spec.Commands
	}
	return p
}

// ProspectiveSet overlays a pending reload's decider changes onto the live
// registry. Built-in Go aggregates are never in adds/removes — they cannot
// change without a rebuild — so they fall through to the live registry.
type ProspectiveSet struct {
	live    functions.CommandTarget
	adds    map[string][]string
	removes map[string]bool
}

func (p *ProspectiveSet) Has(aggregate string) bool {
	if _, ok := p.adds[aggregate]; ok {
		return true
	}
	if p.removes[aggregate] {
		return false
	}
	return p.live.Has(aggregate)
}

func (p *ProspectiveSet) Commands(aggregate string) []string {
	if cmds, ok := p.adds[aggregate]; ok {
		// nil here means the incoming decider declares no //@commands, which
		// ValidateReactorSpec reads as "unverifiable" — the same answer the
		// registry gives for an undeclaring aggregate, deliberately
		return cmds
	}
	if p.removes[aggregate] {
		return nil
	}
	return p.live.Commands(aggregate)
}

func (s *State) reloadFunctions(ctx context.Context, functionsDir string) (*ReloadReport, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	mode, err := s.Store.Mode(ctx)
	if err != nil {
		return nil, err
	}
	report := &ReloadReport{Mode: mode}

	// load into a fresh runtime with the same dependencies; on any error
	// the old runtime and every live registration stay untouched
	fresh := functions.NewGojaRuntime(func(msg string, args ...any) { s.App.Logger().Info(msg, args...) })
	s.CarryCapabilities(fresh)
	loaded, err := functions.LoadDir(fresh, s.App, functionsDir)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------
	// effects tier: safe to swap in any mode
	// -----------------------------------------------------------------
	for _, consumer := range s.FnRuntime.Consumers() {
		s.Engine.Unregister(consumer.Name())
	}
	for _, consumer := range fresh.Consumers() {
		s.Engine.Register(consumer)
		report.EffectsReloaded = append(report.EffectsReloaded, consumer.Name())
	}

	// Reactors ride the effect tier: they declare no schema, so they swap in
	// ANY mode. That is a behavioural claim (a reactor-only change must not
	// need the maintenance barrier), and it is what makes the tier usable
	// for ordinary saga edits. They dispatch through the registry, so the
	// fresh runtime needs it before any delivery happens.
	fresh.SetRegistry(s.Registry)
	for _, spec := range s.JSReactors {
		s.Engine.Unregister(spec.Name())
	}
	// //@dispatches gate (F-2). Validated against a PROSPECTIVE command set,
	// not the live registry: deciders swap further down this function, so at
	// this point the registry still holds the old ones, and checking against
	// it would refuse a decider and a reactor added in the SAME maintenance
	// reload. Moving reactors after deciders is not the fix — it would break
	// the behavioural claim above, since in running mode deciders do not
	// reload at all and reactors must still be able to.
	prospective := s.ProspectiveCommands(mode, loaded)
	var keptReactors []*functions.ReactorSpec
	for _, spec := range loaded.Reactors {
		if err := functions.ValidateReactorSpec(prospective, spec); err != nil {
			// refusal keeps the old reactor serving, exactly as a refused
			// decider does — and it is REPORTED, which is the whole point:
			// the silent version of this is the fault being fixed
			// the error already names the reactor, so it is not prefixed
			// again the way a refused decider's short reason is
			report.ReactorsRefused = append(report.ReactorsRefused, err.Error())
			continue
		}
		keptReactors = append(keptReactors, spec)
	}
	s.JSReactors = keptReactors
	for _, spec := range keptReactors {
		s.Engine.Register(spec)
		report.ReactorsReloaded = append(report.ReactorsReloaded, spec.Reactor)
	}
	sort.Strings(report.ReactorsReloaded)
	sort.Strings(report.ReactorsRefused)

	s.HTTPFns.ReplaceFrom(loaded.HTTP)
	report.HTTPReloaded = loaded.HTTP.Names()
	sort.Strings(report.HTTPReloaded)
	sort.Strings(report.EffectsReloaded)

	for _, id := range s.CronJobs {
		s.App.Cron().Remove(id)
	}
	s.CronJobs = nil
	for _, job := range fresh.CronJobs() {
		id := "fn:" + job.Name
		if err := s.App.Cron().Add(id, job.Schedule, job.Fire); err != nil {
			return nil, fmt.Errorf("reload: cron function %s: %w", job.Name, err)
		}
		s.CronJobs = append(s.CronJobs, id)
		report.CronReloaded = append(report.CronReloaded, job.Name)
	}
	sort.Strings(report.CronReloaded)

	s.FnRuntime = fresh

	// -----------------------------------------------------------------
	// schema-bearing tier: behind the maintenance barrier
	// -----------------------------------------------------------------
	if mode != events.ModeMaintenance {
		report.SchemaTier = "skipped: not in maintenance"
		return report, nil
	}
	report.SchemaTier = "reloaded"

	if err := functions.ReconcileSchemas(s.App, loaded.Projections, s.SchemaDefaultRule); err != nil {
		return nil, fmt.Errorf("reload: schema reconcile: %w", err)
	}

	// Make the collection cache explicitly consistent after reconcile.
	//
	// PocketBase serves record routes from a cached collection set, and
	// reconcile's own app.Save() calls refresh it as a side effect — so this
	// is normally redundant. It is here because depending on a dependency's
	// internal side effect for a correctness property is a silent coupling:
	// were Save to stop refreshing, a collection created by a reload would
	// exist, project into correctly, and still answer "404 Missing
	// collection context" until a restart. Cheap, idempotent, once per
	// schema-tier reload, and it makes the requirement explicit.
	//
	// HONEST PROVENANCE: written to fix exactly that 404, seen once by hand.
	// It did NOT reproduce — four controlled runs, including one faithfully
	// replicating the original conditions, all served the new collection
	// immediately without this call. Defensive hardening, not a fix for a
	// characterised bug. Do not infer a defect that was never demonstrated.
	if err := s.App.ReloadCachedCollections(); err != nil {
		return nil, fmt.Errorf("reload: refresh collection cache: %w", err)
	}

	// JS deciders: re-validate before swap; refusals keep the old code
	// serving. Built-in (Go) aggregates can never be displaced.
	newManaged := map[string]*functions.DeciderSpec{}
	var validated []*functions.DeciderSpec
	for _, spec := range loaded.Deciders {
		if s.Registry.Has(spec.Aggregate) {
			if _, isJS := s.JSDeciders[spec.Aggregate]; !isJS {
				report.DecidersRefused = append(report.DecidersRefused,
					spec.Aggregate+" (collides with a built-in decider)")
				continue
			}
		}
		if err := functions.ValidateDeciderSpec(s.Store, spec); err != nil {
			report.DecidersRefused = append(report.DecidersRefused,
				fmt.Sprintf("%s (%v)", spec.Aggregate, err))
			// keep the previously registered code serving, if any
			if old, ok := s.JSDeciders[spec.Aggregate]; ok {
				newManaged[spec.Aggregate] = old
				validated = append(validated, old)
			}
			continue
		}
		s.Registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
		newManaged[spec.Aggregate] = spec
		validated = append(validated, spec)
		report.DecidersReloaded = append(report.DecidersReloaded, spec.Aggregate)
	}
	for agg := range s.JSDeciders {
		if _, ok := newManaged[agg]; !ok {
			s.Registry.Unregister(agg)
			report.DecidersRemoved = append(report.DecidersRemoved, agg)
		}
	}
	s.JSDeciders = newManaged
	s.Store.SetUpcaster(functions.BuildUpcaster(validated))

	// JS projections: swap consumers by checkpoint name
	for _, p := range s.JSProjs {
		s.Engine.Unregister(p.Name())
	}
	s.JSProjs = nil
	for _, spec := range loaded.Projections {
		p := spec.Consumer()
		s.Engine.Register(p)
		s.JSProjs = append(s.JSProjs, p)
		report.Projections = append(report.Projections, spec.Name)
	}

	// newly declared collections must be write-guarded too (binding again
	// for an already-guarded collection is harmless: deny is idempotent)
	writeguard.Register(s.App, projectionCollections(s.JSProjs)...)

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
