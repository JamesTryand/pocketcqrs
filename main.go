package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"pocketcqrs/aggregates"
	"pocketcqrs/consumers"
	"pocketcqrs/decider"
	"pocketcqrs/events"
	"pocketcqrs/functions"
	"pocketcqrs/gateway"
	"pocketcqrs/projections"
	"pocketcqrs/reactors"
	"pocketcqrs/writeguard"

	_ "pocketcqrs/migrations"
)

// components is filled during bootstrap, before the server starts.
type components struct {
	app      core.App
	store    *events.Store
	registry *decider.Registry
	engine   *consumers.Engine
	httpFns  *functions.HTTPRegistry
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

	var functionsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&functionsDir,
		"functionsDir",
		"pb_functions",
		"the directory with the user defined JS functions",
	)
	app.RootCmd.AddCommand(newProjectionCommand(c))
	app.RootCmd.ParseFlags(os.Args[1:])

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		dataDir := e.App.DataDir()
		if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
			return err
		}
		c.app = e.App

		// event store (source of truth) next to PocketBase's data.db
		store, err := events.Open(filepath.Join(dataDir, "events.db"))
		if err != nil {
			return err
		}
		c.store = store

		// write side: deciders + command handling
		c.registry = decider.NewRegistry(store)
		aggregates.RegisterAll(c.registry)

		logger := e.App.Logger()

		// durable consumption of the log (projections + functions)
		c.engine = consumers.NewEngine(store,
			func(msg string, args ...any) { logger.Warn(msg, args...) })

		// read side: projections into PocketBase collections
		projs := allProjections(e.App)
		for _, p := range projs {
			c.engine.Register(p)
		}

		// sagas: reactors dispatch follow-up commands through the registry
		c.engine.Register(reactors.AsConsumer(reactors.Fulfillment(), c.registry,
			func(msg string, args ...any) { logger.Info(msg, args...) }))

		// write-guard: no out-of-band writes on projection-owned collections
		writeguard.Register(e.App, projections.GuardedCollections(projs...)...)

		// functions (FaaS): JS functions loaded from functionsDir; event
		// functions are delivered durably through the consumers engine
		rt := functions.NewGojaRuntime(
			func(msg string, args ...any) { logger.Info(msg, args...) })
		rt.SetReader(functions.NewAppReader(e.App))
		httpFns, err := functions.LoadDir(rt, functionsDir)
		if err != nil {
			return err
		}
		c.httpFns = httpFns
		for _, fc := range rt.Consumers() {
			c.engine.Register(fc)
		}

		return e.Next()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		gateway.RegisterRoutes(e, c.registry, gatewayCfg)
		functions.RegisterHTTPRoutes(e, c.httpFns, !gatewayCfg.AllowAnonymous)
		c.engine.Start(context.Background())
		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
