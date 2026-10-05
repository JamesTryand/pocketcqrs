package pii

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jamestryand/pocketcqrs/kms"
)

// Buffer is the batching half of the lazy/batched design. Reveal (or
// RevealRaw) queues a Pending value here; nothing reaches the facade until
// Flush, which sends one decrypt-batch call per subject (more past
// kms.MaxBatchItems), different subjects concurrently, matching the facade's
// one-key-per-batch-call shape.
//
// With a Cache, a reveal it can answer (a hit, or a subject it knows is
// erased) settles at once and never joins the batch; flushed plaintexts fill
// it, and a destroyed key marks the subject erased in it.
//
// One per request or replay pass — not a singleton (the Cache is). Create
// one, reveal what's needed, Flush once, discard. Not safe for concurrent
// use.
type Buffer struct {
	client    kms.Client
	cache     *Cache
	bySubject map[string][]pendingReveal
	order     []string
}

type pendingReveal struct {
	cipher string
	settle func(outcome) error
}

type outcome struct {
	redacted bool
	plain    []byte
}

// NewBuffer returns an empty buffer over client; cache may be nil.
func NewBuffer(client kms.Client, cache *Cache) *Buffer {
	return &Buffer{client: client, cache: cache, bySubject: map[string][]pendingReveal{}}
}

// Len is the number of reveals waiting for Flush.
func (b *Buffer) Len() int {
	n := 0
	for _, ps := range b.bySubject {
		n += len(ps)
	}
	return n
}

// enqueue settles at once when the cache can answer; otherwise it parks
// settle for Flush. The returned error is only ever from an immediate settle.
func (b *Buffer) enqueue(subject, cipher string, settle func(outcome) error) error {
	if b.cache != nil {
		if b.cache.IsErased(subject) {
			return settle(outcome{redacted: true})
		}
		if plain, ok := b.cache.Get(subject, cipher); ok {
			return settle(outcome{plain: plain})
		}
	}
	if _, seen := b.bySubject[subject]; !seen {
		b.order = append(b.order, subject)
	}
	b.bySubject[subject] = append(b.bySubject[subject], pendingReveal{cipher: cipher, settle: settle})
	return nil
}

// Reveal queues *p for the next Flush when it is Pending, which then sets
// *p to Known or Redacted. Any other state is left alone. A cache hit
// settles *p immediately; the only error returned here is from that (a
// plaintext that doesn't decode as T).
func Reveal[T any](b *Buffer, p *Value[T]) error {
	if p.state != Pending {
		return nil
	}
	subject, cipher := p.subject, p.cipher
	return b.enqueue(subject, cipher, func(o outcome) error {
		if o.redacted {
			*p = NewRedacted[T](subject)
			return nil
		}
		v, err := decode[T](o.plain)
		if err != nil {
			return fmt.Errorf("pii: subject %q: %w", subject, err)
		}
		*p = Value[T]{state: Known, subject: subject, cipher: cipher, v: v}
		return nil
	})
}

// RevealRaw queues one envelope's ciphertext without knowing the field's Go
// type, for the read side; settle receives the plaintext JSON, or redacted.
func (b *Buffer) RevealRaw(subject, cipher string, settle func(plain []byte, redacted bool)) {
	_ = b.enqueue(subject, cipher, func(o outcome) error {
		settle(o.plain, o.redacted)
		return nil
	})
}

// Flush decrypts everything queued. A failed call fails only its own
// subject's values (they stay Pending), and a per-item facade error fails
// only that value; the returned error joins them all, each a
// *kms.ProtocolError or the transport error, so kms.IsUnavailable holds for
// it. Flushing with nothing queued is a no-op.
func (b *Buffer) Flush(ctx context.Context) error {
	if len(b.bySubject) == 0 {
		return nil
	}
	work, order := b.bySubject, b.order
	b.bySubject, b.order = map[string][]pendingReveal{}, nil

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	report := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, subject := range order {
		wg.Add(1)
		go func(subject string, pending []pendingReveal) {
			defer wg.Done()
			b.flushSubject(ctx, subject, pending, report)
		}(subject, work[subject])
	}
	wg.Wait()
	return errors.Join(errs...)
}

// flushSubject runs a subject's chunks in order, so a destroyed key found by
// the first chunk settles the rest without another call.
func (b *Buffer) flushSubject(ctx context.Context, subject string, pending []pendingReveal, report func(error)) {
	settle := func(p pendingReveal, o outcome) {
		if err := p.settle(o); err != nil {
			report(err)
		}
	}
	for start := 0; start < len(pending); start += kms.MaxBatchItems {
		chunk := pending[start:min(start+kms.MaxBatchItems, len(pending))]
		ciphers := make([]string, len(chunk))
		for i, p := range chunk {
			ciphers[i] = p.cipher
		}
		items, state, err := b.client.DecryptBatch(ctx, subject, ciphers)
		if err != nil {
			report(fmt.Errorf("pii: reveal for subject %q: %w", subject, err))
			continue
		}
		if state == kms.KeyDestroyed {
			if b.cache != nil {
				b.cache.MarkErased(subject)
			}
			for _, p := range pending[start:] {
				settle(p, outcome{redacted: true})
			}
			return
		}
		for i, p := range chunk {
			it := items[i]
			if !it.Succeeded() {
				report(&kms.ProtocolError{Msg: fmt.Sprintf("decrypt-batch item error for subject %q: %s", subject, it.Err)})
				continue
			}
			if b.cache != nil {
				b.cache.Put(subject, p.cipher, it.Value)
			}
			settle(p, outcome{plain: it.Value})
		}
	}
}
