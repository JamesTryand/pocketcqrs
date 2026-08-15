package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// DecidedCommand is one command's already-decided-but-not-yet-appended
// outcome, ready to be committed as part of a batch (item 4's batching
// event writer). NewEvents may be nil/empty -- a command that decided
// successfully with no resulting events is a valid outcome, not an error.
type DecidedCommand struct {
	// CommandID is stamped into every produced event's metadata as
	// "commandId" -- the durable proof, checked by CommandApplied, that
	// this command already ran.
	CommandID        string
	Aggregate        string
	AggregateID      string
	ExpectedSequence int64
	NewEvents        []NewEvent
}

// ErrBatchAborted is set on every result in a batch that rolled back
// because a DIFFERENT command in it conflicted -- this command's own
// ExpectedSequence was fine, but nothing in the batch was committed. A
// caller must treat this exactly like ErrConcurrency (leave the command
// pending, redecide next pass), not like "committed with zero events" --
// those two cases would otherwise look identical (Events nil, Err nil).
var ErrBatchAborted = errors.New("events: batch aborted by another command's conflict")

// BatchResult is one DecidedCommand's outcome after CommitBatch. Err == nil
// means committed -- Events may still be empty for a command that decided
// zero effect. Err != nil (ErrConcurrency or ErrBatchAborted) always means
// nothing was committed for this command in this call.
type BatchResult struct {
	CommandID string
	// Events is nil if Err is set, or if the command produced none.
	Events []Event
	// Err is events.ErrConcurrency if this command's ExpectedSequence did
	// not match the transaction's view when the batch was validated.
	Err error
}

// CommitBatch atomically appends every DecidedCommand's events in one
// transaction, stamping each event's metadata with its originating
// command's id. Held under the store's existing mutex for the whole call --
// the same lock and the same per-stream sequence check Append uses -- so a
// batch and a concurrent direct Append (e.g. a reactor dispatching outside
// the queue) on the same aggregate/id cannot silently interleave, exactly
// as today's individual Appends already can't.
//
// Two commands in the batch for the SAME stream validate correctly against
// each other: sequence numbers are tracked as a running, in-transaction
// value per stream, seeded from the database only the first time a stream
// is touched, so a later command's ExpectedSequence is checked against the
// earlier command's staged (not-yet-committed) effect, not a stale
// pre-batch snapshot.
//
// If ANY command in the batch fails its sequence check, the transaction is
// rolled back in full -- nothing is inserted, for any command, conflicting
// or not. CommitBatch does not retry internally: every command in a rolled
// -back batch remains exactly as undecided-as-far-as-the-store-can-tell as
// before the call, and the caller (the batching writer) is expected to
// simply leave them pending for its next pass, where they are freshly
// re-decided against then-current state -- not to surgically resubmit a
// reduced batch with stale ExpectedSequence values.
func (s *Store) CommitBatch(ctx context.Context, batch []DecidedCommand) ([]BatchResult, error) {
	if len(batch) == 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type streamKey struct{ aggregate, id string }
	running := map[streamKey]int64{}
	getCurrent := func(aggregate, aggregateID string) (int64, error) {
		key := streamKey{aggregate, aggregateID}
		if v, ok := running[key]; ok {
			return v, nil
		}
		var v int64
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE aggregate = ? AND aggregate_id = ?`,
			aggregate, aggregateID,
		).Scan(&v)
		return v, err
	}

	// Validation pass: every command's ExpectedSequence must match what
	// this transaction sees. Nothing is inserted yet.
	results := make([]BatchResult, len(batch))
	bases := make([]int64, len(batch))
	anyConflict := false
	for i, dc := range batch {
		results[i].CommandID = dc.CommandID

		current, err := getCurrent(dc.Aggregate, dc.AggregateID)
		if err != nil {
			return nil, err
		}
		if current != dc.ExpectedSequence {
			results[i].Err = fmt.Errorf("%w: stream %s/%s is at sequence %d, expected %d",
				ErrConcurrency, dc.Aggregate, dc.AggregateID, current, dc.ExpectedSequence)
			anyConflict = true
			continue
		}
		bases[i] = current
		running[streamKey{dc.Aggregate, dc.AggregateID}] = current + int64(len(dc.NewEvents))
	}
	if anyConflict {
		for i := range results {
			if results[i].Err == nil {
				results[i].Err = ErrBatchAborted
			}
		}
		return results, nil
	}

	// Insert pass: validation already confirmed every sequence is exactly
	// right, so this proceeds unconditionally in original batch order.
	for i, dc := range batch {
		if len(dc.NewEvents) == 0 {
			continue
		}
		base := bases[i]
		appended := make([]Event, 0, len(dc.NewEvents))
		for j, ne := range dc.NewEvents {
			ev := Event{
				ID:          newID(),
				Aggregate:   dc.Aggregate,
				AggregateID: dc.AggregateID,
				Sequence:    base + int64(j) + 1,
				Type:        ne.Type,
				Data:        ne.Data,
				Metadata:    stampCommandID(ne.Metadata, dc.CommandID),
				Version:     ne.Version,
			}
			if ev.Version <= 0 {
				ev.Version = 1
			}
			err = tx.QueryRowContext(ctx,
				`INSERT INTO events (id, aggregate, aggregate_id, sequence, type, data, metadata, version)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING position, created`,
				ev.ID, ev.Aggregate, ev.AggregateID, ev.Sequence, ev.Type, string(ev.Data), string(ev.Metadata), ev.Version,
			).Scan(&ev.Position, &ev.Created)
			if err != nil {
				return nil, err
			}
			appended = append(appended, ev)
		}
		results[i].Events = appended
	}

	if s.CommitBatchFault != nil {
		if err := s.CommitBatchFault(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	for i := range results {
		for _, ev := range results[i].Events {
			s.publish(ev)
		}
	}
	return results, nil
}

// CommandApplied reports whether an event carrying commandId in its
// metadata already exists for the given stream at position > afterPosition.
//
// This is the batching event writer's crash-recovery check: because
// CommitBatch stamps commandId into the same transaction that appends the
// event, the event's presence IS the durable proof the command already
// ran — there is no separate completion flag that could fall out of sync
// with it. Used only on a worker resuming after a restart, never on the
// hot append path; afterPosition (the log's head position at the moment
// the command was originally enqueued) bounds the scan to events appended
// since, not the whole log — see idx_events_command_id, the partial index
// this query is written to use.
func (s *Store) CommandApplied(ctx context.Context, aggregate, aggregateID, commandID string, afterPosition int64) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM events
		 WHERE aggregate = ? AND aggregate_id = ? AND position > ?
		   AND json_extract(metadata, '$.commandId') = ?
		 LIMIT 1`,
		aggregate, aggregateID, afterPosition, commandID,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// stampCommandID merges {"commandId": id} into existing metadata, the
// convention CommandApplied's restart-check query looks for. Existing keys
// are preserved.
func stampCommandID(existing json.RawMessage, id string) json.RawMessage {
	m := map[string]any{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &m)
	}
	m["commandId"] = id
	out, _ := json.Marshal(m)
	return out
}
