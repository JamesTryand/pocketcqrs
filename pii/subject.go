package pii

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
)

// The built-in data-subject aggregate. Erasure isn't a property of any
// domain aggregate that happens to mention a person: an order's stream holds
// the customer's email, but the customer outlives the order and is erased
// independently of it. So the subject gets its own stream (id = subject id),
// and every reader learns of an erasure the way it learns anything else:
// from an event. Deliberately not modelled in eventmodelschema — erasure is
// a runtime concern, not something a document describes.
//
// Erased is terminal. A person who is erased and later returns is a NEW
// subject with a new id and a new key, never a reactivation: reusing the id
// gains nothing (a destroyed key's material is gone, so a new key of the
// same name can't read the old ciphertext) and would undo the erasure by
// reconnecting a live identity to the history erasure severed. Subject ids
// are therefore opaque, never derived from personal data, never reused.
const (
	SubjectAggregate    = "dataSubject"
	EraseSubjectCommand = "EraseSubject"
	SubjectErasedEvent  = "SubjectErased"
)

// SubjectState is the folded state of a data subject's stream.
type SubjectState struct{ Erased bool }

// DataSubject is the pure decider. The stream id IS the subject id, so
// nothing in the payload names it, and no personal data reaches this
// aggregate.
func DataSubject() *decider.Decider[SubjectState] {
	return &decider.Decider[SubjectState]{
		Commands:     []string{EraseSubjectCommand},
		InitialState: func() SubjectState { return SubjectState{} },
		Decide: func(cmd decider.Command, state SubjectState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case EraseSubjectCommand:
				// Idempotent: erasing an erased subject appends nothing, so a
				// retried erasure request is harmless.
				if state.Erased {
					return nil, nil
				}
				return []events.NewEvent{{Type: SubjectErasedEvent, Data: json.RawMessage(`{}`)}}, nil
			default:
				return nil, errors.New("unknown command: " + cmd.Name)
			}
		},
		Evolve: func(state SubjectState, ev events.Event) (SubjectState, error) {
			if ev.Type == SubjectErasedEvent {
				state.Erased = true
			}
			return state, nil
		},
	}
}

// RegisterDataSubjects registers the built-in aggregate. A host that stores
// any personal data needs it, or nothing can ever be erased.
func RegisterDataSubjects(r *decider.Registry) {
	decider.Register(r, SubjectAggregate, DataSubject())
}

// SubjectStatus answers "has this subject been erased?".
type SubjectStatus interface {
	IsErased(ctx context.Context, subjectID string) (bool, error)
}

// StoreSubjectStatus reads the subject's own stream from the local event
// store — the database the command is about to append to, so no network
// hop, and still correct while the key service is unreachable. Protectors
// take one and refuse to encrypt fresh personal data for an erased subject.
//
// What it doesn't close: a write that passes this check can still race an
// erasure that lands immediately afterwards. The facade closes that: it
// refuses to re-create a key for a subject it has destroyed one for (409),
// which kms.Client.EnsureKey surfaces as the same *kms.SubjectErasedError.
type StoreSubjectStatus struct{ Loader decider.StreamLoader }

func (s StoreSubjectStatus) IsErased(ctx context.Context, subjectID string) (bool, error) {
	stream, err := s.Loader.LoadStream(ctx, SubjectAggregate, subjectID)
	if err != nil {
		return false, err
	}
	for _, ev := range stream {
		if ev.Type == SubjectErasedEvent {
			return true, nil
		}
	}
	return false, nil
}

func isErasure(ev events.Event) bool {
	return ev.Type == SubjectErasedEvent && ev.Aggregate == SubjectAggregate
}

// KeyDestroyerName is the key destroyer's durable checkpoint name. The cache
// evictor reads it (RegisterCacheEvictor), so keep the two in step.
const KeyDestroyerName = "pii:key-destroyer"

// KeyDestroyer destroys a subject's key when their erasure lands in the log.
// A plain consumer rather than a reactor: it performs an external effect
// instead of dispatching a command. DestroyKey is idempotent and the engine
// retries a failed event, so at-least-once delivery is exactly right. Run it
// on the writer only.
type KeyDestroyer struct{ KMS kms.Client }

func (KeyDestroyer) Name() string { return KeyDestroyerName }

func (d KeyDestroyer) Apply(ctx context.Context, ev events.Event) error {
	if !isErasure(ev) {
		return nil
	}
	return d.KMS.DestroyKey(ctx, ev.AggregateID)
}

// CacheEvictor drops an erased subject's plaintext from this process's
// Cache when SubjectErased lands in the log. Its position belongs to the
// process, not the database, because the cache it empties is per-process: a
// durable, shared checkpoint would let one process advance another's
// position past an erasure its cache never saw. So it keeps its own,
// in-memory checkpoint (consumers.OwnCheckpoints). Register it with
// RegisterCacheEvictor, on every node.
type CacheEvictor struct {
	cache       *Cache
	checkpoints *consumers.MemoryCheckpoints
}

func (*CacheEvictor) Name() string { return "pii:cache-evictor" }

func (e *CacheEvictor) Checkpoints() consumers.CheckpointStore { return e.checkpoints }

func (e *CacheEvictor) Apply(_ context.Context, ev events.Event) error {
	if isErasure(ev) {
		e.cache.MarkErased(ev.AggregateID)
	}
	return nil
}

// RegisterCacheEvictor registers a CacheEvictor whose process-local position
// starts at the key destroyer's durable position in durable (the engine's
// checkpoint store).
//
// Why there: every erasure before that position already has its key
// destroyed, so a reveal for that subject gets "destroyed" from the facade
// and is marked erased rather than cached; every erasure after it, the
// evictor reads. Starting at the log head instead would miss an erasure the
// destroyer hasn't handled yet: its key is still live, so its plaintext
// could be cached with nothing left to evict it. If the destroyer has never
// run against this store the seed is 0 and the evictor scans the whole log
// once — slower, still safe.
func RegisterCacheEvictor(ctx context.Context, engine *consumers.Engine, cache *Cache, durable consumers.CheckpointStore) (*CacheEvictor, error) {
	start, err := durable.Checkpoint(ctx, KeyDestroyerName)
	if err != nil {
		return nil, err
	}
	ev := &CacheEvictor{cache: cache, checkpoints: &consumers.MemoryCheckpoints{Start: start}}
	engine.Register(ev)
	return ev, nil
}
