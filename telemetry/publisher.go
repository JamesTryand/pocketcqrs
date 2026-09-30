package telemetry

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamestryand/pocketcqrs/opsport"
)

// DefaultPublishTimeout is contract section 8.5: a publish not completed within
// a second is dropped.
const DefaultPublishTimeout = time.Second

// Publisher is the optional telemetry push (contract section 8;
// STATE-MACHINES.md, machine 5): a snapshot as soon as the node has an
// identity, then every Interval, and once more when the node begins draining,
// so a monitor sees not_ready/draining rather than a node that went quiet.
//
// Best-effort, and never a dependency. A snapshot that cannot be sent within
// PublishTimeout is dropped: never retried, never queued while the bus is
// away. A failure is logged once when it starts and once when it ends, and
// changes nothing else: it never reaches /healthz or /readyz, and never slows
// a command or an event, since it runs on its own loop and only reads.
type Publisher struct {
	health         *opsport.Health
	transport      Transport
	interval       time.Duration
	logf           func(string, ...any)
	PublishTimeout time.Duration
	now            func() time.Time
	identityPoll   time.Duration

	one     sync.Mutex // one snapshot at a time
	failing bool

	published atomic.Int64
	dropped   atomic.Int64

	finalOnce sync.Once
	final     sync.WaitGroup
}

// NewPublisher builds a publisher; logf may be nil.
func NewPublisher(h *opsport.Health, t Transport, interval time.Duration, logf func(string, ...any)) *Publisher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Publisher{health: h, transport: t, interval: interval, logf: logf,
		PublishTimeout: DefaultPublishTimeout, now: time.Now, identityPoll: 100 * time.Millisecond}
}

// Published and Dropped count snapshots so far (for tests and logs).
func (p *Publisher) Published() int64 { return p.published.Load() }
func (p *Publisher) Dropped() int64   { return p.dropped.Load() }

// Run publishes until ctx ends: it waits for an identity (the key is the node
// id, known once identity resolves during boot), publishes at once, then on the
// interval. It returns when ctx ends and does not wait for a final snapshot:
// Close does.
func (p *Publisher) Run(ctx context.Context) {
	for p.health.Identity() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.identityPoll):
		}
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		p.publishOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// NotifyDraining is the node beginning to drain: publish one more snapshot
// now (its readiness already says draining), without waiting for the interval.
// It returns at once; Close waits for it.
func (p *Publisher) NotifyDraining() {
	p.finalOnce.Do(func() {
		p.final.Add(1)
		go func() {
			defer p.final.Done()
			p.publishOnce(context.Background())
		}()
	})
}

// Close waits for a final snapshot still going out (bounded by PublishTimeout)
// and releases the transport. Stop Run first.
func (p *Publisher) Close() error {
	p.final.Wait()
	return p.transport.Close()
}

// publishOnce builds and sends one snapshot, or drops it. It never fails.
func (p *Publisher) publishOnce(stop context.Context) {
	id := p.health.Identity()
	if id == nil {
		return
	}
	p.one.Lock()
	defer p.one.Unlock()
	ctx, cancel := context.WithTimeout(stop, p.PublishTimeout)
	defer cancel()
	payload := Payload(p.health.Metrics().Snapshot(ctx, p.health), id.NodeID, id.Role, p.now())
	err := p.transport.Publish(ctx, id.NodeID, payload)
	switch {
	case err == nil:
		p.published.Add(1)
		if p.failing {
			p.failing = false
			p.logf("telemetry: publishing resumed")
		}
	case stop.Err() != nil:
		// stopping: not a failure of the bus
	default:
		p.dropped.Add(1)
		if !p.failing {
			p.failing = true
			p.logf("telemetry: snapshot dropped, the bus is unreachable (%s); further drops are silent until it recovers", describe(err))
		}
	}
}

func describe(err error) string {
	if err == context.DeadlineExceeded {
		return "timed out"
	}
	return fmt.Sprint(err)
}
