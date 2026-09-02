package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// LoadDeciderSource loads a JS decider from source held in memory — the
// editor's candidate code, which has not been written to disk (and may never
// be, if the dry-run says it should not be).
func LoadDeciderSource(rt *GojaRuntime, name, src string) (*DeciderSpec, error) {
	t, err := parseTriggers(src)
	if err != nil {
		return nil, fmt.Errorf("functions: %s: %w", name, err)
	}
	if t.decider == "" {
		return nil, fmt.Errorf("functions: %s: missing //@trigger decider directive", name)
	}
	return buildDeciderSpec(rt, name, src, t)
}

// LoadProjectionSource loads a JS projection from source held in memory.
func LoadProjectionSource(rt *GojaRuntime, app core.App, name, src string) (*ProjectionSpec, error) {
	t, err := parseTriggers(src)
	if err != nil {
		return nil, fmt.Errorf("functions: %s: %w", name, err)
	}
	if t.projection == "" {
		return nil, fmt.Errorf("functions: %s: missing //@trigger projection directive", name)
	}
	return buildProjectionSpec(rt, app, name, src, t)
}

// LoadDeciderFile loads a single JS decider file (for the dryrun CLI).
func LoadDeciderFile(rt *GojaRuntime, path string) (*DeciderSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadDeciderSource(rt, filepath.Base(path), string(raw))
}

// LoadProjectionFile loads a single JS projection file (for the dryrun CLI).
func LoadProjectionFile(rt *GojaRuntime, app core.App, path string) (*ProjectionSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadProjectionSource(rt, app, filepath.Base(path), string(raw))
}

// DeciderDryRun reports a decider dry-run over existing history.
type DeciderDryRun struct {
	Aggregate string
	Streams   int
	Events    int
	// State is the folded final state (only set for single-stream runs).
	State any
}

// DryRunDecider folds existing streams of the spec's aggregate through the
// candidate code without appending anything. streamID limits the run to one
// stream ("" = all streams). Evolve errors fail the run.
func DryRunDecider(store *events.Store, spec *DeciderSpec, streamID string) (*DeciderDryRun, error) {
	ctx := context.Background()
	d := spec.UntypedDecider()
	out := &DeciderDryRun{Aggregate: spec.Aggregate}

	var ids []string
	if streamID != "" {
		ids = []string{streamID}
	} else {
		var err error
		ids, err = store.ListStreams(ctx, spec.Aggregate)
		if err != nil {
			return nil, err
		}
	}

	for _, id := range ids {
		stream, err := store.LoadStream(ctx, spec.Aggregate, id)
		if err != nil {
			return nil, err
		}
		state := d.Initial()
		for _, ev := range stream {
			// upcast explicitly with the CANDIDATE spec: new transforms
			// under test must fire even if the store's upcaster (built
			// from the deployed specs) does not know them yet (idempotent
			// otherwise — transforms fire only on exact version match)
			if ev, err = spec.upcast(ev); err != nil {
				return nil, fmt.Errorf("dryrun: upcasting %s/%s failed at %s#%d: %w",
					spec.Aggregate, id, ev.Type, ev.Sequence, err)
			}
			if !contains(spec.Handles, ev.Type) {
				return nil, fmt.Errorf("dryrun: stream %s contains %s which is not declared in //@handles",
					id, ev.Type)
			}
			if state, err = d.Evolve(state, ev); err != nil {
				return nil, fmt.Errorf("dryrun: folding %s/%s failed at %s#%d: %w",
					spec.Aggregate, id, ev.Type, ev.Sequence, err)
			}
			out.Events++
		}
		out.Streams++
		if streamID != "" {
			out.State = state
		}
	}
	return out, nil
}

// DecideDryRun reports what a command WOULD do against a stream.
//
// A domain rejection is a RESULT here, not an error. "The decider refuses
// this" is a successful answer to "what would happen?", and it is the
// ordinary answer for half the cases worth testing — so a caller must be
// able to tell it apart from a candidate that will not load, a stream that
// cannot be read, or history that will not fold. Those stay errors.
//
// Collapsing the two is exactly the failure this type exists to prevent: the
// editor could not distinguish "your source is broken" from "your source
// correctly refused", and an eventmodelschema `error` scenario — whose
// EXPECTED outcome is a rejection — could not be asserted at all.
type DecideDryRun struct {
	// Produced is what decide() returned; empty when Rejected.
	Produced []events.NewEvent
	// Rejected reports that decide() refused the command: a domain verdict.
	Rejected bool
	// Message is the rejection's text. Only set when Rejected.
	Message string
}

// DryRunDecide folds one stream to its current state and runs decide with
// the given command, reporting what it WOULD produce — or that the decider
// refused. Nothing is appended. This is the "given (prior events), when
// (command), then (outcome messages)" half of the harness.
func DryRunDecide(store *events.Store, spec *DeciderSpec, streamID string, cmd decider.Command, meta map[string]any) (*DecideDryRun, error) {
	// the contract gate: a candidate that cannot fold real history is
	// unusable, which is a failure of the REQUEST, not a verdict on the
	// command
	if _, err := DryRunDecider(store, spec, ""); err != nil {
		return nil, err
	}

	ctx := context.Background()
	d := spec.UntypedDecider()
	stream, err := store.LoadStream(ctx, spec.Aggregate, streamID)
	if err != nil {
		return nil, err
	}
	state := d.Initial()
	for _, ev := range stream {
		if ev, err = spec.upcast(ev); err != nil {
			return nil, fmt.Errorf("dryrun: upcasting %s/%s failed at %s#%d: %w",
				spec.Aggregate, streamID, ev.Type, ev.Sequence, err)
		}
		if state, err = d.Evolve(state, ev); err != nil {
			return nil, fmt.Errorf("dryrun: folding %s/%s failed at %s#%d: %w",
				spec.Aggregate, streamID, ev.Type, ev.Sequence, err)
		}
	}

	// from here down, and ONLY here, a failure is the domain's answer
	produced, err := d.Decide(cmd, state, meta)
	if err != nil {
		return &DecideDryRun{Rejected: true, Message: err.Error()}, nil
	}
	return &DecideDryRun{Produced: produced}, nil
}

// ProjectionDryRun reports a projection simulation over history.
type ProjectionDryRun struct {
	Name    string
	Events  int
	Upserts int
	Deletes int
	// IgnoredValues counts values project() returned that were not row ops.
	// They are dropped at runtime too, so a projection that returns plain
	// rows writes nothing — this is what lets a dry run say so out loud
	// rather than just reporting zero upserts.
	IgnoredValues int
	// Rows is the final simulated row set per collection
	// (collection -> key -> fields), mirroring applyOp semantics (upsert
	// merges, increment adds to the running value, delete removes,
	// groupByBump finds-or-creates a nested entry and adds to ITS running
	// value).
	Rows map[string]map[string]map[string]any
}

// DryRunProjection runs a candidate projection over the whole event log
// in memory — no collections are read (except via the projection's own
// pb.query calls) and nothing is written.
func DryRunProjection(store *events.Store, spec *ProjectionSpec) (*ProjectionDryRun, error) {
	ctx := context.Background()
	var log []events.Event
	var pos int64
	for {
		batch, err := store.Poll(ctx, pos, 100)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, ev := range batch {
			pos = ev.Position
			log = append(log, ev)
		}
	}
	return runProjectionOver(spec, log, false)
}

// DryRunProjectionOver runs a candidate projection over the SUPPLIED events
// instead of the real log — "given exactly these events, what rows result?".
//
// Reads are ISOLATED: the projection's `pb` binding returns nothing. A
// read-modify-write projection querying live collections mid-simulation
// would give an answer that depends on the real database, which is how a
// fixture assertion passes vacuously. That makes this stricter than
// DryRunProjection, which deliberately keeps live reads because it is
// simulating against real history.
//
// Useful well beyond schema work: it reproduces a projection defect from a
// hand-written log, with no instance state involved at all.
func DryRunProjectionOver(spec *ProjectionSpec, fixture []events.Event) (*ProjectionDryRun, error) {
	return runProjectionOver(spec, fixture, true)
}

func runProjectionOver(spec *ProjectionSpec, log []events.Event, isolated bool) (*ProjectionDryRun, error) {
	out := &ProjectionDryRun{Name: spec.Name, Rows: map[string]map[string]map[string]any{}}
	for _, ev := range log {
		if !contains(spec.EventTypes, ev.Type) {
			continue
		}
		out.Events++

		result, err := spec.runtime.runProjectionWith(spec, ev, isolated)
		if err != nil {
			return nil, fmt.Errorf("dryrun: projection %s failed at event %d: %w", spec.Name, ev.Position, err)
		}
		ops, ignored, err := normalizeOps(result)
		if err != nil {
			return nil, fmt.Errorf("dryrun: projection %s: %w", spec.Name, err)
		}
		out.IgnoredValues += ignored
		for _, op := range ops {
			s, err := spec.resolveSchema(op)
			if err != nil {
				return nil, fmt.Errorf("dryrun: projection %s: %w", spec.Name, err)
			}
			rows := out.Rows[s.Collection]
			if rows == nil {
				rows = map[string]map[string]any{}
				out.Rows[s.Collection] = rows
			}
			key := fmt.Sprint(op.key)
			if op.delete {
				out.Deletes++
				delete(rows, key)
				continue
			}
			row := rows[key]
			if row == nil {
				row = map[string]any{s.Key: op.key}
				rows[key] = row
			}

			if op.increment {
				out.Upserts++
				var current float64
				if row[op.incField] != nil {
					current, _ = toFloat(row[op.incField])
				}
				row[op.incField] = current + op.delta
				continue
			}

			if op.groupBy {
				out.Upserts++
				rows, _ := row[op.groupByField].([]map[string]any)
				idx := groupByRowIndex(rows, op.groupKeyField, op.groupKeyValue)
				if idx < 0 {
					rows = append(rows, map[string]any{op.groupKeyField: op.groupKeyValue})
					idx = len(rows) - 1
				}
				current, _ := toFloat(rows[idx][op.subfield])
				rows[idx][op.subfield] = current + op.delta
				row[op.groupByField] = rows
				continue
			}

			out.Upserts++
			for name, value := range op.fields {
				if reservedRowFields[name] || name == s.Key {
					continue
				}
				row[name] = value
			}
		}
	}
	return out, nil
}

// DiffRows compares a simulated row set against the live collection rows
// (PublicExport shape, keyed by the key field) and returns human-readable
// difference lines (empty = identical).
func DiffRows(simulated map[string]map[string]any, live []map[string]any, keyField string) []string {
	var diffs []string
	liveByKey := map[string]map[string]any{}
	for _, row := range live {
		liveByKey[fmt.Sprint(row[keyField])] = row
	}
	for key, simRow := range simulated {
		liveRow, ok := liveByKey[key]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("- row %s=%s missing from live collection", keyField, key))
			continue
		}
		for name, simVal := range simRow {
			if name == keyField {
				continue
			}
			liveVal := liveRow[name]
			if !jsonEqual(simVal, liveVal) {
				diffs = append(diffs, fmt.Sprintf("~ row %s=%s field %s: simulated %v, live %v",
					keyField, key, name, simVal, liveVal))
			}
		}
	}
	for key := range liveByKey {
		if _, ok := simulated[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("+ live row %s=%s not produced by the simulation", keyField, key))
		}
	}
	return diffs
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
