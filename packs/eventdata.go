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
}

// ImportEvents reads evtsFile (as written by ExportEvents) and bulk-inserts
// via store.ImportEvents, refusing on any stream-key or aggregate-name
// collision. The store marks every imported event (events.ImportMarker):
// they already happened at the source deployment, side effects included, so
// effect-tier consumers (reactors, event-triggered functions, the external
// caller) fold them without acting on them, and projections apply them so
// the imported history shows up in read models (see consumers.Effect).
//
// No checkpoint is touched. This used to fast-forward every effect
// consumer's checkpoint past the batch, which also skipped whatever a
// lagging consumer had not reached before the import, and was saved after
// the import committed, so a running server could act on the imported
// events in between. The marker holds whatever a consumer's lag, on replay,
// and with a server running.
//
// If dryRun is true, the collision check runs and the counts are returned,
// but store.ImportEvents is never called: nothing is written. An empty
// evtsFile (no events) is a no-op, matching Store.ImportEvents' own
// empty-batch no-op.
func ImportEvents(ctx context.Context, store *events.Store, evtsFile string, dryRun bool) (*ImportEventsResult, error) {
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

	if dryRun {
		// Runs the identical collision check ImportEvents itself uses
		// (Store.CheckImportCollisions), then computes the position the
		// batch WOULD land at from the store's current MaxPosition — never
		// calling Store.ImportEvents, so nothing is written even inside a
		// transaction that gets rolled back. A write landing between this
		// read and a real import can change the answer; that is an
		// accepted property of any preview, not a correctness bug here.
		//
		// current + len(batch) assumes AUTOINCREMENT hands out exactly
		// len(batch) consecutive positions starting at current+1, which
		// holds only because nothing in this codebase ever deletes a row
		// from the events table (confirmed: no DELETE FROM events path
		// anywhere; projection rebuild wipes a projection's OWN pb
		// collections and checkpoint, never the log). If a future feature
		// ever deletes log rows, SQLite's sqlite_sequence (which
		// AUTOINCREMENT reads from) still reflects the highest position
		// ever used, not MAX(position) over what remains — this arithmetic
		// would then need Store to expose that value directly instead of
		// deriving it from MaxPosition.
		if err := store.CheckImportCollisions(ctx, batch); err != nil {
			return nil, err
		}
		result.Imported = len(batch)
		return result, nil
	}
	imported, err := store.ImportEvents(ctx, batch)
	if err != nil {
		return nil, err
	}
	result.Imported = len(imported)
	return result, nil
}
