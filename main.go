package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/reactors"
	"github.com/jamestryand/pocketcqrs/writeguard"

	_ "github.com/jamestryand/pocketcqrs/migrations"
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
	app.RootCmd.AddCommand(newProjectionCommand(c))
	app.RootCmd.AddCommand(newDeadletterCommand(c))
	app.RootCmd.AddCommand(newDryrunCommand(c))
	app.RootCmd.AddCommand(newSystemCommand(c))
	app.RootCmd.AddCommand(newCatalogCommand(c))
	app.RootCmd.AddCommand(newPackCommand(c, &functionsDir))
	app.RootCmd.ParseFlags(os.Args[1:])

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

		// write side: deciders + command handling
		c.registry = decider.NewRegistry(store)
		aggregates.RegisterAll(c.registry)
		c.jsDeciders = map[string]*functions.DeciderSpec{}

		// the gateway rejects domain commands while the system is in
		// maintenance mode (hot reload of schema-bearing functions)
		gatewayCfg.Mode = store.Mode

		logger := e.App.Logger()

		// durable consumption of the log (projections + functions)
		c.engine = consumers.NewEngine(store,
			func(msg string, args ...any) { logger.Warn(msg, args...) })

		// read side: projections into PocketBase collections
		projs := allProjections(e.App)
		for _, p := range projs {
			c.engine.Register(p)
		}

		// functions (FaaS): JS functions loaded from functionsDir
		rt := functions.NewGojaRuntime(
			func(msg string, args ...any) { logger.Info(msg, args...) })
		rt.SetReader(functions.NewAppReader(e.App))
		rt.SetStore(store)
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

		// sagas: reactors dispatch follow-up commands through the registry
		c.engine.Register(reactors.AsConsumer(reactors.Fulfillment(), c.registry,
			func(msg string, args ...any) { logger.Info(msg, args...) }))

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
		guarded := allProjections(e.App)
		for _, p := range c.jsProjs {
			guarded = append(guarded, p)
		}
		writeguard.Register(e.App, projections.GuardedCollections(guarded...)...)

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
