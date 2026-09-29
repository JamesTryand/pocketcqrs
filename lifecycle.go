package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// defaultDrainDeadline is --cqrsDrainDeadline's default: how long a node may
// spend draining on shutdown (in-flight requests, then the background loops:
// consumer engine, batch writer, pruners, finishing their in-flight unit)
// before the stores are closed under whatever is left. One deadline covers
// both. Kept well inside a typical supervisor's stop grace period (Docker's
// is 10s).
const defaultDrainDeadline = 5 * time.Second

// background owns the long-running loops main starts on serve, so the
// termination hook can tell them to stop and wait for them (the second half
// of the drain: readiness has closed and requests have finished by then).
type background struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newBackground() *background {
	ctx, cancel := context.WithCancel(context.Background())
	return &background{ctx: ctx, cancel: cancel}
}

// Go runs fn in a goroutine with the shared stop context. fn must return
// once that context is done.
func (b *background) Go(fn func(ctx context.Context)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		fn(b.ctx)
	}()
}

// Stop signals every loop to stop and waits up to timeout for them to
// return. It reports whether they all did; false means at least one is
// still running when the caller proceeds.
func (b *background) Stop(timeout time.Duration) bool {
	b.cancel()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// closeStores closes every store this binary opened itself (PocketBase's
// own DBs are closed by PocketBase). Nil-safe: a failed or partial
// bootstrap leaves some unset. Errors are logged, not returned — the
// process is exiting either way.
func (c *components) closeStores() {
	closeOne := func(name string, closeFn func() error) {
		if err := closeFn(); err != nil {
			log.Printf("shutdown: closing %s: %v", name, err)
		}
	}
	if c.commandQueue != nil {
		closeOne("commandqueue.db", c.commandQueue.Close)
	}
	if c.idempotency != nil {
		closeOne("idempotency.db", c.idempotency.Close)
	}
	if c.verifyCache != nil {
		closeOne("authverify.db", c.verifyCache.Close)
	}
	if c.checkpoints != nil {
		closeOne("checkpoints.db", c.checkpoints.Close)
	}
	if c.State != nil && c.Store != nil {
		closeOne("events.db", c.Store.Close)
	}
}
