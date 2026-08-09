package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/migrations"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/reactors"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// components is filled during bootstrap, before the server starts.
type components struct {
	app       core.App
	store     *events.Store
	registry  *decider.Registry
	engine    *consumers.Engine
	httpFns   *functions.HTTPRegistry
	jsProjs   []*functions.JSProjection
	fnRuntime *functions.GojaRuntime

	// outbound backs the $http binding, or is nil when
	// --cqrsAllowOutboundHTTP is absent (the default). Held here because a
	// hot reload builds a fresh runtime and must carry it across — otherwise
	// $http would work until the first reload and then vanish.
	outbound *outbound.Client

	// jsDeciders tracks JS-managed aggregates (vs built-in Go deciders)
	// with their active specs, for hot-reload swaps and upcaster rebuilds.
	jsDeciders map[string]*functions.DeciderSpec
	// jsReactors tracks the active JS reactors so a reload can unregister
	// exactly what it registered (their checkpoint keys are stable, so a
	// swapped reactor resumes where the old code left off).
	jsReactors []*functions.ReactorSpec
	// cronJobs lists the registered cron job ids ("fn:"+name).
	cronJobs []string
	// reloadMu serializes hot reloads.
	reloadMu sync.Mutex

	// tutorial mirrors --tutorial. Set once immediately after ParseFlags,
	// before anything reads it, so OnBootstrap and every subcommand's RunE
	// see the same answer. When false this repo's example domains are not
	// wired at all — the platform ships empty.
	tutorial bool
}

func main() {
	app := pocketbase.New()
	c := &components{}

	var gatewayCfg gateway.Config
	app.RootCmd.PersistentFlags().BoolVar(
		&gatewayCfg.AllowAnonymous,
		"cqrsAllowAnonymous",
		false,
		"allow anonymous CQRS command execution (dev only; no actor metadata is stamped)",
	)

	// Outbound HTTP for the effect/reactor tiers. Off by default: with these
	// unset, core's posture is exactly what it was before the feature existed.
	var allowOutboundHTTP bool
	app.RootCmd.PersistentFlags().BoolVar(
		&allowOutboundHTTP,
		"cqrsAllowOutboundHTTP",
		false,
		"allow event, cron and reactor functions to call out over HTTP via $http; not //@trigger http functions, and never deciders or projections",
	)
	var outboundHosts []string
	app.RootCmd.PersistentFlags().StringArrayVar(
		&outboundHosts,
		"cqrsOutboundHost",
		nil,
		"a hostname $http may call, repeatable; the list is deployment-wide, not per-function (no entries = nothing permitted)",
	)
	var allowPrivateOutbound bool
	app.RootCmd.PersistentFlags().BoolVar(
		&allowPrivateOutbound,
		"cqrsAllowPrivateOutbound",
		false,
		"let $http reach loopback and private ranges (dev and internal services; link-local stays blocked)",
	)

	var strictBoot bool
	app.RootCmd.PersistentFlags().BoolVar(
		&strictBoot,
		"cqrsStrictBoot",
		false,
		"abort startup if a JS decider fails validation (default: skip the decider and keep serving)",
	)

	var functionsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&functionsDir,
		"functionsDir",
		"pb_functions",
		"the directory with the user defined JS functions",
	)
	var tutorial bool
	app.RootCmd.PersistentFlags().BoolVar(
		&tutorial,
		"tutorial",
		false,
		"register this repo's example domains (task, order) and their collections; off by default — pocketcqrs ships empty",
	)

	app.RootCmd.AddCommand(newProjectionCommand(c))
	app.RootCmd.AddCommand(newDeadletterCommand(c))
	app.RootCmd.AddCommand(newDryrunCommand(c))
	app.RootCmd.AddCommand(newSystemCommand(c))
	app.RootCmd.AddCommand(newCatalogCommand(c))
	app.RootCmd.AddCommand(newPackCommand(c, &functionsDir))
	app.RootCmd.AddCommand(newSchemaCommand(c, &functionsDir))
	app.RootCmd.ParseFlags(os.Args[1:])
	c.tutorial = tutorial

	// Registering the example migrations is a decision, taken here: an
	// unregistered migration is never applied AND never recorded, so the
	// examples can be switched off and on again without either direction
	// being a one-way door. See migrations.RegisterExamples.
	if c.tutorial {
		migrations.RegisterExamples()
	}

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		// run the default bootstrap first: it creates the data dir and,
		// crucially, opens the PocketBase DBs — ReconcileSchemas below
		// needs a live DB
		if err := e.Next(); err != nil {
			return err
		}

		dataDir := e.App.DataDir()
		if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
			return err
		}
		c.app = e.App

		// collections-as-DDL: apply the shipped app migrations now. Serve
		// would run them later (apis.Serve), which is too late for
		// ReconcileSchemas — relation targets may be migration-created
		// collections. Idempotent; system migrations already ran in e.Next().
		if err := e.App.RunAppMigrations(); err != nil {
			return err
		}

		// event store (source of truth) next to PocketBase's data.db
		store, err := events.Open(filepath.Join(dataDir, "events.db"))
		if err != nil {
			return err
		}
		c.store = store

		// write side: deciders + command handling. The platform registers no
		// aggregates of its own — task and order are example content, and
		// without --tutorial their names are free for a JS decider to claim.
		c.registry = decider.NewRegistry(store)
		if c.tutorial {
			aggregates.RegisterAll(c.registry)
		}
		c.jsDeciders = map[string]*functions.DeciderSpec{}

		// the gateway rejects domain commands while the system is in
		// maintenance mode (hot reload of schema-bearing functions)
		gatewayCfg.Mode = store.Mode

		logger := e.App.Logger()

		// durable consumption of the log (projections + functions)
		c.engine = consumers.NewEngine(store,
			func(msg string, args ...any) { logger.Warn(msg, args...) })

		// read side: projections into PocketBase collections
		projs := c.allProjections(e.App)
		for _, p := range projs {
			c.engine.Register(p)
		}

		// functions (FaaS): JS functions loaded from functionsDir
		rt := functions.NewGojaRuntime(
			func(msg string, args ...any) { logger.Info(msg, args...) })
		rt.SetReader(functions.NewAppReader(e.App))
		rt.SetStore(store)

		// outbound HTTP, only if asked for. An empty allow-list with the
		// flag on permits NOTHING and says so — "no entries" is not "no
		// restriction", which is the reading that made an empty writeguard
		// list guard everything in v0.4.0.
		if allowOutboundHTTP {
			client, err := outbound.New(outbound.Config{
				AllowedHosts: outboundHosts,
				AllowPrivate: allowPrivateOutbound,
				Timeout:      functions.OutboundTimeout,
				MaxInFlight:  functions.OutboundMaxInFlight,
				MaxBodyBytes: functions.OutboundMaxBodyBytes,
			})
			if err != nil {
				return fmt.Errorf("outbound HTTP config: %w", err)
			}
			c.outbound = client
			rt.SetOutbound(client)
			if len(outboundHosts) == 0 {
				logger.Warn("outbound HTTP is enabled but no --cqrsOutboundHost was given, " +
					"so every $http call will be refused")
			} else {
				logger.Info("outbound HTTP enabled for effect and reactor functions",
					"hosts", strings.Join(outboundHosts, ","),
					"allowPrivate", allowPrivateOutbound)
			}
		}

		c.fnRuntime = rt
		loaded, err := functions.LoadDir(rt, e.App, functionsDir)
		if err != nil {
			return err
		}
		c.httpFns = loaded.HTTP

		// JS deciders (tier 3): dry-run validated against existing history
		// at boot, then registered alongside the Go deciders. Failures are
		// refused loudly — the rest of the system keeps serving, unless
		// --cqrsStrictBoot is set (boot aborts instead).
		var validatedDeciders []*functions.DeciderSpec
		for _, spec := range loaded.Deciders {
			if c.registry.Has(spec.Aggregate) {
				if strictBoot {
					return fmt.Errorf("strict boot: JS decider aggregate %q collides with an existing decider", spec.Aggregate)
				}
				logger.Error("JS decider aggregate collides with an existing decider, skipped",
					"aggregate", spec.Aggregate)
				continue
			}
			if err := functions.ValidateDeciderSpec(store, spec); err != nil {
				if strictBoot {
					return fmt.Errorf("strict boot: JS decider %q failed validation: %w", spec.Aggregate, err)
				}
				logger.Error("JS decider failed validation, NOT registered",
					"aggregate", spec.Aggregate, "error", err)
				continue
			}
			c.registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
			c.jsDeciders[spec.Aggregate] = spec
			validatedDeciders = append(validatedDeciders, spec)
			logger.Info("JS decider active", "aggregate", spec.Aggregate)
		}

		// store-level upcasting: the validated deciders' transform chains
		// compose into the store's read path, so every consumer (deciders,
		// projections, functions, reactors) sees events at their latest
		// schema version. Only validated specs contribute.
		c.store.SetUpcaster(functions.BuildUpcaster(validatedDeciders))

		// JS projection schemas are materialized at boot (a restart IS the
		// maintenance window), additively: create/extend, never drop
		if err := functions.ReconcileSchemas(e.App, loaded.Projections); err != nil {
			return err
		}

		// engine registration order matters: Go projections first, then JS
		// projections (which may read Go-maintained collections), then
		// reactors and effect functions
		for _, spec := range loaded.Projections {
			c.jsProjs = append(c.jsProjs, spec.Consumer())
		}
		for _, p := range c.jsProjs {
			c.engine.Register(p)
		}

		// sagas: reactors dispatch follow-up commands through the registry.
		// The fulfillment saga is example content — it wires the example
		// order aggregate to the example task one, so it only exists when
		// they do.
		if c.tutorial {
			c.engine.Register(reactors.AsConsumer(reactors.Fulfillment(), c.registry,
				func(msg string, args ...any) { logger.Info(msg, args...) }))
		}

		// JS reactors (tier 4): same dispatch rule as the Go ones, reached
		// from a function file. The registry must be installed BEFORE they
		// are registered as consumers — a reactor without one fails loudly
		// rather than quietly doing nothing.
		rt.SetRegistry(c.registry)
		c.jsReactors = loaded.Reactors
		for _, spec := range loaded.Reactors {
			c.engine.Register(spec)
			logger.Info("JS reactor active", "reactor", spec.Reactor, "on", spec.EventTypes)
		}

		// write-guard: no out-of-band writes on projection-owned collections
		guarded := c.allProjections(e.App)
		for _, p := range c.jsProjs {
			guarded = append(guarded, p)
		}
		cols := projections.GuardedCollections(guarded...)

		// An example collection can outlive its projection: boot once with
		// --tutorial and then without, and `tasks` is still there with
		// nothing maintaining it. Keep guarding it — an unmaintained read
		// model is bad, an unmaintained read model anyone can write to is
		// worse — but say so, because a collection that quietly stopped
		// updating is the kind of thing found months later.
		if !c.tutorial {
			var orphaned []string
			for _, name := range exampleCollections(e.App) {
				if slices.Contains(cols, name) {
					continue // something else owns this name now, and maintains it
				}
				if _, err := e.App.FindCollectionByNameOrId(name); err != nil {
					continue // never created, or since dropped
				}
				orphaned = append(orphaned, name)
			}
			if len(orphaned) > 0 {
				cols = append(cols, orphaned...)
				logger.Warn("example collections exist but nothing is projecting into them: "+
					"they stay write-guarded and read-only, and will not be updated. "+
					"Start with --tutorial to maintain them again, or delete them.",
					"collections", strings.Join(orphaned, ", "))
			}
		}

		writeguard.Register(e.App, cols...)

		// effect functions: durable delivery through the consumers engine
		for _, fc := range rt.Consumers() {
			c.engine.Register(fc)
		}

		// cron functions: scheduled by PocketBase's cron service
		for _, job := range rt.CronJobs() {
			id := "fn:" + job.Name
			if err := e.App.Cron().Add(id, job.Schedule, job.Fire); err != nil {
				return err
			}
			c.cronJobs = append(c.cronJobs, id)
		}

		return nil
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		gateway.RegisterRoutes(e, c.registry, gatewayCfg)
		functions.RegisterHTTPRoutes(e, c.httpFns, !gatewayCfg.AllowAnonymous)
		registerReloadRoute(e, c, functionsDir)
		registerFunctionAdminRoutes(e, c, functionsDir)
		registerCatalogRoute(e, c)
		registerOpsRoutes(e, c)
		c.engine.Start(context.Background())
		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
