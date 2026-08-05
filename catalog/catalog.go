// Package catalog introspects the running platform — aggregates, event
// types, consumers, collections, functions and reactor flows — into a
// single document (the catalog), rendered as JSON, Markdown + Mermaid, or
// domain-doc skeletons.
//
// Two information sources, deliberately distinguished: DECLARED metadata
// (registered deciders, function specs, schemas) and EMPIRICAL metadata
// (what the event log actually contains: event types, version ranges,
// stream counts, reactor flows derived from causation metadata).
package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/projections"
)

// Catalog is the full introspection document.
type Catalog struct {
	GeneratedAt string       `json:"generatedAt"`
	Mode        string       `json:"mode"`
	Totals      Totals       `json:"totals"`
	Aggregates  []Aggregate  `json:"aggregates"`
	Consumers   []Consumer   `json:"consumers"`
	Collections []Collection `json:"collections"`
	Functions   Functions    `json:"functions"`
	Flows       []Flow       `json:"flows"`
	// Diagram is the platform flowchart as Mermaid source (the same
	// rendering the CLI embeds in Markdown). API consumers — e.g. the
	// ops dashboard — can show the diagram without reimplementing the
	// renderer.
	Diagram string `json:"mermaid"`
}

// Totals are log-wide counters.
type Totals struct {
	Events int64 `json:"events"`
	// MaxPosition is the head of the log. It is >= Events: positions are
	// AUTOINCREMENT and rolled back appends burn values, so this — not
	// Events — is what a consumer's checkpoint should be compared against.
	MaxPosition        int64 `json:"maxPosition"`
	Streams            int64 `json:"streams"`
	DeadLettersPending int64 `json:"deadLettersPending"`
}

// Aggregate is one registered aggregate with declared and empirical facts.
type Aggregate struct {
	Name   string `json:"name"`
	Origin string `json:"origin"` // "go" | "js"
	// Commands is what the aggregate DECLARES it accepts, if anything.
	// Unlike events it cannot be observed: commands leave no trace in the
	// log, so an aggregate that declares none reports none — which means
	// "not stated", never "accepts nothing".
	Commands   []string    `json:"commands,omitempty"`
	Handles    []string    `json:"handles,omitempty"`
	Transforms []string    `json:"transforms,omitempty"` // "Type v1->v2"
	Streams    int64       `json:"streams"`
	Events     []EventType `json:"events"`
}

// EventType is one empirical event type of an aggregate.
type EventType struct {
	Type       string `json:"type"`
	Count      int64  `json:"count"`
	MinVersion int64  `json:"minVersion"`
	MaxVersion int64  `json:"maxVersion"`
}

// Consumer is one engine consumer with its kind, triggers and checkpoint.
type Consumer struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // "go-projection" | "js-projection" | "reactor" | "effect-function"
	EventTypes  []string `json:"eventTypes,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Checkpoint  int64    `json:"checkpoint"`
}

// Collection is one PocketBase collection with its ownership facts.
type Collection struct {
	Name    string   `json:"name"`
	Guarded bool     `json:"guarded"`
	Owner   string   `json:"owner"` // projection name, or "" when not projection-owned
	Key     string   `json:"key,omitempty"`
	Fields  []string `json:"fields"` // "name:type"
}

// Functions lists the non-consumer function surface.
type Functions struct {
	HTTP []string  `json:"http"`
	Cron []CronJob `json:"cron"`
}

// CronJob is one registered cron function.
type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
}

// Flow is one empirical reactor mapping edge.
type Flow struct {
	Reactor string `json:"reactor"`
	Cause   string `json:"cause"`  // "aggregate/Type"
	Target  string `json:"target"` // "aggregate/Type"
	Count   int64  `json:"count"`
}

// Inputs bundles the live system references Build reads.
type Inputs struct {
	App           core.App
	Store         *events.Store
	Registry      *decider.Registry
	Engine        *consumers.Engine
	Runtime       *functions.GojaRuntime
	HTTP          *functions.HTTPRegistry
	GoProjections []projections.Projection
	JSProjs       []*functions.JSProjection
	JSDeciders    map[string]*functions.DeciderSpec
}

// Build collects the catalog from the live components and the event log.
func Build(ctx context.Context, in Inputs) (*Catalog, error) {
	c := &Catalog{GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05.000Z")}

	mode, err := in.Store.Mode(ctx)
	if err != nil {
		return nil, err
	}
	c.Mode = mode

	// ---- empirical: the log ----
	typeStats, err := in.Store.EventTypeStats(ctx)
	if err != nil {
		return nil, err
	}
	streamCounts, err := in.Store.StreamCounts(ctx)
	if err != nil {
		return nil, err
	}
	checkpoints, err := in.Store.Checkpoints(ctx)
	if err != nil {
		return nil, err
	}
	flows, err := in.Store.ReactorFlows(ctx)
	if err != nil {
		return nil, err
	}
	c.Totals.Events, c.Totals.Streams, err = in.Store.LogTotals(ctx)
	if err != nil {
		return nil, err
	}
	c.Totals.MaxPosition, err = in.Store.MaxPosition(ctx)
	if err != nil {
		return nil, err
	}
	letters, err := in.Store.DeadLetters(ctx, false)
	if err != nil {
		return nil, err
	}
	c.Totals.DeadLettersPending = int64(len(letters))

	// ---- aggregates (declared registrations + empirical events) ----
	eventsByAggregate := map[string][]EventType{}
	for _, st := range typeStats {
		eventsByAggregate[st.Aggregate] = append(eventsByAggregate[st.Aggregate], EventType{
			Type: st.Type, Count: st.Count, MinVersion: st.MinVersion, MaxVersion: st.MaxVersion,
		})
	}
	for _, name := range in.Registry.Aggregates() {
		agg := Aggregate{
			Name:     name,
			Origin:   "go",
			Commands: in.Registry.Commands(name),
			Streams:  streamCounts[name],
			Events:   eventsByAggregate[name],
		}
		if spec, ok := in.JSDeciders[name]; ok {
			agg.Origin = "js"
			agg.Handles = spec.Handles
			for _, t := range spec.Transforms {
				agg.Transforms = append(agg.Transforms, fmt.Sprintf("%s v%d->v%d", t.Type, t.From, t.To))
			}
		}
		c.Aggregates = append(c.Aggregates, agg)
	}

	// ---- consumers ----
	effectTypes := map[string][]string{}
	for _, fn := range in.Runtime.EventFunctions() {
		effectTypes["fn:"+fn.Name] = fn.EventTypes
	}
	projKinds := map[string]string{}
	projCollections := map[string][]string{}
	projEventTypes := map[string][]string{}
	for _, p := range in.GoProjections {
		projKinds[p.Name()] = "go-projection"
		projCollections[p.Name()] = p.Collections()
		// Go projections optionally declare their triggers for the catalog
		if tp, ok := p.(interface{ EventTypes() []string }); ok {
			projEventTypes[p.Name()] = tp.EventTypes()
		}
	}
	for _, p := range in.JSProjs {
		spec := p.Spec()
		projKinds[spec.Name] = "js-projection"
		projCollections[spec.Name] = p.Collections()
		projEventTypes[spec.Name] = spec.EventTypes
	}
	for _, name := range in.Engine.Names() {
		cons := Consumer{Name: name, Checkpoint: checkpoints[name]}
		switch {
		case projKinds[name] != "":
			cons.Kind = projKinds[name]
			cons.Collections = projCollections[name]
			cons.EventTypes = projEventTypes[name]
		case strings.HasPrefix(name, "reactor:"):
			cons.Kind = "reactor"
		case strings.HasPrefix(name, "fn:"):
			cons.Kind = "effect-function"
			cons.EventTypes = effectTypes[name]
		default:
			cons.Kind = "unknown"
		}
		c.Consumers = append(c.Consumers, cons)
	}

	// ---- collections ----
	guarded := map[string]bool{}
	owner := map[string]string{}
	key := map[string]string{}
	for _, p := range in.GoProjections {
		for _, col := range p.Collections() {
			guarded[col] = true
			owner[col] = p.Name()
		}
	}
	for _, p := range in.JSProjs {
		spec := p.Spec()
		for _, s := range spec.Schemas {
			guarded[s.Collection] = true
			owner[s.Collection] = spec.Name
			key[s.Collection] = s.Key
		}
	}
	cols, err := in.App.FindAllCollections()
	if err != nil {
		return nil, err
	}
	for _, col := range cols {
		if col.IsAuth() || strings.HasPrefix(col.Name, "_") {
			continue // auth and system collections are PocketBase's own surface
		}
		info := Collection{
			Name:    col.Name,
			Guarded: guarded[col.Name],
			Owner:   owner[col.Name],
			Key:     key[col.Name],
		}
		for _, f := range col.Fields {
			if f.GetSystem() {
				continue // id/created/updated are on every collection
			}
			info.Fields = append(info.Fields, f.GetName()+":"+f.Type())
		}
		c.Collections = append(c.Collections, info)
	}
	sort.Slice(c.Collections, func(i, j int) bool { return c.Collections[i].Name < c.Collections[j].Name })

	// ---- functions ----
	c.Functions.HTTP = in.HTTP.Names()
	sort.Strings(c.Functions.HTTP)
	for _, job := range in.Runtime.CronJobs() {
		c.Functions.Cron = append(c.Functions.Cron, CronJob{Name: job.Name, Schedule: job.Schedule})
	}
	sort.Slice(c.Functions.Cron, func(i, j int) bool { return c.Functions.Cron[i].Name < c.Functions.Cron[j].Name })

	// ---- flows ----
	for _, f := range flows {
		c.Flows = append(c.Flows, Flow{
			Reactor: f.Reactor,
			Cause:   f.CauseAggregate + "/" + f.CauseType,
			Target:  f.TargetAggregate + "/" + f.TargetType,
			Count:   f.Count,
		})
	}

	c.Diagram = c.Mermaid()

	return c, nil
}
