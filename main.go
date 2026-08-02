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
	"pocketcqrs/writeguard"

	_ "pocketcqrs/migrations"
)

// components is filled during bootstrap, before the server starts.
type components struct {
	store    *events.Store
	registry *decider.Registry
	engine   *consumers.Engine
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
	app.RootCmd.ParseFlags(os.Args[1:])

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		dataDir := e.App.DataDir()
		if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
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

		logger := e.App.Logger()

		// durable consumption of the log (projections + functions)
		c.engine = consumers.NewEngine(store,
			func(msg string, args ...any) { logger.Warn(msg, args...) })

		// read side: projections into PocketBase collections
		projs := []projections.Projection{projections.NewTasks(e.App)}
		for _, p := range projs {
			c.engine.Register(p)
		}

		// write-guard: no out-of-band writes on projection-owned collections
		writeguard.Register(e.App, projections.GuardedCollections(projs...)...)

		// functions (FaaS): effect functions on domain events, delivered
		// durably through the same consumers engine
		rt := functions.NewGojaRuntime(
			func(msg string, args ...any) { logger.Info(msg, args...) })
		if err := functions.RegisterBuiltins(rt); err != nil {
			return err
		}
		for _, fc := range rt.Consumers() {
			c.engine.Register(fc)
		}

		return e.Next()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		gateway.RegisterRoutes(e, c.registry, gatewayCfg)
		c.engine.Start(context.Background())
		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
