package decider

import (
	"context"
	"encoding/json"

	"github.com/jamestryand/pocketcqrs/events"
)

// Protector is an aggregate's personal-data hook, registered with
// RegisterProtected; nil (plain Register) is today's path, unchanged. It is
// I/O at the edges around a pure core: Decide and Evolve never see it, and
// it never sees inside them. Generated for every aggregate whose events
// carry pii fields (package pii supplies the parts).
//
//   - Reveal runs only when a decide returned ErrRevealRequired, i.e. it
//     read a stored value that is still ciphertext. It decrypts the state's
//     pending values and the command's sealed ones in one batch, returning
//     the revealed state and the command with its reveals attached
//     (Command.WithReveals); the registry then decides once more. Safe
//     because decide is pure. A decide that reads no personal data never
//     triggers it, so costs no round trip.
//   - Protect runs after a successful decide, before anything is encoded or
//     appended: it encrypts every fresh personal-data value in the events'
//     typed Payloads under its subject's key, and refuses (fails closed) a
//     value already sealed under a different subject than the event names.
type Protector[S any] interface {
	Reveal(ctx context.Context, state S, cmd Command) (S, Command, error)
	Protect(ctx context.Context, aggregateID string, evs []events.NewEvent) ([]events.NewEvent, error)
}

// Sealer encrypts a command payload's personal-data fields in place (each
// becomes a {"$pii":...} envelope) before anything durable sees the command.
// The batching writer calls it through Registry.Seal before enqueueing, so
// commandqueue.db never holds plaintext and erasure reaches queued commands
// too. A Protector that also implements Sealer is used for both.
type Sealer interface {
	Seal(ctx context.Context, cmd Command) (json.RawMessage, error)
}

// RegisterProtected is Register with a personal-data Protector (see
// Protector). A nil p is exactly Register.
func RegisterProtected[S any](r *Registry, aggregate string, d *Decider[S], p Protector[S]) {
	Register(r, aggregate, d)
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.deciders[aggregate]
	e.reveal = func(ctx context.Context, state any, cmd Command) (any, Command, error) {
		return p.Reveal(ctx, state.(S), cmd)
	}
	e.protect = p.Protect
	if s, ok := p.(Sealer); ok {
		e.seal = s.Seal
	}
	r.deciders[aggregate] = e
}

// Seal returns cmd's payload with the aggregate's personal-data fields
// encrypted, or the payload unchanged when the aggregate has no Sealer (or
// isn't registered — dispatch reports that). See Sealer.
func (r *Registry) Seal(ctx context.Context, aggregate string, cmd Command) (json.RawMessage, error) {
	r.mu.RLock()
	d, ok := r.deciders[aggregate]
	r.mu.RUnlock()
	if !ok || d.seal == nil {
		return cmd.Payload, nil
	}
	return d.seal(ctx, cmd)
}

// Reveal is one decrypted (or erased) sealed command value, attached to a
// Command by a Protector's Reveal for the second decide to read without any
// I/O. Plaintext is the value's JSON.
type Reveal struct {
	Plaintext []byte
	Redacted  bool
}

type revealKey struct{ subject, cipher string }

// WithReveals returns a copy of c carrying the given reveals, keyed by
// (subject, ciphertext). Never serialized: Command's JSON shape is
// unchanged, so a reveal can't reach any store.
func (c Command) WithReveals(reveals map[[2]string]Reveal) Command {
	m := make(map[revealKey]Reveal, len(reveals)+len(c.reveals))
	for k, v := range c.reveals {
		m[k] = v
	}
	for k, v := range reveals {
		m[revealKey{k[0], k[1]}] = v
	}
	c.reveals = m
	return c
}

// Revealed looks up a sealed command value revealed for this decide. A pure
// lookup — package pii's ResolveIn uses it to turn a pending command value
// into a known one inside Decide.
func (c Command) Revealed(subject, cipher string) (Reveal, bool) {
	r, ok := c.reveals[revealKey{subject, cipher}]
	return r, ok
}
