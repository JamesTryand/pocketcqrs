// Command extcaller is the reference wiring for running an
// external-service-caller against a pocketcqrs deployment.
//
// It has no built-in rules: BuildRequest/HandleResponse are Go closures (see
// extcaller.Rule's doc comment for why), so a real deployment adds its own
// in the rules function below, or copies this file as a starting point for
// its own binary. What this file demonstrates is the wiring every
// deployment needs regardless of which third party it's calling: opening
// the two event stores, building a bounded outbound.Client from the same
// global allow-list flags pocketcqrs core itself uses, building the gateway
// client, and running the consumer to completion on shutdown.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jamestryand/pocketcqrs/outbound"

	"github.com/jamestryand/pocketcqrs-extensions/extcaller"
	"github.com/jamestryand/pocketcqrs-extensions/internal/localstore"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		name            = flag.String("name", "default", "this consumer instance's name (checkpoint key: extcall:<name>)")
		sourceEventsDB  = flag.String("sourceEventsDB", "", "path to the upstream pocketcqrs deployment's events.db (required)")
		localStorePath  = flag.String("localStore", "", "path to this component's own local store, for checkpoints and dead letters (required)")
		gatewayURL      = flag.String("gatewayURL", "", "the upstream deployment's gateway base URL, e.g. http://localhost:8090 (required)")
		gatewayTimeout  = flag.Duration("gatewayTimeout", 10*time.Second, "timeout for one gateway dispatch call")
		allowedHosts    = flag.String("allowedHosts", "", "comma-separated global allow-list of third-party hosts (required; empty allows nothing, same as pocketcqrs core's own outbound.Config)")
		allowPrivate    = flag.Bool("allowPrivateOutbound", false, "permit loopback/private-range outbound destinations (dev only)")
		outboundTimeout = flag.Duration("outboundTimeout", 3*time.Second, "timeout for one third-party call")
		maxInFlight     = flag.Int("outboundMaxInFlight", 16, "process-wide cap on concurrent third-party calls")
		maxBodyBytes    = flag.Int64("outboundMaxBodyBytes", 1<<20, "cap on a third-party response body, in bytes")
		retryAttempts   = flag.Int("retryAttempts", 3, "max attempts per event before dead-lettering")
		retryBackoff    = flag.Duration("retryBackoff", 2*time.Second, "fixed pause between attempts")
	)
	flag.Parse()

	if *sourceEventsDB == "" || *localStorePath == "" || *gatewayURL == "" {
		flag.Usage()
		os.Exit(2)
	}

	// The gateway token is read from the environment, deliberately never a
	// flag: flags are visible in process listings and shell history, and
	// this is a credential. Minting it (a superuser token or a dedicated
	// service-account record) is an operator decision this repo doesn't
	// build tooling for.
	token := os.Getenv("EXTCALLER_GATEWAY_TOKEN")
	if token == "" {
		log.Fatal("EXTCALLER_GATEWAY_TOKEN must be set")
	}

	logger := func(msg string, args ...any) { log.Println(append([]any{msg}, args...)...) }

	stores, err := localstore.Open(*sourceEventsDB, *localStorePath)
	if err != nil {
		return err
	}
	defer stores.Close()

	var hosts []string
	for _, h := range strings.Split(*allowedHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	outClient, err := outbound.New(outbound.Config{
		AllowedHosts: hosts,
		AllowPrivate: *allowPrivate,
		Timeout:      *outboundTimeout,
		MaxInFlight:  *maxInFlight,
		MaxBodyBytes: *maxBodyBytes,
	})
	if err != nil {
		return err
	}

	gwClient := extcaller.NewGatewayClient(*gatewayURL, token, *gatewayTimeout)

	consumer, err := extcaller.New(extcaller.Config{
		Name:        *name,
		Rules:       rules(),
		Outbound:    outClient,
		Gateway:     gwClient,
		DeadLetters: stores.Local,
		Retry:       extcaller.RetryPolicy{MaxAttempts: *retryAttempts, Backoff: *retryBackoff},
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	engine := stores.Engine(logger)
	engine.Register(consumer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger("extcaller starting", "consumer", consumer.Name())
	engine.Start(ctx)
	<-ctx.Done()
	logger("extcaller shutting down")
	return nil
}

// rules is the extension point: add this deployment's []extcaller.Rule
// here. Empty by default — see the package doc comment.
func rules() []extcaller.Rule {
	return nil
}
