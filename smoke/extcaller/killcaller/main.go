//go:build smoke

// Command killcaller is the extcaller wiring of cmd/extcaller with one rule,
// built only by the outbox kill test (TestOutboxSurvivesRealProcessKills),
// which needs the caller as a real process it can kill mid-work.
//
// The rule: on TaskCreated for a task not itself an echo, call the third
// party with the task's id, then create the task "echo-<id>". Echo tasks
// are called for too (a rule matches by event type only) but create
// nothing, so the chain stops after one step.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/extcaller"
	"github.com/jamestryand/pocketcqrs/internal/localstore"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/outbox"
)

func main() {
	var (
		sourceEventsDB = flag.String("sourceEventsDB", "", "the backend's events.db")
		localStorePath = flag.String("localStore", "", "this caller's own store (checkpoint, dead letters)")
		outboxPath     = flag.String("outbox", "", "this caller's outbox file")
		gatewayURL     = flag.String("gatewayURL", "", "the backend's base URL")
		thirdURL       = flag.String("thirdURL", "", "the fake third party's base URL")
	)
	flag.Parse()
	token := os.Getenv("EXTCALLER_GATEWAY_TOKEN")
	if *sourceEventsDB == "" || *localStorePath == "" || *outboxPath == "" || *gatewayURL == "" || *thirdURL == "" || token == "" {
		log.Fatal("all flags and EXTCALLER_GATEWAY_TOKEN are required")
	}
	logger := func(msg string, args ...any) { log.Println(append([]any{msg}, args...)...) }

	stores, err := localstore.Open(*sourceEventsDB, *localStorePath)
	if err != nil {
		log.Fatal(err)
	}
	defer stores.Close()
	box, err := outbox.Open(*outboxPath)
	if err != nil {
		log.Fatal(err)
	}
	defer box.Close()

	u, err := url.Parse(*thirdURL)
	if err != nil {
		log.Fatal(err)
	}
	out, err := outbound.New(outbound.Config{
		AllowedHosts: []string{u.Hostname()},
		AllowPrivate: true,
		Timeout:      5 * time.Second,
		MaxInFlight:  8,
		MaxBodyBytes: 1 << 16,
	})
	if err != nil {
		log.Fatal(err)
	}

	rule := extcaller.Rule{
		EventType: "TaskCreated",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: *thirdURL + "/lookup?id=" + url.QueryEscape(ev.AggregateID)}, nil
		},
		HandleResponse: func(ev events.Event, _ *outbound.Response) ([]extcaller.FollowUp, error) {
			if strings.HasPrefix(ev.AggregateID, "echo-") {
				return nil, nil
			}
			payload, _ := json.Marshal(map[string]string{"title": "echo of " + ev.AggregateID})
			return []extcaller.FollowUp{{Aggregate: "task", ID: "echo-" + ev.AggregateID, Name: "CreateTask", Payload: payload}}, nil
		},
	}
	consumer, err := extcaller.New(extcaller.Config{
		Name:        "kill",
		Rules:       []extcaller.Rule{rule},
		Outbound:    out,
		Gateway:     extcaller.NewGatewayClient(*gatewayURL, token, 10*time.Second),
		DeadLetters: stores.Local,
		Outbox:      box,
		Retry:       extcaller.RetryPolicy{MaxAttempts: 5, Backoff: 100 * time.Millisecond},
		Logger:      logger,
	})
	if err != nil {
		log.Fatal(err)
	}
	engine := stores.Engine(logger)
	engine.Register(consumer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	engine.Start(ctx)
	go consumer.Worker().Run(ctx)
	<-ctx.Done()
}
