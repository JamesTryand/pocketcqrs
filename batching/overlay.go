package batching

import (
	"context"

	"github.com/jamestryand/pocketcqrs/events"
)

// overlay makes not-yet-committed events from earlier commands in the same
// batch window visible to a later command's decide call, transparently:
// deciders never see it, they just call LoadStream through it like any
// other events.Store. Without this, two commands for the same aggregate
// stream landing in one window would both decide against the same durable
// snapshot and both believe they produce the next sequence number.
//
// Purely an in-memory, per-window cache -- never a source of truth. On a
// crash before the window commits, it's rebuilt from nothing on the next
// pass by replaying the same still-pending queue rows in the same order,
// which is what makes the pre-commit crash case safe by construction (see
// events.CommitBatch's doc comment for the corresponding post-commit case).
type overlay struct {
	store   *events.Store
	pending map[string][]events.Event
}

func newOverlay(store *events.Store) *overlay {
	return &overlay{store: store, pending: map[string][]events.Event{}}
}

func streamKey(aggregate, id string) string { return aggregate + "\x00" + id }

// LoadStream satisfies decider.StreamLoader: the durable stream plus
// whatever this window has already decided for it, in order.
func (o *overlay) LoadStream(ctx context.Context, aggregate, id string) ([]events.Event, error) {
	durable, err := o.store.LoadStream(ctx, aggregate, id)
	if err != nil {
		return nil, err
	}
	pending := o.pending[streamKey(aggregate, id)]
	if len(pending) == 0 {
		return durable, nil
	}
	out := make([]events.Event, 0, len(durable)+len(pending))
	out = append(out, durable...)
	out = append(out, pending...)
	return out, nil
}

// record stages a just-decided command's events as pending for the rest of
// this window, so a later command for the same stream sees them via
// LoadStream. baseSequence is the sequence the command was decided
// against (durable + prior-pending count at decide time).
func (o *overlay) record(aggregate, id string, newEvents []events.NewEvent, baseSequence int64) {
	if len(newEvents) == 0 {
		return
	}
	staged := make([]events.Event, len(newEvents))
	for i, ne := range newEvents {
		staged[i] = events.Event{
			Aggregate:   aggregate,
			AggregateID: id,
			Sequence:    baseSequence + int64(i) + 1,
			Type:        ne.Type,
			Data:        ne.Data,
			Metadata:    ne.Metadata,
			Version:     ne.Version,
		}
	}
	key := streamKey(aggregate, id)
	o.pending[key] = append(o.pending[key], staged...)
}
