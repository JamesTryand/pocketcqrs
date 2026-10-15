// Package verifyharness runs a document's scenarios against Go code: the
// deciders and projections `schema import --lang go` generates, or a host's
// own. schema verify compiles it, with the code under test and a small
// generated test file naming that code's constructors, into a scratch
// module, and runs it once. It reads the decides and projection runs to
// answer (wire.Input) and writes the answers (wire.Output); emschema
// compares them with the scenarios, as it does for the JS dry run, so the
// two checks can't disagree about what a scenario means.
//
// Deciders run through the registry's DecideWithMeta over the scenario's own
// events, nothing appended, personal data sealed and revealed by an
// in-memory key service. Each view scenario gets its own PocketBase test app
// with every read model's collection, so a projection writes rows as it
// would live, and reads them back as PocketBase serves them.
package verifyharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/verifyharness/wire"
)

// Projection is what the harness needs of a Go projection.
type Projection interface {
	Apply(ctx context.Context, ev events.Event) error
}

// Spec is the code under test: the generated test file builds it.
type Spec struct {
	// Register registers every decider, protected ones with deps.
	Register func(r *decider.Registry, deps pii.Deps)
	// Projections builds each read model's projection, by collection.
	Projections map[string]func(app core.App) Projection
}

// Main reads wire.EnvIn, answers it and writes wire.EnvOut.
func Main(spec Spec) error {
	raw, err := os.ReadFile(os.Getenv(wire.EnvIn))
	if err != nil {
		return err
	}
	var in wire.Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	out := Answer(spec, in)
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(os.Getenv(wire.EnvOut), encoded, 0o644)
}

// Answer runs every decide and projection run in.
func Answer(spec Spec, in wire.Input) wire.Output { return answer(spec, in, kms.NewMemory()) }

func answer(spec Spec, in wire.Input, keys kms.Client) wire.Output {
	ctx := context.Background()
	deps := pii.Deps{KMS: keys, Cache: pii.NewCache(0)}
	reg := decider.NewRegistry(nil)
	spec.Register(reg, deps)

	var out wire.Output
	for _, c := range in.Commands {
		out.Commands = append(out.Commands, decide(ctx, reg, keys, c))
	}
	for _, v := range in.Views {
		out.Views = append(out.Views, project(ctx, spec, keys, in.Collections, v))
	}
	return out
}

// stream is a scenario's own events, as the registry loads a stream.
type stream []events.Event

func (s stream) LoadStream(context.Context, string, string) ([]events.Event, error) { return s, nil }

func decide(ctx context.Context, reg *decider.Registry, keys kms.Client, c wire.Command) (res wire.CommandResult) {
	res.ID = c.ID
	// a hand hook's stand-in panics its marker: the answer is HAND, which
	// emschema tells from the message, as it does for the JS stand-in
	defer func() {
		if r := recover(); r != nil {
			res = wire.CommandResult{ID: c.ID, Rejected: true, Message: fmt.Sprint(r)}
		}
	}()
	meta := map[string]any{}
	for k, v := range c.Meta {
		meta[k] = v
	}
	produced, _, err := reg.DecideWithMeta(ctx, stream(c.Given), c.Aggregate, c.Stream, c.Command, meta)
	if err != nil {
		var rejection *decider.RejectionError
		if errors.As(err, &rejection) {
			res.Rejected, res.Message = true, rejection.Err.Error()
			return res
		}
		res.Error = err.Error()
		return res
	}
	for _, e := range produced {
		data, err := reveal(ctx, keys, e.Data)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Produced = append(res.Produced, wire.Produced{Type: e.Type, Data: data})
	}
	return res
}

func reveal(ctx context.Context, keys kms.Client, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	plain, err := pii.RevealAll(ctx, keys, v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(plain)
}

// systemFields are PocketBase's own, not the read model's.
var systemFields = map[string]bool{"id": true, "collectionId": true, "collectionName": true, "created": true, "updated": true}

func project(ctx context.Context, spec Spec, keys kms.Client, collections []wire.Collection, v wire.View) (res wire.ViewResult) {
	res.ID = v.ID
	app, err := tests.NewTestApp()
	if err != nil {
		res.Error = "the test app did not start: " + err.Error()
		return res
	}
	defer app.Cleanup()
	byName := map[string]wire.Collection{}
	for _, c := range collections {
		byName[c.Name] = c
		col := core.NewBaseCollection(c.Name)
		for _, f := range c.Fields {
			col.Fields.Add(field(f))
		}
		if err := app.Save(col); err != nil {
			res.Error = fmt.Sprintf("creating %s: %v", c.Name, err)
			return res
		}
	}
	res.Rows = map[string]map[string]map[string]any{}
	for _, run := range v.Runs {
		build, ok := spec.Projections[run.Collection]
		if !ok {
			res.Error = fmt.Sprintf("no projection for %s", run.Collection)
			return res
		}
		p := build(app)
		for _, ev := range run.Events {
			if err := p.Apply(ctx, ev); err != nil {
				res.Error = fmt.Sprintf("%s failed at event %d (%s): %v", run.Collection, ev.Position, ev.Type, err)
				return res
			}
		}
		c := byName[run.Collection]
		rows, err := readRows(ctx, app, keys, c)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Rows[run.Collection] = rows
	}
	return res
}

func field(f wire.Field) core.Field {
	if f.PII {
		return &core.TextField{Name: f.Name}
	}
	switch f.Type {
	case "number":
		return &core.NumberField{Name: f.Name}
	case "bool":
		return &core.BoolField{Name: f.Name}
	case "date":
		return &core.DateField{Name: f.Name}
	case "json":
		return &core.JSONField{Name: f.Name}
	}
	return &core.TextField{Name: f.Name}
}

// readRows is a collection's rows as PocketBase serves them, keyed by the
// key field, personal data revealed.
func readRows(ctx context.Context, app core.App, keys kms.Client, c wire.Collection) (map[string]map[string]any, error) {
	recs, err := app.FindAllRecords(c.Name)
	if err != nil {
		return nil, err
	}
	piiCols := map[string]bool{}
	for _, f := range c.Fields {
		if f.PII {
			piiCols[f.Name] = true
		}
	}
	out := map[string]map[string]any{}
	for _, rec := range recs {
		raw, err := json.Marshal(rec.PublicExport())
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		for k := range systemFields {
			delete(row, k)
		}
		for name := range piiCols {
			text, ok := row[name].(string)
			if !ok || text == "" {
				row[name] = nil
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(text), &v); err != nil {
				return nil, fmt.Errorf("%s.%s is not an envelope's JSON: %w", c.Name, name, err)
			}
			if row[name], err = pii.RevealAll(ctx, keys, v); err != nil {
				return nil, err
			}
		}
		out[rec.GetString(c.Key)] = row
	}
	return out, nil
}
