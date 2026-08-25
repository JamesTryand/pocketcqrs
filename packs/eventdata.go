package packs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jamestryand/pocketcqrs/events"
)

// eventsFile is the ndjson file holding a pack's exported event history,
// written into / read from the same directory as manifest.json. Event data
// is a separate, opt-in layer from the pack's code (pb_functions/,
// collections.json) — see events-db-slice-merge-scope.md.
const eventsFile = "events.ndjson"

// exportedEvent is the ndjson line shape: an events.Event with Position
// always omitted. Position is store-local (a global auto-increment specific
// to one events.db) and is never portable between deployments — the field
// is dropped, not merely left at its zero value, so a reader of the file
// never mistakes it for a meaningful number.
type exportedEvent struct {
	ID          string          `json:"id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregateId"`
	Sequence    int64           `json:"sequence"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Version     int64           `json:"version"`
	Created     string          `json:"created"`
}

// ExportEvents writes outFile (ndjson, one exportedEvent per line) covering
// every stream of every aggregate in aggregates, read RAW (pre-upcast, via
// Store.LoadStreamRaw — never Store.LoadStream: see that method's doc
// comment for why) and written in (aggregate, aggregateId, sequence) order —
// a stable, diffable, reviewable order, not a claim about cross-stream
// causal order (pack-portability-scope.md's Layer 4: positions are never
// interleaved on import either). Refuses, before writing anything, if
// aggregates is empty.
func ExportEvents(ctx context.Context, store *events.Store, aggregates []string, outFile string) (int, error) {
	if len(aggregates) == 0 {
		return 0, fmt.Errorf("packs: no aggregates to export events for (empty Aggregates list)")
	}
	names := append([]string(nil), aggregates...)
	sort.Strings(names)

	f, err := os.Create(outFile)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	count := 0
	for _, name := range names {
		ids, err := store.ListStreams(ctx, name)
		if err != nil {
			return count, fmt.Errorf("packs: list streams for %q: %w", name, err)
		}
		sort.Strings(ids)
		for _, id := range ids {
			evs, err := store.LoadStreamRaw(ctx, name, id)
			if err != nil {
				return count, fmt.Errorf("packs: load stream %s/%s: %w", name, id, err)
			}
			for _, ev := range evs {
				out := exportedEvent{
					ID: ev.ID, Aggregate: ev.Aggregate, AggregateID: ev.AggregateID,
					Sequence: ev.Sequence, Type: ev.Type, Data: ev.Data,
					Metadata: ev.Metadata, Version: ev.Version, Created: ev.Created,
				}
				raw, err := json.Marshal(out)
				if err != nil {
					return count, err
				}
				if _, err := w.Write(raw); err != nil {
					return count, err
				}
				if err := w.WriteByte('\n'); err != nil {
					return count, err
				}
				count++
			}
		}
	}
	if err := w.Flush(); err != nil {
		return count, err
	}
	return count, nil
}

// checkpointStore is a local, minimal mirror of consumers.CheckpointStore
// (Checkpoint/SaveCheckpoint) — *events.Store satisfies it structurally.
// Defined here, not imported from consumers, so packs gains no new
// dependency edge just for this: packs already depends on functions, and
// must not also depend on consumers.
type checkpointStore interface {
	Checkpoint(ctx context.Context, name string) (int64, error)
	SaveCheckpoint(ctx context.Context, name string, position int64) error
}

// ImportEventsResult summarizes an ImportEvents call (a real one or a
// dry run).
type ImportEventsResult struct {
	// DryRun is true when nothing was actually written (see the CLI's
	// --dry-run flag) — the rest of the fields describe what WOULD happen.
	DryRun bool
	// Imported is the number of events that were (or, on a dry run, would
	// be) inserted.
	Imported int
	// Streams lists the (aggregate, aggregateId) pairs touched, sorted.
	Streams []string
	// AdvancedCheckpoints maps consumer name -> the position its checkpoint
	// was (or would be) fast-forwarded to, for every effect-tier consumer
	// (reactors and event-triggered effect functions) skipped past the
	// imported batch. Pure projections are deliberately absent: they
	// replay the imported history instead of skipping it.
	AdvancedCheckpoints map[string]int64
}

// ImportEvents reads evtsFile (as written by ExportEvents), bulk-inserts via
// store.ImportEvents (refusing on any stream-key or aggregate-name
// collision), then advances every currently-registered EFFECT-tier
// consumer's checkpoint — reactors (name prefix "reactor:" or
// "fn-reactor:") AND event-triggered effect functions (prefix "fn:") — past
// the batch's max position, so nothing with a real-world side effect
// re-fires against events that already happened elsewhere. Pure projections
// (Go and JS) are deliberately excluded: they replay safely (the existing
// `projection rebuild` mechanism already proves this) and are exactly what
// should catch up on the imported history so the merged/sliced data
// actually shows up in read models.
//
// If dryRun is true, the collision check and the would-be checkpoint
// advances are computed and returned, but store.ImportEvents is never
// called and no checkpoint is saved — nothing is written.
//
// consumerNames is the caller's engine.Names() snapshot (a *consumers.Engine
// in the running app). An empty evtsFile (no events) is a no-op, matching
// Store.ImportEvents' own empty-batch no-op.
func ImportEvents(ctx context.Context, store *events.Store, checkpoints checkpointStore, consumerNames []string, evtsFile string, dryRun bool) (*ImportEventsResult, error) {
	f, err := os.Open(evtsFile)
	if err != nil {
		return nil, fmt.Errorf("packs: %s: %w", eventsFile, err)
	}
	defer f.Close()

	var batch []events.Event
	streamSet := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e exportedEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("packs: invalid %s line: %w", eventsFile, err)
		}
		batch = append(batch, events.Event{
			ID: e.ID, Aggregate: e.Aggregate, AggregateID: e.AggregateID,
			Sequence: e.Sequence, Type: e.Type, Data: e.Data,
			Metadata: e.Metadata, Version: e.Version, Created: e.Created,
		})
		streamSet[e.Aggregate+"/"+e.AggregateID] = true
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("packs: reading %s: %w", eventsFile, err)
	}

	result := &ImportEventsResult{DryRun: dryRun}
	if len(batch) == 0 {
		return result, nil
	}

	streams := make([]string, 0, len(streamSet))
	for k := range streamSet {
		streams = append(streams, k)
	}
	sort.Strings(streams)
	result.Streams = streams

	var maxPos int64
	if dryRun {
		// Runs the identical collision check ImportEvents itself uses
		// (Store.CheckImportCollisions), then computes the position the
		// batch WOULD land at from the store's current MaxPosition — never
		// calling Store.ImportEvents, so nothing is written even inside a
		// transaction that gets rolled back. A write landing between this
		// read and a real import can change the answer; that is an
		// accepted property of any preview, not a correctness bug here.
		if err := store.CheckImportCollisions(ctx, batch); err != nil {
			return nil, err
		}
		current, err := store.MaxPosition(ctx)
		if err != nil {
			return nil, err
		}
		maxPos = current + int64(len(batch))
		result.Imported = len(batch)
	} else {
		imported, err := store.ImportEvents(ctx, batch)
		if err != nil {
			return nil, err
		}
		result.Imported = len(imported)
		for _, ev := range imported {
			if ev.Position > maxPos {
				maxPos = ev.Position
			}
		}
	}

	advanced := map[string]int64{}
	for _, name := range consumerNames {
		if strings.HasPrefix(name, "reactor:") || strings.HasPrefix(name, "fn-reactor:") || strings.HasPrefix(name, "fn:") {
			advanced[name] = maxPos
			if !dryRun {
				if err := checkpoints.SaveCheckpoint(ctx, name, maxPos); err != nil {
					return nil, fmt.Errorf("packs: advancing checkpoint %q: %w", name, err)
				}
			}
		}
	}
	result.AdvancedCheckpoints = advanced
	return result, nil
}
